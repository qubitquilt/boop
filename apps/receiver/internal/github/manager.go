package github

import (
	"crypto/rsa"
	"fmt"
	"net/http"
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

	mu      sync.Mutex
	clients map[int64]*Client
}

// NewManager returns a Manager that can hand out per-installation
// Clients on demand. Each Client mints its own installation token
// (cached) the first time it's used.
func NewManager(cfg AppConfig) *Manager {
	return &Manager{
		cfg:      cfg,
		baseHTTP: &http.Client{Timeout: httpTimeout},
		clients:  make(map[int64]*Client),
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

const (
	httpTimeout     = 15 * time.Second
	tokenTTLRefresh = 5 * time.Minute
	appJWTTTL       = 10 * time.Minute
	appJWTLeeway    = 30 * time.Second
)