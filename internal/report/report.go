// Package report renders a terminal dashboard from a thefeed server's hourly
// DNS reports. It serves nothing on the network: it reads the server's
// rotating report file (`<data-dir>/dns_hourly.jsonl`), or stdin, and
// optionally the chat bbolt file for a live account count. It mirrors the
// aggregations of scripts/thefeed_log_report.py and adds chat stats + bars.
package report

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

const logMark = "[dns_hourly] "

// reservedNames labels the high control channels (mirrors the python script).
var reservedNames = map[int]string{
	0:     "feed metadata",
	65534: "send message (write)",
	65533: "admin / management",
	65532: "upstream init (write)",
	65531: "upstream data (write)",
	65530: "latest app version",
	65529: "channel titles",
	65528: "relay address block",
	65527: "profile-picture index",
	65525: "chat info",
}

// queries-per-channel-fetch estimate (operator figure, matches the script).
const qpcLow, qpcHigh = 30, 80

type hourlyReport struct {
	Type     string `json:"type"`
	From     string `json:"from"`
	To       string `json:"to"`
	Total    int64  `json:"totalDnsQueries"`
	Metadata int64  `json:"totalMetadataQueries"`
	Version  int64  `json:"totalVersionQueries"`
	Media    int64  `json:"totalMediaQueries"`
	Chat     int64  `json:"totalChatQueries"`
	Invalid  int64  `json:"totalInvalidQueries"`
	Channels []struct {
		Channel int    `json:"channel"`
		Name    string `json:"name"`
		Queries int64  `json:"queries"`
	} `json:"channels"`
	Domains []struct {
		Domain  string `json:"domain"`
		Queries int64  `json:"queries"`
	} `json:"domains"`
	ChatStats map[string]int64 `json:"chat"`
}

// aggregate accumulates parsed reports.
type aggregate struct {
	reports         int
	total           int64
	metadata        int64
	version         int64
	media           int64
	chat            int64
	invalid         int64
	channels        map[string]int64 // content channels: name -> queries
	reserved        map[int]int64
	domains         map[string]int64
	hours           map[int]int64 // hour-of-day -> total queries
	series          []int64       // per-report total, in order (for the sparkline)
	lastChatStats   map[string]int64
	firstTo, lastTo time.Time
}

func newAggregate() *aggregate {
	return &aggregate{
		channels: map[string]int64{},
		reserved: map[int]int64{},
		domains:  map[string]int64{},
		hours:    map[int]int64{},
	}
}

func isReserved(ch int) bool {
	if _, ok := reservedNames[ch]; ok {
		return true
	}
	return ch >= 10000 && ch <= 60000 // media blob range
}

func (a *aggregate) add(rep *hourlyReport) {
	if rep.Type != "dns_hourly_report" {
		return
	}
	a.reports++
	a.total += rep.Total
	a.metadata += rep.Metadata
	a.version += rep.Version
	a.media += rep.Media
	a.chat += rep.Chat
	a.invalid += rep.Invalid
	a.series = append(a.series, rep.Total)
	if rep.ChatStats != nil {
		a.lastChatStats = rep.ChatStats
	}
	ts := rep.To
	if ts == "" {
		ts = rep.From
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		a.hours[t.Hour()] += rep.Total
		if a.firstTo.IsZero() || t.Before(a.firstTo) {
			a.firstTo = t
		}
		if t.After(a.lastTo) {
			a.lastTo = t
		}
	}
	for _, ch := range rep.Channels {
		if isReserved(ch.Channel) {
			a.reserved[ch.Channel] += ch.Queries
		} else {
			name := ch.Name
			if name == "" {
				name = fmt.Sprintf("channel %d", ch.Channel)
			}
			a.channels[name] += ch.Queries
		}
	}
	for _, d := range rep.Domains {
		a.domains[d.Domain] += d.Queries
	}
}

// channelFetch = total - overhead (matches the python script).
func (a *aggregate) channelFetch() int64 {
	var reserved int64
	for _, q := range a.reserved {
		reserved += q
	}
	cf := a.total - a.metadata - a.version - a.media - reserved
	if cf < 0 {
		cf = 0
	}
	return cf
}

func parseLines(path string) (*aggregate, error) {
	var f *os.File
	if path == "" || path == "-" {
		f = os.Stdin
	} else {
		var err error
		f, err = os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
	}
	agg := newAggregate()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		// Accept both the canonical report file (one raw JSON object per
		// line) and journald/stdout lines that carry the [dns_hourly] marker.
		jstr := strings.TrimSpace(sc.Text())
		if i := strings.Index(jstr, logMark); i >= 0 {
			jstr = strings.TrimSpace(jstr[i+len(logMark):])
		}
		if !strings.HasPrefix(jstr, "{") {
			continue
		}
		var rep hourlyReport
		if json.Unmarshal([]byte(jstr), &rep) != nil {
			continue
		}
		agg.add(&rep)
	}
	return agg, sc.Err()
}

// liveAccountCount opens the chat bbolt file read-only and counts accounts.
func liveAccountCount(path string) (int, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return 0, err
	}
	defer db.Close()
	n := 0
	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("chat_accounts"))
		if b == nil {
			return nil
		}
		n = b.Stats().KeyN
		return nil
	})
	return n, err
}

// Options configures Run.
type Options struct {
	Path    string        // report JSONL path, or "-" for stdin
	ChatDB  string        // optional chat bbolt path for a live account count
	Top     int           // number of top channels to show (0 → 15)
	Refresh time.Duration // 0 = render once; >0 = redraw every interval
	Out     io.Writer     // defaults to os.Stdout
}

// Run renders the dashboard once, or live on a ticker until interrupted.
func Run(opts Options) error {
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Top <= 0 {
		opts.Top = 15
	}
	live := opts.Refresh > 0 && opts.Path != "" && opts.Path != "-"

	render := func() error {
		agg, err := parseLines(opts.Path)
		if err != nil {
			return err
		}
		accounts := -1
		if opts.ChatDB != "" {
			if n, err := liveAccountCount(opts.ChatDB); err == nil {
				accounts = n
			}
		}
		out := renderDashboard(agg, accounts, opts.Top, live)
		if live {
			// Home, clear from cursor down, and clear scrollback so successive
			// frames overwrite cleanly instead of piling up in the buffer.
			fmt.Fprint(opts.Out, "\x1b[H\x1b[J\x1b[3J")
		}
		fmt.Fprint(opts.Out, out)
		return nil
	}

	if err := render(); err != nil {
		return err
	}
	if !live {
		return nil
	}
	ticker := time.NewTicker(opts.Refresh)
	defer ticker.Stop()
	for range ticker.C {
		if err := render(); err != nil {
			fmt.Fprintln(os.Stderr, "report:", err)
		}
	}
	return nil
}
