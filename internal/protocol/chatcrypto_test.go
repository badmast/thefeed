package protocol

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"testing"
)

func TestChatPeerHandle(t *testing.T) {
	var a [AddressSize]byte
	copy(a[:], []byte{1, 2, 3, 4, 5, 6})
	h := ChatPeerHandle(a)
	if len(h) != ChatPeerHandleSize || h != [ChatPeerHandleSize]byte{1, 2, 3, 4} {
		t.Fatalf("handle = %x", h)
	}
}

// chatParty is a test helper holding one user's derived keys.
type chatParty struct {
	id     ed25519.PrivateKey
	enc    *ecdh.PrivateKey
	encPub []byte
	addr   [AddressSize]byte
}

func newChatParty(t *testing.T) chatParty {
	t.Helper()
	seed, err := GenerateSeed()
	if err != nil {
		t.Fatal(err)
	}
	id, err := DeriveIdentityKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := DeriveEncryptionKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	return chatParty{
		id:     id,
		enc:    enc,
		encPub: enc.PublicKey().Bytes(),
		addr:   Address(id.Public().(ed25519.PublicKey)),
	}
}

func TestDeriveKeysDeterministic(t *testing.T) {
	seed := []byte("0123456789abcdef0123456789abcdef")

	id1, err := DeriveIdentityKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := DeriveIdentityKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(id1, id2) {
		t.Fatal("identity key not deterministic")
	}

	enc1, err := DeriveEncryptionKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	enc2, err := DeriveEncryptionKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(enc1.Bytes(), enc2.Bytes()) {
		t.Fatal("encryption key not deterministic")
	}

	// Identity and encryption keys must be independent.
	if bytes.Equal(id1.Seed(), enc1.Bytes()) {
		t.Fatal("identity and encryption keys derived identically")
	}
}

func TestDeriveKeysDifferentSeeds(t *testing.T) {
	a, err := DeriveIdentityKey([]byte("seed-a-aaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := DeriveIdentityKey([]byte("seed-b-bbbbbbbbbbbbbbbbbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("different seeds produced the same identity key")
	}
}

func TestDeriveEmptySeed(t *testing.T) {
	if _, err := DeriveIdentityKey(nil); err == nil {
		t.Fatal("expected error for empty seed")
	}
	if _, err := DeriveEncryptionKey(nil); err == nil {
		t.Fatal("expected error for empty seed")
	}
}

func TestAddressStable(t *testing.T) {
	u := newChatParty(t)
	pub := u.id.Public().(ed25519.PublicKey)
	if Address(pub) != u.addr {
		t.Fatal("address not stable")
	}
}

func TestChatContentKeySymmetric(t *testing.T) {
	a := newChatParty(t)
	b := newChatParty(t)

	k1, err := ChatContentKey(a.enc, b.encPub, a.addr, b.addr, 5)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := ChatContentKey(b.enc, a.encPub, a.addr, b.addr, 5)
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatal("content key not symmetric")
	}

	// Different seq → different key.
	k3, err := ChatContentKey(a.enc, b.encPub, a.addr, b.addr, 6)
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k3 {
		t.Fatal("content key not bound to seq")
	}

	// Swapped direction → different key.
	k4, err := ChatContentKey(a.enc, b.encPub, b.addr, a.addr, 5)
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k4 {
		t.Fatal("content key not bound to direction")
	}
}

func TestChatReceiptMAC(t *testing.T) {
	a := newChatParty(t) // message sender
	b := newChatParty(t) // recipient, signs the receipt

	// Both ends derive the same pair receipt key from their static ECDH.
	ka, err := ChatReceiptKey(a.enc, b.encPub)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := ChatReceiptKey(b.enc, a.encPub)
	if err != nil {
		t.Fatal(err)
	}
	if ka != kb {
		t.Fatal("receipt key not symmetric")
	}

	// B signs delivery of a→b up to seq 5; A recomputes the same tuple.
	rcpt := ChatReceiptMAC(kb, a.addr, b.addr, 5)
	if rcpt != ChatReceiptMAC(ka, a.addr, b.addr, 5) {
		t.Fatal("receipt does not verify across the two ends")
	}
	// Bound to seq: a higher watermark can't reuse a lower one's proof.
	if ChatReceiptMAC(ka, a.addr, b.addr, 6) == rcpt {
		t.Fatal("receipt not bound to seq")
	}
	// Bound to direction: can't be reflected onto the reverse pair.
	if ChatReceiptMAC(ka, b.addr, a.addr, 5) == rcpt {
		t.Fatal("receipt not bound to direction")
	}
	// Unforgeable by a third party (no pair key): this is what stops a malicious
	// server from fabricating a ✓✓.
	c := newChatParty(t)
	kc, err := ChatReceiptKey(c.enc, a.encPub)
	if err != nil {
		t.Fatal(err)
	}
	if ChatReceiptMAC(kc, a.addr, b.addr, 5) == rcpt {
		t.Fatal("receipt forgeable without the pair key")
	}
}

func TestChatServerSharedKeySymmetric(t *testing.T) {
	client := newChatParty(t)
	ek, err := GenerateEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	ekPub := ek.PublicKey().Bytes()

	k1, err := ChatServerSharedKey(client.enc, ekPub, client.encPub, ekPub)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := ChatServerSharedKey(ek, client.encPub, client.encPub, ekPub)
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatal("server shared key not symmetric")
	}
}

func TestChatSessionKeysSymmetric(t *testing.T) {
	eph, err := GenerateEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	ek, err := GenerateEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	ephPub := eph.PublicKey().Bytes()
	ekPub := ek.PublicKey().Bytes()

	r1, m1, err := ChatSessionKeys(eph, ekPub, ephPub, ekPub)
	if err != nil {
		t.Fatal(err)
	}
	r2, m2, err := ChatSessionKeys(ek, ephPub, ephPub, ekPub)
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r2 || m1 != m2 {
		t.Fatal("session keys not symmetric")
	}
	if r1 == m1 {
		t.Fatal("routing and mac keys identical")
	}
}

func TestChatMACs(t *testing.T) {
	var kss, mk [KeySize]byte
	copy(kss[:], bytes.Repeat([]byte{7}, KeySize))
	copy(mk[:], bytes.Repeat([]byte{9}, KeySize))
	var src, dst [AddressSize]byte
	src[0], dst[0] = 1, 2

	m1 := ChatServerMAC(kss, src, dst, 3, []byte("ct"))
	m2 := ChatServerMAC(kss, src, dst, 3, []byte("ct"))
	if m1 != m2 {
		t.Fatal("server mac not deterministic")
	}
	if m1 == ChatServerMAC(kss, src, dst, 4, []byte("ct")) {
		t.Fatal("server mac ignores seq")
	}
	if m1 == ChatServerMAC(kss, src, dst, 3, []byte("cx")) {
		t.Fatal("server mac ignores ciphertext")
	}

	c1 := ChatChunkMAC(mk, 42, 1, []byte("chunk"))
	if c1 != ChatChunkMAC(mk, 42, 1, []byte("chunk")) {
		t.Fatal("chunk mac not deterministic")
	}
	if c1 == ChatChunkMAC(mk, 42, 2, []byte("chunk")) {
		t.Fatal("chunk mac ignores index")
	}
	if c1 == ChatChunkMAC(mk, 43, 1, []byte("chunk")) {
		t.Fatal("chunk mac ignores session")
	}
}
