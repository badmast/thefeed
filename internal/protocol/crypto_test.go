package protocol

import (
	"bytes"
	"testing"
)

func TestEncryptFixedNonceRoundTrip(t *testing.T) {
	var k [KeySize]byte
	k[2] = 1
	pt := []byte("single-use content")
	ct, err := EncryptFixedNonce(k, pt)
	if err != nil {
		t.Fatal(err)
	}
	if len(ct) != len(pt)+16 { // no transmitted nonce, full GCM tag
		t.Fatalf("ct len = %d", len(ct))
	}
	got, err := DecryptFixedNonce(k, ct)
	if err != nil || string(got) != string(pt) {
		t.Fatalf("decrypt: %v %q", err, got)
	}
	bad := append([]byte(nil), ct...)
	bad[0] ^= 1
	if _, err := DecryptFixedNonce(k, bad); err == nil {
		t.Fatal("tamper accepted")
	}
}

func TestDeriveKeys(t *testing.T) {
	qk1, rk1, err := DeriveKeys("test-passphrase")
	if err != nil {
		t.Fatalf("DeriveKeys: %v", err)
	}
	qk2, rk2, err := DeriveKeys("test-passphrase")
	if err != nil {
		t.Fatalf("DeriveKeys: %v", err)
	}
	if qk1 != qk2 || rk1 != rk2 {
		t.Error("same passphrase should produce same keys")
	}
	qk3, rk3, err := DeriveKeys("different-passphrase")
	if err != nil {
		t.Fatalf("DeriveKeys: %v", err)
	}
	if qk1 == qk3 || rk1 == rk3 {
		t.Error("different passphrase should produce different keys")
	}
	if qk1 == rk1 {
		t.Error("query and response keys should differ")
	}
}

func TestEncryptDecrypt(t *testing.T) {
	key := [KeySize]byte{}
	copy(key[:], "test-key-32-bytes-long-xxxxxxxx")
	plaintext := []byte("Hello, World!")
	ciphertext, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(ciphertext, plaintext) {
		t.Error("ciphertext should differ from plaintext")
	}
	decrypted, err := Decrypt(key, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("decrypted: got %q, want %q", decrypted, plaintext)
	}
}

func TestDecryptWrongKey(t *testing.T) {
	key1 := [KeySize]byte{}
	key2 := [KeySize]byte{}
	copy(key1[:], "key-one-32-bytes-long-xxxxxxxxx")
	copy(key2[:], "key-two-32-bytes-long-xxxxxxxxx")
	ciphertext, _ := Encrypt(key1, []byte("secret"))
	_, err := Decrypt(key2, ciphertext)
	if err == nil {
		t.Error("expected error when decrypting with wrong key")
	}
}

func TestDecryptTooShort(t *testing.T) {
	key := [KeySize]byte{}
	_, err := Decrypt(key, []byte{0x01, 0x02})
	if err == nil {
		t.Error("expected error for short ciphertext")
	}
}

func TestEncryptProducesDifferentCiphertexts(t *testing.T) {
	key := [KeySize]byte{}
	copy(key[:], "test-key-32-bytes-long-xxxxxxxx")
	ct1, _ := Encrypt(key, []byte("same data"))
	ct2, _ := Encrypt(key, []byte("same data"))
	if bytes.Equal(ct1, ct2) {
		t.Error("two encryptions should produce different ciphertexts")
	}
}
