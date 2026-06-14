package protocol

import (
	"bytes"
	"strings"
	"testing"
)

const chatTestDomain = "chat.example.com"

var chatTestQK = [KeySize]byte{9: 7}

func mustData(t *testing.T, idx uint8, chunk []byte) []byte {
	t.Helper()
	b, err := BuildChatDataPlain(idx, chunk)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestChatCellUniformLengthAndRoundTrip checks every op fits one ≤40-char label
// and seals/round-trips.
func TestChatCellUniformLengthAndRoundTrip(t *testing.T) {
	var ks [KeySize]byte
	ks[0] = 11
	sel := [chatSelectorSize]byte{0xDE, 0xAD, 0xBE}

	plaintexts := [][]byte{
		BuildChatStatusPlain(),
		BuildChatFetchPlain([ChatPeerHandleSize]byte{1, 2, 3, 4}, 0x01020304, 7),
		BuildChatAckPlain([ChatPeerHandleSize]byte{1, 2, 3, 4}, 0xAABBCC, [ChatReceiptMACSize]byte{1, 2, 3, 4, 5, 6}),
		BuildChatSendStatusPlain([AddressSize]byte{9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 1, 2}),
		BuildChatKeyFetchPlain([AddressSize]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}),
		BuildChatSendStartPlain([AddressSize]byte{2, 4, 6, 8, 10, 12, 14, 16, 18, 20, 22, 24}, 500),
		mustData(t, 5, bytes.Repeat([]byte{0x5A}, ChatDataChunkSize)),
		BuildChatFinPlain(0xDEADBEEF),
	}
	for i, pt := range plaintexts {
		counter := uint32(i)
		payload, err := SealChatCellPayload(ks, sel, counter, pt)
		if err != nil {
			t.Fatalf("seal op %d: %v", i, err)
		}
		qn, err := EncodeChatCell(chatTestQK, QuerySingleLabel, sel, counter, payload, chatTestDomain)
		if err != nil {
			t.Fatalf("encode op %d: %v", i, err)
		}
		label := qn[:strings.IndexByte(qn, '.')]
		if len(label) > 40 {
			t.Fatalf("op %d label %d chars (>40): %q", i, len(label), label)
		}
		gotSel, gotCtr, gotPayload, err := DecodeChatCell(chatTestQK, qn, chatTestDomain)
		if err != nil {
			t.Fatalf("decode op %d: %v", i, err)
		}
		if gotSel != sel || gotCtr != counter {
			t.Fatalf("op %d selector/counter mismatch", i)
		}
		opened, err := OpenChatCellPayload(ks, gotSel, gotCtr, gotPayload)
		if err != nil {
			t.Fatalf("open op %d: %v", i, err)
		}
		if ChatPlainOp(opened) != ChatPlainOp(pt) {
			t.Fatalf("op %d opcode mismatch", i)
		}
		if !bytes.Equal(opened[:len(pt)], pt) {
			t.Fatalf("op %d plaintext mismatch", i)
		}
	}
}

// TestChatCellVariableBudget: a cell round-trips at any budget B, the query
// name shrinks as B shrinks (the whole point — RFC §8.2), and out-of-range
// budgets are rejected.
func TestChatCellVariableBudget(t *testing.T) {
	var ks [KeySize]byte
	ks[1] = 7
	sel := [chatSelectorSize]byte{0x12, 0x34, 0x56}
	pt := BuildChatStatusPlain() // 1 byte — fits even the smallest budget

	prevLabel := 0
	for bi, budget := range []int{ChatCellPlainMax, ChatCellPlainSize, ChatCellPlainMin} { // 21, 15, 6
		counter := uint32(bi)
		payload, err := SealChatCellPayloadN(ks, sel, counter, pt, budget)
		if err != nil {
			t.Fatalf("seal B=%d: %v", budget, err)
		}
		if len(payload) != budget+ChatSealTagSize {
			t.Fatalf("B=%d: payload %d, want %d", budget, len(payload), budget+ChatSealTagSize)
		}
		qn, err := EncodeChatCell(chatTestQK, QuerySingleLabel, sel, counter, payload, chatTestDomain)
		if err != nil {
			t.Fatalf("encode B=%d: %v", budget, err)
		}
		label := qn[:strings.IndexByte(qn, '.')]
		if prevLabel != 0 && len(label) >= prevLabel {
			t.Fatalf("B=%d label %d not shorter than previous %d", budget, len(label), prevLabel)
		}
		prevLabel = len(label)
		s, c, p, err := DecodeChatCell(chatTestQK, qn, chatTestDomain)
		if err != nil || s != sel || c != counter {
			t.Fatalf("decode B=%d: %v", budget, err)
		}
		opened, err := OpenChatCellPayload(ks, s, c, p)
		if err != nil {
			t.Fatalf("open B=%d: %v", budget, err)
		}
		if !bytes.Equal(opened[:len(pt)], pt) {
			t.Fatalf("B=%d plaintext mismatch", budget)
		}
	}
	if _, err := SealChatCellPayloadN(ks, sel, 0, pt, ChatCellPlainMin-1); err == nil {
		t.Fatal("budget below min should fail")
	}
	if _, err := SealChatCellPayloadN(ks, sel, 0, pt, ChatCellPlainMax+ChatJitterMax+1); err == nil {
		t.Fatal("padded length above max+jitter should fail")
	}
}

// TestChatCellJitter: jitter is deterministic per (selector, counter) — so a
// retransmit reproduces the exact query name (cacheable) — in range, and varies
// across cells (so lengths spread, not a single spike).
func TestChatCellJitter(t *testing.T) {
	var qk [KeySize]byte
	qk[0] = 3
	sel := [chatSelectorSize]byte{1, 2, 3}
	seen := map[int]bool{}
	for ctr := uint32(0); ctr < 256; ctr++ {
		j := ChatCellJitter(qk, sel, ctr)
		if j < 0 || j > ChatJitterMax {
			t.Fatalf("jitter %d out of range at ctr %d", j, ctr)
		}
		if j2 := ChatCellJitter(qk, sel, ctr); j2 != j {
			t.Fatalf("jitter not deterministic at ctr %d: %d vs %d", ctr, j, j2)
		}
		seen[j] = true
	}
	if len(seen) < 2 {
		t.Fatalf("jitter does not vary across cells: %v", seen)
	}
	// A different query key gives a different schedule (key-dependent).
	var qk2 [KeySize]byte
	qk2[0] = 9
	diff := false
	for ctr := uint32(0); ctr < 64; ctr++ {
		if ChatCellJitter(qk, sel, ctr) != ChatCellJitter(qk2, sel, ctr) {
			diff = true
			break
		}
	}
	if !diff {
		t.Fatal("jitter independent of query key")
	}
}

func TestChatCellMultiLabel(t *testing.T) {
	var ks [KeySize]byte
	sel := [chatSelectorSize]byte{1, 0, 0}
	payload, _ := SealChatCellPayload(ks, sel, 0, BuildChatStatusPlain())
	qn, err := EncodeChatCell(chatTestQK, QueryMultiLabel, sel, 0, payload, chatTestDomain)
	if err != nil {
		t.Fatal(err)
	}
	s, c, p, err := DecodeChatCell(chatTestQK, qn, chatTestDomain)
	if err != nil || s != sel || c != 0 {
		t.Fatalf("multi decode: %v", err)
	}
	if _, err := OpenChatCellPayload(ks, s, c, p); err != nil {
		t.Fatalf("multi open: %v", err)
	}
}

func TestChatCellTamperReject(t *testing.T) {
	var ks [KeySize]byte
	ks[3] = 5
	sel := [chatSelectorSize]byte{0, 0, 9}
	payload, _ := SealChatCellPayload(ks, sel, 4, BuildChatFinPlain(123))
	qn, _ := EncodeChatCell(chatTestQK, QuerySingleLabel, sel, 4, payload, chatTestDomain)
	bad := []byte(qn)
	if bad[20] == 'a' { // flip a char in the sealed payload region
		bad[20] = 'b'
	} else {
		bad[20] = 'a'
	}
	s, c, p, err := DecodeChatCell(chatTestQK, string(bad), chatTestDomain)
	if err == nil {
		if _, err = OpenChatCellPayload(ks, s, c, p); err == nil {
			t.Fatal("tampered cell opened")
		}
	}
}

// TestChatCellHeaderMasked guards the anti-DPI property: two cells in the same
// session with consecutive low counters must not share a long QNAME prefix and
// must have no long char-runs (the visible selector+counter are masked). Inputs
// are fixed, so it is deterministic.
func TestChatCellHeaderMasked(t *testing.T) {
	var ks [KeySize]byte
	sel := [chatSelectorSize]byte{0x35, 0x45, 0x88}
	enc := func(ctr uint32) string {
		pl, _ := SealChatCellPayload(ks, sel, ctr, BuildChatStatusPlain())
		qn, err := EncodeChatCell(chatTestQK, QuerySingleLabel, sel, ctr, pl, chatTestDomain)
		if err != nil {
			t.Fatal(err)
		}
		return qn[:strings.IndexByte(qn, '.')]
	}
	a, b := enc(0), enc(1)
	cp := 0
	for cp < len(a) && cp < len(b) && a[cp] == b[cp] {
		cp++
	}
	if cp > 4 {
		t.Fatalf("masked cells share a %d-char prefix:\n %q\n %q", cp, a, b)
	}
	best, run := 1, 1
	for i := 1; i < len(a); i++ {
		if a[i] == a[i-1] {
			run++
		} else {
			run = 1
		}
		if run > best {
			best = run
		}
	}
	if best >= 8 {
		t.Fatalf("masked cell has a %d-char run: %q", best, a)
	}
}

func TestChatResponseSealRoundTrip(t *testing.T) {
	var ks [KeySize]byte
	ks[1] = 2
	sel := [chatSelectorSize]byte{7, 0, 0}
	body := []byte{0x01, 0x02, 0x03}
	sealed := SealChatResponse(ks, sel, 42, ChatStatusOK, body)
	st, gotBody, err := OpenChatResponse(ks, sel, 42, sealed)
	if err != nil || st != ChatStatusOK || !bytes.Equal(gotBody, body) {
		t.Fatalf("resp: %v st=%d body=%x", err, st, gotBody)
	}
	if _, _, err := OpenChatResponse(ks, sel, 43, sealed); err == nil {
		t.Fatal("response opened under wrong counter")
	}
	var ks2 [KeySize]byte
	if _, _, err := OpenChatResponse(ks2, sel, 42, sealed); err == nil {
		t.Fatal("response opened under wrong key")
	}
}

func TestChatHandshakeStreamRoundTrip(t *testing.T) {
	eph, _ := GenerateEphemeralKey()
	var addr [AddressSize]byte
	addr[0] = 0xAB
	var proof [ChatAccountProofSize]byte
	proof[0] = 0xCD
	bootPlain := BuildChatAuthBootstrapPlain(addr, 1750000000, proof)
	var ks [KeySize]byte
	ks[0] = 1
	setupTag := [chatSelectorSize]byte{0x33, 0, 0}
	sealedBoot := SealChat(ks, setupTag[:], ChatBootstrapCounter(), bootPlain)
	stream := BuildChatHandshakeStream(eph.PublicKey().Bytes(), ChatProtocolVersion, ChatHandshakeAuth, sealedBoot)

	gotEph, gotVer, kind, gotSealed, err := ParseChatHandshakeStream(stream)
	if err != nil {
		t.Fatal(err)
	}
	if kind != ChatHandshakeAuth || gotVer != ChatProtocolVersion || !bytes.Equal(gotEph, eph.PublicKey().Bytes()) {
		t.Fatal("stream header mismatch")
	}
	opened, err := OpenChat(ks, setupTag[:], ChatBootstrapCounter(), gotSealed)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseChatAuthBootstrapPlain(opened)
	if err != nil {
		t.Fatal(err)
	}
	if b.Addr != addr || b.TS != 1750000000 || b.Proof != proof {
		t.Fatalf("bootstrap mismatch: %+v", b)
	}
}

func TestChatInfoRoundTrip(t *testing.T) {
	ek, err := GenerateEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	info := ChatInfo{
		MinVersion: 1,
		MaxVersion: 1,
		Enabled:    true,
		Domains:    []string{"c1.example.com", "c2.other.net"},
		EkPub:      ek.PublicKey().Bytes(),
		Limits:     DefaultChatLimits(),
	}
	info.Limits.SendPerHour = 42

	enc := EncodeChatInfo(info)
	got, err := ParseChatInfo(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got.MinVersion != 1 || got.MaxVersion != 1 || !got.Enabled {
		t.Fatal("version/enabled mismatch")
	}
	if strings.Join(got.Domains, ",") != strings.Join(info.Domains, ",") {
		t.Fatalf("domains mismatch: %v", got.Domains)
	}
	if !bytes.Equal(got.EkPub, info.EkPub) {
		t.Fatal("ek mismatch")
	}
	if got.Limits != info.Limits {
		t.Fatalf("limits mismatch: %+v", got.Limits)
	}

	dis := EncodeChatInfo(ChatInfo{MinVersion: 1, MaxVersion: 1, Enabled: false, Limits: DefaultChatLimits()})
	gotDis, err := ParseChatInfo(dis)
	if err != nil {
		t.Fatal(err)
	}
	if gotDis.Enabled {
		t.Fatal("disabled flag not preserved")
	}
}

func TestChatInfoGarbage(t *testing.T) {
	if _, err := ParseChatInfo([]byte{0x01, 0x00}); err == nil {
		t.Fatal("expected error on truncated TLV")
	}
	if _, err := ParseChatInfo([]byte{0x00}); err == nil {
		t.Fatal("expected error on missing version")
	}
}
