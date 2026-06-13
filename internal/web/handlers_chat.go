package web

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
)

// Chat web layer. One identity (seed) per data dir, shared across profiles;
// one thread per contact with messages tagged by the server they travelled
// through. Contact names never leave this machine.

const (
	chatDirName = "chat"
	// chatPollInterval is the idle background poll cadence (no UI watching).
	chatPollInterval = 3 * time.Minute
	// chatPollActiveInterval is the faster cadence used while a UI is open, so
	// incoming messages surface quickly even when the user is on the feed and
	// the messenger view is closed.
	chatPollActiveInterval = 30 * time.Second
	chatPollFirstWait      = 45 * time.Second
	// chatEnableRegisterAttempts: eager register+poll retries on server enable, so
	// a flaky network still registers promptly (not only via the background loop).
	chatEnableRegisterAttempts = 6
)

// chatIdentityFile is chat/identity.json.
type chatIdentityFile struct {
	Seed     string `json:"seed"` // base32, no padding
	BackedUp bool   `json:"backedUp,omitempty"`
}

// chatStoredMsg is one thread message on disk.
type chatStoredMsg struct {
	Dir    string `json:"dir"` // "in" | "out"
	Seq    uint32 `json:"seq"`
	Text   string `json:"text"`
	TS     int64  `json:"ts"`
	Server string `json:"server,omitempty"` // server key (main feed domain)
}

// chatThreadFile is one peer's conversation on disk.
type chatThreadFile struct {
	Msgs       []chatStoredMsg   `json:"msgs"`
	LastOutSeq map[string]uint32 `json:"lastOutSeq,omitempty"` // per server key
	Unread     int               `json:"unread,omitempty"`
	Pinned     bool              `json:"pinned,omitempty"`
	// Server is the chat server (profile main domain) this conversation is
	// bound to — chosen when the chat was started, used for sending.
	Server string `json:"server,omitempty"`
	// LastAccepted/LastDelivered cache the server's ✓/✓✓ counters so ticks
	// render correctly right after a restart, instead of showing 🕓 until the
	// first peer-status DNS round trip returns.
	LastAccepted  uint32 `json:"acc,omitempty"`
	LastDelivered uint32 `json:"del,omitempty"`
	// ServerSetAt: when the user last switched send server — the UI shows the
	// "peer switched" banner only for peer messages newer than this.
	ServerSetAt int64 `json:"serverSetAt,omitempty"`
}

// chatThreadsFile is chat/threads.json.
type chatThreadsFile struct {
	Threads map[string]*chatThreadFile `json:"threads"`
}

// perServerChat is one chat-enabled profile's client. The same identity is
// reused across every server, so the user has one address everywhere.
// backoffUntil is mutated from concurrent poll/availability goroutines, so it
// is guarded by chatHub.mu (see backedOffLocked / setBackoff).
type perServerChat struct {
	serverKey    string // lowercased main domain (provenance tag)
	name         string // profile nickname (for the picker)
	domain       string
	client       *client.ChatClient
	f            *client.Fetcher // its fetcher, so resolvers can be re-synced live
	backoffUntil time.Time       // skip polling until this time (guarded by chatHub.mu)
}

// chatHub owns the web-layer chat state. Chat is multi-server: it polls and
// sends across EVERY profile that pins a server key, so switching the active
// feed profile never cuts off conversations on another server.
type chatHub struct {
	s   *Server
	dir string

	mu        sync.Mutex
	identity  *client.ChatIdentity
	backedUp  bool
	ctx       context.Context
	servers   map[string]*perServerChat // by serverKey
	activeKey string                    // the active profile's server key
	contacts  map[string]string
	threads   map[string]*chatThreadFile
	// enabled is the set of server keys the user has explicitly opted in to.
	// Only these are polled and registered on (publishing the chat pubkey), so
	// an untrusted feed server never silently receives the user's identity.
	enabled map[string]bool
	// lastResolvers is the resolver list last pushed to the chat fetchers; chat
	// resolvers track the feed's live healthy pool, re-synced only on change.
	lastResolvers []string

	sendMu sync.Mutex // one upload at a time (uplink is scarce)
	pollMu sync.Mutex // one inbox poll at a time
}

func newChatHub(s *Server, dataDir string) *chatHub {
	h := &chatHub{
		s:        s,
		dir:      filepath.Join(dataDir, chatDirName),
		servers:  make(map[string]*perServerChat),
		contacts: make(map[string]string),
		threads:  make(map[string]*chatThreadFile),
		enabled:  make(map[string]bool),
	}
	h.loadState()
	return h
}

func chatServerKey(domain string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
}

var chatSeedEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

func (h *chatHub) loadState() {
	if raw, err := os.ReadFile(filepath.Join(h.dir, "identity.json")); err == nil {
		var f chatIdentityFile
		if json.Unmarshal(raw, &f) == nil {
			if seed, err := chatSeedEnc.DecodeString(strings.ToUpper(f.Seed)); err == nil && len(seed) == protocol.SeedSize {
				if id, err := client.NewChatIdentity(seed); err == nil {
					h.identity = id
					h.backedUp = f.BackedUp
				}
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(h.dir, "contacts.json")); err == nil {
		_ = json.Unmarshal(raw, &h.contacts)
	}
	if raw, err := os.ReadFile(filepath.Join(h.dir, "threads.json")); err == nil {
		var f chatThreadsFile
		if json.Unmarshal(raw, &f) == nil && f.Threads != nil {
			h.threads = f.Threads
		}
	}
	if raw, err := os.ReadFile(filepath.Join(h.dir, "servers.json")); err == nil {
		var en map[string]bool
		if json.Unmarshal(raw, &en) == nil && en != nil {
			h.enabled = en
		}
	}
}

// writeJSONAtomic persists v at path via tmp+rename.
func (h *chatHub) writeJSONAtomic(name string, v any) error {
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	path := filepath.Join(h.dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// saveIdentityLocked persists identity.json. Caller holds mu.
func (h *chatHub) saveIdentityLocked() {
	if h.identity == nil {
		return
	}
	_ = h.writeJSONAtomic("identity.json", chatIdentityFile{
		Seed:     strings.ToLower(chatSeedEnc.EncodeToString(h.identity.Seed)),
		BackedUp: h.backedUp,
	})
}

func (h *chatHub) saveContactsLocked() {
	_ = h.writeJSONAtomic("contacts.json", h.contacts)
}

func (h *chatHub) saveThreadsLocked() {
	_ = h.writeJSONAtomic("threads.json", chatThreadsFile{Threads: h.threads})
}

func (h *chatHub) saveEnabledLocked() {
	_ = h.writeJSONAtomic("servers.json", h.enabled)
}

// reset rebuilds the per-server chat clients from the current profiles and
// (re)starts the poll loop on the new lifetime context. Called from
// initFetcher on every config/profile change.
func (h *chatHub) reset(ctx context.Context) {
	// Run on a separate goroutine: reset is called from initFetcher while it
	// holds Server.mu (write), and rebuildServersLocked → chatResolvers takes
	// Server.mu (read). A sync.RWMutex is NOT reentrant, so doing that on the
	// same goroutine would deadlock and hang the client at startup.
	go func() {
		h.mu.Lock()
		h.ctx = ctx
		h.rebuildServersLocked()
		h.mu.Unlock()
		h.runPollLoop(ctx)
	}()
}

// rebuildServersLocked builds one ChatClient per profile that pins a server
// key (chat is fail-closed: no sk → no chat). The shared identity is reused.
// Caller holds mu.
func (h *chatHub) rebuildServersLocked() {
	h.servers = make(map[string]*perServerChat)
	h.activeKey = ""
	if h.identity == nil || h.ctx == nil {
		return
	}
	pl, err := h.s.loadProfiles()
	if err != nil {
		return
	}
	resolvers := h.s.chatResolvers()
	for _, p := range pl.Profiles {
		if strings.TrimSpace(p.Config.ServerKey) == "" {
			continue
		}
		key := chatServerKey(p.Config.Domain)
		if key == "" || h.servers[key] != nil {
			continue
		}
		f, ferr := h.s.buildChatFetcher(p.Config, resolvers, h.ctx)
		if ferr != nil {
			h.s.addLog("[chat] server " + key + ": " + ferr.Error())
			continue
		}
		h.servers[key] = &perServerChat{
			serverKey: key,
			name:      p.Nickname,
			domain:    p.Config.Domain,
			client:    client.NewChatClient(f, h.identity),
			f:         f,
		}
		if p.ID == pl.Active {
			h.activeKey = key
		}
	}
}

// ensureIdentityLocked creates the identity on first use, then builds the
// per-server clients. Caller holds mu.
func (h *chatHub) ensureIdentityLocked() error {
	if h.identity != nil {
		return nil
	}
	seed, err := protocol.GenerateSeed()
	if err != nil {
		return err
	}
	id, err := client.NewChatIdentity(seed)
	if err != nil {
		return err
	}
	h.identity = id
	h.backedUp = false
	h.saveIdentityLocked()
	h.rebuildServersLocked()
	return nil
}

// serverFor returns the per-server client for a server key, or nil.
func (h *chatHub) serverFor(key string) *perServerChat {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.servers[chatServerKey(key)]
}

// snapshotServers returns the current per-server clients.
func (h *chatHub) snapshotServers() []*perServerChat {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*perServerChat, 0, len(h.servers))
	for _, ps := range h.servers {
		out = append(out, ps)
	}
	return out
}

// resolveServer picks the chat server for a conversation: an explicitly
// requested server key, else the thread's bound server, else the active
// profile's server, else any. Returns nil when no chat server exists.
func (h *chatHub) resolveServer(addr, requested string) *perServerChat {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := chatServerKey(requested)
	if key == "" {
		if th := h.threads[addr]; th != nil && th.Server != "" {
			key = th.Server
		}
	}
	if key == "" {
		key = h.activeKey
	}
	if ps := h.servers[key]; ps != nil {
		return ps
	}
	for _, ps := range h.servers {
		return ps
	}
	return nil
}

// chatServerBackoff: a server with no chat shouldn't be re-queried often; a
// transient failure is retried sooner.
func chatServerBackoff(err error) time.Duration {
	if errors.Is(err, client.ErrChatDisabled) ||
		errors.Is(err, client.ErrChatNoServerKey) ||
		errors.Is(err, client.ErrChatUnverified) {
		return 30 * time.Minute
	}
	return 2 * time.Minute
}

// runPollLoop polls every chat server in the background while an identity
// exists (a user who never opened chat is never auto-registered).
func (h *chatHub) runPollLoop(ctx context.Context) {
	timer := time.NewTimer(chatPollFirstWait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if n := h.pollAllServers(ctx, nil); n > 0 {
			h.s.broadcastChat(map[string]any{"type": "inbox", "got": n})
			// Native notifier (mobile): the web UI handles foreground alerts, but
			// a backgrounded app has its WebView suspended — let the platform post
			// a system notification. The handler itself decides foreground gating.
			h.s.notifyNewMessages(n)
		}
		interval := chatPollInterval
		if h.s.hasUIClients() {
			interval = chatPollActiveInterval
		}
		timer.Reset(interval)
	}
}

// backedOff reports whether ps is currently in its no-chat/transient backoff
// window. setBackoff updates the window. Both guard the shared backoffUntil
// under h.mu, since polls and availability probes run concurrently.
func (h *chatHub) backedOff(ps *perServerChat, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return now.Before(ps.backoffUntil)
}

func (h *chatHub) setBackoff(ps *perServerChat, until time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ps.backoffUntil = until
}

// serverEnabled reports whether the user has opted in to publish/poll on this
// server key. h.enabled is the single guarded source of truth (read from the
// unlocked poll path and the availability goroutines).
func (h *chatHub) serverEnabled(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.enabled[key]
}

// syncResolvers pushes the feed's current healthy resolver pool into every chat
// fetcher so chat rides the same live, scored resolver list as the feed instead
// of a stale snapshot taken when the fetcher was built. It only re-pushes (and
// logs) when the list actually changes. Called before each poll cycle and
// availability probe.
func (h *chatHub) syncResolvers() {
	rs := h.s.chatResolvers()
	if len(rs) == 0 {
		return
	}
	h.mu.Lock()
	changed := !equalStrings(h.lastResolvers, rs)
	if changed {
		h.lastResolvers = append([]string(nil), rs...)
	}
	servers := make([]*perServerChat, 0, len(h.servers))
	for _, ps := range h.servers {
		servers = append(servers, ps)
	}
	h.mu.Unlock()
	if !changed {
		return
	}
	for _, ps := range servers {
		if ps.f != nil {
			ps.f.SetActiveResolvers(rs)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pollAllServers polls each non-backed-off server; a server that reports no
// chat is backed off so we stop hammering it. Returns total new messages.
func (h *chatHub) pollAllServers(ctx context.Context, progress client.ChatProgress) int {
	h.mu.Lock()
	idOK := h.identity != nil
	h.mu.Unlock()
	if !idOK {
		return 0
	}
	h.syncResolvers() // ride the feed's current healthy resolver pool
	now := time.Now()
	total := 0
	for _, ps := range h.snapshotServers() {
		// Only poll (which opens a read session and auto-registers) servers the
		// user has explicitly enabled — never publish the identity otherwise.
		if !h.serverEnabled(ps.serverKey) || h.backedOff(ps, now) {
			continue
		}
		n, err := h.pollServer(ctx, ps, progress)
		if err != nil {
			h.setBackoff(ps, time.Now().Add(chatServerBackoff(err)))
			continue
		}
		h.setBackoff(ps, time.Time{}) // clear backoff on success
		total += n
	}
	return total
}

// pollServer fetches one server's inbox, stores new messages (tagged with the
// server), and acks them.
func (h *chatHub) pollServer(ctx context.Context, ps *perServerChat, progress client.ChatProgress) (int, error) {
	h.pollMu.Lock()
	defer h.pollMu.Unlock()

	msgs, err := ps.client.FetchInbox(ctx, progress)
	if err != nil {
		return 0, err
	}
	if len(msgs) == 0 {
		return 0, nil
	}

	h.mu.Lock()
	added := 0
	maxSeq := map[string]uint32{}
	for _, m := range msgs {
		addr := client.ChatAddressString(m.From)
		// ACK every fetched message — including ones already stored from a
		// prior poll whose ACK was lost — else a lost ACK wedges the sender's
		// per-pair quota until the message TTL-expires.
		if m.Seq > maxSeq[addr] {
			maxSeq[addr] = m.Seq
		}
		th := h.threads[addr]
		if th == nil {
			th = &chatThreadFile{}
			h.threads[addr] = th
		}
		if th.Server == "" {
			th.Server = ps.serverKey
		}
		dup := false
		for _, ex := range th.Msgs {
			if ex.Dir == "in" && ex.Seq == m.Seq && ex.Server == ps.serverKey {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		th.Msgs = append(th.Msgs, chatStoredMsg{
			Dir: "in", Seq: m.Seq, Text: m.Text,
			TS: time.Now().Unix(), Server: ps.serverKey,
		})
		th.Unread++
		added++
	}
	if added > 0 {
		h.saveThreadsLocked()
	}
	h.mu.Unlock()

	if added > 0 {
		// Render the new messages immediately. The per-peer ACK below is a DNS
		// round trip (seconds on a flaky resolver) that only frees the sender's
		// quota — it has nothing to do with display and must not delay the UI.
		// Notification fires separately (the 'inbox' event), so this render-only
		// signal doesn't double-notify.
		h.s.broadcastChat(map[string]any{"type": "inboxstored", "got": added})
	}

	for addr, seq := range maxSeq {
		peer, perr := client.ParseChatAddress(addr)
		if perr != nil {
			continue
		}
		if err := ps.client.Ack(ctx, peer, seq); err != nil {
			h.s.addLog("[chat] ack " + addr[:6] + ": " + err.Error())
		}
	}
	return added, nil
}

// broadcastChat pushes a chat SSE event to all connected UIs.
func (s *Server) broadcastChat(payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.broadcast("event: chat\ndata: " + string(raw) + "\n\n")
}

// chatStatusKey maps driver errors to i18n keys for the UI.
func chatStatusKey(err error) string {
	var serr *client.ChatStatusError
	switch {
	case errors.Is(err, client.ErrChatNoServerKey):
		return "chat_err_no_key"
	case errors.Is(err, client.ErrChatServerDisabled):
		return "chat_err_server_disabled"
	case errors.Is(err, client.ErrChatUnreachable):
		return "chat_err_unreachable"
	case errors.Is(err, client.ErrChatDisabled):
		return "chat_err_disabled"
	case errors.Is(err, client.ErrChatUnverified):
		return "chat_err_unverified"
	case errors.As(err, &serr):
		switch serr.Status {
		case protocol.ChatStatusRateLimited:
			return "chat_err_rate_limited"
		case protocol.ChatStatusInboxFull:
			return "chat_err_inbox_full"
		case protocol.ChatStatusPairQuota:
			return "chat_err_pair_quota"
		case protocol.ChatStatusUnknownRecipient, protocol.ChatStatusNotFound:
			return "chat_err_unknown_recipient"
		case protocol.ChatStatusBusy:
			return "chat_err_busy"
		}
	}
	return "chat_err_generic"
}

// chatSafetyEmojiTable is the 64-emoji alphabet for the conversation safety
// code (both clients derive the same 5 emojis from the pair's identity keys).
var chatSafetyEmojiTable = []string{
	"🐶", "🐱", "🦊", "🐻", "🐼", "🐨", "🐯", "🦁",
	"🐮", "🐷", "🐸", "🐵", "🐔", "🐧", "🐦", "🦆",
	"🦉", "🐴", "🦄", "🐝", "🐢", "🐙", "🦋", "🐬",
	"🐳", "🐊", "🦒", "🐘", "🦔", "🐿", "🦜", "🦢",
	"🍎", "🍊", "🍋", "🍉", "🍇", "🍓", "🍒", "🍑",
	"🥝", "🍍", "🥥", "🌽", "🥕", "🍄", "🌻", "🌹",
	"🌵", "🍀", "🌲", "⭐", "🌙", "☀", "⛅", "🌈",
	"⚡", "❄", "🔥", "🎲", "🎈", "🎵", "⚽", "🔑",
}

// chatSafetyEmojis derives the shared safety code for two identity keys.
func chatSafetyEmojis(pubA, pubB []byte) []string {
	a, b := pubA, pubB
	if string(a) > string(b) {
		a, b = b, a
	}
	sum := sha256.Sum256(append(append([]byte("thefeed-chat-safety-v1"), a...), b...))
	out := make([]string, 5)
	for i := 0; i < 5; i++ {
		out[i] = chatSafetyEmojiTable[int(sum[i])%len(chatSafetyEmojiTable)]
	}
	return out
}

// ---- HTTP handlers ----

// handleChatInfo reports the local identity WITHOUT creating one — creating
// keys (and later publishing them) is an explicit opt-in via handleChatEnable.
// When no identity exists yet it returns exists:false so the UI shows the
// "enable messenger" prompt instead of silently registering an account.
func (s *Server) handleChatInfo(w http.ResponseWriter, r *http.Request) {
	h := s.chat
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.identity == nil {
		writeJSON(w, map[string]any{"exists": false})
		return
	}
	anyEnabled := false
	for key := range h.servers {
		if h.enabled[key] {
			anyEnabled = true
			break
		}
	}
	writeJSON(w, map[string]any{
		"exists":      true,
		"address":     client.ChatAddressString(h.identity.Addr),
		"backedUp":    h.backedUp,
		"serverCount": len(h.servers),
		// anyEnabled is the server-side source of truth for "has the user set up
		// a chat server" — the client uses it to drive first-run guidance. It
		// lives on disk (h.enabled), so it survives the Android loopback-port
		// change that wipes localStorage.
		"anyEnabled": anyEnabled,
	})
}

// handleChatAvailability probes EVERY chat server (each profile that pins a
// server key) in parallel and reports per-server availability plus the
// aggregate. The frontend uses the list to pick a server when starting a new
// chat and to gate composing.
func (s *Server) handleChatAvailability(w http.ResponseWriter, r *http.Request) {
	h := s.chat
	h.mu.Lock()
	hasID := h.identity != nil
	h.mu.Unlock()

	resp := map[string]any{"available": false}
	if !hasID {
		// No identity yet → the UI shows the "enable messenger" prompt; don't
		// create keys or probe here.
		resp["needsCreate"] = true
		writeJSON(w, resp)
		return
	}
	h.syncResolvers() // probe over the feed's current healthy resolver pool
	servers := h.snapshotServers()
	if len(servers) == 0 {
		// No profile pins a server key → chat is fail-closed everywhere.
		resp["reason"] = "chat_err_no_key"
		writeJSON(w, resp)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	type sres struct {
		Key       string `json:"key"`
		Name      string `json:"name,omitempty"`
		Domain    string `json:"domain"`
		Available bool   `json:"available"`
		Enabled   bool   `json:"enabled"`
		Reason    string `json:"reason,omitempty"`
	}
	out := make([]sres, len(servers))
	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		anyUsable   bool // enabled AND reachable
		limits      map[string]any
		firstReason string
	)
	for i, ps := range servers {
		wg.Add(1)
		go func(i int, ps *perServerChat) {
			defer wg.Done()
			cctx, c := context.WithTimeout(ctx, 45*time.Second)
			defer c()
			info, err := ps.client.EnsureInfo(cctx)
			res := sres{Key: ps.serverKey, Name: ps.name, Domain: ps.domain, Enabled: h.serverEnabled(ps.serverKey)}
			if err != nil {
				res.Reason = chatStatusKey(err)
				h.setBackoff(ps, time.Now().Add(chatServerBackoff(err)))
			} else {
				res.Available = true
				h.setBackoff(ps, time.Time{}) // reachable → clear any backoff
				mu.Lock()
				if res.Enabled {
					anyUsable = true
				}
				if limits == nil {
					limits = map[string]any{
						"maxMsgBytes": info.Limits.MaxMsgBytes,
						"sendPerHour": info.Limits.SendPerHour,
						"inboxCap":    info.Limits.InboxCap,
						"ttlHours":    info.Limits.TTLHours,
					}
				}
				mu.Unlock()
			}
			out[i] = res
		}(i, ps)
	}
	wg.Wait()

	for _, r := range out {
		if firstReason == "" && r.Reason != "" {
			firstReason = r.Reason
		}
	}

	resp["servers"] = out
	// "available" means the user can actually chat now: at least one enabled,
	// reachable server. Reachable-but-not-enabled servers appear in the list so
	// the user can turn them on.
	resp["available"] = anyUsable
	if limits != nil {
		resp["limits"] = limits
	}
	if !anyUsable && firstReason != "" {
		resp["reason"] = firstReason
	}
	writeJSON(w, resp)
}

// handleChatEnable is the explicit chat opt-in. action "create" generates the
// local identity (no key leaves the machine until a server is turned on);
// action "server" turns the messenger on/off for one server. Registration —
// publishing the chat pubkey — happens ONLY for servers turned on here.
func (s *Server) handleChatEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Action string `json:"action"` // "create" | "server"
		Server string `json:"server"`
		On     bool   `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	h := s.chat
	switch req.Action {
	case "create":
		h.mu.Lock()
		err := h.ensureIdentityLocked()
		addr := ""
		if h.identity != nil {
			addr = client.ChatAddressString(h.identity.Addr)
		}
		h.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "address": addr})

	case "server":
		key := chatServerKey(req.Server)
		if key == "" {
			http.Error(w, "invalid server", 400)
			return
		}
		h.mu.Lock()
		if h.identity == nil {
			h.mu.Unlock()
			http.Error(w, "no identity", 400)
			return
		}
		if req.On {
			h.enabled[key] = true
		} else {
			delete(h.enabled, key)
		}
		h.saveEnabledLocked()
		ps := h.servers[key]
		ctx := h.ctx
		h.mu.Unlock()

		// Register on the newly-enabled server (and pull mail) now, retrying a
		// flaky network. pollServer opens a session, which auto-registers.
		if req.On && ps != nil && ctx != nil {
			go func() {
				var lastErr error
				for attempt := 0; attempt < chatEnableRegisterAttempts; attempt++ {
					n, err := h.pollServer(ctx, ps, nil)
					if err == nil {
						h.setBackoff(ps, time.Time{}) // reachable → clear backoff
						if n > 0 {
							h.s.broadcastChat(map[string]any{"type": "inbox", "got": n})
						}
						return
					}
					lastErr = err
					h.s.addLog("[chat] enable " + key + ": " + err.Error())
					backoff := time.Duration(attempt+1) * 1500 * time.Millisecond
					if backoff > 5*time.Second {
						backoff = 5 * time.Second
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(backoff):
					}
				}
				// Eager attempt exhausted — hand off to the background poll loop.
				h.setBackoff(ps, time.Now().Add(chatServerBackoff(lastErr)))
			}()
		}
		writeJSON(w, map[string]any{"ok": true})

	default:
		http.Error(w, "unknown action", 400)
	}
}

// handleChatSeed serves and manages the recovery code.
func (s *Server) handleChatSeed(w http.ResponseWriter, r *http.Request) {
	h := s.chat
	h.mu.Lock()
	defer h.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		if h.identity == nil {
			http.Error(w, "no identity", 404)
			return
		}
		code := strings.ToLower(chatSeedEnc.EncodeToString(h.identity.Seed))
		var grouped []string
		for len(code) > 4 {
			grouped = append(grouped, code[:4])
			code = code[4:]
		}
		grouped = append(grouped, code)
		writeJSON(w, map[string]any{"recovery": strings.Join(grouped, "-"), "backedUp": h.backedUp})
	case http.MethodPost:
		var req struct {
			Action   string `json:"action"`
			Recovery string `json:"recovery"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		switch req.Action {
		case "backedup":
			h.backedUp = true
			h.saveIdentityLocked()
			writeJSON(w, map[string]any{"ok": true})
		case "import":
			clean := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(req.Recovery))
			seed, err := chatSeedEnc.DecodeString(clean)
			if err != nil || len(seed) != protocol.SeedSize {
				http.Error(w, "invalid recovery code", 400)
				return
			}
			id, err := client.NewChatIdentity(seed)
			if err != nil {
				http.Error(w, "invalid recovery code", 400)
				return
			}
			h.identity = id
			h.backedUp = true
			h.saveIdentityLocked()
			h.rebuildServersLocked() // rebind all per-server clients to the new identity
			writeJSON(w, map[string]any{"ok": true, "address": client.ChatAddressString(id.Addr)})
		default:
			http.Error(w, "unknown action", 400)
		}
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// handleChatContacts lists and edits the local contact book.
func (s *Server) handleChatContacts(w http.ResponseWriter, r *http.Request) {
	h := s.chat
	switch r.Method {
	case http.MethodGet:
		h.mu.Lock()
		type entry struct {
			Addr string `json:"addr"`
			Name string `json:"name"`
		}
		out := make([]entry, 0, len(h.contacts))
		for a, n := range h.contacts {
			out = append(out, entry{a, n})
		}
		h.mu.Unlock()
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		writeJSON(w, out)
	case http.MethodPost:
		var req struct {
			Addr string `json:"addr"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if _, err := client.ParseChatAddress(req.Addr); err != nil {
			http.Error(w, "invalid address", 400)
			return
		}
		addr := strings.ToLower(strings.TrimSpace(req.Addr))
		h.mu.Lock()
		if strings.TrimSpace(req.Name) == "" {
			delete(h.contacts, addr)
		} else {
			h.contacts[addr] = strings.TrimSpace(req.Name)
		}
		h.saveContactsLocked()
		h.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// handleChatThreads lists conversations, newest first.
func (s *Server) handleChatThreads(w http.ResponseWriter, r *http.Request) {
	h := s.chat
	h.mu.Lock()
	type entry struct {
		Addr     string `json:"addr"`
		Name     string `json:"name,omitempty"`
		LastText string `json:"lastText,omitempty"`
		LastTS   int64  `json:"lastTs,omitempty"`
		LastDir  string `json:"lastDir,omitempty"`
		Unread   int    `json:"unread,omitempty"`
		Pinned   bool   `json:"pinned,omitempty"`
	}
	out := make([]entry, 0, len(h.threads))
	for addr, th := range h.threads {
		e := entry{Addr: addr, Name: h.contacts[addr], Unread: th.Unread, Pinned: th.Pinned}
		if n := len(th.Msgs); n > 0 {
			last := th.Msgs[n-1]
			e.LastText, e.LastTS, e.LastDir = last.Text, last.TS, last.Dir
		}
		out = append(out, e)
	}
	// Contacts without a thread still appear (so you can start a chat).
	for addr, name := range h.contacts {
		if _, ok := h.threads[addr]; !ok {
			out = append(out, entry{Addr: addr, Name: name})
		}
	}
	h.mu.Unlock()
	// Pinned first, then most-recent first.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		return out[i].LastTS > out[j].LastTS
	})
	writeJSON(w, out)
}

// handleChatThread mutates one conversation: delete it, or pin/unpin it.
func (s *Server) handleChatThread(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Peer   string `json:"peer"`
		Action string `json:"action"` // delete | clear | pin | unpin
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if _, err := client.ParseChatAddress(req.Peer); err != nil {
		http.Error(w, "invalid peer", 400)
		return
	}
	addr := strings.ToLower(strings.TrimSpace(req.Peer))
	h := s.chat
	h.mu.Lock()
	defer h.mu.Unlock()
	switch req.Action {
	case "delete":
		// Drop local history AND the saved contact name: otherwise the contact
		// (named on rename) keeps the row alive in handleChatThreads, so a
		// "deleted" conversation reappears. The server-side inbox is E2E +
		// TTL'd and not ours to delete here.
		delete(h.threads, addr)
		delete(h.contacts, addr)
		h.saveThreadsLocked()
		h.saveContactsLocked()
	case "clear":
		// Wipe local message history but keep the conversation itself — the
		// contact name, server binding, seq counters and delivery ticks all
		// stay, so the chat stays open and the next send keeps numbering
		// correctly. Local-only: the server keeps no delivered messages.
		if th := h.threads[addr]; th != nil {
			th.Msgs = nil
			th.Unread = 0
			h.saveThreadsLocked()
		}
	case "pin", "unpin":
		th := h.threads[addr]
		if th == nil {
			th = &chatThreadFile{}
			h.threads[addr] = th
		}
		th.Pinned = req.Action == "pin"
		h.saveThreadsLocked()
	default:
		http.Error(w, "unknown action", 400)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleChatMessages returns one conversation (optionally clearing unread).
func (s *Server) handleChatMessages(w http.ResponseWriter, r *http.Request) {
	h := s.chat
	addr := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("peer")))
	if _, err := client.ParseChatAddress(addr); err != nil {
		http.Error(w, "invalid peer", 400)
		return
	}
	h.mu.Lock()
	th := h.threads[addr]
	if th == nil {
		th = &chatThreadFile{}
	}
	if r.URL.Query().Get("markRead") == "1" && th.Unread != 0 {
		th.Unread = 0
		h.saveThreadsLocked()
	}
	msgs := th.Msgs
	if msgs == nil {
		msgs = []chatStoredMsg{} // a typed-nil slice marshals to null; force []
	}
	resp := map[string]any{
		"peer":        addr,
		"name":        h.contacts[addr],
		"msgs":        msgs,
		"server":      th.Server,
		"accepted":    th.LastAccepted,
		"delivered":   th.LastDelivered,
		"serverSetAt": th.ServerSetAt,
	}
	h.mu.Unlock()
	writeJSON(w, resp)
}

// handleChatSend uploads one message, streaming progress over SSE.
func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Peer   string `json:"peer"`
		Text   string `json:"text"`
		Server string `json:"server"` // chat server (profile domain) to send via
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	peer, err := client.ParseChatAddress(req.Peer)
	if err != nil {
		http.Error(w, "invalid peer", 400)
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		http.Error(w, "empty message", 400)
		return
	}

	h := s.chat
	peerStr := strings.ToLower(strings.TrimSpace(req.Peer))
	ps := h.resolveServer(peerStr, req.Server)
	if ps == nil {
		writeJSON(w, map[string]any{"ok": false, "error": "chat_err_disabled"})
		return
	}
	chatc, serverKey := ps.client, ps.serverKey

	// Sending requires an identity, and is itself consent to use this server —
	// enable it so the background loop keeps polling for replies.
	h.mu.Lock()
	if h.identity == nil {
		h.mu.Unlock()
		writeJSON(w, map[string]any{"ok": false, "error": "chat_err_disabled"})
		return
	}
	if !h.enabled[serverKey] {
		h.enabled[serverKey] = true
		h.saveEnabledLocked()
	}
	h.mu.Unlock()

	h.sendMu.Lock()
	defer h.sendMu.Unlock()

	h.mu.Lock()
	th := h.threads[peerStr]
	if th == nil {
		th = &chatThreadFile{}
		h.threads[peerStr] = th
	}
	if th.Server == "" {
		th.Server = serverKey // bind the conversation to this server
	}
	if th.LastOutSeq == nil {
		th.LastOutSeq = make(map[string]uint32)
	}
	lastSeq := th.LastOutSeq[serverKey]
	h.mu.Unlock()
	seq := lastSeq + 1

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	if lastSeq == 0 {
		// First send to this peer through this server (or local state was
		// lost): recover the seq from the server.
		if next, err := chatc.NextSeq(ctx, peer); err == nil && next > seq {
			seq = next
		}
	}

	progress := func(done, total int) {
		s.broadcastChat(map[string]any{
			"type": "progress", "op": "send", "peer": peerStr,
			"done": done, "total": total,
		})
	}

	res, err := chatc.SendMessage(ctx, peer, seq, text, progress)
	if err != nil {
		var serr *client.ChatStatusError
		if errors.As(err, &serr) && serr.Status == protocol.ChatStatusReplay {
			// Seq raced (another device): retry once just above the
			// server's counter.
			seq = serr.LastAccepted + 1
			res, err = chatc.SendMessage(ctx, peer, seq, text, progress)
		}
	}
	if err != nil {
		key := chatStatusKey(err)
		out := map[string]any{"ok": false, "error": key}
		var serr *client.ChatStatusError
		if errors.As(err, &serr) && serr.ResetUnix != 0 {
			out["resetUnix"] = serr.ResetUnix
			out["remaining"] = serr.Remaining
		}
		s.addLog("[chat] send failed: " + err.Error())
		writeJSON(w, out)
		return
	}

	// Re-fetch the thread under the lock: a delete may have removed the
	// pre-send pointer during the (multi-minute) upload, and reusing it would
	// persist the map without this conversation, dropping the sent message.
	h.mu.Lock()
	th = h.threads[peerStr]
	if th == nil {
		th = &chatThreadFile{Server: serverKey}
		h.threads[peerStr] = th
	}
	if th.LastOutSeq == nil {
		th.LastOutSeq = make(map[string]uint32)
	}
	th.LastOutSeq[serverKey] = res.Seq
	if res.Seq > th.LastAccepted {
		th.LastAccepted = res.Seq // a committed send IS accepted (✓)
	}
	th.Msgs = append(th.Msgs, chatStoredMsg{
		Dir: "out", Seq: res.Seq, Text: text,
		TS: time.Now().Unix(), Server: serverKey,
	})
	h.saveThreadsLocked()
	h.mu.Unlock()

	writeJSON(w, map[string]any{
		"ok": true, "seq": res.Seq,
		"remaining": res.Remaining, "resetUnix": res.ResetUnix,
	})
}

// handleChatSetServer rebinds a conversation to another server, only after
// confirming the peer is registered there.
func (s *Server) handleChatSetServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Peer   string `json:"peer"`
		Server string `json:"server"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	peer, err := client.ParseChatAddress(req.Peer)
	if err != nil {
		http.Error(w, "invalid peer", 400)
		return
	}
	addr := strings.ToLower(strings.TrimSpace(req.Peer))
	h := s.chat
	h.mu.Lock()
	ps := h.servers[chatServerKey(req.Server)]
	enabled := ps != nil && h.enabled[ps.serverKey]
	h.mu.Unlock()
	if ps == nil {
		writeJSON(w, map[string]any{"ok": false, "error": "chat_err_disabled"})
		return
	}
	if !enabled {
		// Must be on first — switching would publish the identity there.
		writeJSON(w, map[string]any{"ok": false, "error": "chat_err_server_off"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	reg, err := ps.client.IsRegistered(ctx, peer)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": chatStatusKey(err)})
		return
	}
	if !reg {
		writeJSON(w, map[string]any{"ok": false, "error": "chat_err_peer_not_on_server"})
		return
	}

	h.mu.Lock()
	th := h.threads[addr]
	if th == nil {
		th = &chatThreadFile{}
		h.threads[addr] = th
	}
	th.Server = ps.serverKey
	th.ServerSetAt = time.Now().Unix()
	h.saveThreadsLocked()
	h.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "server": ps.serverKey})
}

// handleChatPoll fetches the inbox now, with download progress over SSE.
func (s *Server) handleChatPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	h := s.chat
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	progress := func(done, total int) {
		s.broadcastChat(map[string]any{
			"type": "progress", "op": "poll",
			"done": done, "total": total,
		})
	}
	n := h.pollAllServers(ctx, progress)
	writeJSON(w, map[string]any{"ok": true, "got": n})
}

// handleChatPeerStatus returns ✓/✓✓ counters and the safety emojis for a
// conversation.
func (s *Server) handleChatPeerStatus(w http.ResponseWriter, r *http.Request) {
	h := s.chat
	peer, err := client.ParseChatAddress(r.URL.Query().Get("peer"))
	if err != nil {
		http.Error(w, "invalid peer", 400)
		return
	}
	addr := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("peer")))
	ps := h.resolveServer(addr, r.URL.Query().Get("server"))
	if ps == nil {
		writeJSON(w, map[string]any{"ok": false, "error": "chat_err_disabled"})
		return
	}
	chatc := ps.client
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	accepted, delivered, err := chatc.PeerStatus(ctx, peer)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": chatStatusKey(err)})
		return
	}
	// Cache the counters in the thread file so ticks survive a restart.
	h.mu.Lock()
	if th := h.threads[addr]; th != nil && (th.LastAccepted != accepted || th.LastDelivered != delivered) {
		th.LastAccepted, th.LastDelivered = accepted, delivered
		h.saveThreadsLocked()
	}
	h.mu.Unlock()
	out := map[string]any{"ok": true, "accepted": accepted, "delivered": delivered}
	if rem, reset, known := chatc.Quota(); known {
		out["quota"] = map[string]any{"remaining": rem, "resetUnix": reset}
	}
	if rec, err := chatc.FetchPeerKey(ctx, peer); err == nil {
		h.mu.Lock()
		myPub := h.identity.Identity.Public().(ed25519.PublicKey)
		h.mu.Unlock()
		out["emojis"] = chatSafetyEmojis(myPub, rec.IdentityPub)
	}
	writeJSON(w, out)
}
