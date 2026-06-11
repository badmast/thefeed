package protocol

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"encoding/binary"
	"fmt"
)

// Chat message and registration envelopes. Senders are registered on the
// recipient's server, so the message envelope carries no public keys and no
// signature: the body is sealed with the static-static content key (AEAD
// success authenticates the sender to the recipient — only the pair can
// compute the key), and a truncated HMAC under the client↔server shared key
// authenticates the sender to the server. Byte layouts are canonical and
// must match byte-for-byte on both sides.

const (
	// ChatMessageVersion is the wire version of message envelopes.
	ChatMessageVersion = 2
	// ChatRegisterVersion is the wire version of registration records.
	ChatRegisterVersion = 1

	// chatRegSignContext domain-separates registration signatures.
	chatRegSignContext = "thefeed-chat-register-v1"

	// ChatMaxPlaintextBytes caps a decompressed message body. Real messages are
	// well under MaxMsgBytes; the cap only bounds memory if a hostile peer sends
	// a deflate bomb (a tiny ciphertext that inflates ~1000x). 64 KiB is far
	// above any legitimate text and far below a memory-DoS.
	ChatMaxPlaintextBytes = 64 * 1024
)

// ChatMessage is a parsed message envelope.
//
// Wire layout:
//
//	ver(1) msg_seq(4 BE) ciphertext(N) srvmac(8)
//
// ciphertext seals the inner payload with the single-use content key
// (AES-256-GCM, FIXED zero nonce — none on the wire — safe because the content
// key is unique per (src,dst,seq)). Inner layout:
//
//	cflag(1) body     // body = deflate(text) or raw text; cflag says which
//
// The sender address is NOT carried: the recipient derives the content key from
// the inbox entry's src, so a wrong src yields a wrong key and AEAD fails —
// misattribution is impossible without an inner copy.
type ChatMessage struct {
	Seq        uint32
	Ciphertext []byte
	SrvMAC     [ChatSrvMACSize]byte
}

// chatMsgMinLen: ver + seq + smallest sealed inner (GCM 16-byte tag over a
// possibly-empty body, fixed nonce so none on the wire) + srvmac.
const chatMsgMinLen = 1 + 4 + 16 + ChatSrvMACSize

// EncodeChatMessage seals text into a message envelope. The text is
// deflate-compressed (store-raw if not smaller) before encryption. contentKey
// is the pair key from ChatContentKey; kss the client↔server key from
// ChatServerSharedKey.
func EncodeChatMessage(contentKey, kss [KeySize]byte, src, dst [AddressSize]byte, seq uint32, text string) ([]byte, error) {
	inner := CompressMessages([]byte(text)) // 1-byte flag + deflate|raw
	ct, err := EncryptFixedNonce(contentKey, inner)
	if err != nil {
		return nil, err
	}
	mac := ChatServerMAC(kss, src, dst, seq, ct)

	out := make([]byte, 0, 1+4+len(ct)+ChatSrvMACSize)
	out = append(out, ChatMessageVersion)
	out = appendUint32(out, seq)
	out = append(out, ct...)
	out = append(out, mac[:]...)
	return out, nil
}

// ParseChatMessage parses a message envelope. It never panics; any
// malformation returns an error.
func ParseChatMessage(data []byte) (*ChatMessage, error) {
	if len(data) < chatMsgMinLen {
		return nil, fmt.Errorf("chat: message envelope too short")
	}
	if data[0] != ChatMessageVersion {
		return nil, fmt.Errorf("chat: unsupported envelope version %d", data[0])
	}
	m := &ChatMessage{Seq: binary.BigEndian.Uint32(data[1:])}
	macStart := len(data) - ChatSrvMACSize
	m.Ciphertext = append([]byte(nil), data[5:macStart]...)
	copy(m.SrvMAC[:], data[macStart:])
	return m, nil
}

// VerifyServerMAC checks the sender-to-server MAC. The server calls this at
// FIN with the session's routing pair and the sender's registered enc key.
func (m *ChatMessage) VerifyServerMAC(kss [KeySize]byte, src, dst [AddressSize]byte) error {
	want := ChatServerMAC(kss, src, dst, m.Seq, m.Ciphertext)
	if !hmac.Equal(want[:], m.SrvMAC[:]) {
		return fmt.Errorf("chat: server mac invalid")
	}
	return nil
}

// Open decrypts and decompresses the body with the pair content key. AEAD
// success itself authenticates the sender (only the pair can compute
// contentKey, which is bound to src,dst,seq), so no inner address check is
// needed.
func (m *ChatMessage) Open(contentKey [KeySize]byte) (string, error) {
	inner, err := DecryptFixedNonce(contentKey, m.Ciphertext)
	if err != nil {
		return "", fmt.Errorf("chat: open message: %w", err)
	}
	body, err := DecompressMessagesLimited(inner, ChatMaxPlaintextBytes)
	if err != nil {
		return "", fmt.Errorf("chat: decompress: %w", err)
	}
	return string(body), nil
}

// RegisterEnvelope is a parsed key-registration record. Wire layout:
//
//	ver(1) identity_pub(32) enc_pub(32) timestamp(4 BE) sig(64)
type RegisterEnvelope struct {
	IdentityPub ed25519.PublicKey
	EncPub      []byte
	Timestamp   uint32
	Signature   []byte
	signed      []byte
}

// RegisterEnvelopeLen is the fixed registration-record length.
const RegisterEnvelopeLen = 1 + ed25519.PublicKeySize + X25519KeySize + 4 + ed25519.SignatureSize

// EncodeRegisterEnvelope builds a registration record binding an x25519
// encryption key to an identity, signed by the identity key. timestamp is
// unix seconds (newest record wins on re-registration).
func EncodeRegisterEnvelope(identity ed25519.PrivateKey, encPub []byte, timestamp uint32) ([]byte, error) {
	if len(identity) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("chat: bad identity key size %d", len(identity))
	}
	if len(encPub) != X25519KeySize {
		return nil, fmt.Errorf("chat: bad enc key size %d", len(encPub))
	}
	idPub, ok := identity.Public().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("chat: bad identity public key")
	}
	body := make([]byte, 0, 1+ed25519.PublicKeySize+X25519KeySize+4)
	body = append(body, ChatRegisterVersion)
	body = append(body, idPub...)
	body = append(body, encPub...)
	body = appendUint32(body, timestamp)

	sig := ed25519.Sign(identity, signedMessage(chatRegSignContext, body))
	return append(body, sig...), nil
}

// ParseRegisterEnvelope parses a registration record. It never panics.
func ParseRegisterEnvelope(data []byte) (*RegisterEnvelope, error) {
	if len(data) != RegisterEnvelopeLen {
		return nil, fmt.Errorf("chat: register envelope wrong length")
	}
	if data[0] != ChatRegisterVersion {
		return nil, fmt.Errorf("chat: unsupported register version %d", data[0])
	}
	sigStart := len(data) - ed25519.SignatureSize
	off := 1
	e := &RegisterEnvelope{}
	e.IdentityPub = append(ed25519.PublicKey(nil), data[off:off+ed25519.PublicKeySize]...)
	off += ed25519.PublicKeySize
	e.EncPub = append([]byte(nil), data[off:off+X25519KeySize]...)
	off += X25519KeySize
	e.Timestamp = binary.BigEndian.Uint32(data[off:])
	e.Signature = append([]byte(nil), data[sigStart:]...)
	e.signed = append([]byte(nil), data[:sigStart]...)
	return e, nil
}

// Verify checks the identity signature. The caller must separately confirm
// Address(IdentityPub) equals the expected address.
func (e *RegisterEnvelope) Verify() error {
	if len(e.IdentityPub) != ed25519.PublicKeySize {
		return fmt.Errorf("chat: bad identity key size")
	}
	if !ed25519.Verify(e.IdentityPub, signedMessage(chatRegSignContext, e.signed), e.Signature) {
		return fmt.Errorf("chat: register signature invalid")
	}
	return nil
}

// Address returns the chat address bound to this registration.
func (e *RegisterEnvelope) Address() [AddressSize]byte {
	return Address(e.IdentityPub)
}

// EncKey returns the registered x25519 encryption key, validated.
func (e *RegisterEnvelope) EncKey() (*ecdh.PublicKey, error) {
	return ecdh.X25519().NewPublicKey(e.EncPub)
}

// SplitChunks splits data into fixed-size chunks; the final chunk may be
// shorter. Returns one empty chunk for empty data so a session always has at
// least one block.
func SplitChunks(data []byte, chunkSize int) [][]byte {
	if chunkSize <= 0 {
		return nil
	}
	if len(data) == 0 {
		return [][]byte{{}}
	}
	chunks := make([][]byte, 0, (len(data)+chunkSize-1)/chunkSize)
	for i := 0; i < len(data); i += chunkSize {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}
		chunks = append(chunks, data[i:end])
	}
	return chunks
}

// ChunkReassembler reassembles a fixed number of chunks arriving out of
// order, tracking which have been received. The bitmap is returned to the
// client so it can retransmit gaps.
type ChunkReassembler struct {
	chunks   [][]byte
	received []bool
	count    int
}

// NewChunkReassembler creates a reassembler expecting total chunks.
func NewChunkReassembler(total int) *ChunkReassembler {
	return &ChunkReassembler{chunks: make([][]byte, total), received: make([]bool, total)}
}

// Add stores a chunk at index; returns false if index is out of range.
// Re-adding an index is idempotent.
func (r *ChunkReassembler) Add(index int, chunk []byte) bool {
	if index < 0 || index >= len(r.chunks) {
		return false
	}
	if !r.received[index] {
		r.received[index] = true
		r.count++
	}
	r.chunks[index] = append([]byte(nil), chunk...)
	return true
}

// Complete reports whether every chunk has been received.
func (r *ChunkReassembler) Complete() bool {
	return len(r.chunks) > 0 && r.count == len(r.chunks)
}

// Bitmap returns the received set as a big-endian bit field: chunk i is bit
// (7 - i%8) of byte i/8.
func (r *ChunkReassembler) Bitmap() []byte {
	bm := make([]byte, (len(r.received)+7)/8)
	for i, ok := range r.received {
		if ok {
			bm[i/8] |= 1 << (7 - uint(i%8))
		}
	}
	return bm
}

// Assemble concatenates the received chunks. Meaningful only when Complete.
func (r *ChunkReassembler) Assemble() []byte {
	var n int
	for _, c := range r.chunks {
		n += len(c)
	}
	out := make([]byte, 0, n)
	for _, c := range r.chunks {
		out = append(out, c...)
	}
	return out
}

func appendUint32(dst []byte, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return append(dst, b[:]...)
}

func signedMessage(context string, body []byte) []byte {
	out := make([]byte, 0, len(context)+len(body))
	out = append(out, context...)
	out = append(out, body...)
	return out
}
