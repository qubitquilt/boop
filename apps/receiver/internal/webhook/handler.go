// Package webhook implements the GitHub webhook receiver.
//
// Responsibilities:
//   - Verify the X-Hub-Signature-256 HMAC on every request.
//   - Filter to pull_request events (opened/reopened/synchronize) and
//     issue_comment events that ask for a review with `@BoopPr review`.
//   - Render the Job template and submit it via the in-cluster K8s API.
//   - Be idempotent: re-deliveries for the same head SHA are no-ops.
//   - Number every review so re-reviews get their own header instead
//     of rewriting the original.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v68/github"
	"golang.org/x/time/rate"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	boopgithub "github.com/michaelruelas/boop-receiver/internal/github"
	"github.com/michaelruelas/boop-receiver/internal/store"
)

const BotMention = "@BoopPr"

// reviewRequestRegex matches an explicit request to Boop to review
// the PR. We accept the literal phrase plus a small set of natural
// request modifiers so common phrasings work.
//
// Matches:
//
//	@BoopPr review
//	@BoopPr, review
//	@BoopPr review please
//	@BoopPr please review
//	@BoopPr to review
//	@BoopPr can review
//	@BoopPr can you review
//	@BoopPr could you review
//	@BoopPr re-review
//
// Does NOT match (a bare mention or a reference, not a request):
//
//	@BoopPr hi
//	@BoopPr look at this code review carefully
//	@BoopPr the prior review was great
//	@BoopPr-bot review
//	@BoopPr2 review
var reviewRequestRegex = regexp.MustCompile(
	`(?i)@BoopPr\b,?\s+(?:please\s+|to\s+|can\s+(?:you\s+)?|could\s+(?:you\s+)?|will\s+(?:you\s+)?|may\s+(?:you\s+)?)?(?:re-)?review\b`,
)

// requestsReview reports whether the comment body asks Boop to
// review the PR. Bare mentions like `@BoopPr hi` do not count.
func requestsReview(body string) bool {
	return reviewRequestRegex.MatchString(body)
}

type Config struct {
	Port            string
	WebhookSecret   string
	JobImage        string
	TargetNamespace string
	BotLogin        string // GitHub login of the bot, used to ignore self-comments
	DBPath          string // sqlite path; empty disables the data layer (legacy mode)
	RunnerToken     string // shared secret for the runner's POST endpoints; empty rejects all runner posts
	InstallPollInterval time.Duration // how often to refresh installations from GitHub; 0 = 5m default
	// QUB-94: cluster-wide default for the OpenRouter SDK feature
	// flag. The runner takes the in-process SDK path when
	// BOOP_USE_OPENROUTER_SDK=1; otherwise it falls back to the
	// opencode subprocess. Per-PR overrides win over this default
	// (see sdkEnabledLabel). Default "0" until the cutover PR
	// ships and a week of clean runs passes.
	OpenRouterSDKDefault string
}

type Handler struct {
	cfg      Config
	logger   *slog.Logger
	kube     kubernetes.Interface
	ghClient *boopgithub.Manager
	store    *store.Store // may be nil; nil disables the data layer (legacy mode)

	// consecutiveConfigMapFallbacks tracks how many submitJob calls
	// in a row fell back to h.cfg.JobImage because currentJobImage
	// failed. Reset to 0 on each successful ConfigMap read. When
	// the count crosses consecutiveFallbackAlertAt, the receiver
	// logs at Error level instead of Warn so a sustained
	// staleness surfaces on log-based alerting instead of only on
	// grep. The original bug shape (receiver submits Jobs with
	// the env-var snapshot while the ConfigMap carries a newer
	// digest) returns under any persistent K8s API failure —
	// RBAC misconfiguration, deleted ConfigMap, wrong namespace.
	// Without an escalation, a sustained staleness is invisible.
	consecutiveConfigMapFallbacks     int
	consecutiveConfigMapFallbacksLock sync.Mutex

	// dedup is an in-memory FIFO ring keyed by X-GitHub-Delivery
	// header. GitHub's webhook deliveries are idempotent (the
	// same delivery ID represents the same physical event), so
	// a re-delivery should be a no-op. Without dedup, a transient
	// K8s API error (network blip, RBAC hiccup) could trigger a
	// second Create that lands while the first one is still
	// settling, leading to two reviews on the same head SHA.
	dedup *deliveryDedup

	// limiter caps the inbound webhook rate at the process
	// level. The exact limit (M4) is intentionally generous —
	// the receiver is one replica, so the only traffic it sees
	// is webhooks from GitHub, which are bounded by GitHub's
	// own delivery rate. The cap exists to stop a misconfigured
	// proxy (or a future test harness) from queueing tens of
	// thousands of webhooks against a single receiver.
	limiter *rate.Limiter
}

// consecutiveFallbackAlertAt is the threshold at which the
// receiver escalates the K8s ConfigMap fallback log line from
// Warn to Error. The number is small enough that a real outage
// (RBAC broken, ConfigMap deleted) trips it within a handful of
// webhooks, and large enough that a transient API blip stays at
// Warn. The threshold is a constant rather than a field because
// no operator has asked for it to be tunable and a misconfiguration
// here would be hard to detect.
const consecutiveFallbackAlertAt = 3

// deliveryDedup is a fixed-size FIFO ring keyed by the GitHub
// delivery ID. Sized for 4096 entries (well above the practical
// webhook rate for a single repo) so the dedup window covers a
// couple of hours of re-deliveries at p99 burst. The on-access
// order is not updated, so a hot entry can be evicted by newer
// deliveries once the ring fills — acceptable here because
// re-deliveries from GitHub are time-bounded and bursty.
type deliveryDedup struct {
	mu      sync.Mutex
	max     int
	entries map[string]time.Time
	order   []string // ring of insertion order; oldest first
}

func newDeliveryDedup(max int) *deliveryDedup {
	if max <= 0 {
		max = 4096
	}
	return &deliveryDedup{
		max:     max,
		entries: make(map[string]time.Time, max),
		order:   make([]string, 0, max),
	}
}

// seen reports whether this delivery ID was previously observed
// within the TTL. Records it on the first call. TTL is large
// enough (1 hour) to cover GitHub's re-delivery behaviour: GitHub
// retries on 5xx for up to ~3 days, but with a 30-min backoff
// after the first hour, so 1 hour catches the burst without
// holding entries forever.
func (d *deliveryDedup) seen(id string) bool {
	if d == nil || id == "" {
		return false
	}
	now := time.Now()
	cutoff := now.Add(-time.Hour)
	d.mu.Lock()
	defer d.mu.Unlock()
	if at, ok := d.entries[id]; ok && at.After(cutoff) {
		return true
	}
	// Evict the oldest entry if at capacity, then insert.
	if _, exists := d.entries[id]; !exists {
		if len(d.order) >= d.max {
			old := d.order[0]
			d.order = d.order[1:]
			delete(d.entries, old)
		}
		d.order = append(d.order, id)
	}
	d.entries[id] = now
	return false
}

func NewHandler(cfg Config, ghClient *boopgithub.Manager, logger *slog.Logger) (*Handler, error) {
	kube, err := newInClusterClient()
	if err != nil {
		return nil, fmt.Errorf("kube client: %w", err)
	}
	// 20 req/s sustained, 40 burst. The receiver is one
	// replica, so a single rate limiter covers the whole
	// process. The values are sized for a healthy steady state
	// (one webhook per minute per repo) plus headroom for
	// bursty re-deliveries.
	limiter := rate.NewLimiter(rate.Limit(20), 40)
	h := &Handler{
		cfg:      cfg,
		logger:   logger,
		kube:     kube,
		ghClient: ghClient,
		dedup:    newDeliveryDedup(4096),
		limiter:  limiter,
	}
	if cfg.DBPath != "" {
		s, err := store.Open(cfg.DBPath)
		if err != nil {
			return nil, fmt.Errorf("open store: %w", err)
		}
		h.store = s
	}
	return h, nil
}

func (h *Handler) Health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

func (h *Handler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	event := r.Header.Get("X-GitHub-Event")
	sig := r.Header.Get("X-Hub-Signature-256")

	// M4: process-level rate limit. Cheap to check, runs before
	// any expensive work (HMAC verify, K8s API call). The
	// limit is per-process; a single replica Deployment makes
	// that the cluster-wide limit. Returns 429 with
	// Retry-After so a backoff-aware client (or a future
	// Traefik middleware) backs off cleanly.
	if h.limiter != nil && !h.limiter.Allow() {
		h.logger.Warn("rate limited", "delivery", deliveryID)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		h.logger.Warn("read body", "delivery", deliveryID, "err", err)
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if !verifySignature(sig, body, h.cfg.WebhookSecret) {
		h.logger.Warn("invalid signature", "delivery", deliveryID)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	// M3: dedup on X-GitHub-Delivery. GitHub's webhook
	// deliveries are idempotent: a re-delivery of the same
	// physical event uses the same delivery ID. Without
	// dedup, a transient K8s error (network blip, RBAC hiccup)
	// could trigger a second review on the same head SHA.
	// We dedup AFTER the HMAC check so an unauthenticated
	// attacker cannot poison the dedup table.
	if h.dedup != nil && deliveryID != "" && h.dedup.seen(deliveryID) {
		h.logger.Info("duplicate delivery", "delivery", deliveryID, "event", event)
		writeAck(w, "duplicate", "delivery id already processed", deliveryID)
		return
	}

	installationID, err := resolveInstallationID(r.Header.Get("X-GitHub-Installation-ID"), body)
	if err != nil {
		h.logger.Warn("invalid installation id", "delivery", deliveryID, "err", err)
		http.Error(w, "invalid installation id", http.StatusBadRequest)
		return
	}

	switch event {
	case "pull_request":
		h.handlePullRequest(ctx, w, deliveryID, installationID, body)
	case "issue_comment":
		h.handleIssueComment(ctx, w, deliveryID, installationID, body)
	default:
		h.logger.Debug("ignored event", "delivery", deliveryID, "event", event)
		writeAck(w, "ignored", "event not handled", deliveryID)
	}
}

func (h *Handler) handlePullRequest(ctx context.Context, w http.ResponseWriter, delivery string, installationID int64, body []byte) {
	pr, err := parsePullRequest(body)
	if err != nil {
		h.logger.Warn("parse pull_request", "delivery", delivery, "err", err)
		http.Error(w, "parse pull_request", http.StatusBadRequest)
		return
	}
	if !isReviewableAction(pr.Action) {
		h.logger.Debug("ignored action", "delivery", delivery, "action", pr.Action)
		writeAck(w, "ignored", "action not reviewable", delivery)
		return
	}
	if hasLabel(pr.Labels, skipReviewLabel) {
		h.logger.Debug("ignored label", "delivery", delivery, "label", skipReviewLabel)
		writeAck(w, "ignored", "skip-review label present", delivery)
		return
	}
	client := h.ghClient.ClientFor(installationID)
	// Check for duplicate before posting a status comment — otherwise a
	// re-delivery of the same head SHA leaves a stranded "🐾" status
	// comment that no runner will update.
	if claimed, _ := h.claimJobSlot(ctx, delivery, pr.Owner, pr.Repo, pr.Number, pr.HeadSHA, w); !claimed {
		return
	}
	reviewNumber, previousHeadSHA, err := h.computeReviewNumber(ctx, client, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		h.logger.Warn("count prior reviews", "delivery", delivery, "err", err)
		// Non-fatal — fall back to 1; the runner still runs.
		reviewNumber = 1
		previousHeadSHA = ""
	}
	// No trigger comment to react to for plain PR events. Post a fresh
	// status comment and pass its id to the Job.
	statusID, err := h.postStatus(ctx, client, pr.Owner, pr.Repo, pr.Number, renderStatusBody(StatusInitial, pr.HeadSHA, "", reviewNumber))
	if err != nil {
		h.logger.Warn("post status comment", "delivery", delivery, "err", err)
		// Non-fatal — the Job still runs.
	}
	h.submitJob(ctx, w, delivery, pr.Owner, pr.Repo, pr.Number, pr.HeadSHA, pr.BaseRef, previousHeadSHA, fmt.Sprintf("pull_request.%s", pr.Action), 0, statusID, installationID, reviewNumber, pr.Labels)
}

func (h *Handler) handleIssueComment(ctx context.Context, w http.ResponseWriter, delivery string, installationID int64, body []byte) {
	ic, err := parseIssueComment(body)
	if err != nil {
		h.logger.Warn("parse issue_comment", "delivery", delivery, "err", err)
		http.Error(w, "parse issue_comment", http.StatusBadRequest)
		return
	}
	if ic.Action != "created" {
		h.logger.Debug("ignored comment action", "delivery", delivery, "action", ic.Action)
		writeAck(w, "ignored", "comment action not created", delivery)
		return
	}
	if ic.IssuePullRequestURL == "" {
		h.logger.Debug("comment on a plain issue, not a PR", "delivery", delivery, "issue", ic.IssueNumber)
		writeAck(w, "ignored", "comment is on an issue, not a PR", delivery)
		return
	}
	// M5: ask the GitHub API for the App's own login so the
	// self-mention check uses the canonical answer instead of a
	// hard-coded BOT_LOGIN env var. The env var is the fallback
	// when the API call fails (e.g. a network blip at startup, or
	// an air-gapped install) — the receiver still ships, it just
	// may mis-attribute one webhook. Failures are logged so a
	// persistent outage surfaces in the receiver's metrics.
	botLogin := h.cfg.BotLogin
	if h.ghClient != nil {
		if apiLogin, err := h.ghClient.AppBotLogin(ctx, installationID); err == nil {
			botLogin = apiLogin
		} else {
			h.logger.Warn("fetch app bot login, falling back to env", "delivery", delivery, "installation", installationID, "err", err)
		}
	}
	if botLogin != "" && strings.EqualFold(ic.SenderLogin, botLogin) {
		h.logger.Debug("ignored self-mention", "delivery", delivery, "sender", ic.SenderLogin)
		writeAck(w, "ignored", "self-mention", delivery)
		return
	}
	if !requestsReview(ic.CommentBody) {
		h.logger.Debug("comment does not request review", "delivery", delivery)
		writeAck(w, "ignored", "no `@BoopPr review` request", delivery)
		return
	}

	client := h.ghClient.ClientFor(installationID)
	pr, err := client.FetchPullRequest(ctx, ic.Owner, ic.Repo, ic.IssueNumber)
	if err != nil {
		h.logger.Error("fetch pull request", "delivery", delivery, "pr", fmt.Sprintf("%s/%s#%d", ic.Owner, ic.Repo, ic.IssueNumber), "err", err)
		http.Error(w, "fetch pr", http.StatusBadGateway)
		return
	}

	// Check for duplicate (active or succeeded) before reacting or
	// posting a status comment — otherwise a same-SHA `@BoopPr review`
	// leaves a stranded 👀 reaction / "Boop's on the case!" status
	// comment with no runner to update it.
	claimed, dupeStatus := h.claimJobSlot(ctx, delivery, ic.Owner, ic.Repo, pr.Number, pr.HeadSHA, w)
	if !claimed {
		if dupeStatus == "active" || dupeStatus == "succeeded" {
			h.replyDuplicateReview(ctx, client, delivery, ic.Owner, ic.Repo, pr.Number, pr.HeadSHA, dupeStatus)
		}
		return
	}

	// React only after we know a run will start, so no-ops do not look
	// like work is underway.
	if err := client.AddCommentReaction(ctx, ic.Owner, ic.Repo, ic.CommentID, "eyes"); err != nil {
		h.logger.Warn("add reaction", "delivery", delivery, "err", err)
	}

	reviewNumber, previousHeadSHA, err := h.computeReviewNumber(ctx, client, ic.Owner, ic.Repo, pr.Number)
	if err != nil {
		h.logger.Warn("count prior reviews", "delivery", delivery, "err", err)
		reviewNumber = 1
		previousHeadSHA = ""
	}

	// Post a status comment the runner will PATCH as it progresses.
	statusID, err := h.postStatus(ctx, client, ic.Owner, ic.Repo, pr.Number, renderStatusBody(StatusInitial, pr.HeadSHA, ic.SenderLogin, reviewNumber))
	if err != nil {
		h.logger.Warn("post status comment", "delivery", delivery, "err", err)
	}

	h.submitJob(ctx, w, delivery, ic.Owner, ic.Repo, pr.Number, pr.HeadSHA, pr.BaseRef, previousHeadSHA, fmt.Sprintf("issue_comment.by=%s", ic.SenderLogin), ic.CommentID, statusID, installationID, reviewNumber, nil)
}

// computeReviewNumber returns the 1-based index of the review this
// run will produce, plus the head SHA of the most recent prior Boop
// summary (empty if no marker was found). The next run labels
// itself #N and — when previousHeadSHA is set — diffs only the delta
// from that commit instead of the full main..head range. Falls
// back to (1, "", nil) on error so a transient GitHub hiccup never
// blocks a review.
func (h *Handler) computeReviewNumber(ctx context.Context, client *boopgithub.Client, owner, repo string, number int) (int, string, error) {
	n, lastSHA, err := client.CountPriorReviews(ctx, owner, repo, number)
	if err != nil {
		return 0, "", err
	}
	return n + 1, lastSHA, nil
}

// Status stages used in renderStatusBody. The runner can refer to the
// same stage constants when it PATCHes the comment.
const (
	StatusInitial = "initial"
	StatusAuth    = "auth"
	StatusClone   = "clone"
	StatusReview  = "review"
	StatusDone    = "done"
	StatusFailed  = "failed"

	// statusTimelineSep is appended to the initial status body so the
	// runner's PATCH path can stay append-only — it never overwrites
	// the receiver-supplied header (which carries the review label and
	// trigger attribution).
	statusTimelineSep = "<!-- boop-timeline -->"
)

func renderStatusBody(stage, sha, by string, reviewNumber int) string {
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	byLine := ""
	if by != "" {
		byLine = fmt.Sprintf("Triggered by @%s\n\n", by)
	}
	// reviewLabel disambiguates re-reviews in the status thread so
	// users can tell at a glance which run produced the comments.
	reviewLabel := "review"
	if reviewNumber > 1 {
		reviewLabel = fmt.Sprintf("re-review #%d", reviewNumber)
	}
	switch stage {
	case StatusInitial:
		return fmt.Sprintf("🐾 **Boop's on the case!** (%s)\n\n%sLast commit: `%s`. Digging in now — updates will appear here.\n\n%s", reviewLabel, byLine, short, statusTimelineSep)
	case StatusAuth:
		return fmt.Sprintf("🤝 **Paw-shaken in** — authenticated with GitHub. (%s)\n\n%sLast commit: `%s`.", reviewLabel, byLine, short)
	case StatusClone:
		return fmt.Sprintf("🥎 **Boop fetched the repo** at `%s`. (%s)\n\n%sTrotting to the PR head and starting the multi-lens review.", short, reviewLabel, byLine)
	case StatusReview:
		return fmt.Sprintf("👃 **Boop is sniffing** — running the multi-lens review on `%s`. (%s)", short, reviewLabel)
	case StatusDone:
		return "💤 **Boop napped.** See the comment below."
	case StatusFailed:
		return fmt.Sprintf("🔄 **Boop chased his tail.** Check the Job logs for details. (%s)", reviewLabel)
	}
	return fmt.Sprintf("boop status: %s", stage)
}

func (h *Handler) postStatus(ctx context.Context, client *boopgithub.Client, owner, repo string, number int, body string) (int64, error) {
	return client.PostIssueComment(ctx, owner, repo, number, body)
}

// claimJobSlot checks whether a Job already exists for this head SHA.
// Returns (true, "") when the caller may create a job.
// Returns (false, status) when the caller must skip: status is
// "active", "succeeded", or "error". On "active"/"succeeded" it writes
// a duplicate ack. On kube errors it writes a 500 and status "error".
// A failed job is deleted so a fresh one can be created.
func (h *Handler) claimJobSlot(ctx context.Context, delivery, owner, repo string, number int, headSHA string, w http.ResponseWriter) (bool, string) {
	jobName := buildJobName(owner, repo, number, headSHA)
	jobStatus, err := h.jobStatus(ctx, jobName)
	if err != nil {
		h.logger.Error("check job", "delivery", delivery, "job", jobName, "err", err)
		http.Error(w, "kube error", http.StatusInternalServerError)
		return false, "error"
	}
	switch jobStatus {
	case "active":
		h.logger.Info("duplicate delivery, job still active", "delivery", delivery, "job", jobName)
		writeAck(w, "duplicate", jobName, delivery)
		return false, "active"
	case "succeeded":
		h.logger.Info("duplicate delivery, job already succeeded", "delivery", delivery, "job", jobName)
		writeAck(w, "duplicate", jobName, delivery)
		return false, "succeeded"
	case "failed":
		h.logger.Info("replacing failed job", "delivery", delivery, "job", jobName)
		if err := h.deleteJob(ctx, jobName); err != nil && !isNotFound(err) {
			h.logger.Error("delete failed job", "delivery", delivery, "job", jobName, "err", err)
			http.Error(w, "kube error", http.StatusInternalServerError)
			return false, "error"
		}
	}
	return true, ""
}

// replyDuplicateReview posts a short PR comment explaining why a same-SHA
// `@BoopPr review` request was a no-op. Non-fatal on API errors.
func (h *Handler) replyDuplicateReview(ctx context.Context, client *boopgithub.Client, delivery, owner, repo string, number int, headSHA, status string) {
	body := duplicateReviewReply(status, headSHA)
	if body == "" {
		return
	}
	if _, err := client.PostIssueComment(ctx, owner, repo, number, body); err != nil {
		h.logger.Warn("post duplicate-review reply", "delivery", delivery, "err", err)
	}
}

// duplicateReviewReply explains a same-SHA no-op in Boop's chrome
// voice (friendly pug, mascot OK). Findings stay plain; this is status.
func duplicateReviewReply(status, headSHA string) string {
	short := shortSHA(headSHA)
	switch status {
	case "active":
		return fmt.Sprintf(
			"🐾 **Already on it.** I'm still sniffing through `%s`. Hang tight — I'll post when that run finishes.",
			short,
		)
	case "succeeded":
		return fmt.Sprintf(
			"🐾 **Already sniffed `%s`.** Push a new commit and I'll take another pass. Want the same SHA again? Delete the Job for this head and ask me to review.",
			short,
		)
	default:
		return ""
	}
}

func (h *Handler) submitJob(ctx context.Context, w http.ResponseWriter, delivery, owner, repo string, number int, headSHA, baseRef, previousHeadSHA, reason string, reactionCommentID, statusCommentID, installationID int64, reviewNumber int, labels []string) {
	jobName := buildJobName(owner, repo, number, headSHA)

	// QUB-94: resolve the BOOP_USE_OPENROUTER_SDK value for this
	// PR. The cluster default + the per-PR label form the value;
	// the decision is logged so a job landing on either path is
	// traceable from the webhook handler logs.
	sdkEnabled := h.resolveSDKEnabled(labels)
	h.logger.Debug("sdk flag resolved", "delivery", delivery, "value", sdkEnabled, "label_present", hasLabel(labels, sdkEnabledLabel), "cluster_default", h.cfg.OpenRouterSDKDefault)

	// Resolve JOB_IMAGE fresh from the boop-config ConfigMap instead
	// of using the env-var snapshot captured at startup. ArgoCD
	// updates the ConfigMap digest without redeploying the receiver
	// Deployment, so the env-var value goes stale until the next
	// receiver restart. Reading on each submit (one extra K8s API
	// call per webhook, webhook rate is bounded) keeps the Job in
	// sync with whatever digest sync-image-digests just landed.
	// Falls back to the env-var snapshot if the API read fails so
	// a transient API hiccup doesn't block a review; the fallback
	// counter escalates to Error-level logging after
	// consecutiveFallbackAlertAt uses in a row.
	image, _ := h.resolveJobImageForSubmit(ctx)

	job, err := buildJob(templateVars{
		Owner:             owner,
		Repo:              repo,
		Number:            fmt.Sprintf("%d", number),
		SHA:               headSHA,
		SHA7:              shortSHA(headSHA),
		BaseRef:           baseRef,
		PreviousHeadSHA:   previousHeadSHA,
		Image:             image,
		StatusCommentID:   fmt.Sprintf("%d", statusCommentID),
		ReactionCommentID: fmt.Sprintf("%d", reactionCommentID),
		ReviewNumber:      fmt.Sprintf("%d", reviewNumber),
		InstallationID:    fmt.Sprintf("%d", installationID),
		BotLogin:          h.cfg.BotLogin,
		JobName:           jobName,
		// Dashboard URL is the receiver Service's in-cluster DNS.
		// Hard-coded to the canonical Service name + namespace so
		// a misconfigured operator can't accidentally point the
		// runner at a public URL (where the token would leak).
		DashboardURL:   "http://boop-receiver.dev-tools.svc.cluster.local:8080",
		DashboardToken: h.cfg.RunnerToken,
		// QUB-94: forwarded into BOOP_USE_OPENROUTER_SDK so the
		// runner takes the in-process SDK path. Defaults to "0"
		// (opencode subprocess) until the cutover.
		OpenRouterSDKEnabled: sdkEnabled,
	})
	if err != nil {
		// Base-ref or installation-id validation failure: this is a
		// 400, not a 500 — the input is a bad request, not a server
		// error. The webhook ack still goes out (GitHub will retry
		// idempotently if the head SHA changes), but a 400 tells
		// any future proxy the request is malformed.
		h.logger.Warn("build job", "delivery", delivery, "err", err)
		http.Error(w, "invalid job spec", http.StatusBadRequest)
		return
	}

	if err := h.createJob(ctx, job); err != nil {
		if isAlreadyExists(err) {
			// Lost a race with another delivery for the same
			// head SHA (between claimJobSlot and createJob).
			// Treat as duplicate so the caller can decide
			// whether to react / reply.
			h.logger.Info("duplicate job race", "delivery", delivery, "job", jobName, "err", err)
			writeAck(w, "duplicate", jobName, delivery)
			return
		}
		h.logger.Error("create job", "delivery", delivery, "job", jobName, "err", err)
		http.Error(w, "create job", http.StatusInternalServerError)
		return
	}

	// Persist the run row so the dashboard has it. This is
	// best-effort: the receiver's job is to submit the Job and
	// ack the webhook. A store write failure is logged but does
	// not fail the webhook (the Job is already in flight; the
	// dashboard will simply not show it, and the next runner
	// status POST will write the row).
	if h.store != nil {
		_, err := h.store.UpsertRun(ctx, store.Run{
			ID:             jobName,
			Owner:          owner,
			Repo:           repo,
			PRNumber:       number,
			CommitSHA:      headSHA,
			BaseRef:        baseRef,
			ReviewNumber:   reviewNumber,
			Reason:         reason,
			InstallationID: installationID,
			Status:         store.StatusRunning,
			StartedAt:      time.Now().UTC(),
		})
		if err != nil {
			h.logger.Warn("upsert run", "delivery", delivery, "job", jobName, "err", err)
		}
	}

	h.logger.Info("job created",
		"delivery", delivery,
		"job", jobName,
		"pr", fmt.Sprintf("%s/%s#%d", owner, repo, number),
		"sha", headSHA,
		"previous_head_sha", previousHeadSHA,
		"reason", reason,
		"installation_id", installationID,
		"status_comment_id", statusCommentID,
		"reaction_comment_id", reactionCommentID,
		"review_number", reviewNumber,
	)
	writeAck(w, "accepted", jobName, delivery)
}

// boopConfigMapName is the ConfigMap the receiver reads for
// JOB_IMAGE. Defined as a constant so the test can use the same
// value when seeding the fake client. The ConfigMap lives in the
// receiver's own namespace (TARGET_NAMESPACE, default "dev-tools")
// and is owned by ArgoCD, not the receiver.
const boopConfigMapName = "boop-config"

// jobImageKey is the data key under which JOB_IMAGE is stored in
// the boop-config ConfigMap. Must match apps/k8s/base/config.yaml.
const jobImageKey = "JOB_IMAGE"

// currentJobImage reads JOB_IMAGE from the boop-config ConfigMap
// in the receiver's namespace and returns the freshest value ArgoCD
// has synced. Falls back to the env-var snapshot (h.cfg.JobImage)
// when the read fails so a transient K8s API hiccup doesn't block
// a review. Not cached: the K8s API round-trip is cheap relative to
// the rest of the webhook handler (token mint + GitHub API calls),
// and skipping the cache keeps the recovery time at "next webhook"
// after ArgoCD syncs a new digest.
//
// The receiver's Role grants configmaps:get/list/watch in
// TARGET_NAMESPACE — see apps/k8s/base/role.yaml. The ConfigMap is
// namespace-scoped, so the receiver can only read it from its own
// namespace, which is the right blast-radius.
func (h *Handler) currentJobImage(ctx context.Context) (string, error) {
	cm, err := h.kube.CoreV1().ConfigMaps(h.cfg.TargetNamespace).Get(ctx, boopConfigMapName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get %s/%s: %w", h.cfg.TargetNamespace, boopConfigMapName, err)
	}
	v, ok := cm.Data[jobImageKey]
	if !ok || v == "" {
		return "", fmt.Errorf("%s/%s missing %q key", h.cfg.TargetNamespace, boopConfigMapName, jobImageKey)
	}
	return v, nil
}

// resolveJobImageForSubmit wraps currentJobImage with the
// fallback-to-env-snapshot policy and the consecutive-failure
// counter. Always returns a non-empty image suitable for the Job
// template: a successful ConfigMap read returns the live digest;
// a failed read returns the env-var snapshot and increments the
// counter (escalating to Error-level logging after
// consecutiveFallbackAlertAt). On success, the counter resets to
// 0 — a single successful read is enough to declare "the API is
// fine, the previous failures were transient."
//
// Returned source is "configmap" or "fallback" so tests can
// assert the path taken without inspecting logs.
func (h *Handler) resolveJobImageForSubmit(ctx context.Context) (image, source string) {
	cm, err := h.currentJobImage(ctx)
	if err == nil {
		h.resetConfigMapFallbacks()
		return cm, "configmap"
	}
	count := h.bumpConfigMapFallbacks()
	level := slog.LevelWarn
	if count >= consecutiveFallbackAlertAt {
		level = slog.LevelError
	}
	h.logger.Log(ctx, level, "read boop-config for JOB_IMAGE, using startup value",
		"err", err,
		"consecutive_fallbacks", count,
		"alert_threshold", consecutiveFallbackAlertAt,
	)
	return h.cfg.JobImage, "fallback"
}

// bumpConfigMapFallbacks increments the consecutive-fallback
// counter under a lock and returns the new value. The lock is a
// separate mutex (not the dedup mutex) so the read-then-increment
// pattern stays a single critical section.
func (h *Handler) bumpConfigMapFallbacks() int {
	h.consecutiveConfigMapFallbacksLock.Lock()
	defer h.consecutiveConfigMapFallbacksLock.Unlock()
	h.consecutiveConfigMapFallbacks++
	return h.consecutiveConfigMapFallbacks
}

// resetConfigMapFallbacks zeroes the counter on a successful
// ConfigMap read. Lock-protected for the same reason as bump.
func (h *Handler) resetConfigMapFallbacks() {
	h.consecutiveConfigMapFallbacksLock.Lock()
	defer h.consecutiveConfigMapFallbacksLock.Unlock()
	h.consecutiveConfigMapFallbacks = 0
}

type templateVars struct {
	Owner             string
	Repo              string
	Number            string
	SHA               string
	SHA7              string
	BaseRef           string
	PreviousHeadSHA   string // head SHA of the most recent prior Boop summary; empty on first review
	Image             string
	StatusCommentID   string // GitHub comment id for live status updates (empty if none)
	ReactionCommentID string // GitHub comment id that received the trigger reaction (empty if none)
	ReviewNumber      string // 1-based review number for this run; runner uses it for headers
	InstallationID    string // GitHub App installation ID, sourced from the webhook header
	BotLogin          string // GitHub login of the bot App (e.g. "booppr[bot]"); used to identify prior review artifacts for cleanup
	DashboardURL      string // receiver Service URL the runner POSTs telemetry to; empty disables telemetry capture
	DashboardToken    string // shared secret for the runner's POSTs; empty disables telemetry capture
	JobName           string // the K8s Job name, which the runner reports back as the run id
	// QUB-94: BOOP_USE_OPENROUTER_SDK value forwarded to the
	// runner. "1" → in-process OpenRouter SDK path; "0" →
	// opencode subprocess. Resolved at submitJob time from the
	// cluster default + the boop:openrouter-sdk per-PR label.
	OpenRouterSDKEnabled string
}

type prMeta struct {
	Action  string
	Owner   string
	Repo    string
	Number  int
	HeadSHA string
	BaseRef string
	Labels  []string
}

// skipReviewLabel is the GitHub label that opts a PR out of Boop
// review. Auto-generated PRs (image-digest syncs, dep bumps, etc.)
// carry it so the receiver acks the webhook as ignored and never
// schedules a review Job.
const skipReviewLabel = "skip-review"

// sdkEnabledLabel is the GitHub label that opts a PR into the
// OpenRouter SDK path for its next review. The cluster-wide
// default (Config.OpenRouterSDKDefault, sourced from
// BOOP_USE_OPENROUTER_SDK on the receiver) still applies; the
// label is a per-PR override. During the QUB-94 rollout the
// cluster default stays at "0" (opencode subprocess) and the
// label is how operators flip a single PR to the SDK path for
// smoke-testing. After the cutover the cluster default flips to
// "1" and the label becomes a no-op opt-in.
const sdkEnabledLabel = "boop:openrouter-sdk"

func hasLabel(labels []string, name string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

// resolveSDKEnabled picks the BOOP_USE_OPENROUTER_SDK value for
// the next review Job on a PR. The cluster default
// (h.cfg.OpenRouterSDKDefault) sets the floor; the per-PR label
// is an opt-in. There is no opt-out label today: during the
// rollout, the cluster default is "0" (opencode) and a label
// switches a single PR to "1" (SDK). After the cutover, the
// cluster default flips to "1" and the label becomes redundant.
// The decision is logged so the operator can see why a given
// Job landed on either path.
func (h *Handler) resolveSDKEnabled(labels []string) string {
	if hasLabel(labels, sdkEnabledLabel) {
		return "1"
	}
	if h.cfg.OpenRouterSDKDefault == "1" {
		return "1"
	}
	return "0"
}

// parseInstallationID reads the X-GitHub-Installation-ID header and
// returns the installation ID. Returns an error if the header is
// missing or unparseable.
func parseInstallationID(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("X-GitHub-Installation-ID header missing")
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid X-GitHub-Installation-ID %q: %w", s, err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("invalid X-GitHub-Installation-ID %q: must be positive", s)
	}
	return id, nil
}

// resolveInstallationID returns the installation ID for a webhook delivery.
// Tries the X-GitHub-Installation-ID header first (a proxy may inject it
// for routing), then falls back to installation.id in the JSON payload,
// which is the canonical location for GitHub App webhooks. Returns an
// error only if neither source is present — the receiver must not act
// on a webhook without knowing which installation fired it.
func resolveInstallationID(headerVal string, body []byte) (int64, error) {
	if id, err := parseInstallationID(headerVal); err == nil {
		return id, nil
	}
	var probe struct {
		Installation *struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.Installation != nil && probe.Installation.ID > 0 {
		return probe.Installation.ID, nil
	}
	return 0, errors.New("installation id not present in header or payload")
}

func parsePullRequest(body []byte) (prMeta, error) {
	var out prMeta
	event, err := github.ParseWebHook("pull_request", body)
	if err != nil {
		return out, fmt.Errorf("parse webhook: %w", err)
	}
	pr, ok := event.(*github.PullRequestEvent)
	if !ok || pr.PullRequest == nil || pr.Repo == nil || pr.Repo.Owner == nil {
		return out, errors.New("malformed pull_request event")
	}
	if pr.Action == nil || pr.PullRequest.Head == nil || pr.PullRequest.Head.SHA == nil ||
		pr.PullRequest.Base == nil || pr.PullRequest.Base.Ref == nil {
		return out, errors.New("missing required pull_request fields")
	}
	out.Action = *pr.Action
	out.Owner = *pr.Repo.Owner.Login
	out.Repo = *pr.Repo.Name
	out.Number = pr.GetNumber()
	out.HeadSHA = *pr.PullRequest.Head.SHA
	out.BaseRef = *pr.PullRequest.Base.Ref
	for _, l := range pr.PullRequest.Labels {
		if l != nil && l.Name != nil {
			out.Labels = append(out.Labels, *l.Name)
		}
	}
	return out, nil
}

// issueCommentMeta is the slice of the issue_comment payload the
// receiver needs. The webhook only fires for PR comments when the
// user mentions the bot.
type issueCommentMeta struct {
	Action              string
	Owner               string
	Repo                string
	IssueNumber         int
	IssuePullRequestURL string // non-empty => this is a PR comment, not a plain issue
	CommentID           int64
	CommentBody         string
	SenderLogin         string
}

func parseIssueComment(body []byte) (issueCommentMeta, error) {
	var out issueCommentMeta
	event, err := github.ParseWebHook("issue_comment", body)
	if err != nil {
		return out, fmt.Errorf("parse webhook: %w", err)
	}
	ic, ok := event.(*github.IssueCommentEvent)
	if !ok || ic.Issue == nil || ic.Repo == nil || ic.Repo.Owner == nil || ic.Comment == nil || ic.Sender == nil {
		return out, errors.New("malformed issue_comment event")
	}
	if ic.Action == nil || ic.Comment.Body == nil || ic.Sender.Login == nil || ic.Issue.Number == nil {
		return out, errors.New("missing required issue_comment fields")
	}
	out.Action = *ic.Action
	out.Owner = *ic.Repo.Owner.Login
	out.Repo = *ic.Repo.Name
	out.IssueNumber = ic.Issue.GetNumber()
	if ic.Issue.PullRequestLinks != nil {
		out.IssuePullRequestURL = ic.Issue.PullRequestLinks.GetURL()
	}
	out.CommentID = ic.Comment.GetID()
	out.CommentBody = *ic.Comment.Body
	out.SenderLogin = *ic.Sender.Login
	return out, nil
}

func isReviewableAction(a string) bool {
	switch a {
	case "opened", "reopened", "synchronize", "ready_for_review":
		return true
	}
	return false
}

var jobNameSanitizer = regexp.MustCompile(`[^a-z0-9-]`)

func buildJobName(owner, repo string, number int, sha string) string {
	raw := fmt.Sprintf("boop-%s-%s-%d-%s", owner, repo, number, shortSHA(sha))
	return jobNameSanitizer.ReplaceAllString(strings.ToLower(raw), "-")
}

func shortSHA(sha string) string {
	if len(sha) >= 7 {
		return sha[:7]
	}
	return sha
}

// jobStatus returns one of: "missing", "active", "failed", "succeeded".
// Used to decide whether to create, skip (duplicate), or replace
// (failed) a Job for a given head SHA.
func (h *Handler) jobStatus(ctx context.Context, name string) (string, error) {
	job, err := h.kube.BatchV1().Jobs(h.cfg.TargetNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return "missing", nil
		}
		return "", err
	}
	if job.Status.Failed > 0 {
		return "failed", nil
	}
	if job.Status.Succeeded > 0 {
		return "succeeded", nil
	}
	return "active", nil
}

func (h *Handler) deleteJob(ctx context.Context, name string) error {
	propagation := metav1.DeletePropagationBackground
	return h.kube.BatchV1().Jobs(h.cfg.TargetNamespace).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
}

func (h *Handler) createJob(ctx context.Context, job *batchv1.Job) error {
	_, err := h.kube.BatchV1().Jobs(h.cfg.TargetNamespace).Create(ctx, job, metav1.CreateOptions{})
	return err
}

func verifySignature(header string, body []byte, secret string) bool {
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	got := strings.TrimPrefix(header, "sha256=")
	return subtle.ConstantTimeCompare([]byte(expected), []byte(got)) == 1
}

func writeAck(w http.ResponseWriter, status, detail, delivery string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   status,
		"detail":   detail,
		"delivery": delivery,
		"ts":       time.Now().UTC().Format(time.RFC3339),
	})
}

// Store returns the receiver's persistent store, or nil if the
// data layer is disabled. main.go uses it to log whether the
// data layer is on or off; the dashboard handlers use it
// directly to read.
func (h *Handler) Store() *store.Store { return h.store }
