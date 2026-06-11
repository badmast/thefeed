package client

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	cryptoRand "crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/sartoopjj/thefeed/internal/protocol"
	"github.com/sartoopjj/thefeed/internal/server"
)

var chatTestDomains = []string{"c1.example.com", "c2.other.net"}

// chatTestServer hosts a real ChatService plus the signed ChatInfo blocks,
// reachable through a fetcher's mocked DNS exchange.
type chatTestServer struct {
	svc        *server.ChatService
	store      *server.ChatStore
	ek         *ecdh.PrivateKey
	qk         [protocol.KeySize]byte
	limits     protocol.ChatLimits
	signKey    ed25519.PrivateKey
	signPub    ed25519.PublicKey
	infoBlocks [][]byte
	infoExtra  []byte
	mu         sync.Mutex
}

// reboot simulates a server restart: RAM sessions are lost but the store (keys,
// inboxes) persists. A fresh service over the same store + ek means cached
// client sessions become UnknownSession, exercising re-handshake recovery.
func (ts *chatTestServer) reboot() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.svc = server.NewChatService(ts.store, ts.ek, ts.qk, ts.limits, chatTestDomains)
}

func newChatTestServer(t *testing.T, limits protocol.ChatLimits) *chatTestServer {
	t.Helper()
	store, err := server.OpenChatStore(filepath.Join(t.TempDir(), "chat.db"), limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	signPub, signKey, err := ed25519.GenerateKey(cryptoRand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ek, err := ecdh.X25519().GenerateKey(cryptoRand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// The service must seal/open under the same query key the test fetchers
	// derive from "test-passphrase".
	qk, _, err := protocol.DeriveKeys("test-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	svc := server.NewChatService(store, ek, qk, limits, chatTestDomains)

	payload := protocol.EncodeChatInfo(svc.Info())
	blocks := protocol.SplitIntoBlocks(payload)
	prefix := []byte{byte(len(blocks) >> 8), byte(len(blocks))}
	blocks[0] = append(prefix, blocks[0]...)
	var concat []byte
	for _, b := range blocks {
		concat = append(concat, b...)
	}
	extra, err := protocol.EncodeExtraBlock(signKey, protocol.ChatInfoChannel, protocol.ContentDigest(concat), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	return &chatTestServer{
		svc: svc, store: store, ek: ek, qk: qk, limits: limits,
		signKey: signKey, signPub: signPub,
		infoBlocks: blocks, infoExtra: extra,
	}
}

// attach wires a fetcher's DNS exchange to this in-process chat server.
func (ts *chatTestServer) attach(t *testing.T, f *Fetcher) {
	t.Helper()
	if err := f.SetServerPublicKey(base64.RawURLEncoding.EncodeToString(ts.signPub)); err != nil {
		t.Fatal(err)
	}
	f.exchangeFn = func(ctx context.Context, m *dns.Msg, addr string) (*dns.Msg, time.Duration, error) {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		qname := strings.TrimSuffix(m.Question[0].Name, ".")
		for _, cd := range chatTestDomains {
			if strings.HasSuffix(strings.ToLower(qname), "."+cd) {
				selector, counter, payload, err := protocol.DecodeChatCell(f.queryKey, qname, cd)
				if err != nil {
					return nil, 0, err
				}
				ts.mu.Lock()
				resp := ts.svc.HandleCell(selector, counter, payload, cd, time.Now())
				ts.mu.Unlock()
				return txtReply(f, m, qname, resp)
			}
		}
		ch, blk, err := protocol.DecodeQuery(f.queryKey, qname, f.domain)
		if err != nil {
			return nil, 0, err
		}
		if ch != protocol.ChatInfoChannel {
			return nil, 0, fmt.Errorf("unexpected channel %d", ch)
		}
		switch {
		case int(blk) < len(ts.infoBlocks):
			return txtReply(f, m, qname, ts.infoBlocks[blk])
		case int(blk) == len(ts.infoBlocks) && ts.infoExtra != nil:
			return txtReply(f, m, qname, ts.infoExtra)
		default:
			return nil, 0, fmt.Errorf("no chat-info block %d", blk)
		}
	}
}

func txtReply(f *Fetcher, m *dns.Msg, qname string, payload []byte) (*dns.Msg, time.Duration, error) {
	encoded, err := protocol.EncodeResponse(f.responseKey, payload, 0)
	if err != nil {
		return nil, 0, err
	}
	resp := new(dns.Msg)
	resp.SetReply(m)
	resp.Rcode = dns.RcodeSuccess
	resp.Answer = []dns.RR{&dns.TXT{
		Hdr: dns.RR_Header{Name: m.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 0},
		Txt: splitTXTString(encoded),
	}}
	return resp, time.Millisecond, nil
}

func splitTXTString(s string) []string {
	var parts []string
	for len(s) > 255 {
		parts = append(parts, s[:255])
		s = s[255:]
	}
	if len(s) > 0 {
		parts = append(parts, s)
	}
	return parts
}

func newChatTestClient(t *testing.T, ts *chatTestServer) *ChatClient {
	t.Helper()
	f := newTestFetcher(t, []string{"9.9.9.9:53"})
	f.scatter = 1
	f.SetTimeout(2 * time.Second)
	ts.attach(t, f)
	seed, err := protocol.GenerateSeed()
	if err != nil {
		t.Fatal(err)
	}
	id, err := NewChatIdentity(seed)
	if err != nil {
		t.Fatal(err)
	}
	return NewChatClient(f, id)
}

func TestChatAddressString(t *testing.T) {
	var addr [protocol.AddressSize]byte
	copy(addr[:], []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})
	s := ChatAddressString(addr)
	got, err := ParseChatAddress(s)
	if err != nil {
		t.Fatal(err)
	}
	if got != addr {
		t.Fatal("address round trip mismatch")
	}
	if _, err := ParseChatAddress("not-base32!!"); err == nil {
		t.Fatal("invalid address accepted")
	}
}

func TestChatClientEndToEnd(t *testing.T) {
	limits := protocol.DefaultChatLimits()
	ts := newChatTestServer(t, limits)
	a := newChatTestClient(t, ts)
	b := newChatTestClient(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Capability discovery, fail-closed verified.
	info, err := a.EnsureInfo(ctx)
	if err != nil {
		t.Fatalf("ensure info: %v", err)
	}
	if len(info.Domains) != 2 || info.Limits.ChunkSize != limits.ChunkSize {
		t.Fatalf("info mismatch: %+v", info)
	}

	// B registers (recipient must exist).
	if err := b.Register(ctx, nil); err != nil {
		t.Fatalf("register B: %v", err)
	}

	// A sends — auto-registers on unknown_sender, with progress reported.
	var progressLog []int
	var progressTotal int
	const text = "پیام اول — hello from A"
	res, err := a.SendMessage(ctx, b.Identity().Addr, 1, text, func(done, total int) {
		progressLog = append(progressLog, done)
		progressTotal = total
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.Seq != 1 {
		t.Fatalf("seq = %d", res.Seq)
	}
	if res.Remaining != limits.SendPerHour-1 {
		t.Fatalf("remaining = %d, want %d", res.Remaining, limits.SendPerHour-1)
	}
	if res.ResetUnix == 0 {
		t.Fatal("reset unix missing")
	}
	if progressTotal == 0 || len(progressLog) == 0 || progressLog[len(progressLog)-1] != progressTotal {
		t.Fatalf("progress did not complete: %v / %d", progressLog, progressTotal)
	}
	for i := 1; i < len(progressLog); i++ {
		if progressLog[i] < progressLog[i-1] {
			t.Fatal("progress went backwards")
		}
	}
	if rem, reset, known := a.Quota(); !known || rem != limits.SendPerHour-1 || reset == 0 {
		t.Fatalf("quota = %d/%d known=%v", rem, reset, known)
	}

	// B polls, decrypts, sees the message with download progress.
	var dlLog []int
	msgs, err := b.FetchInbox(ctx, func(done, total int) { dlLog = append(dlLog, done) })
	if err != nil {
		t.Fatalf("fetch inbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("inbox = %d messages", len(msgs))
	}
	if msgs[0].From != a.Identity().Addr || msgs[0].Seq != 1 || msgs[0].Text != text {
		t.Fatalf("message mismatch: %+v", msgs[0])
	}
	if len(dlLog) == 0 {
		t.Fatal("no download progress reported")
	}

	// ✓ before ack, ✓✓ after.
	acc, del, err := a.PeerStatus(ctx, b.Identity().Addr)
	if err != nil {
		t.Fatal(err)
	}
	if acc != 1 || del != 0 {
		t.Fatalf("pre-ack status = (%d,%d)", acc, del)
	}
	if err := b.Ack(ctx, a.Identity().Addr, 1); err != nil {
		t.Fatalf("ack: %v", err)
	}
	acc, del, err = a.PeerStatus(ctx, b.Identity().Addr)
	if err != nil {
		t.Fatal(err)
	}
	if acc != 1 || del != 1 {
		t.Fatalf("post-ack status = (%d,%d)", acc, del)
	}

	// Inbox now empty; NextSeq recovers from the server.
	msgs, err = b.FetchInbox(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatal("inbox not freed after ack")
	}
	next, err := a.NextSeq(ctx, b.Identity().Addr)
	if err != nil {
		t.Fatal(err)
	}
	if next != 2 {
		t.Fatalf("next seq = %d, want 2", next)
	}

	// Re-sending the already-committed seq is idempotent at the server (lost
	// FIN-OK retransmit) — succeeds without error and stores no duplicate.
	if _, err = a.SendMessage(ctx, b.Identity().Addr, 1, "سلام B — first message", nil); err != nil {
		t.Fatalf("idempotent resend err = %v", err)
	}
	if msgs, err := b.FetchInbox(ctx, nil); err != nil || len(msgs) != 0 {
		t.Fatalf("idempotent resend re-delivered: %d msgs, err=%v", len(msgs), err)
	}

	// Reply direction works too (B → A).
	if _, err := b.SendMessage(ctx, a.Identity().Addr, 1, "جواب", nil); err != nil {
		t.Fatalf("reply: %v", err)
	}
	msgs, err = a.FetchInbox(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "جواب" {
		t.Fatalf("reply inbox: %+v", msgs)
	}
}

func TestChatClientSessionRecovery(t *testing.T) {
	limits := protocol.DefaultChatLimits()
	ts := newChatTestServer(t, limits)
	a := newChatTestClient(t, ts)
	b := newChatTestClient(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := b.Register(ctx, nil); err != nil {
		t.Fatalf("register B: %v", err)
	}
	if _, err := a.SendMessage(ctx, b.Identity().Addr, 1, "before reboot", nil); err != nil {
		t.Fatalf("send 1: %v", err)
	}

	// Server reboots: A's and B's cached sessions are now unknown.
	ts.reboot()

	// A's next send hits UnknownSession on the first op, re-handshakes (auth,
	// account persisted), and succeeds — no error surfaces to the caller.
	if _, err := a.SendMessage(ctx, b.Identity().Addr, 2, "after reboot", nil); err != nil {
		t.Fatalf("send after reboot: %v", err)
	}
	msgs, err := b.FetchInbox(ctx, nil)
	if err != nil {
		t.Fatalf("fetch after reboot: %v", err)
	}
	// Both messages are present (seq 1 was never acked); the key check is that
	// the post-reboot send went through after a transparent re-handshake.
	found := false
	for _, m := range msgs {
		if m.Seq == 2 && m.Text == "after reboot" {
			found = true
		}
	}
	if !found {
		t.Fatalf("post-reboot message not delivered: %+v", msgs)
	}
}

// TestChatClientLossyTransport drops the first transmission of every in-context
// cell (simulating UDP loss) and expects the cell retransmit layer to deliver
// every op anyway — one lost datagram must not fail a whole SendMessage.
func TestChatClientLossyTransport(t *testing.T) {
	ts := newChatTestServer(t, protocol.DefaultChatLimits())
	a := newChatTestClient(t, ts)
	b := newChatTestClient(t, ts)

	var lossMu sync.Mutex
	seen := map[string]bool{}
	inner := a.f.exchangeFn
	a.f.exchangeFn = func(ctx context.Context, m *dns.Msg, addr string) (*dns.Msg, time.Duration, error) {
		qname := strings.TrimSuffix(m.Question[0].Name, ".")
		for _, cd := range chatTestDomains {
			if !strings.HasSuffix(strings.ToLower(qname), "."+cd) {
				continue
			}
			sel, _, _, err := protocol.DecodeChatCell(a.f.queryKey, qname, cd)
			// Handshake cells have their own round-level retry; the retransmit
			// layer under test covers in-context cells, whose qname is identical
			// across attempts.
			if err == nil && !protocol.ChatIsHandshakeSelector(sel) {
				lossMu.Lock()
				first := !seen[qname]
				seen[qname] = true
				lossMu.Unlock()
				if first {
					return nil, 0, errors.New("synthetic packet loss")
				}
			}
		}
		return inner(ctx, m, addr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := b.Register(ctx, nil); err != nil {
		t.Fatalf("register B: %v", err)
	}
	if _, err := a.SendMessage(ctx, b.Identity().Addr, 1, "through the noise", nil); err != nil {
		t.Fatalf("send over lossy transport: %v", err)
	}
	msgs, err := b.FetchInbox(ctx, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != "through the noise" {
		t.Fatalf("message not delivered intact: %+v", msgs)
	}
	lossMu.Lock()
	dropped := len(seen)
	lossMu.Unlock()
	if dropped == 0 {
		t.Fatal("transport never dropped a cell — test exercised nothing")
	}
}

// TestChatClientCounterCapRehandshakes drives the per-session op counter up to
// the cap and verifies the next op transparently re-handshakes (resetting the
// counter) instead of sealing into the reserved counter region — which would
// reuse an AES-CTR nonce.
func TestChatClientCounterCapRehandshakes(t *testing.T) {
	ts := newChatTestServer(t, protocol.DefaultChatLimits())
	a := newChatTestClient(t, ts)
	b := newChatTestClient(t, ts)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := b.Register(ctx, nil); err != nil {
		t.Fatalf("register B: %v", err)
	}
	if _, err := a.SendMessage(ctx, b.Identity().Addr, 1, "first", nil); err != nil {
		t.Fatalf("send 1: %v", err)
	}

	// Pin the live session's counter at the cap; the prior selector stays valid
	// server-side, so only the proactive cap (not a SessionLost) can trigger the
	// re-handshake we want to observe.
	a.mu.Lock()
	if !a.sessUp {
		a.mu.Unlock()
		t.Fatal("expected a live session after first send")
	}
	a.sendCounter = chatMaxSessionCounter
	prevRef := a.sessRef
	a.mu.Unlock()

	if _, err := a.SendMessage(ctx, b.Identity().Addr, 2, "after cap", nil); err != nil {
		t.Fatalf("send after counter cap: %v", err)
	}

	a.mu.Lock()
	newCounter := a.sendCounter
	newRef := a.sessRef
	a.mu.Unlock()
	if newCounter >= chatMaxSessionCounter {
		t.Fatalf("counter not reset after cap: %#x", newCounter)
	}
	if newRef == prevRef {
		t.Fatal("session selector unchanged — no re-handshake happened")
	}

	msgs, err := b.FetchInbox(ctx, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var got2 bool
	for _, m := range msgs {
		if m.Seq == 2 && m.Text == "after cap" {
			got2 = true
		}
	}
	if !got2 {
		t.Fatalf("post-cap message not delivered: %+v", msgs)
	}
}

func TestChatClientFailClosed(t *testing.T) {
	limits := protocol.DefaultChatLimits()
	ts := newChatTestServer(t, limits)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// No pinned server key → chat refuses to start.
	f := newTestFetcher(t, []string{"9.9.9.9:53"})
	f.scatter = 1
	ts.attach(t, f)
	f.serverPubKey = nil
	seed, _ := protocol.GenerateSeed()
	id, _ := NewChatIdentity(seed)
	c := NewChatClient(f, id)
	if _, err := c.EnsureInfo(ctx); !errors.Is(err, ErrChatNoServerKey) {
		t.Fatalf("err = %v, want ErrChatNoServerKey", err)
	}

	// Unsigned ChatInfo (no ExtraBlock) → unverified, fail-closed.
	ts2 := newChatTestServer(t, limits)
	ts2.infoExtra = nil
	c2 := newChatTestClient(t, ts2)
	c2.f.SetTimeout(300 * time.Millisecond)
	if _, err := c2.EnsureInfo(ctx); !errors.Is(err, ErrChatUnverified) {
		t.Fatalf("err = %v, want ErrChatUnverified", err)
	}

	// Tampered ChatInfo signature → unverified.
	ts3 := newChatTestServer(t, limits)
	ts3.infoExtra[len(ts3.infoExtra)-1] ^= 0x01
	ts3.infoBlocks[0][len(ts3.infoBlocks[0])-1] ^= 0x01
	c3 := newChatTestClient(t, ts3)
	if _, err := c3.EnsureInfo(ctx); !errors.Is(err, ErrChatUnverified) {
		t.Fatalf("err = %v, want ErrChatUnverified", err)
	}
}

func TestChatClientUnknownRecipient(t *testing.T) {
	limits := protocol.DefaultChatLimits()
	ts := newChatTestServer(t, limits)
	a := newChatTestClient(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var ghost [protocol.AddressSize]byte
	ghost[3] = 0x77
	_, err := a.SendMessage(ctx, ghost, 1, "anyone there?", nil)
	var serr *ChatStatusError
	if !errors.As(err, &serr) || serr.Status != protocol.ChatStatusNotFound {
		t.Fatalf("err = %v, want keyfetch not_found", err)
	}
}
