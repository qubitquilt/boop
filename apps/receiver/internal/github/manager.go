package github

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AppConfig holds the static credentials of the GitHub App. The
// installation is identified per-webhook, first from the
// X-GitHub-Installation-ID header (if a proxy injects it) and then
// from installation.id in the JSON payload; it is NOT pinned here.
type AppConfig struct {
	AppID      int64
	PrivateKey *rsa.PrivateKey
}

// Manager owns App credentials and the per-installation token cache.
// Safe for concurrent use.
type Manager struct {
	cfg      AppConfig
	baseHTTP *http.Client

	mu              sync.Mutex
	clients         map[int64]*Client
	botLoginCache   map[int64]botLoginCacheEntry
	installations   []Installation
	installationsAt time.Time
}

// botLoginCacheEntry stores the bot login for one installation,
// plus the time it was resolved. A short TTL catches the case
// where the App is reinstalled under a new login (rare, but
// possible during a bot migration); the receiver will pick up
// the new login within one TTL.
type botLoginCacheEntry struct {
	login string
	at    time.Time
}

const botLoginCacheTTL = 24 * time.Hour

// NewManager returns a Manager that can hand out per-installation
// Clients on demand. Each Client mints its own installation token
// (cached) the first time it's used.
func NewManager(cfg AppConfig) *Manager {
	return &Manager{
		cfg:           cfg,
		baseHTTP:      &http.Client{Timeout: httpTimeout},
		clients:       make(map[int64]*Client),
		botLoginCache: make(map[int64]botLoginCacheEntry),
	}
}

// SetBaseHTTPForTest replaces the manager's underlying HTTP
// client. Tests use it to point GitHub calls at a httptest.Server
// without having to stand up a TLS terminator. Test-only — the
// `*_test.go` convention would scope this to the github package's
// own tests, but the webhook package's tests also need it, so it
// is exported. Do not call from production code.
//
// QUB-99: swapping baseHTTP once redirects every API call the
// Manager makes (AppBotLogin, ListInstallations, and every
// per-installation Client call) to the test server.
func (m *Manager) SetBaseHTTPForTest(c *http.Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.baseHTTP = c
}

// ClientFor returns the per-installation Client, creating and caching
// one on first use. Subsequent calls with the same installationID
// return the same *Client.
func (m *Manager) ClientFor(installationID int64) *Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clients[installationID]; ok {
		return c
	}
	c := &Client{
		m:              m,
		installationID: installationID,
	}
	m.clients[installationID] = c
	return c
}

// appJWT mints a short-lived JWT signed with the App's private key,
// used to call the /app endpoints (e.g., to create an installation
// token for a specific installation).
func (m *Manager) appJWT(now time.Time) (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    fmt.Sprintf("%d", m.cfg.AppID),
		IssuedAt:  jwt.NewNumericDate(now.Add(-appJWTLeeway)),
		ExpiresAt: jwt.NewNumericDate(now.Add(appJWTTTL)),
	})
	return tok.SignedString(m.cfg.PrivateKey)
}

// AppBotLogin returns the GitHub login of the App (e.g. "booppr[bot]").
// The receiver uses this to recognise self-comments on issue_comment
// events so it doesn't trigger a review in response to its own prior
// messages. The hard-coded BOT_LOGIN env var (passed in via main.go)
// is the fallback for air-gapped setups where the App cannot reach
// api.github.com at startup; this call is the canonical answer and
// is cached per-installation. (The bot login is the same for every
// installation of a given App, but caching per-install keeps the call
// site simple and matches how installation tokens are cached.)
//
// Reaches the GitHub API as the App (using an app JWT, not an
// installation token) because the App's own login is the same
// for every installation. Endpoint:
//
//	GET /app -> { "id": ..., "slug": "booppr", "name": "BoopPr", ... }
//
// The bot login is constructed as `lower(slug) + "[bot]"`. The
// previous implementation read `account.login` from
// `GET /app/installations/{installation_id}`, but that field is the
// installing user or org, not the App's bot. On a personal-account
// install the two happen to be the same string, so every comment
// from the owner was treated as a self-mention.
//
// M5: previously the receiver trusted a hard-coded
// BOT_LOGIN env var. Drift between env and the App's actual
// login (after a bot rename, a key rotation that changes the
// bot's slug, or a misconfigured Helm values file) would
// either silence the self-mention check (every comment
// retriggers a review) or block it (the receiver never
// recognises its own replies). Asking the API for the
// canonical login closes that drift.
func (m *Manager) AppBotLogin(ctx context.Context, installationID int64) (string, error) {
	if installationID <= 0 {
		return "", fmt.Errorf("invalid installation id: %d", installationID)
	}
	m.mu.Lock()
	if entry, ok := m.botLoginCache[installationID]; ok {
		if time.Since(entry.at) < botLoginCacheTTL && entry.login != "" {
			m.mu.Unlock()
			return entry.login, nil
		}
	}
	m.mu.Unlock()

	now := time.Now()
	jwtStr, err := m.appJWT(now)
	if err != nil {
		return "", fmt.Errorf("mint app jwt: %w", err)
	}

	url := appInfoURL

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwtStr)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "boop-receiver")

	res, err := m.baseHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch app: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read app body: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch app: %d %s", res.StatusCode, string(body))
	}

	var probe struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", fmt.Errorf("decode app body: %w", err)
	}
	if probe.Slug == "" {
		return "", fmt.Errorf("app response has no slug")
	}
	login := strings.ToLower(probe.Slug) + "[bot]"

	m.mu.Lock()
	m.botLoginCache[installationID] = botLoginCacheEntry{login: login, at: now}
	m.mu.Unlock()

	return login, nil
}

// Installation is the slice of the GitHub App installation object
// the receiver persists. We keep only the fields the dashboard
// reads; the full payload is on the wire but storing the rest is
// wasted bytes and a slow SELECT.
//
// AccountType is the canonical GitHub field ("User" or
// "Organization") so the dashboard can show a different icon per
// type. RepositorySelection is "all" or "selected"; the dashboard
// uses this to annotate a repo as "App installed on the whole org"
// vs "App installed on this one repo only".
type Installation struct {
	ID                  int64     `json:"id"`
	AccountLogin        string    `json:"account_login"`
	AccountType         string    `json:"account_type"`
	RepositorySelection string    `json:"repository_selection,omitempty"`
	InstalledAt         time.Time `json:"installed_at,omitempty"`
}

// ListInstallations returns every installation of the App,
// cached for installationsCacheTTL so the dashboard's poll does
// not hammer the GitHub API.
//
// The call is App-level (uses an App JWT, not an installation
// token) because we need every installation, not the one
// attached to the current webhook. We page through GitHub's
// response, which returns up to 100 installations per page.
//
// On any API error, the cache is left untouched and the error
// returned. The caller (the receiver's background poller) logs
// and retries on the next tick.
func (m *Manager) ListInstallations(ctx context.Context) ([]Installation, error) {
	m.mu.Lock()
	if time.Since(m.installationsAt) < installationsCacheTTL && len(m.installations) > 0 {
		cached := m.installations
		m.mu.Unlock()
		return cached, nil
	}
	m.mu.Unlock()

	now := time.Now()
	jwtStr, err := m.appJWT(now)
	if err != nil {
		return nil, fmt.Errorf("mint app jwt: %w", err)
	}

	// Walk pages. GitHub's pagination caps at 100 per page; the
	// dashboard needs every installation, so the loop is
	// unconditional until the API stops handing out next links.
	var out []Installation
	for page := 1; ; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, installationsListURL, nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+jwtStr)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "boop-receiver")

		q := req.URL.Query()
		q.Set("per_page", "100")
		q.Set("page", fmt.Sprintf("%d", page))
		req.URL.RawQuery = q.Encode()

		res, err := m.baseHTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch installations page %d: %w", page, err)
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read installations page %d: %w", page, err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch installations page %d: %d %s", page, res.StatusCode, string(body))
		}

		var pageInstalls []struct {
			ID      int64 `json:"id"`
			Account *struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"account"`
			RepositorySelection string    `json:"repository_selection"`
			CreatedAt           time.Time `json:"created_at"`
		}
		if err := json.Unmarshal(body, &pageInstalls); err != nil {
			return nil, fmt.Errorf("decode installations page %d: %w", page, err)
		}
		for _, ins := range pageInstalls {
			if ins.Account == nil {
				continue
			}
			out = append(out, Installation{
				ID:                  ins.ID,
				AccountLogin:        ins.Account.Login,
				AccountType:         ins.Account.Type,
				RepositorySelection: ins.RepositorySelection,
				InstalledAt:         ins.CreatedAt,
			})
		}
		// The `Link` header is the only signal GitHub gives for
		// the next page; if it's missing or has no `rel="next"`,
		// we're done. Parsing the header in-house avoids
		// pulling in net/http's link parsing for a single use.
		if !hasNextPage(res.Header.Get("Link")) {
			break
		}
	}

	m.mu.Lock()
	m.installations = out
	m.installationsAt = time.Now()
	m.mu.Unlock()

	return out, nil
}

// hasNextPage parses GitHub's Link header and reports whether
// the next page is present. GitHub uses the standard RFC 5988
// shape: `<url>; rel="next", <url>; rel="last"`. We only care
// about "next".
func hasNextPage(link string) bool {
	if link == "" {
		return false
	}
	for _, part := range strings.Split(link, ",") {
		if strings.Contains(part, `rel="next"`) {
			return true
		}
	}
	return false
}

const (
	httpTimeout           = 15 * time.Second
	tokenTTLRefresh       = 5 * time.Minute
	appJWTTTL             = 10 * time.Minute
	appJWTLeeway          = 30 * time.Second
	installationsCacheTTL = 5 * time.Minute
)

// appInfoURL is the endpoint AppBotLogin queries. It is a var (not a
// const) so tests can point it at a httptest.Server without standing
// up a TLS terminator. Test-only accessors below the var; not
// production API.
var appInfoURL = "https://api.github.com/app"

// AppInfoURLForTest returns the current appInfoURL value. Tests
// use it to save and restore the original when they swap in a
// httptest.Server. Test-only.
func AppInfoURLForTest() string { return appInfoURL }

// SetAppInfoURLForTest replaces the appInfoURL value. Tests use it
// to point AppBotLogin at a httptest.Server. Test-only.
func SetAppInfoURLForTest(s string) { appInfoURL = s }


// installationsListURL is the App endpoint that returns every
// installation of the App. Paged by GitHub; we walk pages until
// NextPage is zero. Like appInfoURL it's a var so tests can
// point it at an httptest.Server.
var installationsListURL = "https://api.github.com/app/installations"
