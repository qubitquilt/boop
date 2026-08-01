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

	mu            sync.Mutex
	clients       map[int64]*Client
	botLoginCache map[int64]botLoginCacheEntry
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

const (
	httpTimeout     = 15 * time.Second
	tokenTTLRefresh = 5 * time.Minute
	appJWTTTL       = 10 * time.Minute
	appJWTLeeway    = 30 * time.Second
)

// appInfoURL is the endpoint AppBotLogin queries. It is a var (not a
// const) so tests can point it at a httptest.Server without standing
// up a TLS terminator.
var appInfoURL = "https://api.github.com/app"
