package protocol

import (
	"bytes"
	"testing"
)

func TestSerializeParseMetadata(t *testing.T) {
	original := &Metadata{
		Marker:    [3]byte{0xAA, 0xBB, 0xCC},
		Timestamp: 1700000000,
		Channels: []ChannelInfo{
			{Name: "VahidOnline", Blocks: 5, LastMsgID: 1234},
			{Name: "test", Blocks: 3, LastMsgID: 5678},
		},
	}
	data := SerializeMetadata(original)
	parsed, err := ParseMetadata(data)
	if err != nil {
		t.Fatalf("ParseMetadata: %v", err)
	}
	if parsed.Marker != original.Marker {
		t.Errorf("marker: got %v, want %v", parsed.Marker, original.Marker)
	}
	if parsed.Timestamp != original.Timestamp {
		t.Errorf("timestamp: got %d, want %d", parsed.Timestamp, original.Timestamp)
	}
	if len(parsed.Channels) != len(original.Channels) {
		t.Fatalf("channels: got %d, want %d", len(parsed.Channels), len(original.Channels))
	}
	for i := range original.Channels {
		if parsed.Channels[i] != original.Channels[i] {
			t.Errorf("channel %d mismatch", i)
		}
	}
}

func TestMetadataFlags(t *testing.T) {
	for _, tc := range []struct {
		tg, chat bool
	}{{false, false}, {true, false}, {false, true}, {true, true}} {
		m := &Metadata{TelegramLoggedIn: tc.tg, ChatAvailable: tc.chat}
		got, err := ParseMetadata(SerializeMetadata(m))
		if err != nil {
			t.Fatal(err)
		}
		if got.TelegramLoggedIn != tc.tg || got.ChatAvailable != tc.chat {
			t.Fatalf("flags tg=%v chat=%v -> got tg=%v chat=%v", tc.tg, tc.chat, got.TelegramLoggedIn, got.ChatAvailable)
		}
	}
}

func TestSerializeParseMessages(t *testing.T) {
	original := []Message{
		{ID: 100, Timestamp: 1700000000, Text: "Hello world"},
		{ID: 101, Timestamp: 1700000060, Text: "Test farsi"},
		{ID: 102, Timestamp: 1700000120, Text: "[IMAGE] Caption"},
	}
	data := SerializeMessages(original)
	parsed, err := ParseMessages(data)
	if err != nil {
		t.Fatalf("ParseMessages: %v", err)
	}
	if len(parsed) != len(original) {
		t.Fatalf("messages: got %d, want %d", len(parsed), len(original))
	}
	for i := range original {
		if parsed[i] != original[i] {
			t.Errorf("message %d mismatch", i)
		}
	}
}

func TestSplitIntoBlocks(t *testing.T) {
	// MaxBlockPayload*3+50 guarantees at least 4 blocks (ceil((MaxBlockPayload*3+50)/MaxBlockPayload) = 4).
	data := bytes.Repeat([]byte("A"), MaxBlockPayload*3+50)
	blocks := SplitIntoBlocks(data)
	if len(blocks) < 4 {
		t.Fatalf("expected at least 4 blocks for %d bytes, got %d", len(data), len(blocks))
	}
	// Every non-last block must be within [MinBlockPayload, MaxBlockPayload].
	for i, b := range blocks[:len(blocks)-1] {
		if len(b) < MinBlockPayload || len(b) > MaxBlockPayload {
			t.Errorf("block %d: size %d, want [%d, %d]", i, len(b), MinBlockPayload, MaxBlockPayload)
		}
	}
	// Reassembled data must equal original.
	var reassembled []byte
	for _, b := range blocks {
		reassembled = append(reassembled, b...)
	}
	if !bytes.Equal(reassembled, data) {
		t.Error("reassembled data does not match original")
	}
}

func TestSplitIntoBlocksEmpty(t *testing.T) {
	blocks := SplitIntoBlocks(nil)
	if len(blocks) != 1 {
		t.Fatalf("empty should produce 1 block, got %d", len(blocks))
	}
}

func TestMessageRoundtripThroughBlocks(t *testing.T) {
	msgs := []Message{
		{ID: 1, Timestamp: 1700000000, Text: "Short"},
		{ID: 2, Timestamp: 1700000001, Text: string(bytes.Repeat([]byte("X"), 300))},
		{ID: 3, Timestamp: 1700000002, Text: "End"},
	}
	data := SerializeMessages(msgs)
	blocks := SplitIntoBlocks(data)
	var reassembled []byte
	for _, b := range blocks {
		reassembled = append(reassembled, b...)
	}
	parsed, err := ParseMessages(reassembled)
	if err != nil {
		t.Fatalf("ParseMessages: %v", err)
	}
	if len(parsed) != len(msgs) {
		t.Fatalf("got %d messages, want %d", len(parsed), len(msgs))
	}
	for i := range msgs {
		if parsed[i] != msgs[i] {
			t.Errorf("message %d mismatch", i)
		}
	}
}

func TestParseMetadataTooShort(t *testing.T) {
	_, err := ParseMetadata([]byte{0x01, 0x02})
	if err == nil {
		t.Error("expected error for short metadata")
	}
}

func TestEncodeDecodeVersionData(t *testing.T) {
	versions := []string{"", "v1.0.0", "v2.3.14", "1.0.0-beta"}
	for _, ver := range versions {
		block, err := EncodeVersionData(ver)
		if err != nil {
			t.Fatalf("EncodeVersionData(%q): %v", ver, err)
		}
		if len(block) < MinBlockPayload {
			t.Errorf("EncodeVersionData(%q): block len %d < MinBlockPayload %d", ver, len(block), MinBlockPayload)
		}
		if len(block) > MaxBlockPayload {
			t.Errorf("EncodeVersionData(%q): block len %d > MaxBlockPayload %d", ver, len(block), MaxBlockPayload)
		}
		got, err := DecodeVersionData(block)
		if err != nil {
			t.Fatalf("DecodeVersionData(%q): %v", ver, err)
		}
		if got != ver {
			t.Errorf("round-trip: got %q, want %q", got, ver)
		}
	}
}

func TestDecodeVersionDataTooShort(t *testing.T) {
	_, err := DecodeVersionData([]byte{0x01})
	if err == nil {
		t.Error("expected error for 1-byte block")
	}
}
