package server

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

func TestReportFileAppendAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rep", "dns_hourly.jsonl")
	rf, err := openReportFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := rf.Append([]byte(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	rf.Close()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 3 || lines[0] != `{"n":0}` || lines[2] != `{"n":2}` {
		t.Fatalf("lines = %v", lines)
	}
}

func TestReportFileRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dns_hourly.jsonl")
	rf, err := openReportFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rf.maxBytes = 100 // force frequent rotation
	rf.maxBackups = 2
	line := []byte(strings.Repeat("x", 40))
	for i := 0; i < 20; i++ {
		if err := rf.Append(line); err != nil {
			t.Fatal(err)
		}
	}
	rf.Close()

	// Current file exists, and at most maxBackups rotated files survive.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("current report file missing: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("backup .1 missing: %v", err)
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Fatalf("backup .2 missing: %v", err)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatal("backup .3 should not exist (exceeds maxBackups)")
	}
}

// TestServerShutdownFlushesReport guards the shutdown ordering: the final
// hourly report must reach the file before the server closes it on ctx cancel.
func TestServerShutdownFlushesReport(t *testing.T) {
	feed := NewFeed([]string{"chan"})
	var qk, rk [protocol.KeySize]byte
	s := NewDNSServer("127.0.0.1:0", "t.example.com", feed, qk, rk, 0, nil, false, "", nil, false)
	path := filepath.Join(t.TempDir(), "dns_hourly.jsonl")
	if err := s.SetReportFile(path); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.ListenAndServe(ctx); close(done) }()

	// Record a query so the final report is non-trivial, then shut down.
	s.reportCh <- reportEvent{channel: 1, resolver: "1.1.1.1", domain: "t.example.com"}
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after cancel")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"type":"dns_hourly_report"`) {
		t.Fatalf("final report not written to file; got %q", data)
	}
	if !strings.Contains(string(data), `"totalDnsQueries":1`) {
		t.Fatalf("report missing the recorded query: %q", data)
	}
}

func TestReportFileDisabled(t *testing.T) {
	rf, err := openReportFile("")
	if err != nil || rf != nil {
		t.Fatalf("empty path: rf=%v err=%v", rf, err)
	}
	// Nil receiver is a no-op.
	if err := rf.Append([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := rf.Close(); err != nil {
		t.Fatal(err)
	}
}
