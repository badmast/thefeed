package protocol

import (
	"bytes"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	var ks [KeySize]byte
	ks[0] = 7
	sel := []byte{0xAA, 0xBB, 0xCC}
	pt := []byte("hello chat body")
	sealed := SealChat(ks, sel, 5, pt)
	if len(sealed) != len(pt)+ChatSealTagSize {
		t.Fatalf("sealed len = %d", len(sealed))
	}
	got, err := OpenChat(ks, sel, 5, sealed)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("open: %v %q", err, got)
	}
	// Deterministic for the same inputs.
	if !bytes.Equal(sealed, SealChat(ks, sel, 5, pt)) {
		t.Fatal("seal not deterministic")
	}
	// Wrong counter, wrong selector, tampered byte, wrong key all reject.
	if _, err := OpenChat(ks, sel, 6, sealed); err == nil {
		t.Fatal("wrong counter accepted")
	}
	if _, err := OpenChat(ks, []byte{1, 2, 3}, 5, sealed); err == nil {
		t.Fatal("wrong selector accepted")
	}
	bad := append([]byte(nil), sealed...)
	bad[0] ^= 1
	if _, err := OpenChat(ks, sel, 5, bad); err == nil {
		t.Fatal("tamper accepted")
	}
	var ks2 [KeySize]byte
	ks2[0] = 9
	if _, err := OpenChat(ks2, sel, 5, sealed); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestSessionKeyAgreement(t *testing.T) {
	eph, _ := GenerateEphemeralKey()
	ek, _ := GenerateEphemeralKey()
	var qk [KeySize]byte
	qk[1] = 3
	cli, err := ChatSessionKey(eph, ek.PublicKey().Bytes(), qk)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ChatSessionKey(ek, eph.PublicKey().Bytes(), qk)
	if err != nil {
		t.Fatal(err)
	}
	if cli != srv {
		t.Fatal("client/server session keys differ")
	}
	// A different passphrase yields a different session key (passphrase gate).
	var qk2 [KeySize]byte
	qk2[1] = 4
	other, _ := ChatSessionKey(ek, eph.PublicKey().Bytes(), qk2)
	if other == srv {
		t.Fatal("passphrase not mixed into the session key")
	}
}
