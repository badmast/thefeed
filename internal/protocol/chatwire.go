package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// Chat wire codec. Every chat query is one uniform DNS cell:
//
//	selector(3) ‖ counter(3) ‖ payload(19)   = 25 bytes → 40 base32 chars
//
// encoded in a single base32 label (or the feed's multi-label hex when the
// query-mode is "double"). Two cell uses, byte-indistinguishable:
//
//   - In-context: selector = the server-assigned 3-byte session ref; payload =
//     SealChat(Ksession, selector, counter, op‖fields) — AES-CTR + 4-byte HMAC,
//     so the op, the destination, and the read metadata are all encrypted and a
//     resolver (even with the public passphrase) learns nothing.
//   - Handshake: selector = a client-chosen setup tag; payload = a 19-byte slab
//     of the handshake stream (eph pubkey ‖ kind ‖ sealed bootstrap). The eph is
//     public (random-looking) and the bootstrap is sealed under Ksession.
//
// Dispatch is by chat sub-domain (the server never runs DecodeQuery on these);
// the server tells handshake from in-context by whether the selector names a
// live session, falling back to "counter==0 ⇒ new handshake, else
// UnknownSession" for an unknown selector.
//
// Responses ride the roomy TXT path: SealChatResponse seals status‖body under
// Ksession with a response-side counter, then the normal EncodeResponse
// passphrase layer wraps it. Trust in the server's ek is rooted in the signed
// ChatInfo (pinned server key); the seal then authenticates the server per-op.

const (
	// ChatChannel is a reserved feed-channel number that feed domains refuse.
	// Chat is served only on its own sub-domains (dispatched by domain), so it
	// is no longer carried in chat queries — it just stays reserved.
	ChatChannel uint16 = 0xFFF6
	// ChatInfoChannel serves the signed chat capability payload on the feed
	// metadata path (block-count-prefixed, like TitlesChannel).
	ChatInfoChannel uint16 = 0xFFF5

	// ChatProtocolVersion is the chat request/response wire version (the high
	// nibble of an op byte). Kept for future changes; not bumped (chat is
	// unreleased, so there is no prior wire to stay compatible with).
	ChatProtocolVersion = 1

	chatCellLen      = 25
	chatSelectorSize = 3
	chatCounterSize  = 3
	// ChatCellPayloadSize is the per-cell payload after selector+counter.
	ChatCellPayloadSize = chatCellLen - chatSelectorSize - chatCounterSize // 19
	// ChatCellPlainSize is the max sealed plaintext (op + fields) per in-context
	// cell (payload minus the seal tag).
	ChatCellPlainSize = ChatCellPayloadSize - ChatSealTagSize // 15
	// ChatDataChunkSize is the message-body bytes carried by one DATA cell.
	ChatDataChunkSize = ChatCellPlainSize - 2 // op(1)+idx(1)+chunk → 13

	// ChatSelectorSize is exported for the session/selector layer.
	ChatSelectorSize = chatSelectorSize

	// chatRespCounterBit separates response nonces from request nonces under one
	// session key (set in the high counter bit for responses).
	chatRespCounterBit = 0x800000
	// chatBootstrapCounter is the fixed counter the handshake bootstrap is
	// sealed under (the stream cells are unsealed, so it never collides).
	chatBootstrapCounter = 0x400000
)

// Chat opcodes (low nibble of the first sealed-plaintext byte).
const (
	ChatOpStatus     = 1
	ChatOpFetch      = 2
	ChatOpAck        = 3
	ChatOpKeyFetch   = 4
	ChatOpSendStatus = 5
	ChatOpSendStart  = 6
	ChatOpData       = 7
	ChatOpFin        = 8
)

// Handshake kinds (the cleartext byte after the eph key in the stream).
const (
	ChatHandshakeAuth     = 1 // bootstrap = addr ‖ ts ‖ account proof
	ChatHandshakeRegister = 2 // bootstrap = full self-signed register record
)

// Chat response status codes.
const (
	ChatStatusOK               = 0
	ChatStatusUnknownRecipient = 1
	ChatStatusInboxFull        = 2
	ChatStatusPairQuota        = 3
	ChatStatusRateLimited      = 4
	ChatStatusUnknownSender    = 5
	ChatStatusBadVersion       = 6
	ChatStatusBusy             = 7
	ChatStatusUnknownSession   = 8
	ChatStatusBadAuth          = 9
	ChatStatusNotFound         = 11
	ChatStatusIncomplete       = 12
	ChatStatusReplay           = 13
	ChatStatusBadRequest       = 14
	ChatStatusDisabled         = 15
)

// chatSelectorHandshakeBit marks a selector as a client-chosen handshake setup
// tag (vs a server-assigned session ref, which clears it). Lets the server tell
// a new handshake cell from an orphaned in-context op on an unknown selector,
// with no extra wire byte — the bit just rides one of the selector's 24 random
// bits, leaving 2^23 ids (RAM-bound anyway).
const chatSelectorHandshakeBit = 0x80

// ChatMarkHandshakeSelector sets the handshake flag (client setup tags).
func ChatMarkHandshakeSelector(sel *[chatSelectorSize]byte) { sel[0] |= chatSelectorHandshakeBit }

// ChatClearHandshakeSelector clears the handshake flag (server session refs).
func ChatClearHandshakeSelector(sel *[chatSelectorSize]byte) { sel[0] &^= chatSelectorHandshakeBit }

// ChatIsHandshakeSelector reports whether the handshake flag is set.
func ChatIsHandshakeSelector(sel [chatSelectorSize]byte) bool {
	return sel[0]&chatSelectorHandshakeBit != 0
}

// ChatSessionLostResp is the 1-byte unsealed sentinel a server returns for an
// in-context cell whose session it no longer knows (TTL expiry or reboot). The
// client can't open a sealed reply for a dead session, so this length-1 marker
// (a sealed reply is always ≥1+ChatSealTagSize) tells it to re-handshake.
var ChatSessionLostResp = []byte{0xE5}

// ChatIsSessionLost reports whether a decoded response is the session-lost
// sentinel.
func ChatIsSessionLost(resp []byte) bool {
	return len(resp) == 1 && resp[0] == ChatSessionLostResp[0]
}

func chatHdr(op byte) byte { return byte(ChatProtocolVersion)<<4 | (op & 0x0F) }

// ChatPlainOp / ChatPlainVersion read the op / version from a sealed-plaintext.
func ChatPlainOp(pt []byte) byte {
	if len(pt) == 0 {
		return 0
	}
	return pt[0] & 0x0F
}

func ChatPlainVersion(pt []byte) byte {
	if len(pt) == 0 {
		return 0
	}
	return pt[0] >> 4
}

func putUint24(b []byte, v uint32) { b[0] = byte(v >> 16); b[1] = byte(v >> 8); b[2] = byte(v) }
func getUint24(b []byte) uint32    { return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]) }
func appendUint24(dst []byte, v uint32) []byte {
	return append(dst, byte(v>>16), byte(v>>8), byte(v))
}

// ---- cell framing ----

func encodeChatLabels(raw []byte, mode QueryEncoding, domain string) (string, error) {
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" {
		return "", fmt.Errorf("empty domain")
	}
	switch mode {
	case QueryMultiLabel:
		return joinQName(splitMultiLabel(hex.EncodeToString(raw)), domain)
	default:
		return joinQName([]string{strings.ToLower(b32.EncodeToString(raw))}, domain)
	}
}

func decodeChatLabels(qname, domain string) ([]byte, error) {
	qname = strings.TrimSuffix(qname, ".")
	domain = strings.TrimSuffix(domain, ".")
	suffix := "." + domain
	if !strings.HasSuffix(strings.ToLower(qname), strings.ToLower(suffix)) {
		return nil, fmt.Errorf("domain mismatch")
	}
	stripped := strings.ReplaceAll(qname[:len(qname)-len(suffix)], ".", "")
	if raw, err := b32.DecodeString(strings.ToUpper(stripped)); err == nil {
		return raw, nil
	}
	if raw, err := hex.DecodeString(stripped); err == nil {
		return raw, nil
	}
	return nil, fmt.Errorf("decode chat cell: invalid encoding")
}

const chatCellMaskInfo = "thefeed-chat-cell-mask-v1"

// chatCellMask derives the keystream that masks the visible selector+counter,
// keyed by the query key and the cell's (random-looking) payload. The payload
// varies per cell, so the mask — and thus the masked selector+counter — varies
// per cell: no constant per-session prefix, no low-counter zero runs. A
// resolver without the passphrase cannot compute it (sees random); the server
// (which has the query key and the payload at a fixed offset) recovers it.
func chatCellMask(queryKey [KeySize]byte, payload []byte) [chatSelectorSize + chatCounterSize]byte {
	h := hmac.New(sha256.New, queryKey[:])
	h.Write([]byte(chatCellMaskInfo))
	h.Write(payload)
	var m [chatSelectorSize + chatCounterSize]byte
	copy(m[:], h.Sum(nil))
	return m
}

// EncodeChatCell packs one uniform cell into a query name. payload must be
// ≤ ChatCellPayloadSize; it is zero-padded to the fixed cell length and the
// selector+counter are masked so the whole name looks random.
func EncodeChatCell(queryKey [KeySize]byte, mode QueryEncoding, selector [chatSelectorSize]byte, counter uint32, payload []byte, domain string) (string, error) {
	if len(payload) > ChatCellPayloadSize {
		return "", fmt.Errorf("chat cell payload too long: %d", len(payload))
	}
	raw := make([]byte, chatCellLen)
	copy(raw[:chatSelectorSize], selector[:])
	putUint24(raw[chatSelectorSize:], counter)
	copy(raw[chatSelectorSize+chatCounterSize:], payload)
	mask := chatCellMask(queryKey, raw[chatSelectorSize+chatCounterSize:chatCellLen])
	for i := 0; i < chatSelectorSize+chatCounterSize; i++ {
		raw[i] ^= mask[i]
	}
	return encodeChatLabels(raw, mode, domain)
}

// DecodeChatCell splits a query name into its selector, counter, and the
// fixed-length payload (still sealed, for in-context cells), un-masking the
// selector+counter first.
func DecodeChatCell(queryKey [KeySize]byte, qname, domain string) (selector [chatSelectorSize]byte, counter uint32, payload []byte, err error) {
	raw, e := decodeChatLabels(qname, domain)
	if e != nil {
		return selector, 0, nil, e
	}
	if len(raw) < chatCellLen {
		return selector, 0, nil, fmt.Errorf("chat cell too short: %d", len(raw))
	}
	mask := chatCellMask(queryKey, raw[chatSelectorSize+chatCounterSize:chatCellLen])
	for i := 0; i < chatSelectorSize+chatCounterSize; i++ {
		raw[i] ^= mask[i]
	}
	copy(selector[:], raw[:chatSelectorSize])
	counter = getUint24(raw[chatSelectorSize:])
	payload = append([]byte(nil), raw[chatSelectorSize+chatCounterSize:chatCellLen]...)
	return selector, counter, payload, nil
}

// SealChatCellPayload seals an op plaintext into a 19-byte cell payload (the
// plaintext is zero-padded to the fixed plain size first, so every in-context
// cell is the same length).
func SealChatCellPayload(ksession [KeySize]byte, selector [chatSelectorSize]byte, counter uint32, plaintext []byte) ([]byte, error) {
	if len(plaintext) > ChatCellPlainSize {
		return nil, fmt.Errorf("chat op plaintext too long: %d", len(plaintext))
	}
	pt := make([]byte, ChatCellPlainSize)
	copy(pt, plaintext)
	return SealChat(ksession, selector[:], counter, pt), nil
}

// OpenChatCellPayload reverses SealChatCellPayload, returning the fixed-size
// plaintext (caller reads op + fields, ignores trailing pad).
func OpenChatCellPayload(ksession [KeySize]byte, selector [chatSelectorSize]byte, counter uint32, payload []byte) ([]byte, error) {
	return OpenChat(ksession, selector[:], counter, payload)
}

// ---- op plaintext builders ----

// BuildChatStatusPlain: INBOX_STATUS (freshness comes from the cell counter).
func BuildChatStatusPlain() []byte { return []byte{chatHdr(ChatOpStatus)} }

// BuildChatFetchPlain: INBOX_FETCH for (seq, block).
func BuildChatFetchPlain(seq uint32, block uint8) []byte {
	return append(appendUint32([]byte{chatHdr(ChatOpFetch)}, seq), block)
}

// BuildChatAckPlain: ACK peer's messages up to upToSeq (peer by handle).
func BuildChatAckPlain(peer [ChatPeerHandleSize]byte, upToSeq uint32) []byte {
	return appendUint24(append([]byte{chatHdr(ChatOpAck)}, peer[:]...), upToSeq)
}

// BuildChatSendStatusPlain: ✓/✓✓ counters for own messages to peer. The full
// address is sent (the recipient may not be in the caller's known pairs, so a
// handle can't be resolved server-side) — it still fits one cell.
func BuildChatSendStatusPlain(peer [AddressSize]byte) []byte {
	return append([]byte{chatHdr(ChatOpSendStatus)}, peer[:]...)
}

// BuildChatKeyFetchPlain: fetch a peer's registration record (full addr).
func BuildChatKeyFetchPlain(addr [AddressSize]byte) []byte {
	return append([]byte{chatHdr(ChatOpKeyFetch)}, addr[:]...)
}

// BuildChatSendStartPlain: start a message upload to dst (src is the session).
func BuildChatSendStartPlain(dst [AddressSize]byte, totalLen uint16) []byte {
	return append(append([]byte{chatHdr(ChatOpSendStart)}, dst[:]...), byte(totalLen>>8), byte(totalLen))
}

// BuildChatDataPlain: one body chunk at index.
func BuildChatDataPlain(idx uint8, chunk []byte) ([]byte, error) {
	if len(chunk) > ChatDataChunkSize {
		return nil, fmt.Errorf("chat chunk too big: %d", len(chunk))
	}
	return append([]byte{chatHdr(ChatOpData), idx}, chunk...), nil
}

// BuildChatFinPlain: commit the upload (crc over the assembled body).
func BuildChatFinPlain(crc uint32) []byte {
	return appendUint32([]byte{chatHdr(ChatOpFin)}, crc)
}

// ---- op plaintext parsers ----

// ChatFetch is a parsed INBOX_FETCH.
type ChatFetch struct {
	Seq   uint32
	Block uint8
}

// ParseChatFetchPlain parses an INBOX_FETCH plaintext.
func ParseChatFetchPlain(pt []byte) (*ChatFetch, error) {
	if len(pt) < 1+4+1 {
		return nil, fmt.Errorf("chat fetch: short")
	}
	return &ChatFetch{Seq: binary.BigEndian.Uint32(pt[1:]), Block: pt[5]}, nil
}

// ChatAck is a parsed ACK.
type ChatAck struct {
	Peer    [ChatPeerHandleSize]byte
	UpToSeq uint32
}

// ParseChatAckPlain parses an ACK plaintext.
func ParseChatAckPlain(pt []byte) (*ChatAck, error) {
	if len(pt) < 1+ChatPeerHandleSize+3 {
		return nil, fmt.Errorf("chat ack: short")
	}
	a := &ChatAck{UpToSeq: getUint24(pt[1+ChatPeerHandleSize:])}
	copy(a.Peer[:], pt[1:])
	return a, nil
}

// ChatSendStatus is a parsed SENDSTATUS.
type ChatSendStatus struct {
	Peer [AddressSize]byte
}

// ParseChatSendStatusPlain parses a SENDSTATUS plaintext.
func ParseChatSendStatusPlain(pt []byte) (*ChatSendStatus, error) {
	if len(pt) < 1+AddressSize {
		return nil, fmt.Errorf("chat sendstatus: short")
	}
	s := &ChatSendStatus{}
	copy(s.Peer[:], pt[1:])
	return s, nil
}

// ChatKeyFetch is a parsed KEYFETCH.
type ChatKeyFetch struct {
	Addr [AddressSize]byte
}

// ParseChatKeyFetchPlain parses a KEYFETCH plaintext.
func ParseChatKeyFetchPlain(pt []byte) (*ChatKeyFetch, error) {
	if len(pt) < 1+AddressSize {
		return nil, fmt.Errorf("chat keyfetch: short")
	}
	k := &ChatKeyFetch{}
	copy(k.Addr[:], pt[1:])
	return k, nil
}

// ChatSendStart is a parsed SEND_START.
type ChatSendStart struct {
	Dst      [AddressSize]byte
	TotalLen uint16
}

// ParseChatSendStartPlain parses a SEND_START plaintext.
func ParseChatSendStartPlain(pt []byte) (*ChatSendStart, error) {
	if len(pt) < 1+AddressSize+2 {
		return nil, fmt.Errorf("chat sendstart: short")
	}
	s := &ChatSendStart{TotalLen: binary.BigEndian.Uint16(pt[1+AddressSize:])}
	copy(s.Dst[:], pt[1:])
	return s, nil
}

// ChatData is a parsed DATA chunk. Chunk is the fixed-size slice; the server
// trims it to the real length using the upload's known total length.
type ChatData struct {
	Index uint8
	Chunk []byte
}

// ParseChatDataPlain parses a DATA plaintext.
func ParseChatDataPlain(pt []byte) (*ChatData, error) {
	if len(pt) < 2 {
		return nil, fmt.Errorf("chat data: short")
	}
	return &ChatData{Index: pt[1], Chunk: append([]byte(nil), pt[2:]...)}, nil
}

// ChatFin is a parsed FIN.
type ChatFin struct {
	CRC32 uint32
}

// ParseChatFinPlain parses a FIN plaintext.
func ParseChatFinPlain(pt []byte) (*ChatFin, error) {
	if len(pt) < 1+4 {
		return nil, fmt.Errorf("chat fin: short")
	}
	return &ChatFin{CRC32: binary.BigEndian.Uint32(pt[1:])}, nil
}

// ---- handshake stream ----

// BuildChatHandshakeStream assembles the reassembled handshake stream:
// eph(32) ‖ kind(1) ‖ sealedBootstrap.
func BuildChatHandshakeStream(ephPub []byte, kind byte, sealedBootstrap []byte) []byte {
	out := make([]byte, 0, X25519KeySize+1+len(sealedBootstrap))
	out = append(out, ephPub...)
	out = append(out, kind)
	return append(out, sealedBootstrap...)
}

// ParseChatHandshakeStream splits a reassembled handshake stream.
func ParseChatHandshakeStream(stream []byte) (ephPub []byte, kind byte, sealedBootstrap []byte, err error) {
	if len(stream) < X25519KeySize+1 {
		return nil, 0, nil, fmt.Errorf("chat handshake: short stream")
	}
	return stream[:X25519KeySize], stream[X25519KeySize], stream[X25519KeySize+1:], nil
}

// ChatBootstrapCounter is the counter the bootstrap blob is sealed under.
func ChatBootstrapCounter() uint32 { return chatBootstrapCounter }

// BuildChatAuthBootstrapPlain builds an auth-handshake bootstrap plaintext.
func BuildChatAuthBootstrapPlain(addr [AddressSize]byte, ts uint32, proof [ChatAccountProofSize]byte) []byte {
	out := make([]byte, 0, AddressSize+4+ChatAccountProofSize)
	out = append(out, addr[:]...)
	out = appendUint32(out, ts)
	return append(out, proof[:]...)
}

// ChatAuthBootstrap is a parsed auth-handshake bootstrap.
type ChatAuthBootstrap struct {
	Addr  [AddressSize]byte
	TS    uint32
	Proof [ChatAccountProofSize]byte
}

// ParseChatAuthBootstrapPlain parses an auth-handshake bootstrap plaintext.
func ParseChatAuthBootstrapPlain(pt []byte) (*ChatAuthBootstrap, error) {
	if len(pt) != AddressSize+4+ChatAccountProofSize {
		return nil, fmt.Errorf("chat bootstrap: wrong length %d", len(pt))
	}
	b := &ChatAuthBootstrap{TS: binary.BigEndian.Uint32(pt[AddressSize:])}
	copy(b.Addr[:], pt[:AddressSize])
	copy(b.Proof[:], pt[AddressSize+4:])
	return b, nil
}

// ---- responses (sealed under the session key, then EncodeResponse-wrapped) ----

func chatRespCounter(reqCounter uint32) uint32 { return reqCounter | chatRespCounterBit }

// SealChatResponse seals status‖body under the session key with a response-side
// counter (distinct from any request nonce).
func SealChatResponse(ksession [KeySize]byte, selector [chatSelectorSize]byte, reqCounter uint32, status byte, body []byte) []byte {
	pt := make([]byte, 0, 1+len(body))
	pt = append(pt, status)
	pt = append(pt, body...)
	return SealChat(ksession, selector[:], chatRespCounter(reqCounter), pt)
}

// OpenChatResponse reverses SealChatResponse.
func OpenChatResponse(ksession [KeySize]byte, selector [chatSelectorSize]byte, reqCounter uint32, sealed []byte) (status byte, body []byte, err error) {
	pt, err := OpenChat(ksession, selector[:], chatRespCounter(reqCounter), sealed)
	if err != nil {
		return 0, nil, err
	}
	if len(pt) < 1 {
		return 0, nil, fmt.Errorf("chat response: empty")
	}
	return pt[0], pt[1:], nil
}

// ChatLimits are the server-advertised chat limits.
type ChatLimits struct {
	ChunkSize      uint8
	MaxMsgBytes    uint16
	InboxCap       uint16
	PerPairCap     uint16
	SendPerHour    uint16
	SessionIdleSec uint16
	SessionHardSec uint16
	TTLHours       uint16
}

// DefaultChatLimits returns the default server limits.
func DefaultChatLimits() ChatLimits {
	return ChatLimits{
		ChunkSize:      ChatDataChunkSize,
		MaxMsgBytes:    500,
		InboxCap:       50,
		PerPairCap:     10,
		SendPerHour:    30,
		SessionIdleSec: 120,
		SessionHardSec: 600,
		TTLHours:       72,
	}
}

// ChatInfo is the chat capability payload served (signed) on the feed metadata
// path. EkPub is the server x25519 key clients run the session handshake
// against — delivered here, under the feed signing key, instead of in the
// import URI. Enabled is false when the operator has chat domains configured
// but has turned chat off.
type ChatInfo struct {
	MinVersion uint8
	MaxVersion uint8
	Enabled    bool
	Domains    []string
	EkPub      []byte
	Limits     ChatLimits
}

// ChatInfo TLV types.
const (
	chatInfoTLVEnd     = 0x00
	chatInfoTLVVersion = 0x01
	chatInfoTLVDomains = 0x02
	chatInfoTLVEkPub   = 0x03
	chatInfoTLVLimits  = 0x04
	chatInfoTLVEnabled = 0x05
)

const chatInfoLimitsLen = 1 + 2 + 2 + 2 + 2 + 2 + 2 + 2

// EncodeChatInfo encodes a ChatInfo payload (TLV; unknown types are skipped by
// old parsers, so fields can be added later).
func EncodeChatInfo(info ChatInfo) []byte {
	buf := make([]byte, 0, 64+len(info.EkPub))
	buf = appendExtraTLV(buf, chatInfoTLVVersion, []byte{info.MinVersion, info.MaxVersion})
	enabled := byte(0)
	if info.Enabled {
		enabled = 1
	}
	buf = appendExtraTLV(buf, chatInfoTLVEnabled, []byte{enabled})
	if len(info.Domains) > 0 {
		buf = appendExtraTLV(buf, chatInfoTLVDomains, []byte(strings.Join(info.Domains, ",")))
	}
	if len(info.EkPub) == X25519KeySize {
		buf = appendExtraTLV(buf, chatInfoTLVEkPub, info.EkPub)
	}
	lim := make([]byte, 0, chatInfoLimitsLen)
	lim = append(lim, info.Limits.ChunkSize)
	for _, v := range []uint16{
		info.Limits.MaxMsgBytes, info.Limits.InboxCap, info.Limits.PerPairCap,
		info.Limits.SendPerHour, info.Limits.SessionIdleSec, info.Limits.SessionHardSec,
		info.Limits.TTLHours,
	} {
		lim = append(lim, byte(v>>8), byte(v))
	}
	buf = appendExtraTLV(buf, chatInfoTLVLimits, lim)
	return append(buf, chatInfoTLVEnd)
}

// ParseChatInfo decodes a ChatInfo payload.
func ParseChatInfo(data []byte) (*ChatInfo, error) {
	info := &ChatInfo{Limits: DefaultChatLimits()}
	off := 0
	for off < len(data) {
		typ := data[off]
		off++
		if typ == chatInfoTLVEnd {
			break
		}
		if off+2 > len(data) {
			return nil, fmt.Errorf("chatinfo: truncated TLV length")
		}
		l := int(binary.BigEndian.Uint16(data[off:]))
		off += 2
		if off+l > len(data) {
			return nil, fmt.Errorf("chatinfo: truncated TLV value")
		}
		val := data[off : off+l]
		off += l

		switch typ {
		case chatInfoTLVVersion:
			if l != 2 {
				return nil, fmt.Errorf("chatinfo: bad version length")
			}
			info.MinVersion, info.MaxVersion = val[0], val[1]
		case chatInfoTLVEnabled:
			if l != 1 {
				return nil, fmt.Errorf("chatinfo: bad enabled length")
			}
			info.Enabled = val[0] != 0
		case chatInfoTLVDomains:
			for _, d := range strings.Split(string(val), ",") {
				if d = strings.TrimSpace(d); d != "" {
					info.Domains = append(info.Domains, d)
				}
			}
		case chatInfoTLVEkPub:
			if l != X25519KeySize {
				return nil, fmt.Errorf("chatinfo: bad ek length %d", l)
			}
			info.EkPub = append([]byte(nil), val...)
		case chatInfoTLVLimits:
			if l != chatInfoLimitsLen {
				return nil, fmt.Errorf("chatinfo: bad limits length %d", l)
			}
			info.Limits.ChunkSize = val[0]
			info.Limits.MaxMsgBytes = binary.BigEndian.Uint16(val[1:])
			info.Limits.InboxCap = binary.BigEndian.Uint16(val[3:])
			info.Limits.PerPairCap = binary.BigEndian.Uint16(val[5:])
			info.Limits.SendPerHour = binary.BigEndian.Uint16(val[7:])
			info.Limits.SessionIdleSec = binary.BigEndian.Uint16(val[9:])
			info.Limits.SessionHardSec = binary.BigEndian.Uint16(val[11:])
			info.Limits.TTLHours = binary.BigEndian.Uint16(val[13:])
		default:
			// Unknown type — skip for forward compatibility.
		}
	}
	if info.MinVersion == 0 || info.MaxVersion == 0 {
		return nil, fmt.Errorf("chatinfo: missing version range")
	}
	return info, nil
}
