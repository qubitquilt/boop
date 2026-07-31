package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return NewManager(AppConfig{AppID: 1, PrivateKey: priv})
}

func TestClientForCachesByInstallationID(t *testing.T) {
	m := newTestManager(t)

	a1 := m.ClientFor(42)
	a2 := m.ClientFor(42)
	if a1 != a2 {
		t.Errorf("ClientFor(42) returned different pointers across calls; want same *Client")
	}
}

func TestClientForDistinctIDs(t *testing.T) {
	m := newTestManager(t)

	a := m.ClientFor(1)
	b := m.ClientFor(2)
	if a == b {
		t.Errorf("ClientFor(1) and ClientFor(2) returned the same *Client; want distinct")
	}
	if a.installationID != 1 {
		t.Errorf("a.installationID = %d, want 1", a.installationID)
	}
	if b.installationID != 2 {
		t.Errorf("b.installationID = %d, want 2", b.installationID)
	}
}

func TestClientForBindsInstallationID(t *testing.T) {
	m := newTestManager(t)

	c := m.ClientFor(99)
	if c == nil {
		t.Fatal("ClientFor(99) returned nil")
	}
	if c.m != m {
		t.Errorf("client.m is not the manager that created it")
	}
	if c.installationID != 99 {
		t.Errorf("client.installationID = %d, want 99", c.installationID)
	}
}

func TestAppBotLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" {
			t.Errorf("AppBotLogin hit %s, want /app", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got == "" || got[:7] != "Bearer " {
			t.Errorf("missing or non-Bearer Authorization header: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":12345,"slug":"BoopPr","name":"BoopPr"}`))
	}))
	defer srv.Close()

	orig := appInfoURL
	appInfoURL = srv.URL + "/app"
	defer func() { appInfoURL = orig }()

	m := newTestManager(t)
	login, err := m.AppBotLogin(context.Background(), 99)
	if err != nil {
		t.Fatalf("AppBotLogin: %v", err)
	}
	if login != "booppr[bot]" {
		t.Errorf("AppBotLogin = %q, want %q", login, "booppr[bot]")
	}
}

func TestAppBotLoginLowercasesSlug(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"slug":"MyApp"}`))
	}))
	defer srv.Close()

	orig := appInfoURL
	appInfoURL = srv.URL + "/app"
	defer func() { appInfoURL = orig }()

	m := newTestManager(t)
	login, err := m.AppBotLogin(context.Background(), 1)
	if err != nil {
		t.Fatalf("AppBotLogin: %v", err)
	}
	if login != "myapp[bot]" {
		t.Errorf("AppBotLogin = %q, want %q", login, "myapp[bot]")
	}
}

func TestAppBotLoginEmptySlug(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	orig := appInfoURL
	appInfoURL = srv.URL + "/app"
	defer func() { appInfoURL = orig }()

	m := newTestManager(t)
	if _, err := m.AppBotLogin(context.Background(), 1); err == nil {
		t.Fatal("AppBotLogin: want error for empty slug, got nil")
	}
}

func TestAppBotLoginCachesPerInstallation(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"slug":"BoopPr"}`))
	}))
	defer srv.Close()

	orig := appInfoURL
	appInfoURL = srv.URL + "/app"
	defer func() { appInfoURL = orig }()

	m := newTestManager(t)
	if _, err := m.AppBotLogin(context.Background(), 1); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := m.AppBotLogin(context.Background(), 1); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if calls != 1 {
		t.Errorf("AppBotLogin made %d HTTP calls within TTL, want 1", calls)
	}
}
