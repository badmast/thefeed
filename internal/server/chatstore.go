package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// ChatStore persists chat accounts: a hot RAM cache in front of a bbolt file
// (pure Go, single mmap'd file). Every mutation is written through to disk, so
// a crash never loses committed state; the RAM cache is only a read cache with
// idle eviction. Upload sessions are NOT here — they are ephemeral and live in
// ChatService.
//
// The RAM cache + its lock are SHARDED by address so commits to different
// accounts proceed in parallel, and writes go through bbolt's Batch (which
// coalesces the concurrent write transactions into far fewer fsyncs). Per
// account, operations still serialize on that account's shard, so ordering and
// correctness are preserved.
type ChatStore struct {
	db     *bolt.DB
	limits protocol.ChatLimits

	shards [chatStoreShards]chatShard

	accountTTL   time.Duration
	maxAccounts  int
	accountCount atomic.Int64

	// syncEvery > 0 enables periodic group durability: per-commit fsync is off
	// and RunSync flushes the page cache to disk on this interval instead.
	syncEvery time.Duration
}

type chatShard struct {
	mu  sync.Mutex
	hot map[[protocol.AddressSize]byte]*chatHotEntry
}

type chatHotEntry struct {
	acc      *chatAccount
	lastUsed time.Time
}

const (
	chatAccountVersion = 1
	// chatIdleEvict drops an account from the RAM cache after this idle time.
	chatIdleEvict = 30 * time.Minute
	// chatStoreShards is the number of RAM-cache shards (power of two). More
	// shards = less same-account-lock contention under concurrent load.
	chatStoreShards = 64
)

// ErrChatAccountsFull is returned by Register when the store is at its
// configured maxAccounts cap.
var ErrChatAccountsFull = fmt.Errorf("chat: account store full")

var chatAccountsBucket = []byte("chat_accounts")

// chatPairState tracks per-(this account ← peer) delivery counters. It lives
// in the RECIPIENT's account, keyed by the sender address.
type chatPairState struct {
	LastAccepted  uint32 `json:"acc"`
	LastDelivered uint32 `json:"del"`
}

// chatInboxMsg is one stored message envelope awaiting fetch.
type chatInboxMsg struct {
	Src      []byte `json:"src"`
	Seq      uint32 `json:"seq"`
	Envelope []byte `json:"env"`
	Received int64  `json:"ts"`
}

// chatAccount is the per-address durable record (JSON blob in bbolt).
type chatAccount struct {
	Version        int                       `json:"v"`
	IdentityPub    []byte                    `json:"idpub"`
	EncPub         []byte                    `json:"encpub"`
	RegRecord      []byte                    `json:"regrec"` // raw signed record, served by KEYFETCH
	RegisteredAt   int64                     `json:"reg"`
	LastActive     int64                     `json:"act"`
	LastOpenTS     uint32                    `json:"opents"`
	LastAckCounter uint32                    `json:"ackctr"`
	SendCount      uint16                    `json:"sendn"`
	SendWindow     int64                     `json:"sendw"`
	Inbox          []chatInboxMsg            `json:"inbox"`
	Pairs          map[string]*chatPairState `json:"pairs"`
}

func pairKeyOf(src [protocol.AddressSize]byte) string { return hex.EncodeToString(src[:]) }

// OpenChatStore opens (or creates) the chat account store at path.
func OpenChatStore(path string, limits protocol.ChatLimits) (*ChatStore, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("chatstore open: %w", err)
	}
	// A small batch delay keeps idle latency low while still coalescing under
	// load (many concurrent commits share one fsync).
	db.MaxBatchDelay = 5 * time.Millisecond
	count := 0
	err = db.Update(func(tx *bolt.Tx) error {
		b, e := tx.CreateBucketIfNotExists(chatAccountsBucket)
		if e != nil {
			return e
		}
		count = b.Stats().KeyN
		return nil
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("chatstore bucket: %w", err)
	}
	s := &ChatStore{db: db, limits: limits}
	for i := range s.shards {
		s.shards[i].hot = make(map[[protocol.AddressSize]byte]*chatHotEntry)
	}
	s.accountCount.Store(int64(count))
	return s, nil
}

// SetAccountTTL sets how long an idle account is kept before deletion.
// 0 (the default) never deletes accounts. Call once after OpenChatStore.
func (s *ChatStore) SetAccountTTL(d time.Duration) { s.accountTTL = d }

// SetMaxAccounts caps total stored accounts (0 = unlimited). Call once after
// OpenChatStore.
func (s *ChatStore) SetMaxAccounts(n int) { s.maxAccounts = n }

// EnablePeriodicSync trades per-commit fsync for a periodic flush every
// interval: commits land in the OS page cache immediately (high throughput,
// CPU/RAM-bound rather than fsync-bound) and RunSync forces them to disk on the
// interval. A crash can lose up to ~interval of just-received messages. Call
// once after OpenChatStore, before serving, then start RunSync.
func (s *ChatStore) EnablePeriodicSync(interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.syncEvery = interval
	s.db.NoSync = true
}

// RunSync flushes the store to disk every syncEvery until ctx is done, with a
// final flush on shutdown. No-op unless EnablePeriodicSync was called.
func (s *ChatStore) RunSync(ctx context.Context) {
	if s.syncEvery <= 0 {
		return
	}
	t := time.NewTicker(s.syncEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := s.db.Sync(); err != nil {
				log.Printf("[chat] final store sync: %v", err)
			}
			return
		case <-t.C:
			if err := s.db.Sync(); err != nil {
				log.Printf("[chat] store sync: %v", err)
			}
		}
	}
}

// AccountCount returns the number of stored accounts.
func (s *ChatStore) AccountCount() int { return int(s.accountCount.Load()) }

// Close flushes nothing (write-through) and closes the file.
func (s *ChatStore) Close() error { return s.db.Close() }

// Limits returns the store's configured limits.
func (s *ChatStore) Limits() protocol.ChatLimits { return s.limits }

func shardIdx(addr [protocol.AddressSize]byte) int { return int(addr[0] & (chatStoreShards - 1)) }

func (s *ChatStore) shardFor(addr [protocol.AddressSize]byte) *chatShard {
	return &s.shards[shardIdx(addr)]
}

// lock2 locks the shards for two addresses in a fixed order (deadlock-free) and
// returns an unlock func. If both map to one shard it is locked once.
func (s *ChatStore) lock2(a, b [protocol.AddressSize]byte) func() {
	ia, ib := shardIdx(a), shardIdx(b)
	if ia == ib {
		s.shards[ia].mu.Lock()
		return func() { s.shards[ia].mu.Unlock() }
	}
	if ia > ib {
		ia, ib = ib, ia
	}
	s.shards[ia].mu.Lock()
	s.shards[ib].mu.Lock()
	return func() { s.shards[ib].mu.Unlock(); s.shards[ia].mu.Unlock() }
}

// loadLocked returns the account for addr, from RAM or disk. Caller holds the
// addr's shard lock.
func (s *ChatStore) loadLocked(addr [protocol.AddressSize]byte, now time.Time) (*chatAccount, error) {
	sh := s.shardFor(addr)
	if e, ok := sh.hot[addr]; ok {
		e.lastUsed = now
		return e.acc, nil
	}
	var acc *chatAccount
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(chatAccountsBucket).Get(addr[:])
		if raw == nil {
			return nil
		}
		acc = &chatAccount{}
		return json.Unmarshal(raw, acc)
	})
	if err != nil {
		return nil, err
	}
	if acc == nil {
		return nil, nil
	}
	sh.hot[addr] = &chatHotEntry{acc: acc, lastUsed: now}
	return acc, nil
}

// putLocked writes accounts through to disk via a coalescing batch and refreshes
// the RAM cache. Caller holds the shard lock(s) for every addr in accs.
func (s *ChatStore) putLocked(now time.Time, accs map[[protocol.AddressSize]byte]*chatAccount) error {
	err := s.db.Batch(func(tx *bolt.Tx) error {
		b := tx.Bucket(chatAccountsBucket)
		for addr, acc := range accs {
			raw, err := json.Marshal(acc)
			if err != nil {
				return err
			}
			if err := b.Put(addr[:], raw); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for addr, acc := range accs {
		s.shardFor(addr).hot[addr] = &chatHotEntry{acc: acc, lastUsed: now}
	}
	return nil
}

// Register creates or refreshes the account for a verified registration record.
func (s *ChatStore) Register(rec *protocol.RegisterEnvelope, raw []byte, now time.Time) error {
	addr := rec.Address()
	sh := s.shardFor(addr)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	acc, err := s.loadLocked(addr, now)
	if err != nil {
		return err
	}
	isNew := acc == nil
	if isNew {
		if s.maxAccounts > 0 && int(s.accountCount.Load()) >= s.maxAccounts {
			return ErrChatAccountsFull
		}
		acc = &chatAccount{Version: chatAccountVersion, Pairs: make(map[string]*chatPairState)}
	}
	acc.IdentityPub = append([]byte(nil), rec.IdentityPub...)
	acc.EncPub = append([]byte(nil), rec.EncPub...)
	acc.RegRecord = append([]byte(nil), raw...)
	acc.RegisteredAt = now.Unix()
	acc.LastActive = now.Unix()
	if err := s.putLocked(now, map[[protocol.AddressSize]byte]*chatAccount{addr: acc}); err != nil {
		return err
	}
	if isNew {
		s.accountCount.Add(1)
	}
	return nil
}

// RegisterRecord returns the raw signed registration record for addr.
func (s *ChatStore) RegisterRecord(addr [protocol.AddressSize]byte, now time.Time) ([]byte, bool, error) {
	sh := s.shardFor(addr)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	acc, err := s.loadLocked(addr, now)
	if err != nil || acc == nil || len(acc.RegRecord) == 0 {
		return nil, false, err
	}
	return acc.RegRecord, true, nil
}

// Keys returns the registered identity and encryption keys for addr.
func (s *ChatStore) Keys(addr [protocol.AddressSize]byte, now time.Time) (identityPub, encPub []byte, ok bool, err error) {
	sh := s.shardFor(addr)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	acc, err := s.loadLocked(addr, now)
	if err != nil || acc == nil {
		return nil, nil, false, err
	}
	return acc.IdentityPub, acc.EncPub, true, nil
}

// sendWindowLocked rolls the hourly send window and returns the reset time.
func sendWindowLocked(acc *chatAccount, now time.Time) (resetUnix uint32) {
	window := now.Truncate(time.Hour).Unix()
	if acc.SendWindow != window {
		acc.SendWindow = window
		acc.SendCount = 0
	}
	return uint32(window + 3600)
}

// SendQuota returns how many messages addr may still send this hour.
func (s *ChatStore) SendQuota(addr [protocol.AddressSize]byte, now time.Time) (remaining uint16, resetUnix uint32, ok bool, err error) {
	sh := s.shardFor(addr)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	acc, err := s.loadLocked(addr, now)
	if err != nil || acc == nil {
		return 0, 0, false, err
	}
	reset := sendWindowLocked(acc, now)
	if acc.SendCount >= s.limits.SendPerHour {
		return 0, reset, true, nil
	}
	return s.limits.SendPerHour - acc.SendCount, reset, true, nil
}

// PrecheckMessage runs the advisory checks for a src→dst message.
func (s *ChatStore) PrecheckMessage(src, dst [protocol.AddressSize]byte, now time.Time) (status byte, remaining uint16, resetUnix uint32, err error) {
	unlock := s.lock2(src, dst)
	defer unlock()

	sender, err := s.loadLocked(src, now)
	if err != nil {
		return protocol.ChatStatusBusy, 0, 0, err
	}
	if sender == nil {
		return protocol.ChatStatusUnknownSender, 0, 0, nil
	}
	reset := sendWindowLocked(sender, now)
	remaining = 0
	if sender.SendCount < s.limits.SendPerHour {
		remaining = s.limits.SendPerHour - sender.SendCount
	}
	if remaining == 0 {
		return protocol.ChatStatusRateLimited, 0, reset, nil
	}

	recip, err := s.loadLocked(dst, now)
	if err != nil {
		return protocol.ChatStatusBusy, 0, 0, err
	}
	if recip == nil {
		return protocol.ChatStatusUnknownRecipient, remaining, reset, nil
	}
	if len(recip.Inbox) >= int(s.limits.InboxCap) {
		return protocol.ChatStatusInboxFull, remaining, reset, nil
	}
	if countFromLocked(recip, src) >= int(s.limits.PerPairCap) {
		return protocol.ChatStatusPairQuota, remaining, reset, nil
	}
	return protocol.ChatStatusOK, remaining, reset, nil
}

func countFromLocked(acc *chatAccount, src [protocol.AddressSize]byte) int {
	n := 0
	for _, m := range acc.Inbox {
		if string(m.Src) == string(src[:]) {
			n++
		}
	}
	return n
}

// CommitMessage stores a fully-verified envelope into dst's inbox, enforcing
// the authoritative quota/replay checks, and writes both accounts in one batch.
func (s *ChatStore) CommitMessage(src, dst [protocol.AddressSize]byte, seq uint32, envelope []byte, now time.Time) (status byte, lastAccepted uint32, remaining uint16, resetUnix uint32, err error) {
	unlock := s.lock2(src, dst)
	defer unlock()

	sender, err := s.loadLocked(src, now)
	if err != nil {
		return protocol.ChatStatusBusy, 0, 0, 0, err
	}
	if sender == nil {
		return protocol.ChatStatusUnknownSender, 0, 0, 0, nil
	}
	reset := sendWindowLocked(sender, now)
	if sender.SendCount >= s.limits.SendPerHour {
		return protocol.ChatStatusRateLimited, 0, 0, reset, nil
	}

	recip, err := s.loadLocked(dst, now)
	if err != nil {
		return protocol.ChatStatusBusy, 0, 0, 0, err
	}
	if recip == nil {
		return protocol.ChatStatusUnknownRecipient, 0, 0, reset, nil
	}
	if recip.Pairs == nil {
		recip.Pairs = make(map[string]*chatPairState)
	}
	pair := recip.Pairs[pairKeyOf(src)]
	if pair == nil {
		pair = &chatPairState{}
		recip.Pairs[pairKeyOf(src)] = pair
	}
	if seq < pair.LastAccepted {
		return protocol.ChatStatusReplay, pair.LastAccepted, s.limits.SendPerHour - sender.SendCount, reset, nil
	}
	if seq == pair.LastAccepted {
		// Idempotent: a lost FIN-OK made the client retransmit the committed
		// message. Report success without storing a duplicate or recharging.
		return protocol.ChatStatusOK, pair.LastAccepted, s.limits.SendPerHour - sender.SendCount, reset, nil
	}
	if len(recip.Inbox) >= int(s.limits.InboxCap) {
		return protocol.ChatStatusInboxFull, pair.LastAccepted, s.limits.SendPerHour - sender.SendCount, reset, nil
	}
	if countFromLocked(recip, src) >= int(s.limits.PerPairCap) {
		return protocol.ChatStatusPairQuota, pair.LastAccepted, s.limits.SendPerHour - sender.SendCount, reset, nil
	}

	recip.Inbox = append(recip.Inbox, chatInboxMsg{
		Src: append([]byte(nil), src[:]...), Seq: seq,
		Envelope: append([]byte(nil), envelope...), Received: now.Unix(),
	})
	pair.LastAccepted = seq
	recip.LastActive = now.Unix()
	sender.SendCount++
	sender.LastActive = now.Unix()

	accs := map[[protocol.AddressSize]byte]*chatAccount{dst: recip}
	if src != dst {
		accs[src] = sender
	}
	if err := s.putLocked(now, accs); err != nil {
		return protocol.ChatStatusBusy, 0, 0, 0, err
	}
	return protocol.ChatStatusOK, seq, s.limits.SendPerHour - sender.SendCount, reset, nil
}

// OpenSession validates a strictly-increasing OPEN timestamp for addr and
// persists it. Retained for completeness (the v2 handshake checks skew inline).
func (s *ChatStore) OpenSession(addr [protocol.AddressSize]byte, ts uint32, now time.Time, commit bool) (identityPub []byte, lastAck uint32, status byte, err error) {
	sh := s.shardFor(addr)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	acc, err := s.loadLocked(addr, now)
	if err != nil {
		return nil, 0, protocol.ChatStatusBusy, err
	}
	if acc == nil {
		return nil, 0, protocol.ChatStatusUnknownSender, nil
	}
	if ts <= acc.LastOpenTS {
		return acc.IdentityPub, acc.LastAckCounter, protocol.ChatStatusBadAuth, nil
	}
	if !commit {
		return acc.IdentityPub, acc.LastAckCounter, protocol.ChatStatusOK, nil
	}
	acc.LastOpenTS = ts
	acc.LastActive = now.Unix()
	if err := s.putLocked(now, map[[protocol.AddressSize]byte]*chatAccount{addr: acc}); err != nil {
		return nil, 0, protocol.ChatStatusBusy, err
	}
	return acc.IdentityPub, acc.LastAckCounter, protocol.ChatStatusOK, nil
}

// ChatInboxEntry describes one waiting message for INBOX_STATUS.
type ChatInboxEntry struct {
	Src    [protocol.AddressSize]byte
	Seq    uint32
	Len    uint16
	Blocks uint8
}

// chatFetchBlockSize is the per-FETCH slice of a stored envelope.
const chatFetchBlockSize = protocol.MaxBlockPayload

// InboxStatus lists the waiting messages for addr.
func (s *ChatStore) InboxStatus(addr [protocol.AddressSize]byte, now time.Time) ([]ChatInboxEntry, error) {
	sh := s.shardFor(addr)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	acc, err := s.loadLocked(addr, now)
	if err != nil || acc == nil {
		return nil, err
	}
	out := make([]ChatInboxEntry, 0, len(acc.Inbox))
	for _, m := range acc.Inbox {
		var src [protocol.AddressSize]byte
		copy(src[:], m.Src)
		blocks := (len(m.Envelope) + chatFetchBlockSize - 1) / chatFetchBlockSize
		if blocks == 0 {
			blocks = 1
		}
		out = append(out, ChatInboxEntry{Src: src, Seq: m.Seq, Len: uint16(len(m.Envelope)), Blocks: uint8(blocks)})
	}
	return out, nil
}

// FetchBlock returns one slice of a stored envelope by (seq, block index).
func (s *ChatStore) FetchBlock(addr [protocol.AddressSize]byte, seq uint32, block uint8, now time.Time) ([]byte, bool, error) {
	sh := s.shardFor(addr)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	acc, err := s.loadLocked(addr, now)
	if err != nil || acc == nil {
		return nil, false, err
	}
	for _, m := range acc.Inbox {
		if m.Seq != seq {
			continue
		}
		start := int(block) * chatFetchBlockSize
		if start >= len(m.Envelope) {
			return nil, false, nil
		}
		end := start + chatFetchBlockSize
		if end > len(m.Envelope) {
			end = len(m.Envelope)
		}
		return append([]byte(nil), m.Envelope[start:end]...), true, nil
	}
	return nil, false, nil
}

// Ack frees delivered messages: removes peer→addr messages with seq ≤ upToSeq
// and bumps last_delivered. Idempotent (the cell seal authenticated the caller).
func (s *ChatStore) Ack(addr, peer [protocol.AddressSize]byte, upToSeq uint32, now time.Time) (status byte, err error) {
	sh := s.shardFor(addr)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	acc, err := s.loadLocked(addr, now)
	if err != nil {
		return protocol.ChatStatusBusy, err
	}
	if acc == nil {
		return protocol.ChatStatusUnknownSender, nil
	}

	kept := acc.Inbox[:0]
	for _, m := range acc.Inbox {
		if string(m.Src) == string(peer[:]) && m.Seq <= upToSeq {
			continue
		}
		kept = append(kept, m)
	}
	acc.Inbox = kept
	if acc.Pairs == nil {
		acc.Pairs = make(map[string]*chatPairState)
	}
	pair := acc.Pairs[pairKeyOf(peer)]
	if pair == nil {
		pair = &chatPairState{}
		acc.Pairs[pairKeyOf(peer)] = pair
	}
	if upToSeq > pair.LastDelivered {
		pair.LastDelivered = upToSeq
	}
	acc.LastActive = now.Unix()
	if err := s.putLocked(now, map[[protocol.AddressSize]byte]*chatAccount{addr: acc}); err != nil {
		return protocol.ChatStatusBusy, err
	}
	return protocol.ChatStatusOK, nil
}

// PairState returns (last_accepted, last_delivered) for messages owner←peer.
func (s *ChatStore) PairState(owner, peer [protocol.AddressSize]byte, now time.Time) (accepted, delivered uint32, ok bool, err error) {
	sh := s.shardFor(owner)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	acc, err := s.loadLocked(owner, now)
	if err != nil || acc == nil {
		return 0, 0, false, err
	}
	if p := acc.Pairs[pairKeyOf(peer)]; p != nil {
		return p.LastAccepted, p.LastDelivered, true, nil
	}
	return 0, 0, true, nil
}

// ResolvePeerHandle maps a 4-byte peer handle to a full peer address within
// owner's known pairs. Returns ok=false if there is no unique match.
func (s *ChatStore) ResolvePeerHandle(owner [protocol.AddressSize]byte, handle [protocol.ChatPeerHandleSize]byte, now time.Time) (peer [protocol.AddressSize]byte, ok bool, err error) {
	sh := s.shardFor(owner)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	acc, err := s.loadLocked(owner, now)
	if err != nil || acc == nil {
		return peer, false, err
	}
	found := 0
	for k := range acc.Pairs {
		raw, e := hex.DecodeString(k)
		if e != nil || len(raw) != protocol.AddressSize {
			continue
		}
		if string(raw[:protocol.ChatPeerHandleSize]) == string(handle[:]) {
			copy(peer[:], raw)
			found++
		}
	}
	return peer, found == 1, nil
}

// Sweep expires idle RAM entries, inbox messages past TTL, and (if an account
// TTL is set) long-inactive accounts.
func (s *ChatStore) Sweep(now time.Time) (expiredMsgs, deletedAccounts int, err error) {
	// Drop idle RAM entries per shard.
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for addr, e := range sh.hot {
			if now.Sub(e.lastUsed) > chatIdleEvict {
				delete(sh.hot, addr)
			}
		}
		sh.mu.Unlock()
	}

	ttl := time.Duration(s.limits.TTLHours) * time.Hour
	msgCutoff := now.Add(-ttl).Unix()
	gcAccounts := s.accountTTL > 0
	accCutoff := int64(0)
	if gcAccounts {
		accCutoff = now.Add(-s.accountTTL).Unix()
	}

	err = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(chatAccountsBucket)
		type upd struct {
			key, raw []byte
		}
		var updates []upd
		var deletes [][]byte
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var acc chatAccount
			if json.Unmarshal(v, &acc) != nil {
				continue
			}
			if gcAccounts && acc.LastActive < accCutoff {
				deletes = append(deletes, append([]byte(nil), k...))
				deletedAccounts++
				continue
			}
			kept := acc.Inbox[:0]
			changed := false
			for _, m := range acc.Inbox {
				if m.Received < msgCutoff {
					expiredMsgs++
					changed = true
					continue
				}
				kept = append(kept, m)
			}
			if changed {
				acc.Inbox = kept
				raw, merr := json.Marshal(&acc)
				if merr != nil {
					return merr
				}
				updates = append(updates, upd{key: append([]byte(nil), k...), raw: raw})
			}
		}
		for _, u := range updates {
			if err := b.Put(u.key, u.raw); err != nil {
				return err
			}
		}
		for _, k := range deletes {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	// Drop any swept account from RAM so stale inboxes aren't served.
	if expiredMsgs > 0 || deletedAccounts > 0 {
		for i := range s.shards {
			sh := &s.shards[i]
			sh.mu.Lock()
			sh.hot = make(map[[protocol.AddressSize]byte]*chatHotEntry)
			sh.mu.Unlock()
		}
	}
	if deletedAccounts > 0 {
		s.accountCount.Add(int64(-deletedAccounts))
	}
	return expiredMsgs, deletedAccounts, nil
}
