package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// withInstallationsServer swaps the package-level
// installationsListURL to point at a httptest.Server that
// returns the given bodies, paginated via the Link header. The
// URL swap is restored on test cleanup so the test cannot leak
// state into another test.
func withInstallationsServer(t *testing.T, pages []string, links []string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The first query param `page` selects which fixture to
		// return. The Manager increments page=1, then page=2,
		// etc. — so we read the URL.
		page := r.URL.Query().Get("page")
		var idx int
		fmt.Sscanf(page, "%d", &idx)
		if idx < 1 || idx > len(pages) {
			http.Error(w, "no page", http.StatusNotFound)
			return
		}
		if idx-1 < len(links) && links[idx-1] != "" {
			w.Header().Set("Link", links[idx-1])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(pages[idx-1]))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/app/installations"
}

func TestListInstallations_SinglePage(t *testing.T) {
	page := `[
		{"id": 1, "account": {"login": "alice", "type": "User"}, "repository_selection": "all", "created_at": "2026-01-15T00:00:00Z"},
		{"id": 2, "account": {"login": "org-b", "type": "Organization"}, "repository_selection": "selected", "created_at": "2026-02-20T00:00:00Z"}
	]`
	url := withInstallationsServer(t, []string{page}, nil)
	old := installationsListURL
	installationsListURL = url
	t.Cleanup(func() { installationsListURL = old })

	m := newTestManager(t)
	got, err := m.ListInstallations(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].AccountLogin != "alice" || got[0].AccountType != "User" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].AccountLogin != "org-b" || got[1].AccountType != "Organization" {
		t.Errorf("second = %+v", got[1])
	}
	if got[0].RepositorySelection != "all" {
		t.Errorf("repo selection = %q", got[0].RepositorySelection)
	}
	if got[1].InstalledAt.IsZero() {
		t.Errorf("installed_at not parsed")
	}
}

func TestListInstallations_Pagination(t *testing.T) {
	page1 := `[{"id": 1, "account": {"login": "a", "type": "User"}, "repository_selection": "all"}]`
	page2 := `[{"id": 2, "account": {"login": "b", "type": "User"}, "repository_selection": "all"}]`
	// Page 1 has a `next` link; page 2 has no Link header (end).
	url := withInstallationsServer(t,
		[]string{page1, page2},
		[]string{`<https://example.com/?page=2>; rel="next"`},
	)
	old := installationsListURL
	installationsListURL = url
	t.Cleanup(func() { installationsListURL = old })

	m := newTestManager(t)
	got, err := m.ListInstallations(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestListInstallations_Cache(t *testing.T) {
	page := `[{"id": 1, "account": {"login": "a", "type": "User"}, "repository_selection": "all"}]`
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(srv.Close)
	url := srv.URL + "/app/installations"
	old := installationsListURL
	installationsListURL = url
	t.Cleanup(func() { installationsListURL = old })

	m := newTestManager(t)
	if _, err := m.ListInstallations(context.Background()); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := m.ListInstallations(context.Background()); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("api calls = %d, want 1 (cache should suppress the second)", got)
	}
}

func TestListInstallations_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	url := srv.URL + "/app/installations"
	old := installationsListURL
	installationsListURL = url
	t.Cleanup(func() { installationsListURL = old })

	m := newTestManager(t)
	_, err := m.ListInstallations(context.Background())
	if err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestHasNextPage(t *testing.T) {
	cases := []struct {
		link string
		want bool
	}{
		{"", false},
		{`<https://x?page=2>; rel="next"`, true},
		{`<https://x?page=2>; rel="prev"`, false},
		{`<https://x?page=2>; rel="next", <https://x?page=5>; rel="last"`, true},
		{`<https://x?page=5>; rel="last"`, false},
	}
	for _, c := range cases {
		if got := hasNextPage(c.link); got != c.want {
			t.Errorf("hasNextPage(%q) = %v, want %v", c.link, got, c.want)
		}
	}
}

// Compile-time check that the Installation struct serializes to
// the shape the store expects.
func TestInstallation_JSONShape(t *testing.T) {
	ins := Installation{
		ID:           42,
		AccountLogin: "alice",
		AccountType:  "User",
		InstalledAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	b, err := json.Marshal(ins)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "account_login", "account_type", "installed_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q in %s", k, string(b))
		}
	}
}
