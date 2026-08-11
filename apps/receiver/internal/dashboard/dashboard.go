// Package dashboard is the operator-facing UI (QUB-111).
//
// Server-rendered HTML using Go's stdlib html/template
// plus HTMX for incremental updates. The same binary
// (boop-receiver) serves the dashboard; no new service,
// no SPA build, no JS framework. One auth token
// (BOOP_DASHBOARD_TOKEN) gates every /dashboard/* route.
//
// Why server-rendered and not a React/Vue SPA: the
// dashboard is an internal tool with a single user (the
// on-call operator), 6 views, and a refresh cadence of
// "every few seconds during an incident, then nothing".
// The build complexity of a SPA is not justified by the
// surface area. HTMX gives us partial updates without
// shipping a JS framework.
//
// Build order (the spec's order): runs list → run
// detail → live ops → exception dock → costs & lenses →
// installations. Each view is a single template + a
// single Go function. New views are 50-100 lines, not
// a Redux saga.
//
// DP-006: the view handlers used to live in this one
// file. They are now split across views_runs.go (the
// run-list + run-detail shape), views_ops.go (live /
// exceptions / costs / retention), and views_admin.go
// (audit / installations / re-run / zero-cost /
// mark-orphaned / health). DTOs and conversions live in
// views.go. This file is the constructor + the route
// table — the small kernel every other file relies on.
package dashboard

import (
	"context"
	"crypto/subtle"
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"github.com/michaelruelas/boop-receiver/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// Actions is the dependency the dashboard pulls from the
// webhook package. The dashboard's form-based actions (re-run,
// zero-out cost) need the K8s jobbuilder, which lives in
// webhook. The dashboard has no business knowing about K8s, so
// the action is a callback the receiver wires up at startup;
// the dashboard only sees the function signature.
//
// CreateRerunJob persists the new run row, creates the K8s Job,
// and returns the new run id. A nil-able set of fields in cfg
// or store is treated as "feature disabled" and the callback
// returns an error so the dashboard can render a 503-style
// "not configured" page rather than silently dropping the
// action.
//
// FetchPodLogs is the on-demand log fetcher the run-detail
// page uses to surface the runner's K8s pod logs. Returns the
// raw log bytes (the runner emits one JSON line per stage
// transition, so the dashboard renders them verbatim) and a
// not-found-style error if the Job's pod is already TTL'd out
// of the namespace. The dashboard treats any error from this
// callback as "logs unavailable" rather than 5xx-ing the page.
type Actions struct {
	CreateRerunJob func(ctx context.Context, prior store.Run, reason string) (newRunID string, err error)
	FetchPodLogs   func(ctx context.Context, jobName string) (logs string, err error)
}

// Handler is the receiver's dashboard endpoint group.
// One Handler per receiver process; the routes are
// mounted under /dashboard/* in main.go and gated by
// BOOP_DASHBOARD_TOKEN.
type Handler struct {
	// store is the read+write data layer the
	// dashboard's view handlers consume. Typed as
	// the Store interface (SP-005) so a test
	// double can stand in for *store.Store in
	// the unit tests; the production wiring
	// passes a real *store.Store.
	store  Store
	logger *slog.Logger
	token  string
	// actions are the cross-package dependencies the
	// dashboard needs to do more than render. Re-run
	// is the only one today; a future "drain queue"
	// or "rotate webhook secret" button would land
	// here too. Nil is fine for a read-only deploy.
	actions Actions
}

// NewHandler builds a dashboard Handler. token is the
// shared secret for the BOOP_DASHBOARD_TOKEN gate; an
// empty token rejects every request — the dashboard is
// opt-in, like the data layer. actions wires the
// cross-package dependencies; passing the zero value
// disables form-initiated K8s actions (the re-run
// button renders disabled and the form POSTs to a
// 503-style "not configured" page).
func NewHandler(st Store, logger *slog.Logger, token string, actions Actions) (*Handler, error) {
	return &Handler{
		store:   st,
		logger:  logger,
		token:   token,
		actions: actions,
	}, nil
}

// renderPage parses the layout + the page-specific
// template together for each request. Per-request
// parsing avoids the parse-time "last define" race
// that hits the base set: when all templates share
// the "title" and "content" block names, the base
// ParseFS makes whichever file was parsed last win
// for every page (alphabetic: runs.html). Parsing
// just two files per request — the layout + the
// page — keeps the page's defines the LAST to land
// in the set, so the page renders its own content.
//
// The cost is two small files re-parsed per request.
// Both are a few KB; the parse is well under a
// millisecond. If profiling later shows this is the
// hot path, the fix is to cache the parsed set per
// page and Clone + reparse the page's defines into
// the cache on each request.
func (h *Handler) renderPage(w http.ResponseWriter, page string, data any) {
	tmpl, err := template.ParseFS(templateFS, "templates/layout.html", "templates/"+page)
	if err != nil {
		h.logger.Warn("dashboard parse", "page", page, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	if err := tmpl.ExecuteTemplate(w, page, data); err != nil {
		h.logger.Warn("dashboard render", "page", page, "err", err)
	}
}

// Middleware gates the /dashboard/* routes with the
// BOOP_DASHBOARD_TOKEN. The check is constant-time and
// the token is compared verbatim against the
// X-Boop-Dashboard-Token header. An empty token means
// the dashboard is disabled (every request gets 401).
func (h *Handler) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.token == "" {
			http.Error(w, "dashboard disabled", http.StatusUnauthorized)
			return
		}
		got := r.Header.Get("X-Boop-Dashboard-Token")
		// Constant-time compare; missing header fails
		// fast because the compare returns 0 on
		// length-mismatch.
		if subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RegisterRoutes wires /dashboard/* on the given mux.
// The middleware is applied to every route.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /dashboard/", h.Middleware(http.HandlerFunc(h.route)))
	mux.Handle("POST /dashboard/", h.Middleware(http.HandlerFunc(h.route)))
}

func (h *Handler) route(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/dashboard")
	path = strings.TrimPrefix(path, "/")
	// Route table — small, ordered most-specific first.
	switch {
	case path == "":
		// /dashboard → redirect to runs list.
		http.Redirect(w, r, "/dashboard/runs", http.StatusSeeOther)
	case path == "runs":
		h.serveRuns(w, r)
	case strings.HasPrefix(path, "runs/"):
		rest := strings.TrimPrefix(path, "runs/")
		// /dashboard/runs/{id}/rerun and
		// /dashboard/runs/{id}/zero-cost are the two
		// form-based action endpoints the exceptions
		// view posts to. Each takes a different
		// verb-suffix split.
		if strings.HasSuffix(rest, "/rerun") && r.Method == http.MethodPost {
			h.serveRerun(w, r, strings.TrimSuffix(rest, "/rerun"))
			return
		}
		if strings.HasSuffix(rest, "/zero-cost") && r.Method == http.MethodPost {
			h.serveZeroCost(w, r, strings.TrimSuffix(rest, "/zero-cost"))
			return
		}
		h.serveRunDetail(w, r, rest)
	case path == "live":
		h.serveLive(w, r)
	case path == "exceptions":
		h.serveExceptions(w, r)
	case path == "costs":
		h.serveCosts(w, r)
	case path == "retention":
		h.serveRetention(w, r)
	case path == "audit":
		h.serveAudit(w, r)
	case path == "installations":
		h.serveInstallations(w, r)
	case strings.HasPrefix(path, "installations/"):
		h.serveInstallationControl(w, r, strings.TrimPrefix(path, "installations/"))
	case strings.HasPrefix(path, "admin/") && r.Method == http.MethodPost:
		// QUB-114: operator-facing admin actions.
		// Today the only endpoint is /admin/mark-orphaned
		// which bulk-marks old, never-heartbeated runs as
		// failed so the dashboard's "live" view doesn't
		// accumulate zombies after a runner telemetry
		// regression. Auth is the same BOOP_DASHBOARD_TOKEN
		// middleware applied to the whole /dashboard/* tree.
		rest := strings.TrimPrefix(path, "admin/")
		switch rest {
		case "mark-orphaned":
			h.serveMarkOrphaned(w, r)
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

// actor derives the audit-log actor from the
// BOOP_DASHBOARD_TOKEN. Today the dashboard's only
// authentication is a shared secret, so the actor is
// a SHA-256 prefix of the token (store.ActorFromToken)
// — stable across requests (same operator, same actor)
// and non-reversible (the token is not in the audit
// log). A future per-user identity layer replaces the
// token-derived actor; the AuditEvent.Actor field is
// already a free-form string so the swap is a
// one-call-site change.
func (h *Handler) actor() string {
	if h.token == "" {
		return "dashboard:disabled"
	}
	return store.ActorFromToken("dashboard:", h.token)
}
