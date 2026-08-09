package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAcceptsJSON(t *testing.T) {
	cases := []struct {
		name   string
		accept string
		want   bool
	}{
		{"empty header", "", true},
		{"wildcard", "*/*", true},
		{"application/json", "application/json", true},
		{"application/json with charset", "application/json; charset=utf-8", true},
		{"application/* wildcard", "application/*", true},
		{"comma list with json", "text/html, application/json", true},
		{"text/html only", "text/html", false},
		{"text/plain only", "text/plain", false},
		{"xml only", "application/xml", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := acceptsJSON(c.accept); got != c.want {
				t.Errorf("acceptsJSON(%q) = %v, want %v", c.accept, got, c.want)
			}
		})
	}
}

// TestEnforceJSONAcceptMiddleware confirms the middleware
// path. The function acceptsJSON treats a missing header
// and `*/*` as permissive (curl and fetch defaults), so a
// GET without Accept passes through. A GET with `Accept:
// text/html` is rejected with 406 (the API has no HTML
// surface to render). A POST is never gated.
func TestEnforceJSONAcceptMiddleware(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	wrap := enforceJSONAccept(next)

	// GET without Accept: permissive, next runs
	called = false
	rr := httptest.NewRecorder()
	wrap(rr, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("GET without Accept: code = %d, want %d (permissive default)", rr.Code, http.StatusOK)
	}
	if !called {
		t.Error("next was not called on permissive-default GET; should be")
	}

	// GET with Accept: text/html: 406, next does not run
	called = false
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	req.Header.Set("Accept", "text/html")
	wrap(rr, req)
	if rr.Code != http.StatusNotAcceptable {
		t.Errorf("GET with Accept text/html: code = %d, want %d", rr.Code, http.StatusNotAcceptable)
	}
	if called {
		t.Error("next was called on 406 path; should not be")
	}

	// GET with Accept: application/json: 200, next runs
	called = false
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	req.Header.Set("Accept", "application/json")
	wrap(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("GET with Accept application/json: code = %d, want %d", rr.Code, http.StatusOK)
	}
	if !called {
		t.Error("next was not called on 200 path; should be")
	}

	// POST without Accept: next runs (middleware is GET-only)
	called = false
	rr = httptest.NewRecorder()
	wrap(rr, httptest.NewRequest(http.MethodPost, "/api/runs/x/telemetry", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("POST without Accept: code = %d, want %d (middleware is GET-only)", rr.Code, http.StatusOK)
	}
	if !called {
		t.Error("next was not called on POST; should be")
	}
}
