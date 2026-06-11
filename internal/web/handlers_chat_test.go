package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newChatTestServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		dataDir: t.TempDir(),
		clients: make(map[chan string]struct{}),
	}
	s.chat = newChatHub(s, s.dataDir)
	return s
}

func TestChatEnableCreatesIdentity(t *testing.T) {
	s := newChatTestServer(t)

	// Fresh hub: info must report exists:false and create NOTHING — creating
	// (and later publishing) keys is an explicit opt-in.
	rec := httptest.NewRecorder()
	s.handleChatInfo(rec, httptest.NewRequest(http.MethodGet, "/api/chat/info", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["exists"] != false {
		t.Fatalf("fresh hub should report exists:false, got %v", resp)
	}
	s.chat.mu.Lock()
	created := s.chat.identity != nil
	s.chat.mu.Unlock()
	if created {
		t.Fatal("handleChatInfo must not create an identity")
	}

	// Availability without an identity tells the UI to prompt, not probe.
	recA := httptest.NewRecorder()
	s.handleChatAvailability(recA, httptest.NewRequest(http.MethodGet, "/api/chat/availability", nil))
	var availResp map[string]any
	if err := json.Unmarshal(recA.Body.Bytes(), &availResp); err != nil {
		t.Fatal(err)
	}
	if availResp["available"] != false || availResp["needsCreate"] != true {
		t.Fatalf("availability without identity: %v", availResp)
	}

	// Explicit opt-in creates the identity.
	recE := httptest.NewRecorder()
	s.handleChatEnable(recE, httptest.NewRequest(http.MethodPost, "/api/chat/enable", strings.NewReader(`{"action":"create"}`)))
	if recE.Code != 200 {
		t.Fatalf("enable create: %d %s", recE.Code, recE.Body.String())
	}
	var en map[string]any
	_ = json.Unmarshal(recE.Body.Bytes(), &en)
	addr, _ := en["address"].(string)
	if len(addr) != 20 {
		t.Fatalf("address = %q", addr)
	}

	// Now info reports the identity, and it persists across a hub reload.
	rec2 := httptest.NewRecorder()
	s.handleChatInfo(rec2, httptest.NewRequest(http.MethodGet, "/api/chat/info", nil))
	var resp2 map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if resp2["exists"] != true || resp2["address"] != addr {
		t.Fatalf("info after create: %v", resp2)
	}
	if h2 := newChatHub(s, s.dataDir); h2.identity == nil {
		t.Fatal("identity not persisted")
	}
}

// TestChatServerEnableConsent guards the per-server opt-in: a server is not
// enabled (so never polled/registered) until the user turns it on, and the
// choice persists.
func TestChatServerEnableConsent(t *testing.T) {
	s := newChatTestServer(t)
	s.chat.mu.Lock()
	_ = s.chat.ensureIdentityLocked()
	s.chat.mu.Unlock()
	const key = "srv.example.com"

	if s.chat.serverEnabled(key) {
		t.Fatal("server enabled by default")
	}

	// Enable (case-insensitive server key).
	rec := httptest.NewRecorder()
	s.handleChatEnable(rec, httptest.NewRequest(http.MethodPost, "/api/chat/enable",
		strings.NewReader(`{"action":"server","server":"SRV.Example.com","on":true}`)))
	if rec.Code != 200 {
		t.Fatalf("enable server: %d %s", rec.Code, rec.Body.String())
	}
	if !s.chat.serverEnabled(key) {
		t.Fatal("server not enabled after opt-in")
	}
	if h2 := newChatHub(s, s.dataDir); !h2.enabled[key] {
		t.Fatal("enabled set not persisted")
	}

	// Disable.
	rec = httptest.NewRecorder()
	s.handleChatEnable(rec, httptest.NewRequest(http.MethodPost, "/api/chat/enable",
		strings.NewReader(`{"action":"server","server":"srv.example.com","on":false}`)))
	if s.chat.serverEnabled(key) {
		t.Fatal("server still enabled after opt-out")
	}

	// Enabling a server before any identity exists is refused.
	s3 := newChatTestServer(t)
	rec = httptest.NewRecorder()
	s3.handleChatEnable(rec, httptest.NewRequest(http.MethodPost, "/api/chat/enable",
		strings.NewReader(`{"action":"server","server":"srv.example.com","on":true}`)))
	if rec.Code == 200 {
		t.Fatal("server enabled without an identity")
	}
}

func TestChatSeedRecoveryRoundTrip(t *testing.T) {
	s := newChatTestServer(t)
	// Create identity (explicit opt-in).
	s.chat.mu.Lock()
	_ = s.chat.ensureIdentityLocked()
	s.chat.mu.Unlock()

	rec := httptest.NewRecorder()
	s.handleChatSeed(rec, httptest.NewRequest(http.MethodGet, "/api/chat/seed", nil))
	var seed struct {
		Recovery string `json:"recovery"`
		BackedUp bool   `json:"backedUp"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &seed); err != nil {
		t.Fatal(err)
	}
	if seed.BackedUp || seed.Recovery == "" {
		t.Fatalf("seed resp: %+v", seed)
	}

	// Mark backed up.
	req := httptest.NewRequest(http.MethodPost, "/api/chat/seed", strings.NewReader(`{"action":"backedup"}`))
	s.handleChatSeed(httptest.NewRecorder(), req)
	if !s.chat.backedUp {
		t.Fatal("backedUp not set")
	}

	// Import the same code into a fresh hub → same address.
	origAddr := s.chat.identity.Addr
	s2 := newChatTestServer(t)
	body := `{"action":"import","recovery":"` + seed.Recovery + `"}`
	rec3 := httptest.NewRecorder()
	s2.handleChatSeed(rec3, httptest.NewRequest(http.MethodPost, "/api/chat/seed", strings.NewReader(body)))
	if rec3.Code != 200 {
		t.Fatalf("import failed: %d %s", rec3.Code, rec3.Body.String())
	}
	if s2.chat.identity.Addr != origAddr {
		t.Fatal("imported identity differs")
	}

	// Garbage rejected.
	rec4 := httptest.NewRecorder()
	s2.handleChatSeed(rec4, httptest.NewRequest(http.MethodPost, "/api/chat/seed", strings.NewReader(`{"action":"import","recovery":"zzz"}`)))
	if rec4.Code == 200 {
		t.Fatal("invalid recovery accepted")
	}
}

func TestChatContactsAndThreads(t *testing.T) {
	s := newChatTestServer(t)
	const addr = "aebagbafaydqqcikbmga"

	// Add a contact.
	body := `{"addr":"` + addr + `","name":"mom"}`
	rec := httptest.NewRecorder()
	s.handleChatContacts(rec, httptest.NewRequest(http.MethodPost, "/api/chat/contacts", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("add contact: %d %s", rec.Code, rec.Body.String())
	}

	// Bad address rejected.
	rec = httptest.NewRecorder()
	s.handleChatContacts(rec, httptest.NewRequest(http.MethodPost, "/api/chat/contacts", strings.NewReader(`{"addr":"short","name":"x"}`)))
	if rec.Code == 200 {
		t.Fatal("invalid contact address accepted")
	}

	// Contact shows up in the thread list even without messages.
	rec = httptest.NewRecorder()
	s.handleChatThreads(rec, httptest.NewRequest(http.MethodGet, "/api/chat/threads", nil))
	var threads []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &threads); err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0]["addr"] != addr || threads[0]["name"] != "mom" {
		t.Fatalf("threads: %+v", threads)
	}

	// Contacts persist across hub reload.
	h2 := newChatHub(s, s.dataDir)
	if h2.contacts[addr] != "mom" {
		t.Fatal("contact not persisted")
	}

	// Messages endpoint returns an empty thread (and validates peer).
	rec = httptest.NewRecorder()
	s.handleChatMessages(rec, httptest.NewRequest(http.MethodGet, "/api/chat/messages?peer="+addr, nil))
	var msgs map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msgs); err != nil {
		t.Fatal(err)
	}
	if msgs["peer"] != addr {
		t.Fatalf("messages: %+v", msgs)
	}
	rec = httptest.NewRecorder()
	s.handleChatMessages(rec, httptest.NewRequest(http.MethodGet, "/api/chat/messages?peer=nope", nil))
	if rec.Code == 200 {
		t.Fatal("invalid peer accepted")
	}
}

// TestChatResetNoDeadlock guards the startup hang: reset is called from
// initFetcher while it holds Server.mu (write); reset's rebuild takes
// Server.mu (read) via chatResolvers. Doing that on the caller's goroutine
// deadlocks (RWMutex isn't reentrant). reset must not block under Server.mu.
func TestChatResetNoDeadlock(t *testing.T) {
	s := newChatTestServer(t)
	// An identity must exist so reset's rebuild actually reaches chatResolvers.
	s.chat.mu.Lock()
	_ = s.chat.ensureIdentityLocked()
	s.chat.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.mu.Lock() // mimic initFetcher holding the write lock
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s.chat.reset(ctx) // must return immediately, not block on Server.mu
		s.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("chat reset deadlocked while Server.mu was held")
	}
}

func TestChatResolveServerPrecedence(t *testing.T) {
	s := newChatTestServer(t)
	h := s.chat
	h.mu.Lock()
	h.servers = map[string]*perServerChat{
		"a.example.com": {serverKey: "a.example.com"},
		"b.example.com": {serverKey: "b.example.com"},
	}
	h.activeKey = "a.example.com"
	h.threads["addr1"] = &chatThreadFile{Server: "b.example.com"}
	h.mu.Unlock()

	// Requested server wins over everything.
	if ps := h.resolveServer("addr1", "a.example.com"); ps == nil || ps.serverKey != "a.example.com" {
		t.Fatal("requested server should win")
	}
	// No request → the thread's bound server.
	if ps := h.resolveServer("addr1", ""); ps == nil || ps.serverKey != "b.example.com" {
		t.Fatal("thread server should win when none requested")
	}
	// No request, no thread server → the active server.
	if ps := h.resolveServer("addr2", ""); ps == nil || ps.serverKey != "a.example.com" {
		t.Fatal("active server should be the fallback")
	}
	// No servers at all → nil.
	h.mu.Lock()
	h.servers = map[string]*perServerChat{}
	h.mu.Unlock()
	if ps := h.resolveServer("addr1", ""); ps != nil {
		t.Fatal("expected nil with no servers")
	}
}

// TestChatBackoffConcurrent guards the per-server backoff against the data
// race the review found: pollAllServers + availability probes mutate
// backoffUntil concurrently. Run under -race.
func TestChatBackoffConcurrent(t *testing.T) {
	h := newChatTestServer(t).chat
	ps := &perServerChat{serverKey: "x"}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); h.setBackoff(ps, time.Now().Add(time.Minute)) }()
		go func() { defer wg.Done(); _ = h.backedOff(ps, time.Now()) }()
	}
	wg.Wait()
}

func TestChatThreadPinAndDelete(t *testing.T) {
	s := newChatTestServer(t)
	const addr = "aebagbafaydqqcikbmga"

	post := func(action string) int {
		rec := httptest.NewRecorder()
		body := `{"peer":"` + addr + `","action":"` + action + `"}`
		s.handleChatThread(rec, httptest.NewRequest(http.MethodPost, "/api/chat/thread", strings.NewReader(body)))
		return rec.Code
	}

	// Pin creates the thread and marks it pinned (and persists).
	if post("pin") != 200 {
		t.Fatal("pin failed")
	}
	if !s.chat.threads[addr].Pinned {
		t.Fatal("thread not pinned")
	}
	if h2 := newChatHub(s, s.dataDir); h2.threads[addr] == nil || !h2.threads[addr].Pinned {
		t.Fatal("pin not persisted")
	}
	if post("unpin") != 200 || s.chat.threads[addr].Pinned {
		t.Fatal("unpin failed")
	}

	// Delete removes the thread.
	if post("delete") != 200 {
		t.Fatal("delete failed")
	}
	if _, ok := s.chat.threads[addr]; ok {
		t.Fatal("thread not deleted")
	}

	// Bad action / bad peer rejected.
	if post("frobnicate") == 200 {
		t.Fatal("unknown action accepted")
	}
	rec := httptest.NewRecorder()
	s.handleChatThread(rec, httptest.NewRequest(http.MethodPost, "/api/chat/thread", strings.NewReader(`{"peer":"nope","action":"pin"}`)))
	if rec.Code == 200 {
		t.Fatal("invalid peer accepted")
	}
}

func TestSameOriginGuard(t *testing.T) {
	called := false
	guarded := sameOriginGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	run := func(method, fetchSite string) (code int, reached bool) {
		called = false
		req := httptest.NewRequest(method, "/api/chat/send", nil)
		if fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		return rec.Code, called
	}

	// Cross-site state-changing request is blocked before reaching the handler.
	if code, reached := run(http.MethodPost, "cross-site"); code != http.StatusForbidden || reached {
		t.Fatalf("cross-site POST: code=%d reached=%v, want 403/false", code, reached)
	}
	// Same-origin / native (no header) writes pass through.
	for _, site := range []string{"same-origin", "same-site", "none", ""} {
		if code, reached := run(http.MethodPost, site); code != http.StatusOK || !reached {
			t.Fatalf("POST Sec-Fetch-Site=%q: code=%d reached=%v, want 200/true", site, code, reached)
		}
	}
	// Reads are never blocked, even cross-site.
	if code, reached := run(http.MethodGet, "cross-site"); code != http.StatusOK || !reached {
		t.Fatalf("cross-site GET: code=%d reached=%v, want 200/true", code, reached)
	}
}
