package protocol

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// Chat identity and key-derivation primitives. A user holds one random seed
// and derives a stable ed25519 identity key plus a separate x25519 encryption
// key from it. The address is a truncated hash of the identity public key, so
// anyone holding the public key can confirm it matches a shared address
// (defeats key substitution). Senders register on every server they send
// through, so message envelopes carry no keys: content authenticity comes
// from the static-static ECDH content key (only the two parties can compute
// it) and server-side sender auth from an HMAC under the client↔server key.
// All key material is pure-Go (crypto/ecdh, crypto/ed25519) so it builds
// under gomobile.

const (
	// SeedSize is the length of a chat identity seed in bytes (256-bit, a
	// 24-word recovery mnemonic).
	SeedSize = 32
	// AddressSize is the length of a chat address in bytes.
	AddressSize = 12
	// X25519KeySize is the length of an x25519 public or private key.
	X25519KeySize = 32
	// ChatSrvMACSize is the truncated HMAC length authenticating a message
	// envelope to the server.
	ChatSrvMACSize = 8
	// ChatChunkMACSize is the truncated HMAC length authenticating one
	// upload chunk to the session.
	ChatChunkMACSize = 4
	// ChatReceiptMACSize is the truncated HMAC length of an end-to-end delivery
	// receipt. 6 bytes keeps the ACK and SEND_STATUS response in one cell, and
	// the MAC is verified offline by the sender (no online forgery oracle), so a
	// malicious server still cannot fabricate a ✓✓ — only withhold a real one.
	ChatReceiptMACSize = 6
)

// HKDF domain-separation labels for chat key derivation.
const (
	chatIdentityInfo = "thefeed-chat-identity-v1"
	chatEncInfo      = "thefeed-chat-encryption-v1"
	chatContentInfo  = "thefeed-chat-content-v1"
	chatServerInfo   = "thefeed-chat-server-v1"
	chatRoutingInfo  = "thefeed-chat-routing-v1"
	chatChunkMACInfo = "thefeed-chat-chunkmac-v1"
	chatSrvMACInfo   = "thefeed-chat-srvmac-v1"
	chatReceiptInfo  = "thefeed-chat-receipt-v1"    // HKDF label for the pair receipt key
	chatReceiptMAC   = "thefeed-chat-receiptmac-v1" // domain prefix inside the receipt MAC
)

// GenerateSeed returns a new random chat identity seed.
func GenerateSeed() ([]byte, error) {
	s := make([]byte, SeedSize)
	if _, err := io.ReadFull(rand.Reader, s); err != nil {
		return nil, fmt.Errorf("chat seed: %w", err)
	}
	return s, nil
}

// DeriveIdentityKey derives the ed25519 identity key from a seed.
func DeriveIdentityKey(seed []byte) (ed25519.PrivateKey, error) {
	if len(seed) == 0 {
		return nil, fmt.Errorf("chat: empty seed")
	}
	var sub [ed25519.SeedSize]byte
	r := hkdf.New(sha256.New, seed, nil, []byte(chatIdentityInfo))
	if _, err := io.ReadFull(r, sub[:]); err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(sub[:]), nil
}

// DeriveEncryptionKey derives the x25519 encryption key from a seed.
func DeriveEncryptionKey(seed []byte) (*ecdh.PrivateKey, error) {
	if len(seed) == 0 {
		return nil, fmt.Errorf("chat: empty seed")
	}
	var sub [X25519KeySize]byte
	r := hkdf.New(sha256.New, seed, nil, []byte(chatEncInfo))
	if _, err := io.ReadFull(r, sub[:]); err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPrivateKey(sub[:])
}

// Address returns the chat address for an ed25519 identity public key: the
// first AddressSize bytes of its SHA-256 hash.
func Address(identityPub ed25519.PublicKey) [AddressSize]byte {
	sum := sha256.Sum256(identityPub)
	var a [AddressSize]byte
	copy(a[:], sum[:])
	return a
}

// ecdhDerive performs x25519 ECDH and reads one AES-256 key from HKDF with
// the given info bytes.
func ecdhDerive(priv *ecdh.PrivateKey, peerPub, info []byte) ([KeySize]byte, error) {
	var key [KeySize]byte
	pub, err := ecdh.X25519().NewPublicKey(peerPub)
	if err != nil {
		return key, fmt.Errorf("chat: bad peer key: %w", err)
	}
	secret, err := priv.ECDH(pub)
	if err != nil {
		return key, fmt.Errorf("chat: ecdh: %w", err)
	}
	r := hkdf.New(sha256.New, secret, nil, info)
	_, err = io.ReadFull(r, key[:])
	return key, err
}

func keyInfo(label string, parts ...[]byte) []byte {
	n := len(label)
	for _, p := range parts {
		n += len(p)
	}
	b := make([]byte, 0, n)
	b = append(b, label...)
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

// ChatContentKey derives the AES-256 key sealing one message body. Both ends
// compute the same key: the sender from its enc private key and the
// recipient's published enc key, the recipient from its enc private key and
// the sender's published enc key. The key is bound to the message's
// (src, dst, seq) so it is valid for exactly one message slot.
func ChatContentKey(own *ecdh.PrivateKey, peerEncPub []byte, src, dst [AddressSize]byte, seq uint32) ([KeySize]byte, error) {
	var seqB [4]byte
	binary.BigEndian.PutUint32(seqB[:], seq)
	return ecdhDerive(own, peerEncPub, keyInfo(chatContentInfo, src[:], dst[:], seqB[:]))
}

// ChatServerSharedKey derives the long-lived client↔server key (used for the
// envelope server MAC). The client calls it with its enc private key and the
// server's published ek; the server with its ek private key and the client's
// registered enc key. clientEncPub and serverEkPub fix the info ordering so
// both sides derive identical bytes.
func ChatServerSharedKey(priv *ecdh.PrivateKey, peerPub, clientEncPub, serverEkPub []byte) ([KeySize]byte, error) {
	return ecdhDerive(priv, peerPub, keyInfo(chatServerInfo, clientEncPub, serverEkPub))
}

// ChatSessionKeys derives the routing-encryption key and the chunk-MAC key
// for one upload session from a fresh ephemeral↔ek ECDH. The client calls it
// with the ephemeral private key and the server ek; the server with the ek
// private key and the ephemeral public key from INIT.
func ChatSessionKeys(priv *ecdh.PrivateKey, peerPub, ephPub, serverEkPub []byte) (routing, mac [KeySize]byte, err error) {
	routing, err = ecdhDerive(priv, peerPub, keyInfo(chatRoutingInfo, ephPub, serverEkPub))
	if err != nil {
		return
	}
	mac, err = ecdhDerive(priv, peerPub, keyInfo(chatChunkMACInfo, ephPub, serverEkPub))
	return
}

// ChatServerMAC authenticates a message envelope to the server: only the
// registered holder of the sender enc key can produce it, and it binds the
// routing pair, the sequence, and the exact ciphertext.
func ChatServerMAC(kss [KeySize]byte, src, dst [AddressSize]byte, seq uint32, ciphertext []byte) [ChatSrvMACSize]byte {
	h := hmac.New(sha256.New, kss[:])
	h.Write([]byte(chatSrvMACInfo))
	h.Write(src[:])
	h.Write(dst[:])
	var seqB [4]byte
	binary.BigEndian.PutUint32(seqB[:], seq)
	h.Write(seqB[:])
	ctSum := sha256.Sum256(ciphertext)
	h.Write(ctSum[:])
	var out [ChatSrvMACSize]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ChatReceiptKey derives the pair key that authenticates delivery receipts.
// Like ChatContentKey it comes from the two peers' static ECDH — so only the
// two ends can compute it, never the relaying server — but it is independent of
// any single message (no seq), so one key covers the whole conversation. The
// recipient signs its ACK watermark with it; the sender verifies, turning ✓✓
// from a server claim into a fact the server cannot forge.
func ChatReceiptKey(own *ecdh.PrivateKey, peerEncPub []byte) ([KeySize]byte, error) {
	return ecdhDerive(own, peerEncPub, keyInfo(chatReceiptInfo))
}

// ChatReceiptMAC is the recipient's proof that it acknowledged sender→recipient
// messages up to upToSeq. The (sender, recipient) order is fixed by the message
// direction (originator first), so both ends bind the same tuple and a receipt
// can't be reflected onto the reverse direction.
func ChatReceiptMAC(receiptKey [KeySize]byte, sender, recipient [AddressSize]byte, upToSeq uint32) [ChatReceiptMACSize]byte {
	h := hmac.New(sha256.New, receiptKey[:])
	h.Write([]byte(chatReceiptMAC))
	h.Write(sender[:])
	h.Write(recipient[:])
	var seqB [4]byte
	binary.BigEndian.PutUint32(seqB[:], upToSeq)
	h.Write(seqB[:])
	var out [ChatReceiptMACSize]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ChatChunkMAC authenticates one upload chunk to its session so an attacker
// who learned the session id still cannot poison the upload.
func ChatChunkMAC(macKey [KeySize]byte, sessionID uint32, index uint8, chunk []byte) [ChatChunkMACSize]byte {
	h := hmac.New(sha256.New, macKey[:])
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[:4], sessionID)
	hdr[4] = index
	h.Write(hdr[:])
	h.Write(chunk)
	var out [ChatChunkMACSize]byte
	copy(out[:], h.Sum(nil))
	return out
}

// GenerateEphemeralKey returns a fresh x25519 key for one upload session.
func GenerateEphemeralKey() (*ecdh.PrivateKey, error) {
	return ecdh.X25519().GenerateKey(rand.Reader)
}

// ChatPeerHandleSize is the short reference to a peer used in ACK/SENDSTATUS
// instead of the full 12-byte address.
const ChatPeerHandleSize = 4

// ChatPeerHandle is the first bytes of a peer address; the server disambiguates
// it within the caller account's known pairs. A collision is 2^-32 with a
// handful of contacts and only ever names one of YOUR own contacts.
func ChatPeerHandle(addr [AddressSize]byte) [ChatPeerHandleSize]byte {
	var h [ChatPeerHandleSize]byte
	copy(h[:], addr[:ChatPeerHandleSize])
	return h
}

// ChatAccountProofSize is the truncated HMAC proving account control in an
// auth handshake.
const ChatAccountProofSize = 8

const chatAccountProofInfo = "thefeed-chat-acct-proof-v1"

// ChatAccountProof binds an auth handshake to the account: HMAC under kss (the
// client↔server shared key, computable only by the account holder and the
// server) over the ephemeral key, address, timestamp and chat domain. The
// domain binding stops cross-server replay.
func ChatAccountProof(kss [KeySize]byte, ephPub []byte, addr [AddressSize]byte, ts uint32, domain string) [ChatAccountProofSize]byte {
	h := hmac.New(sha256.New, kss[:])
	h.Write([]byte(chatAccountProofInfo))
	h.Write(ephPub)
	h.Write(addr[:])
	var tb [4]byte
	binary.BigEndian.PutUint32(tb[:], ts)
	h.Write(tb[:])
	h.Write([]byte(strings.ToLower(strings.TrimSuffix(domain, "."))))
	var out [ChatAccountProofSize]byte
	copy(out[:], h.Sum(nil))
	return out
}
