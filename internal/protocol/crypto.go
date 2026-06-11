package protocol

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

const (
	KeySize   = 32 // AES-256
	NonceSize = 12 // GCM nonce
)

// DeriveKeys derives separate query and response AES-256 keys from a passphrase using HKDF.
func DeriveKeys(passphrase string) (queryKey, responseKey [KeySize]byte, err error) {
	master := sha256.Sum256([]byte(passphrase))

	qr := hkdf.New(sha256.New, master[:], nil, []byte("thefeed-query"))
	if _, err = io.ReadFull(qr, queryKey[:]); err != nil {
		return
	}

	rr := hkdf.New(sha256.New, master[:], nil, []byte("thefeed-response"))
	_, err = io.ReadFull(rr, responseKey[:])
	return
}

func newGCM(key [KeySize]byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Encrypt encrypts plaintext using AES-256-GCM. Returns nonce+ciphertext+tag.
func Encrypt(key [KeySize]byte, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt decrypts AES-256-GCM ciphertext (nonce+ciphertext+tag).
func Decrypt(key [KeySize]byte, ciphertext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	if len(ciphertext) < gcm.NonceSize()+gcm.Overhead() {
		return nil, fmt.Errorf("ciphertext too short: %d bytes", len(ciphertext))
	}

	nonce := ciphertext[:gcm.NonceSize()]
	return gcm.Open(nil, nonce, ciphertext[gcm.NonceSize():], nil)
}

// EncryptFixedNonce seals with an all-zero nonce and no transmitted nonce —
// ONLY for single-use keys (a key that never seals two different plaintexts),
// e.g. the chat content key, which is unique per (src,dst,seq). Output is
// ciphertext+tag, no nonce prefix.
func EncryptFixedNonce(key [KeySize]byte, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return gcm.Seal(nil, make([]byte, gcm.NonceSize()), plaintext, nil), nil
}

// DecryptFixedNonce reverses EncryptFixedNonce.
func DecryptFixedNonce(key [KeySize]byte, ct []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(ct) < gcm.Overhead() {
		return nil, fmt.Errorf("ciphertext too short: %d bytes", len(ct))
	}
	return gcm.Open(nil, make([]byte, gcm.NonceSize()), ct, nil)
}

// encryptQueryBlock encrypts an 8-byte query payload using a direct AES-256 block cipher.
// The payload is expanded to one AES block (16 bytes) with 8 trailing zero bytes before
// encryption. No nonce or auth tag needed: the 4 random bytes in the payload guarantee
// unique ciphertext per query. Result is always 16 bytes.
func encryptQueryBlock(key [KeySize]byte, payload []byte) ([]byte, error) {
	if len(payload) != QueryPayloadSize {
		return nil, fmt.Errorf("encryptQueryBlock: payload must be %d bytes, got %d", QueryPayloadSize, len(payload))
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	var buf [aes.BlockSize]byte
	copy(buf[:QueryPayloadSize], payload) // bytes 8-15 stay zero
	block.Encrypt(buf[:], buf[:])
	return buf[:], nil
}

// decryptQueryBlock decrypts a query ciphertext produced by encryptQueryBlock.
// Accepts ciphertext with optional random suffix bytes (≥ BlockSize); only the
// first BlockSize bytes are used. Verifies the last 8 bytes of plaintext are zero.
func decryptQueryBlock(key [KeySize]byte, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < aes.BlockSize {
		return nil, fmt.Errorf("decryptQueryBlock: need at least %d bytes, got %d", aes.BlockSize, len(ciphertext))
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	var buf [aes.BlockSize]byte
	block.Decrypt(buf[:], ciphertext[:aes.BlockSize]) // ignore suffix
	for i := QueryPayloadSize; i < aes.BlockSize; i++ {
		if buf[i] != 0 {
			return nil, fmt.Errorf("decryptQueryBlock: integrity check failed (wrong key?)")
		}
	}
	return buf[:QueryPayloadSize], nil
}
