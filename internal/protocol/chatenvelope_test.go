package protocol

import (
	"bytes"
	"testing"
)

// pairKeys derives the content key (both directions checked symmetric in
// chatcrypto_test) and a client↔server key for envelope tests.
func pairKeys(t *testing.T, sender, recip chatParty, seq uint32) (content, kss [KeySize]byte) {
	t.Helper()
	var err error
	content, err = ChatContentKey(sender.enc, recip.encPub, sender.addr, recip.addr, seq)
	if err != nil {
		t.Fatal(err)
	}
	ek, err := GenerateEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	kss, err = ChatServerSharedKey(sender.enc, ek.PublicKey().Bytes(), sender.encPub, ek.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return content, kss
}

func TestChatMessageRoundTrip(t *testing.T) {
	sender := newChatParty(t)
	recip := newChatParty(t)
	const text = "سلام — hello"
	const seq = 7

	content, kss := pairKeys(t, sender, recip, seq)
	raw, err := EncodeChatMessage(content, kss, sender.addr, recip.addr, seq, text)
	if err != nil {
		t.Fatal(err)
	}

	m, err := ParseChatMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.Seq != seq {
		t.Fatalf("seq = %d, want %d", m.Seq, seq)
	}
	if err := m.VerifyServerMAC(kss, sender.addr, recip.addr); err != nil {
		t.Fatalf("server mac: %v", err)
	}

	// Recipient derives the same content key from its own private key.
	rk, err := ChatContentKey(recip.enc, sender.encPub, sender.addr, recip.addr, seq)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Open(rk)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != text {
		t.Fatalf("text = %q, want %q", got, text)
	}
}

func TestChatMessageWrongRecipient(t *testing.T) {
	sender := newChatParty(t)
	recip := newChatParty(t)
	other := newChatParty(t)

	content, kss := pairKeys(t, sender, recip, 1)
	raw, err := EncodeChatMessage(content, kss, sender.addr, recip.addr, 1, "for recip only")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := ParseChatMessage(raw)

	// A third party (or the server) cannot derive the pair key.
	wk, err := ChatContentKey(other.enc, sender.encPub, sender.addr, recip.addr, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open(wk); err == nil {
		t.Fatal("wrong recipient opened the message")
	}
}

func TestChatMessageTampered(t *testing.T) {
	sender := newChatParty(t)
	recip := newChatParty(t)

	content, kss := pairKeys(t, sender, recip, 1)
	raw, err := EncodeChatMessage(content, kss, sender.addr, recip.addr, 1, "tamper me")
	if err != nil {
		t.Fatal(err)
	}

	// Flip a ciphertext byte: server MAC fails, and decryption fails.
	raw[10] ^= 0x01
	m, err := ParseChatMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.VerifyServerMAC(kss, sender.addr, recip.addr); err == nil {
		t.Fatal("tampered envelope passed server mac")
	}
	rk, _ := ChatContentKey(recip.enc, sender.encPub, sender.addr, recip.addr, 1)
	if _, err := m.Open(rk); err == nil {
		t.Fatal("tampered envelope decrypted")
	}
}

func TestChatMessageSeqMismatch(t *testing.T) {
	sender := newChatParty(t)
	recip := newChatParty(t)

	content, kss := pairKeys(t, sender, recip, 5)
	raw, err := EncodeChatMessage(content, kss, sender.addr, recip.addr, 5, "x")
	if err != nil {
		t.Fatal(err)
	}
	// Rewriting the outer seq must be caught (inner seq + key binding).
	raw[4] = 9
	m, err := ParseChatMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	rk, _ := ChatContentKey(recip.enc, sender.encPub, sender.addr, recip.addr, 9)
	if _, err := m.Open(rk); err == nil {
		t.Fatal("outer seq rewrite went unnoticed")
	}
}

func TestChatMessageCompression(t *testing.T) {
	sender := newChatParty(t)
	recip := newChatParty(t)
	const seq = 3
	// A long, compressible text (Persian repeated) should shrink the envelope
	// below raw text length + fixed overhead.
	text := ""
	for i := 0; i < 30; i++ {
		text += "سلام دوست من، حال شما چطور است؟ "
	}
	content, kss := pairKeys(t, sender, recip, seq)
	raw, err := EncodeChatMessage(content, kss, sender.addr, recip.addr, seq, text)
	if err != nil {
		t.Fatal(err)
	}
	overhead := 1 + 4 + 16 + ChatSrvMACSize + 1 // ver+seq+gcmtag+srvmac+cflag
	if len(raw) >= len(text)+overhead {
		t.Fatalf("compressible text not compressed: env=%d raw-text=%d", len(raw), len(text))
	}
	m, _ := ParseChatMessage(raw)
	if m.VerifyServerMAC(kss, sender.addr, recip.addr) != nil {
		t.Fatal("srvmac")
	}
	rk, _ := ChatContentKey(recip.enc, sender.encPub, sender.addr, recip.addr, seq)
	got, err := m.Open(rk)
	if err != nil || got != text {
		t.Fatalf("open: %v len=%d", err, len(got))
	}
}

func TestParseChatMessageGarbage(t *testing.T) {
	if _, err := ParseChatMessage([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error on short input")
	}
	bad := make([]byte, chatMsgMinLen)
	bad[0] = 0xFF
	if _, err := ParseChatMessage(bad); err == nil {
		t.Fatal("expected error on bad version")
	}
}

func TestRegisterEnvelopeRoundTrip(t *testing.T) {
	u := newChatParty(t)
	raw, err := EncodeRegisterEnvelope(u.id, u.encPub, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != RegisterEnvelopeLen {
		t.Fatalf("len = %d, want %d", len(raw), RegisterEnvelopeLen)
	}
	env, err := ParseRegisterEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if env.Address() != u.addr {
		t.Fatal("register address mismatch")
	}
	if !bytes.Equal(env.EncPub, u.encPub) {
		t.Fatal("enc pub mismatch")
	}
	if env.Timestamp != 1700000000 {
		t.Fatalf("timestamp = %d", env.Timestamp)
	}
	if _, err := env.EncKey(); err != nil {
		t.Fatalf("enc key: %v", err)
	}
}

func TestRegisterEnvelopeTampered(t *testing.T) {
	u := newChatParty(t)
	raw, err := EncodeRegisterEnvelope(u.id, u.encPub, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	raw[1+32] ^= 0x01 // flip a byte in the enc-pub field
	env, err := ParseRegisterEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Verify(); err == nil {
		t.Fatal("tampered register verified")
	}
}

func TestSplitChunksAndReassemble(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefgh"), 13) // 104 bytes
	const chunkSize = 7
	chunks := SplitChunks(data, chunkSize)
	want := (len(data) + chunkSize - 1) / chunkSize
	if len(chunks) != want {
		t.Fatalf("chunks = %d, want %d", len(chunks), want)
	}

	r := NewChunkReassembler(len(chunks))
	// Add out of order: odds first, then evens.
	for i := 1; i < len(chunks); i += 2 {
		r.Add(i, chunks[i])
	}
	if r.Complete() {
		t.Fatal("complete too early")
	}
	for i := 0; i < len(chunks); i += 2 {
		r.Add(i, chunks[i])
	}
	if !r.Complete() {
		t.Fatal("not complete after all chunks")
	}
	if !bytes.Equal(r.Assemble(), data) {
		t.Fatal("reassembled data mismatch")
	}
}

func TestChunkReassemblerBitmap(t *testing.T) {
	r := NewChunkReassembler(10)
	r.Add(0, []byte("a"))
	r.Add(9, []byte("b"))
	bm := r.Bitmap()
	if len(bm) != 2 {
		t.Fatalf("bitmap len = %d, want 2", len(bm))
	}
	if bm[0] != 0x80 { // bit 7 of byte 0 = chunk 0
		t.Fatalf("byte0 = %08b, want 10000000", bm[0])
	}
	if bm[1] != 0x40 { // bit (7-9%8=6) of byte 1 = chunk 9
		t.Fatalf("byte1 = %08b, want 01000000", bm[1])
	}
	if r.Add(10, []byte("x")) {
		t.Fatal("out-of-range index accepted")
	}
}

func TestSplitChunksEmpty(t *testing.T) {
	chunks := SplitChunks(nil, 8)
	if len(chunks) != 1 || len(chunks[0]) != 0 {
		t.Fatalf("empty split = %v, want one empty chunk", chunks)
	}
}

func TestDecompressMessagesLimited(t *testing.T) {
	// A deflate "bomb": a tiny stream that inflates far past the cap.
	bomb := CompressMessages(make([]byte, 1<<20)) // 1 MiB of zeros → a few bytes
	if bomb[0] != compressionDeflate {
		t.Fatalf("expected deflate, got 0x%02x", bomb[0])
	}
	if _, err := DecompressMessagesLimited(bomb, ChatMaxPlaintextBytes); err == nil {
		t.Fatal("over-cap decompression accepted — zip bomb not rejected")
	}
	// Unbounded still works (the feed path relies on this).
	out, err := DecompressMessagesLimited(bomb, 0)
	if err != nil || len(out) != 1<<20 {
		t.Fatalf("unbounded decompress: len=%d err=%v", len(out), err)
	}
	// A normal small message round-trips under the cap.
	msg := []byte("the quick brown fox jumps over the lazy dog")
	got, err := DecompressMessagesLimited(CompressMessages(msg), ChatMaxPlaintextBytes)
	if err != nil || string(got) != string(msg) {
		t.Fatalf("round-trip: got %q err=%v", got, err)
	}
	// Exactly-at-cap is allowed; one byte over is not (compressionNone path).
	atCap := append([]byte{compressionNone}, make([]byte, ChatMaxPlaintextBytes)...)
	if _, err := DecompressMessagesLimited(atCap, ChatMaxPlaintextBytes); err != nil {
		t.Fatalf("at-cap rejected: %v", err)
	}
	overCap := append([]byte{compressionNone}, make([]byte, ChatMaxPlaintextBytes+1)...)
	if _, err := DecompressMessagesLimited(overCap, ChatMaxPlaintextBytes); err == nil {
		t.Fatal("over-cap raw body accepted")
	}
}
