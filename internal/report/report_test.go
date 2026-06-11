package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAndAggregate(t *testing.T) {
	const log = `noise line
Jun 10 host thefeed[1]: [dns_hourly] {"type":"dns_hourly_report","from":"2026-06-10T09:00:00Z","to":"2026-06-10T10:00:00Z","totalDnsQueries":1000,"totalMetadataQueries":100,"totalMediaQueries":50,"totalChatQueries":40,"totalInvalidQueries":3,"channels":[{"channel":1,"name":"A","queries":400},{"channel":0,"queries":100},{"channel":65525,"queries":20}],"domains":[{"domain":"t.example.com","queries":700}],"chat":{"accounts":12,"messages":5}}
[dns_hourly] {"type":"dns_hourly_report","from":"2026-06-10T10:00:00Z","to":"2026-06-10T11:00:00Z","totalDnsQueries":2000,"totalMetadataQueries":200,"channels":[{"channel":1,"name":"A","queries":900}],"chat":{"accounts":15,"messages":9}}
[dns_hourly] {"type":"something_else","totalDnsQueries":99999}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	agg, err := parseLines(path)
	if err != nil {
		t.Fatal(err)
	}
	if agg.reports != 2 {
		t.Fatalf("reports = %d, want 2 (non-report type ignored)", agg.reports)
	}
	if agg.total != 3000 || agg.metadata != 300 || agg.chat != 40 {
		t.Fatalf("totals: total=%d meta=%d chat=%d", agg.total, agg.metadata, agg.chat)
	}
	if agg.channels["A"] != 1300 {
		t.Fatalf("channel A = %d, want 1300", agg.channels["A"])
	}
	// channel 0 (metadata) and 65525 (chat info) are reserved, not content.
	if _, ok := agg.channels["channel 0"]; ok {
		t.Fatal("reserved channel leaked into content channels")
	}
	if agg.reserved[65525] != 20 {
		t.Fatalf("chat-info reserved = %d, want 20", agg.reserved[65525])
	}
	if agg.domains["t.example.com"] != 700 {
		t.Fatalf("domain total = %d", agg.domains["t.example.com"])
	}
	// channelFetch = 3000 - 300 - 0(version) - 50(media) - reserved(100+20)
	if cf := agg.channelFetch(); cf != 2530 {
		t.Fatalf("channelFetch = %d, want 2530", cf)
	}
	// latest chat stats win.
	if agg.lastChatStats["accounts"] != 15 {
		t.Fatalf("last accounts = %d, want 15", agg.lastChatStats["accounts"])
	}

	out := renderDashboard(agg, 21, 10, false)
	for _, want := range []string{"thefeed server report", "Top channels", "Chat (messenger)", "Registered accounts", "t.example.com"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	// -chat-db live count (21) overrides the report's 15.
	if !strings.Contains(out, "21") {
		t.Fatal("live account count not rendered")
	}
}

func TestRenderEmpty(t *testing.T) {
	out := renderDashboard(newAggregate(), -1, 10, false)
	if !strings.Contains(out, "No hourly reports") {
		t.Fatal("empty dashboard missing hint")
	}
}

func TestBarAndSparkline(t *testing.T) {
	if got := bar(0, 100, 10); strings.TrimRight(got, " ") != "" {
		t.Fatalf("zero bar not blank: %q", got)
	}
	if got := bar(100, 100, 10); !strings.Contains(got, "█") {
		t.Fatalf("full bar empty: %q", got)
	}
	if s := sparkline([]int64{1, 5, 9}, 10); len([]rune(s)) != 3 {
		t.Fatalf("sparkline rune count = %d, want 3", len([]rune(s)))
	}
}
