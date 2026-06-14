package server

import (
	"bytes"
	"crypto/ed25519"
	"path/filepath"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

type chatTestUser struct {
	id     ed25519.PrivateKey
	encPub []byte
	addr   [protocol.AddressSize]byte
	rec    *protocol.RegisterEnvelope
	raw    []byte
}

func newChatTestUser(t *testing.T) chatTestUser {
	t.Helper()
	seed, err := protocol.GenerateSeed()
	if err != nil {
		t.Fatal(err)
	}
	id, err := protocol.DeriveIdentityKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := protocol.DeriveEncryptionKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	encPub := enc.PublicKey().Bytes()
	raw, err := protocol.EncodeRegisterEnvelope(id, encPub, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := protocol.ParseRegisterEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	return chatTestUser{
		id:     id,
		encPub: encPub,
		addr:   rec.Address(),
		rec:    rec,
		raw:    raw,
	}
}

func newTestStore(t *testing.T) *ChatStore {
	t.Helper()
	limits := protocol.DefaultChatLimits()
	limits.InboxCap = 4
	limits.PerPairCap = 2
	limits.SendPerHour = 3
	s, err := OpenChatStore(filepath.Join(t.TempDir(), "chat.db"), limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestChatStoreRegisterAndKeys(t *testing.T) {
	s := newTestStore(t)
	u := newChatTestUser(t)
	now := time.Unix(1750000000, 0)

	if err := s.Register(u.rec, u.raw, now); err != nil {
		t.Fatal(err)
	}
	id, enc, ok, err := s.Keys(u.addr, now)
	if err != nil || !ok {
		t.Fatalf("keys: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(id, u.rec.IdentityPub) || !bytes.Equal(enc, u.encPub) {
		t.Fatal("stored keys mismatch")
	}

	// Unknown account.
	var other [protocol.AddressSize]byte
	if _, _, ok, _ := s.Keys(other, now); ok {
		t.Fatal("unknown account returned keys")
	}
}

func TestChatStoreCommitAndQuota(t *testing.T) {
	s := newTestStore(t)
	a := newChatTestUser(t) // sender
	b := newChatTestUser(t) // recipient
	now := time.Unix(1750003600, 0).Truncate(time.Hour)

	if err := s.Register(a.rec, a.raw, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(b.rec, b.raw, now); err != nil {
		t.Fatal(err)
	}

	st, rem, _, err := s.PrecheckMessage(a.addr, b.addr, now)
	if err != nil || st != protocol.ChatStatusOK {
		t.Fatalf("precheck: %d err=%v", st, err)
	}
	if rem != 3 {
		t.Fatalf("remaining = %d, want 3", rem)
	}

	// Commit seq 1, 2 → per-pair cap (2) reached.
	for seq := uint32(1); seq <= 2; seq++ {
		st, last, _, _, err := s.CommitMessage(a.addr, b.addr, seq, []byte{byte(seq)}, now)
		if err != nil || st != protocol.ChatStatusOK {
			t.Fatalf("commit %d: %d err=%v", seq, st, err)
		}
		if last != seq {
			t.Fatalf("lastAccepted = %d, want %d", last, seq)
		}
	}
	st, _, _, _, _ = s.CommitMessage(a.addr, b.addr, 3, []byte{3}, now)
	if st != protocol.ChatStatusPairQuota {
		t.Fatalf("pair quota not enforced: %d", st)
	}

	// Replay: a strictly-older seq is rejected (seq 1 < lastAccepted 2).
	st, _, _, _, _ = s.CommitMessage(a.addr, b.addr, 1, []byte{1}, now)
	if st != protocol.ChatStatusReplay {
		t.Fatalf("replay not detected: %d", st)
	}

	// Unknown sender / recipient.
	var nobody [protocol.AddressSize]byte
	if st, _, _, _, _ = s.CommitMessage(nobody, b.addr, 1, []byte{1}, now); st != protocol.ChatStatusUnknownSender {
		t.Fatalf("unknown sender: %d", st)
	}
	if st, _, _, _, _ = s.CommitMessage(a.addr, nobody, 9, []byte{9}, now); st != protocol.ChatStatusUnknownRecipient {
		t.Fatalf("unknown recipient: %d", st)
	}
}

func TestChatStoreRateLimitWindow(t *testing.T) {
	s := newTestStore(t)
	a := newChatTestUser(t)
	b := newChatTestUser(t)
	now := time.Unix(1750003600, 0).Truncate(time.Hour)
	s.Register(a.rec, a.raw, now)
	s.Register(b.rec, b.raw, now)

	// SendPerHour=3 but PerPairCap=2 — send 2 to b, then rate-check via quota.
	s.CommitMessage(a.addr, b.addr, 1, []byte{1}, now)
	s.CommitMessage(a.addr, b.addr, 2, []byte{2}, now)
	rem, reset, ok, err := s.SendQuota(a.addr, now)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if rem != 1 {
		t.Fatalf("remaining = %d, want 1", rem)
	}
	wantReset := uint32(now.Unix() + 3600)
	if reset != wantReset {
		t.Fatalf("reset = %d, want %d", reset, wantReset)
	}

	// Third send exhausts the hour.
	c := newChatTestUser(t)
	s.Register(c.rec, c.raw, now)
	if st, _, _, _, _ := s.CommitMessage(a.addr, c.addr, 1, []byte{1}, now); st != protocol.ChatStatusOK {
		t.Fatalf("third send: %d", st)
	}
	if st, _, _, _, _ := s.CommitMessage(a.addr, c.addr, 2, []byte{2}, now); st != protocol.ChatStatusRateLimited {
		t.Fatalf("rate limit not enforced: %d", st)
	}

	// Next hour: counter resets.
	later := now.Add(time.Hour)
	rem, _, _, _ = s.SendQuota(a.addr, later)
	if rem != 3 {
		t.Fatalf("remaining after window = %d, want 3", rem)
	}
}

func TestChatStoreInboxFetchAck(t *testing.T) {
	s := newTestStore(t)
	a := newChatTestUser(t)
	b := newChatTestUser(t)
	now := time.Unix(1750003600, 0)
	s.Register(a.rec, a.raw, now)
	s.Register(b.rec, b.raw, now)

	env := bytes.Repeat([]byte("E"), protocol.MaxBlockPayload+10) // 2 blocks
	if st, _, _, _, _ := s.CommitMessage(a.addr, b.addr, 1, env, now); st != protocol.ChatStatusOK {
		t.Fatal("commit failed")
	}

	entries, err := s.InboxStatus(b.addr, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox = %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Src != a.addr || e.Seq != 1 || int(e.Len) != len(env) || e.Blocks != 2 {
		t.Fatalf("entry mismatch: %+v", e)
	}

	// Fetch both blocks, reassemble.
	var got []byte
	for blk := uint8(0); blk < e.Blocks; blk++ {
		part, ok, err := s.FetchBlock(b.addr, a.addr, 1, blk, now)
		if err != nil || !ok {
			t.Fatalf("fetch block %d: ok=%v err=%v", blk, ok, err)
		}
		got = append(got, part...)
	}
	if !bytes.Equal(got, env) {
		t.Fatal("fetched envelope mismatch")
	}
	if _, ok, _ := s.FetchBlock(b.addr, a.addr, 1, 5, now); ok {
		t.Fatal("out-of-range block served")
	}

	// ACK frees it and bumps delivered.
	if st, _ := s.Ack(b.addr, a.addr, 1, nil, now); st != protocol.ChatStatusOK {
		t.Fatal("ack failed")
	}
	entries, _ = s.InboxStatus(b.addr, now)
	if len(entries) != 0 {
		t.Fatal("inbox not freed after ack")
	}
	acc, del, _, ok, _ := s.PairState(b.addr, a.addr, now)
	if !ok || acc != 1 || del != 1 {
		t.Fatalf("pair state = (%d,%d), want (1,1)", acc, del)
	}

	// ACK is idempotent (the seal authenticates; replaying is harmless).
	if st, _ := s.Ack(b.addr, a.addr, 1, nil, now); st != protocol.ChatStatusOK {
		t.Fatal("repeated ack should be idempotent OK")
	}

	// Freed slot allows a new message at a higher seq.
	if st, _, _, _, _ := s.CommitMessage(a.addr, b.addr, 5, []byte{5}, now); st != protocol.ChatStatusOK {
		t.Fatal("commit after ack failed")
	}
}

func TestChatStoreOpenSessionMonotonic(t *testing.T) {
	s := newTestStore(t)
	u := newChatTestUser(t)
	now := time.Unix(1750003600, 0)
	s.Register(u.rec, u.raw, now)

	id, _, st, err := s.OpenSession(u.addr, 100, now, true)
	if err != nil || st != protocol.ChatStatusOK {
		t.Fatalf("open: %d err=%v", st, err)
	}
	if !bytes.Equal(id, u.rec.IdentityPub) {
		t.Fatal("identity mismatch")
	}
	// Same or older ts rejected.
	if _, _, st, _ := s.OpenSession(u.addr, 100, now, true); st != protocol.ChatStatusBadAuth {
		t.Fatalf("replayed open accepted: %d", st)
	}
	if _, _, st, _ := s.OpenSession(u.addr, 101, now, true); st != protocol.ChatStatusOK {
		t.Fatal("newer open rejected")
	}
}

func TestChatStorePersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.db")
	limits := protocol.DefaultChatLimits()
	now := time.Unix(1750003600, 0)

	a := newChatTestUser(t)
	b := newChatTestUser(t)

	s1, err := OpenChatStore(path, limits)
	if err != nil {
		t.Fatal(err)
	}
	s1.Register(a.rec, a.raw, now)
	s1.Register(b.rec, b.raw, now)
	if st, _, _, _, _ := s1.CommitMessage(a.addr, b.addr, 7, []byte("persisted"), now); st != protocol.ChatStatusOK {
		t.Fatal("commit failed")
	}
	s1.Close()

	s2, err := OpenChatStore(path, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	entries, err := s2.InboxStatus(b.addr, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Seq != 7 {
		t.Fatalf("inbox after reopen: %+v", entries)
	}
	blk, ok, _ := s2.FetchBlock(b.addr, a.addr, 7, 0, now)
	if !ok || string(blk) != "persisted" {
		t.Fatal("envelope lost across reopen")
	}
}

// TestChatStorePeriodicModePersistence: in periodic mode mutations live only in
// RAM until a flush; Close must flush dirty accounts so a clean shutdown loses
// nothing across reopen.
func TestChatStorePeriodicModePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.db")
	limits := protocol.DefaultChatLimits()
	now := time.Unix(1750003600, 0)

	a := newChatTestUser(t)
	b := newChatTestUser(t)

	s1, err := OpenChatStore(path, limits)
	if err != nil {
		t.Fatal(err)
	}
	// A long interval so nothing auto-flushes; only Close persists.
	s1.EnablePeriodicSync(time.Hour)
	s1.Register(a.rec, a.raw, now)
	s1.Register(b.rec, b.raw, now)
	if st, _, _, _, _ := s1.CommitMessage(a.addr, b.addr, 3, []byte("ram only"), now); st != protocol.ChatStatusOK {
		t.Fatal("commit failed")
	}
	if st, _ := s1.Ack(b.addr, a.addr, 0, nil, now); st != protocol.ChatStatusOK {
		t.Fatal("ack failed")
	}
	s1.Close()

	s2, err := OpenChatStore(path, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, _, ok, _ := s2.Keys(a.addr, now); !ok {
		t.Fatal("account a lost across reopen")
	}
	entries, err := s2.InboxStatus(b.addr, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Seq != 3 {
		t.Fatalf("inbox after reopen: %+v", entries)
	}
	acc, del, _, _, _ := s2.PairState(b.addr, a.addr, now)
	if acc != 3 || del != 0 {
		t.Fatalf("pair state after reopen: acc=%d del=%d", acc, del)
	}
}

func TestChatStoreSweepTTL(t *testing.T) {
	s := newTestStore(t)
	a := newChatTestUser(t)
	b := newChatTestUser(t)
	now := time.Unix(1750003600, 0)
	s.Register(a.rec, a.raw, now)
	s.Register(b.rec, b.raw, now)
	s.CommitMessage(a.addr, b.addr, 1, []byte{1}, now)

	// Not yet expired.
	expired, deleted, err := s.Sweep(now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if expired != 0 || deleted != 0 {
		t.Fatalf("premature sweep: %d %d", expired, deleted)
	}

	// Past TTL (72h default).
	expired, _, err = s.Sweep(now.Add(73 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if expired != 1 {
		t.Fatalf("expired = %d, want 1", expired)
	}
	entries, _ := s.InboxStatus(b.addr, now.Add(73*time.Hour))
	if len(entries) != 0 {
		t.Fatal("expired message still served")
	}

	// Account GC is off by default — even far in the future, accounts stay.
	_, deleted, err = s.Sweep(now.Add(400 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("accounts deleted with GC off: %d", deleted)
	}
}

func TestChatStoreAccountTTL(t *testing.T) {
	s := newTestStore(t)
	s.SetAccountTTL(50 * 24 * time.Hour)
	a := newChatTestUser(t)
	b := newChatTestUser(t)
	now := time.Unix(1750003600, 0)
	s.Register(a.rec, a.raw, now)
	s.Register(b.rec, b.raw, now)
	if s.AccountCount() != 2 {
		t.Fatalf("account count = %d, want 2", s.AccountCount())
	}

	// Before the account TTL: kept.
	if _, deleted, _ := s.Sweep(now.Add(49 * 24 * time.Hour)); deleted != 0 {
		t.Fatalf("premature account GC: %d", deleted)
	}
	// After: both idle accounts removed.
	_, deleted, err := s.Sweep(now.Add(51 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	if s.AccountCount() != 0 {
		t.Fatalf("account count after GC = %d, want 0", s.AccountCount())
	}
}

func TestChatStoreMaxAccounts(t *testing.T) {
	s := newTestStore(t)
	s.SetMaxAccounts(1)
	a := newChatTestUser(t)
	b := newChatTestUser(t)
	now := time.Unix(1750003600, 0)
	if err := s.Register(a.rec, a.raw, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(b.rec, b.raw, now); err != ErrChatAccountsFull {
		t.Fatalf("second register err = %v, want ErrChatAccountsFull", err)
	}
	// Re-registering an existing account is always allowed (no new slot).
	if err := s.Register(a.rec, a.raw, now.Add(time.Minute)); err != nil {
		t.Fatalf("re-register existing: %v", err)
	}
}

func TestChatStoreCommitIdempotent(t *testing.T) {
	s := newTestStore(t)
	a := newChatTestUser(t)
	b := newChatTestUser(t)
	now := time.Unix(1750003600, 0)
	s.Register(a.rec, a.raw, now)
	s.Register(b.rec, b.raw, now)

	if st, _, _, _, _ := s.CommitMessage(a.addr, b.addr, 1, []byte("hello"), now); st != protocol.ChatStatusOK {
		t.Fatalf("first commit: %d", st)
	}
	// Re-commit of the same seq (lost FIN-OK retransmit) → OK, not Replay, and
	// no duplicate stored.
	st, last, _, _, _ := s.CommitMessage(a.addr, b.addr, 1, []byte("hello"), now)
	if st != protocol.ChatStatusOK || last != 1 {
		t.Fatalf("idempotent recommit: st=%d last=%d", st, last)
	}
	entries, _ := s.InboxStatus(b.addr, now)
	if len(entries) != 1 {
		t.Fatalf("inbox = %d, want 1 (no duplicate)", len(entries))
	}
	// A strictly older seq is still a replay.
	if st, _, _, _, _ := s.CommitMessage(a.addr, b.addr, 0, []byte("x"), now); st != protocol.ChatStatusReplay {
		t.Fatalf("older seq: %d, want Replay", st)
	}
}

// TestChatStoreFetchDisambiguatesSenders guards the receive-path bug: two
// senders can both have a pending seq=1 (seq is per-pair), so FetchBlock must
// key on (src, seq), not seq alone, or it returns the wrong sender's envelope.
func TestChatStoreFetchDisambiguatesSenders(t *testing.T) {
	s := newTestStore(t)
	a := newChatTestUser(t)
	c := newChatTestUser(t)
	b := newChatTestUser(t)
	now := time.Unix(1750003600, 0)
	s.Register(a.rec, a.raw, now)
	s.Register(c.rec, c.raw, now)
	s.Register(b.rec, b.raw, now)

	envA := []byte("from-A-at-seq-1")
	envC := []byte("from-C-at-seq-1")
	if st, _, _, _, _ := s.CommitMessage(a.addr, b.addr, 1, envA, now); st != protocol.ChatStatusOK {
		t.Fatal("commit A")
	}
	if st, _, _, _, _ := s.CommitMessage(c.addr, b.addr, 1, envC, now); st != protocol.ChatStatusOK {
		t.Fatal("commit C")
	}

	gotA, ok, _ := s.FetchBlock(b.addr, a.addr, 1, 0, now)
	if !ok || string(gotA) != string(envA) {
		t.Fatalf("fetch A seq1 = %q (ok=%v), want %q", gotA, ok, envA)
	}
	gotC, ok, _ := s.FetchBlock(b.addr, c.addr, 1, 0, now)
	if !ok || string(gotC) != string(envC) {
		t.Fatalf("fetch C seq1 = %q (ok=%v), want %q", gotC, ok, envC)
	}
}
