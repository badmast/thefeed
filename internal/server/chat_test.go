package server

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"hash/crc32"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

const simDomain = "chat.example.com"

// simClient is a minimal client that drives the cell protocol against a
// ChatService in-process (no DNS), exercising the server end-to-end.
type simClient struct {
	id   ed25519.PrivateKey
	enc  *ecdh.PrivateKey
	addr [protocol.AddressSize]byte
}

func newSimClient(t *testing.T) simClient {
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
	return simClient{id: id, enc: enc, addr: protocol.Address(id.Public().(ed25519.PublicKey))}
}

func newChatSvc(t *testing.T) (*ChatService, [protocol.KeySize]byte, []byte) {
	t.Helper()
	store := newTestStore(t)
	ek, err := protocol.GenerateEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	qk, _, err := protocol.DeriveKeys("svc-pass")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewChatService(store, ek, qk, store.Limits(), []string{simDomain})
	return svc, qk, ek.PublicKey().Bytes()
}

// feedStream streams a handshake stream as cells; the completing cell returns
// the sealed handshake response.
func feedStream(t *testing.T, svc *ChatService, tag [protocol.ChatSelectorSize]byte, stream []byte) []byte {
	t.Helper()
	n := (len(stream) + protocol.ChatCellPayloadSize - 1) / protocol.ChatCellPayloadSize
	var resp []byte
	for i := 0; i < n; i++ {
		start := i * protocol.ChatCellPayloadSize
		end := start + protocol.ChatCellPayloadSize
		if end > len(stream) {
			end = len(stream)
		}
		chunk := make([]byte, protocol.ChatCellPayloadSize)
		copy(chunk, stream[start:end])
		resp = svc.HandleCell(tag, uint32(i), chunk, simDomain, time.Now())
	}
	return resp
}

func handshakeTag() [protocol.ChatSelectorSize]byte {
	var tag [protocol.ChatSelectorSize]byte
	_, _ = rand.Read(tag[:])
	protocol.ChatMarkHandshakeSelector(&tag)
	return tag
}

func openHandshake(t *testing.T, ks [protocol.KeySize]byte, tag [protocol.ChatSelectorSize]byte, resp []byte) [protocol.ChatSelectorSize]byte {
	t.Helper()
	st, body, err := protocol.OpenChatResponse(ks, tag, protocol.ChatBootstrapCounter(), resp)
	if err != nil || st != protocol.ChatStatusOK {
		t.Fatalf("handshake st=%d err=%v resp=%x", st, err, resp)
	}
	// body = serverUnix(4) ‖ sessionRef(3) ‖ ttl(2)
	var ref [protocol.ChatSelectorSize]byte
	copy(ref[:], body[4:4+protocol.ChatSelectorSize])
	return ref
}

func registerHandshake(t *testing.T, svc *ChatService, qk [protocol.KeySize]byte, ekPub []byte, c simClient) (ref [protocol.ChatSelectorSize]byte, ks [protocol.KeySize]byte) {
	t.Helper()
	eph, _ := protocol.GenerateEphemeralKey()
	ks, _ = protocol.ChatSessionKey(eph, ekPub, qk)
	tag := handshakeTag()
	rec, err := protocol.EncodeRegisterEnvelope(c.id, c.enc.PublicKey().Bytes(), 1750000000)
	if err != nil {
		t.Fatal(err)
	}
	sealedBoot := protocol.SealChat(ks, tag[:], protocol.ChatBootstrapCounter(), rec)
	stream := protocol.BuildChatHandshakeStream(eph.PublicKey().Bytes(), protocol.ChatHandshakeRegister, sealedBoot)
	return openHandshake(t, ks, tag, feedStream(t, svc, tag, stream)), ks
}

func authHandshake(t *testing.T, svc *ChatService, qk [protocol.KeySize]byte, ekPub []byte, c simClient) (ref [protocol.ChatSelectorSize]byte, ks [protocol.KeySize]byte) {
	t.Helper()
	eph, _ := protocol.GenerateEphemeralKey()
	ks, _ = protocol.ChatSessionKey(eph, ekPub, qk)
	tag := handshakeTag()
	ts := uint32(time.Now().Unix()) // within the server's skew window
	kss, _ := protocol.ChatServerSharedKey(c.enc, ekPub, c.enc.PublicKey().Bytes(), ekPub)
	proof := protocol.ChatAccountProof(kss, eph.PublicKey().Bytes(), c.addr, ts, simDomain)
	boot := protocol.BuildChatAuthBootstrapPlain(c.addr, ts, proof)
	sealedBoot := protocol.SealChat(ks, tag[:], protocol.ChatBootstrapCounter(), boot)
	stream := protocol.BuildChatHandshakeStream(eph.PublicKey().Bytes(), protocol.ChatHandshakeAuth, sealedBoot)
	return openHandshake(t, ks, tag, feedStream(t, svc, tag, stream)), ks
}

func chatOp(t *testing.T, svc *ChatService, ref [protocol.ChatSelectorSize]byte, ks [protocol.KeySize]byte, ctr *uint32, plain []byte) (byte, []byte) {
	t.Helper()
	payload, err := protocol.SealChatCellPayload(ks, ref, *ctr, plain)
	if err != nil {
		t.Fatal(err)
	}
	resp := svc.HandleCell(ref, *ctr, payload, simDomain, time.Now())
	st, body, err := protocol.OpenChatResponse(ks, ref, *ctr, resp)
	cur := *ctr
	*ctr++
	if err != nil {
		t.Fatalf("op ctr=%d open: %v resp=%x", cur, err, resp)
	}
	return st, body
}

func TestChatRegisterHandshakeAndStatus(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	a := newSimClient(t)
	ref, ks := registerHandshake(t, svc, qk, ekPub, a)
	ctr := uint32(0)
	st, body := chatOp(t, svc, ref, ks, &ctr, protocol.BuildChatStatusPlain())
	if st != protocol.ChatStatusOK {
		t.Fatalf("status st=%d", st)
	}
	if len(body) < 7 || body[6] != 0 {
		t.Fatalf("expected empty inbox, body=%x", body)
	}
}

func TestChatAuthHandshakeAfterRegister(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	a := newSimClient(t)
	registerHandshake(t, svc, qk, ekPub, a) // creates the account
	ref, ks := authHandshake(t, svc, qk, ekPub, a)
	ctr := uint32(0)
	if st, _ := chatOp(t, svc, ref, ks, &ctr, protocol.BuildChatStatusPlain()); st != protocol.ChatStatusOK {
		t.Fatalf("auth-session status st=%d", st)
	}
}

func TestChatUnknownSessionSentinel(t *testing.T) {
	svc, _, _ := newChatSvc(t)
	var ref [protocol.ChatSelectorSize]byte
	_, _ = rand.Read(ref[:])
	protocol.ChatClearHandshakeSelector(&ref) // unknown, non-handshake selector
	var ks [protocol.KeySize]byte
	payload, _ := protocol.SealChatCellPayload(ks, ref, 0, protocol.BuildChatStatusPlain())
	resp := svc.HandleCell(ref, 0, payload, simDomain, time.Now())
	if !protocol.ChatIsSessionLost(resp) {
		t.Fatalf("expected session-lost sentinel, got %x", resp)
	}
}

func TestChatSendRoundTrip(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	a := newSimClient(t)
	b := newSimClient(t)
	refA, ksA := registerHandshake(t, svc, qk, ekPub, a)
	refB, ksB := registerHandshake(t, svc, qk, ekPub, b)

	const text = "hello world سلام دوست"
	const seq = uint32(1)
	contentKey, _ := protocol.ChatContentKey(a.enc, b.enc.PublicKey().Bytes(), a.addr, b.addr, seq)
	kssA, _ := protocol.ChatServerSharedKey(a.enc, ekPub, a.enc.PublicKey().Bytes(), ekPub)
	env, err := protocol.EncodeChatMessage(contentKey, kssA, a.addr, b.addr, seq, text)
	if err != nil {
		t.Fatal(err)
	}

	// A: send-start → data chunks → fin.
	ctrA := uint32(0)
	if st, _ := chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatSendStartPlain(b.addr, uint16(len(env)))); st != protocol.ChatStatusOK {
		t.Fatalf("send-start st=%d", st)
	}
	for i, ch := range protocol.SplitChunks(env, protocol.ChatDataChunkSize) {
		d, _ := protocol.BuildChatDataPlain(uint8(i), ch)
		if st, _ := chatOp(t, svc, refA, ksA, &ctrA, d); st != protocol.ChatStatusOK {
			t.Fatalf("data %d st=%d", i, st)
		}
	}
	if st, _ := chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatFinPlain(crc32.ChecksumIEEE(env))); st != protocol.ChatStatusOK {
		t.Fatalf("fin st=%d", st)
	}

	// B: status shows 1 waiting from A.
	ctrB := uint32(0)
	st, body := chatOp(t, svc, refB, ksB, &ctrB, protocol.BuildChatStatusPlain())
	if st != protocol.ChatStatusOK || body[6] != 1 {
		t.Fatalf("B status st=%d count=%d", st, body[6])
	}
	entry := body[7:]
	var src [protocol.AddressSize]byte
	copy(src[:], entry[:protocol.AddressSize])
	if src != a.addr {
		t.Fatal("inbox src mismatch")
	}
	eSeq := binary.BigEndian.Uint32(entry[protocol.AddressSize:])
	blocks := entry[protocol.AddressSize+6]

	// B: fetch + reassemble + decrypt.
	var got []byte
	for blk := uint8(0); blk < blocks; blk++ {
		st, fb := chatOp(t, svc, refB, ksB, &ctrB, protocol.BuildChatFetchPlain(eSeq, blk))
		if st != protocol.ChatStatusOK {
			t.Fatalf("fetch blk %d st=%d", blk, st)
		}
		got = append(got, fb...)
	}
	m, err := protocol.ParseChatMessage(got)
	if err != nil {
		t.Fatal(err)
	}
	rk, _ := protocol.ChatContentKey(b.enc, a.enc.PublicKey().Bytes(), a.addr, b.addr, m.Seq)
	gotText, err := m.Open(rk)
	if err != nil || gotText != text {
		t.Fatalf("open: %v %q", err, gotText)
	}

	// B: ack (peer by handle) → inbox freed.
	handle := protocol.ChatPeerHandle(a.addr)
	if st, _ := chatOp(t, svc, refB, ksB, &ctrB, protocol.BuildChatAckPlain(handle, m.Seq)); st != protocol.ChatStatusOK {
		t.Fatalf("ack st=%d", st)
	}
	st, body = chatOp(t, svc, refB, ksB, &ctrB, protocol.BuildChatStatusPlain())
	if st != protocol.ChatStatusOK || body[6] != 0 {
		t.Fatalf("inbox not freed: count=%d", body[6])
	}
}

// sendStartBitSet reports whether chunk i is marked received in a SEND_START
// response (6 quota bytes, then the reassembler bitmap, MSB-first).
func sendStartBitSet(body []byte, i int) bool {
	bm := body[6:]
	return i/8 < len(bm) && bm[i/8]&(1<<(7-uint(i%8))) != 0
}

// TestChatResumesPartialUpload verifies that a repeat SEND_START for the same
// in-progress message RESUMES instead of discarding the chunks already received
// — so a client retrying on the same live session doesn't re-send the half it
// already delivered. (A new session resets, since sess.upload is nil there.)
func TestChatResumesPartialUpload(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	a := newSimClient(t)
	b := newSimClient(t)
	refA, ksA := registerHandshake(t, svc, qk, ekPub, a)
	registerHandshake(t, svc, qk, ekPub, b) // recipient must exist to send

	const text = "a long enough message to span several chunks — سلام دوست عزیز"
	const seq = uint32(1)
	contentKey, _ := protocol.ChatContentKey(a.enc, b.enc.PublicKey().Bytes(), a.addr, b.addr, seq)
	kssA, _ := protocol.ChatServerSharedKey(a.enc, ekPub, a.enc.PublicKey().Bytes(), ekPub)
	env, err := protocol.EncodeChatMessage(contentKey, kssA, a.addr, b.addr, seq, text)
	if err != nil {
		t.Fatal(err)
	}
	chunks := protocol.SplitChunks(env, protocol.ChatDataChunkSize)
	if len(chunks) < 2 {
		t.Fatalf("need a multi-chunk message, got %d chunk(s)", len(chunks))
	}

	ctrA := uint32(0)
	// Fresh SEND_START: chunk 0 not yet received.
	st, body := chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatSendStartPlain(b.addr, uint16(len(env))))
	if st != protocol.ChatStatusOK {
		t.Fatalf("send-start st=%d", st)
	}
	if sendStartBitSet(body, 0) {
		t.Fatal("fresh send-start should not report chunk 0 as received")
	}
	// Deliver only chunk 0, then a transport blip — the client retries by
	// re-issuing SEND_START on the SAME session.
	d0, _ := protocol.BuildChatDataPlain(0, chunks[0])
	if st, _ := chatOp(t, svc, refA, ksA, &ctrA, d0); st != protocol.ChatStatusOK {
		t.Fatalf("data 0 st=%d", st)
	}
	st, body = chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatSendStartPlain(b.addr, uint16(len(env))))
	if st != protocol.ChatStatusOK {
		t.Fatalf("resume send-start st=%d", st)
	}
	if !sendStartBitSet(body, 0) {
		t.Fatal("resumed send-start must report chunk 0 already received (no re-send)")
	}

	// Finish the remaining chunks + FIN: the resumed upload still commits.
	for i := 1; i < len(chunks); i++ {
		d, _ := protocol.BuildChatDataPlain(uint8(i), chunks[i])
		if st, _ := chatOp(t, svc, refA, ksA, &ctrA, d); st != protocol.ChatStatusOK {
			t.Fatalf("data %d st=%d", i, st)
		}
	}
	if st, _ := chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatFinPlain(crc32.ChecksumIEEE(env))); st != protocol.ChatStatusOK {
		t.Fatalf("fin st=%d", st)
	}
}

// TestChatSendStartResetsDifferentMessage is the negative of resume: a
// SEND_START for a DIFFERENT message (here, a different total length) must NOT
// resume onto the in-progress upload's chunks — it starts fresh, so messages
// can never be mixed.
func TestChatSendStartResetsDifferentMessage(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	a := newSimClient(t)
	b := newSimClient(t)
	refA, ksA := registerHandshake(t, svc, qk, ekPub, a)
	registerHandshake(t, svc, qk, ekPub, b)

	const text = "a long enough message to span several chunks سلام دوست"
	contentKey, _ := protocol.ChatContentKey(a.enc, b.enc.PublicKey().Bytes(), a.addr, b.addr, 1)
	kssA, _ := protocol.ChatServerSharedKey(a.enc, ekPub, a.enc.PublicKey().Bytes(), ekPub)
	env, err := protocol.EncodeChatMessage(contentKey, kssA, a.addr, b.addr, 1, text)
	if err != nil {
		t.Fatal(err)
	}
	chunks := protocol.SplitChunks(env, protocol.ChatDataChunkSize)
	if len(chunks) < 2 {
		t.Fatalf("need a multi-chunk message, got %d", len(chunks))
	}

	ctrA := uint32(0)
	if st, _ := chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatSendStartPlain(b.addr, uint16(len(env)))); st != protocol.ChatStatusOK {
		t.Fatalf("send-start st=%d", st)
	}
	d0, _ := protocol.BuildChatDataPlain(0, chunks[0])
	if st, _ := chatOp(t, svc, refA, ksA, &ctrA, d0); st != protocol.ChatStatusOK {
		t.Fatalf("data 0 st=%d", st)
	}
	// SEND_START for a different total length → fresh upload, chunk 0 NOT carried.
	st, body := chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatSendStartPlain(b.addr, uint16(len(env)+protocol.ChatDataChunkSize)))
	if st != protocol.ChatStatusOK {
		t.Fatalf("reset send-start st=%d", st)
	}
	if sendStartBitSet(body, 0) {
		t.Fatal("a different message (other length) must reset, not resume onto chunk 0")
	}
}
