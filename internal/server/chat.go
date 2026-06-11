package server

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/binary"
	"hash/crc32"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// ChatService handles chat requests on the dedicated chat domain(s). Every
// query is one uniform sealed cell. A per-connection eph↔ek handshake yields a
// session (Ksession + a server-assigned selector); all later ops are sealed
// under Ksession, so the op, the destination and the read metadata are all
// encrypted. Sessions, uploads and handshake reassemblies are RAM-only and
// bounded; accounts/inboxes live in the ChatStore.
type ChatService struct {
	store    *ChatStore
	ek       *ecdh.PrivateKey
	ekPub    []byte
	queryKey [protocol.KeySize]byte
	limits   protocol.ChatLimits
	domains  []string

	mu         sync.Mutex
	sessions   map[[protocol.ChatSelectorSize]byte]*chatSession
	handshakes map[[protocol.ChatSelectorSize]byte]*chatHandshake
	perAccount map[[protocol.AddressSize]byte]int

	statQueries   atomic.Int64
	statInits     atomic.Int64 // completed handshakes
	statCommits   atomic.Int64
	statRegisters atomic.Int64
}

type chatSession struct {
	account  [protocol.AddressSize]byte
	ksession [protocol.KeySize]byte
	created  time.Time
	lastSeen time.Time
	upload   *chatUpload
}

type chatUpload struct {
	dst      [protocol.AddressSize]byte
	totalLen int
	reasm    *protocol.ChunkReassembler
}

type chatHandshake struct {
	chunks  [][]byte // stream payloads by index
	created time.Time
}

const (
	// chatMaxSessions is a RAM-bound soft cap on concurrent live sessions.
	chatMaxSessions = 50000
	// chatMaxPerAccount bounds concurrent sessions per account (flood guard).
	chatMaxPerAccount = 32
	chatHandshakeTTL  = 30 * time.Second
	chatOpenSkew      = 10 * time.Minute
	chatSweepEvery    = 5 * time.Minute
	// chatEnvelopeOverhead bounds a SEND_START total length headroom over the
	// configured max message bytes (ver+seq+gcm tag+srvmac+cflag, plus slack).
	chatEnvelopeOverhead = 48
)

// NewChatService creates the chat handler. domains are the dedicated chat
// sub-domains (already validated against feed domains by the DNS layer).
func NewChatService(store *ChatStore, ek *ecdh.PrivateKey, queryKey [protocol.KeySize]byte, limits protocol.ChatLimits, domains []string) *ChatService {
	return &ChatService{
		store:      store,
		ek:         ek,
		ekPub:      ek.PublicKey().Bytes(),
		queryKey:   queryKey,
		limits:     limits,
		domains:    domains,
		sessions:   make(map[[protocol.ChatSelectorSize]byte]*chatSession),
		handshakes: make(map[[protocol.ChatSelectorSize]byte]*chatHandshake),
		perAccount: make(map[[protocol.AddressSize]byte]int),
	}
}

// Domains returns the chat sub-domains.
func (c *ChatService) Domains() []string { return c.domains }

// Info returns the ChatInfo payload advertised (signed) on the feed metadata
// path.
func (c *ChatService) Info() protocol.ChatInfo {
	return protocol.ChatInfo{
		MinVersion: protocol.ChatProtocolVersion,
		MaxVersion: protocol.ChatProtocolVersion,
		Enabled:    true,
		Domains:    c.domains,
		EkPub:      c.ekPub,
		Limits:     c.limits,
	}
}

// RunSweeper periodically expires sessions, handshakes, old messages, and idle
// accounts until ctx is done.
func (c *ChatService) RunSweeper(ctx context.Context) {
	ticker := time.NewTicker(chatSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			c.mu.Lock()
			c.cleanupLocked(now)
			c.mu.Unlock()
			if msgs, accs, err := c.store.Sweep(now); err != nil {
				log.Printf("[chat] sweep: %v", err)
			} else if msgs > 0 || accs > 0 {
				log.Printf("[chat] sweep: expired %d message(s), deleted %d account(s)", msgs, accs)
			}
		}
	}
}

// StatsSnapshot returns the per-window counters for the hourly report and
// resets them. `accounts` and `sessions` are point-in-time gauges.
func (c *ChatService) StatsSnapshot() map[string]int64 {
	c.mu.Lock()
	sessions := int64(len(c.sessions))
	c.mu.Unlock()
	return map[string]int64{
		"queries":   c.statQueries.Swap(0),
		"inits":     c.statInits.Swap(0),
		"messages":  c.statCommits.Swap(0),
		"registers": c.statRegisters.Swap(0),
		"accounts":  int64(c.store.AccountCount()),
		"sessions":  sessions,
	}
}

func (c *ChatService) cleanupLocked(now time.Time) {
	idle := time.Duration(c.limits.SessionIdleSec) * time.Second
	hard := time.Duration(c.limits.SessionHardSec) * time.Second
	for ref, s := range c.sessions {
		if now.Sub(s.lastSeen) > idle || now.Sub(s.created) > hard {
			c.perAccount[s.account]--
			if c.perAccount[s.account] <= 0 {
				delete(c.perAccount, s.account)
			}
			delete(c.sessions, ref)
		}
	}
	for tag, h := range c.handshakes {
		if now.Sub(h.created) > chatHandshakeTTL {
			delete(c.handshakes, tag)
		}
	}
}

// HandleCell processes one decoded chat cell and returns the response bytes (to
// be wrapped by EncodeResponse). An empty slice means "incomplete handshake,
// keep going"; ChatSessionLostResp means "re-handshake".
func (c *ChatService) HandleCell(selector [protocol.ChatSelectorSize]byte, counter uint32, payload []byte, domain string, now time.Time) []byte {
	c.statQueries.Add(1)
	c.mu.Lock()
	sess := c.sessions[selector]
	c.mu.Unlock()
	if sess != nil {
		return c.handleInContext(sess, selector, counter, payload, now)
	}
	if protocol.ChatIsHandshakeSelector(selector) {
		return c.handleHandshakeCell(selector, uint8(counter), payload, domain, now)
	}
	// Unknown selector for an in-context cell → the session is gone (expiry or
	// reboot). Tell the client to re-handshake.
	return protocol.ChatSessionLostResp
}

// ---- handshake ----

func (h *chatHandshake) add(idx uint8, payload []byte) {
	for len(h.chunks) <= int(idx) {
		h.chunks = append(h.chunks, nil)
	}
	h.chunks[idx] = append([]byte(nil), payload...)
}

// assemble returns the full handshake stream once enough cells have arrived.
func (h *chatHandshake) assemble() ([]byte, bool) {
	if len(h.chunks) < 2 || h.chunks[0] == nil || h.chunks[1] == nil {
		return nil, false
	}
	// stream[X25519KeySize] = kind, which lands in chunk[1] at offset
	// X25519KeySize - ChatCellPayloadSize.
	kindOff := protocol.X25519KeySize - protocol.ChatCellPayloadSize
	if kindOff < 0 || kindOff >= len(h.chunks[1]) {
		return nil, false
	}
	var bootLen int
	switch h.chunks[1][kindOff] {
	case protocol.ChatHandshakeAuth:
		bootLen = protocol.AddressSize + 4 + protocol.ChatAccountProofSize + protocol.ChatSealTagSize
	case protocol.ChatHandshakeRegister:
		bootLen = protocol.RegisterEnvelopeLen + protocol.ChatSealTagSize
	default:
		return nil, false
	}
	total := protocol.X25519KeySize + 1 + bootLen
	n := (total + protocol.ChatCellPayloadSize - 1) / protocol.ChatCellPayloadSize
	buf := make([]byte, 0, n*protocol.ChatCellPayloadSize)
	for i := 0; i < n; i++ {
		if i >= len(h.chunks) || h.chunks[i] == nil {
			return nil, false
		}
		buf = append(buf, h.chunks[i]...)
	}
	if len(buf) < total {
		return nil, false
	}
	return buf[:total], true
}

func (c *ChatService) handleHandshakeCell(setupTag [protocol.ChatSelectorSize]byte, idx uint8, payload []byte, domain string, now time.Time) []byte {
	c.mu.Lock()
	h := c.handshakes[setupTag]
	if h == nil {
		if len(c.handshakes) >= chatMaxSessions {
			c.mu.Unlock()
			return []byte{} // shed load; client retries
		}
		h = &chatHandshake{created: now}
		c.handshakes[setupTag] = h
	}
	h.add(idx, payload)
	stream, ok := h.assemble()
	if !ok {
		c.mu.Unlock()
		return []byte{} // incomplete — client keeps streaming / resending
	}
	delete(c.handshakes, setupTag)
	c.mu.Unlock()
	return c.completeHandshake(setupTag, stream, domain, now)
}

func (c *ChatService) completeHandshake(setupTag [protocol.ChatSelectorSize]byte, stream []byte, domain string, now time.Time) []byte {
	ephPub, kind, sealedBoot, err := protocol.ParseChatHandshakeStream(stream)
	if err != nil {
		return []byte{}
	}
	ksession, err := protocol.ChatSessionKey(c.ek, ephPub, c.queryKey)
	if err != nil {
		return []byte{}
	}
	boot, err := protocol.OpenChat(ksession, setupTag[:], protocol.ChatBootstrapCounter(), sealedBoot)
	if err != nil {
		// Wrong passphrase or corrupt — looks like a non-chat query; drop.
		return []byte{}
	}

	// Every handshake reply (even errors) carries the server's unix time first,
	// so a clock-skewed client can correct its offset and retry the auth proof.
	hsResp := func(status byte, extra []byte) []byte {
		body := make([]byte, 4, 4+len(extra))
		binary.BigEndian.PutUint32(body, uint32(now.Unix()))
		body = append(body, extra...)
		return protocol.SealChatResponse(ksession, setupTag, protocol.ChatBootstrapCounter(), status, body)
	}

	var account [protocol.AddressSize]byte
	switch kind {
	case protocol.ChatHandshakeRegister:
		rec, e := protocol.ParseRegisterEnvelope(boot)
		if e != nil || rec.Verify() != nil {
			return hsResp(protocol.ChatStatusBadAuth, nil)
		}
		account = rec.Address()
		if err := c.store.Register(rec, boot, now); err != nil {
			if err == ErrChatAccountsFull {
				return hsResp(protocol.ChatStatusBusy, nil)
			}
			return hsResp(protocol.ChatStatusBusy, nil)
		}
		c.statRegisters.Add(1)
	case protocol.ChatHandshakeAuth:
		b, e := protocol.ParseChatAuthBootstrapPlain(boot)
		if e != nil {
			return hsResp(protocol.ChatStatusBadRequest, nil)
		}
		if diff := int64(b.TS) - now.Unix(); diff < -int64(chatOpenSkew.Seconds()) || diff > int64(chatOpenSkew.Seconds()) {
			return hsResp(protocol.ChatStatusBadAuth, nil)
		}
		_, encPub, ok, kerr := c.store.Keys(b.Addr, now)
		if kerr != nil {
			return hsResp(protocol.ChatStatusBusy, nil)
		}
		if !ok {
			return hsResp(protocol.ChatStatusUnknownSender, nil)
		}
		kss, kerr := protocol.ChatServerSharedKey(c.ek, encPub, encPub, c.ekPub)
		if kerr != nil {
			return hsResp(protocol.ChatStatusBadRequest, nil)
		}
		if protocol.ChatAccountProof(kss, ephPub, b.Addr, b.TS, domain) != b.Proof {
			return hsResp(protocol.ChatStatusBadAuth, nil)
		}
		account = b.Addr
	default:
		return []byte{}
	}

	c.mu.Lock()
	if len(c.sessions) >= chatMaxSessions || c.perAccount[account] >= chatMaxPerAccount {
		c.mu.Unlock()
		return hsResp(protocol.ChatStatusBusy, nil)
	}
	ref := c.allocSelectorLocked()
	c.sessions[ref] = &chatSession{account: account, ksession: ksession, created: now, lastSeen: now}
	c.perAccount[account]++
	c.statInits.Add(1)
	c.mu.Unlock()

	ttl := uint16(c.limits.SessionHardSec)
	extra := make([]byte, 0, protocol.ChatSelectorSize+2)
	extra = append(extra, ref[:]...)
	extra = append(extra, byte(ttl>>8), byte(ttl))
	return hsResp(protocol.ChatStatusOK, extra)
}

// allocSelectorLocked mints a free, non-handshake session ref. Caller holds mu.
func (c *ChatService) allocSelectorLocked() [protocol.ChatSelectorSize]byte {
	var ref [protocol.ChatSelectorSize]byte
	for {
		_, _ = rand.Read(ref[:])
		protocol.ChatClearHandshakeSelector(&ref)
		if _, exists := c.sessions[ref]; !exists {
			return ref
		}
	}
}

// ---- in-context ops ----

func (c *ChatService) handleInContext(sess *chatSession, selector [protocol.ChatSelectorSize]byte, counter uint32, payload []byte, now time.Time) []byte {
	pt, err := protocol.OpenChat(sess.ksession, selector[:], counter, payload)
	if err != nil {
		// Bad seal — corruption or a stale selector collision; ask to re-handshake.
		return protocol.ChatSessionLostResp
	}
	c.mu.Lock()
	sess.lastSeen = now
	c.mu.Unlock()

	resp := func(status byte, body []byte) []byte {
		return protocol.SealChatResponse(sess.ksession, selector, counter, status, body)
	}

	switch protocol.ChatPlainOp(pt) {
	case protocol.ChatOpStatus:
		return c.opStatus(sess, resp, now)
	case protocol.ChatOpFetch:
		return c.opFetch(sess, pt, resp, now)
	case protocol.ChatOpAck:
		return c.opAck(sess, pt, resp, now)
	case protocol.ChatOpSendStatus:
		return c.opSendStatus(sess, pt, resp, now)
	case protocol.ChatOpKeyFetch:
		return c.opKeyFetch(pt, resp, now)
	case protocol.ChatOpSendStart:
		return c.opSendStart(sess, pt, resp, now)
	case protocol.ChatOpData:
		return c.opData(sess, pt, resp)
	case protocol.ChatOpFin:
		return c.opFin(sess, pt, resp, now)
	default:
		return resp(protocol.ChatStatusBadRequest, nil)
	}
}

func appendQuota(body []byte, remaining uint16, reset uint32) []byte {
	body = append(body, byte(remaining>>8), byte(remaining))
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], reset)
	return append(body, b[:]...)
}

func (c *ChatService) opStatus(sess *chatSession, resp func(byte, []byte) []byte, now time.Time) []byte {
	entries, err := c.store.InboxStatus(sess.account, now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	remaining, reset, _, err := c.store.SendQuota(sess.account, now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	if len(entries) > 255 {
		entries = entries[:255]
	}
	body := appendQuota(nil, remaining, reset)
	body = append(body, byte(len(entries)))
	for _, e := range entries {
		body = append(body, e.Src[:]...)
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], e.Seq)
		body = append(body, b[:]...)
		body = append(body, byte(e.Len>>8), byte(e.Len), e.Blocks)
	}
	return resp(protocol.ChatStatusOK, body)
}

func (c *ChatService) opFetch(sess *chatSession, pt []byte, resp func(byte, []byte) []byte, now time.Time) []byte {
	f, err := protocol.ParseChatFetchPlain(pt)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	block, ok, err := c.store.FetchBlock(sess.account, f.Seq, f.Block, now)
	if err != nil || !ok {
		return resp(protocol.ChatStatusNotFound, nil)
	}
	return resp(protocol.ChatStatusOK, block)
}

func (c *ChatService) opAck(sess *chatSession, pt []byte, resp func(byte, []byte) []byte, now time.Time) []byte {
	a, err := protocol.ParseChatAckPlain(pt)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	peer, ok, err := c.store.ResolvePeerHandle(sess.account, a.Peer, now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	if !ok {
		return resp(protocol.ChatStatusNotFound, nil)
	}
	status, err := c.store.Ack(sess.account, peer, a.UpToSeq, now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	return resp(status, nil)
}

func (c *ChatService) opSendStatus(sess *chatSession, pt []byte, resp func(byte, []byte) []byte, now time.Time) []byte {
	s, err := protocol.ParseChatSendStatusPlain(pt)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	// Caller is the sender; counters live in the recipient's account.
	accepted, delivered, ok, err := c.store.PairState(s.Peer, sess.account, now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	if !ok {
		return resp(protocol.ChatStatusNotFound, nil)
	}
	body := make([]byte, 8)
	binary.BigEndian.PutUint32(body, accepted)
	binary.BigEndian.PutUint32(body[4:], delivered)
	return resp(protocol.ChatStatusOK, body)
}

func (c *ChatService) opKeyFetch(pt []byte, resp func(byte, []byte) []byte, now time.Time) []byte {
	k, err := protocol.ParseChatKeyFetchPlain(pt)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	rec, ok, err := c.store.RegisterRecord(k.Addr, now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	if !ok {
		return resp(protocol.ChatStatusNotFound, nil)
	}
	return resp(protocol.ChatStatusOK, rec)
}

func (c *ChatService) opSendStart(sess *chatSession, pt []byte, resp func(byte, []byte) []byte, now time.Time) []byte {
	ss, err := protocol.ParseChatSendStartPlain(pt)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	maxLen := int(c.limits.MaxMsgBytes) + chatEnvelopeOverhead
	if ss.TotalLen == 0 || int(ss.TotalLen) > maxLen {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	status, remaining, reset, err := c.store.PrecheckMessage(sess.account, ss.Dst, now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	if status != protocol.ChatStatusOK {
		return resp(status, appendQuota(nil, remaining, reset))
	}
	chunks := (int(ss.TotalLen) + protocol.ChatDataChunkSize - 1) / protocol.ChatDataChunkSize
	if chunks < 1 || chunks > 255 {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	c.mu.Lock()
	sess.upload = &chatUpload{dst: ss.Dst, totalLen: int(ss.TotalLen), reasm: protocol.NewChunkReassembler(chunks)}
	bm := sess.upload.reasm.Bitmap()
	c.mu.Unlock()
	return resp(protocol.ChatStatusOK, append(appendQuota(nil, remaining, reset), bm...))
}

func (c *ChatService) opData(sess *chatSession, pt []byte, resp func(byte, []byte) []byte) []byte {
	d, err := protocol.ParseChatDataPlain(pt)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	c.mu.Lock()
	up := sess.upload
	if up == nil {
		c.mu.Unlock()
		return resp(protocol.ChatStatusUnknownSession, nil)
	}
	realLen := up.totalLen - int(d.Index)*protocol.ChatDataChunkSize
	if realLen > protocol.ChatDataChunkSize {
		realLen = protocol.ChatDataChunkSize
	}
	if realLen < 0 || realLen > len(d.Chunk) {
		c.mu.Unlock()
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	up.reasm.Add(int(d.Index), d.Chunk[:realLen])
	bm := up.reasm.Bitmap()
	c.mu.Unlock()
	return resp(protocol.ChatStatusOK, bm)
}

func (c *ChatService) opFin(sess *chatSession, pt []byte, resp func(byte, []byte) []byte, now time.Time) []byte {
	fin, err := protocol.ParseChatFinPlain(pt)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	c.mu.Lock()
	up := sess.upload
	if up == nil {
		c.mu.Unlock()
		return resp(protocol.ChatStatusUnknownSession, nil)
	}
	if !up.reasm.Complete() {
		bm := up.reasm.Bitmap()
		c.mu.Unlock()
		return resp(protocol.ChatStatusIncomplete, bm)
	}
	assembled := up.reasm.Assemble()
	dst := up.dst
	sess.upload = nil
	c.mu.Unlock()

	if len(assembled) != up.totalLen || crc32.ChecksumIEEE(assembled) != fin.CRC32 {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	m, err := protocol.ParseChatMessage(assembled)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	_, encPub, ok, err := c.store.Keys(sess.account, now)
	if err != nil || !ok {
		return resp(protocol.ChatStatusUnknownSender, nil)
	}
	kss, err := protocol.ChatServerSharedKey(c.ek, encPub, encPub, c.ekPub)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	if m.VerifyServerMAC(kss, sess.account, dst) != nil {
		return resp(protocol.ChatStatusBadAuth, nil)
	}
	status, lastAccepted, remaining, reset, err := c.store.CommitMessage(sess.account, dst, m.Seq, assembled, now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	if status == protocol.ChatStatusOK {
		c.statCommits.Add(1)
	}
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, lastAccepted)
	body = appendQuota(body, remaining, reset)
	return resp(status, body)
}
