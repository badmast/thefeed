package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseDefaultProfileResolvers asserts the preset parses to a
// non-empty list of host:port entries.
func TestParseDefaultProfileResolvers(t *testing.T) {
	got := parseDefaultProfileResolvers()
	if len(got) == 0 {
		t.Fatal("preset parsed to empty list")
	}
	for _, r := range got {
		if !strings.Contains(r, ":") {
			t.Errorf("resolver %q missing port", r)
		}
	}
}

// TestHandleProfileDefaults checks the endpoint returns both built-in
// profiles plus the shared resolver preset, and rejects non-GET.
func TestHandleProfileDefaults(t *testing.T) {
	s := newTestServerWithProfiles(t, nil)

	rec := httptest.NewRecorder()
	s.handleProfileDefaults(rec, httptest.NewRequest(http.MethodGet, "/api/profiles/defaults", nil))
	if rec.Code != 200 {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	var resp struct {
		Profiles  []defaultProfile `json:"profiles"`
		Resolvers []string         `json:"resolvers"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Profiles) != 3 {
		t.Fatalf("profiles = %d, want 3", len(resp.Profiles))
	}
	for _, p := range resp.Profiles {
		if p.Nickname == "" || p.Domain == "" || p.Key == "" {
			t.Errorf("incomplete profile: %+v", p)
		}
		if p.ServerKey == "" {
			t.Errorf("profile %q missing server key", p.Nickname)
		}
	}
	if len(resp.Resolvers) != len(parseDefaultProfileResolvers()) {
		t.Errorf("resolvers = %d, want %d", len(resp.Resolvers), len(parseDefaultProfileResolvers()))
	}

	rec = httptest.NewRecorder()
	s.handleProfileDefaults(rec, httptest.NewRequest(http.MethodPost, "/api/profiles/defaults", nil))
	if rec.Code != 405 {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}
}
