package client

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	cryptoRand "crypto/rand"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
	"github.com/sartoopjj/thefeed/internal/server"
)

func dnsEnvInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// TestChatLoadDNS is a true end-to-end load test: a real DNS server on a
// loopback UDP port, driven by real ChatClients (real handshake, sealed cells,
// ARQ, UDP round-trips). Unlike TestChatLoad (in-process), this includes the
// full network path, so its latency reflects real DNS round-trips. Opt-in:
//
//	CHAT_LOAD_DNS=1 CHAT_USERS=50 CHAT_MSGS=10 go test ./internal/client/ -run TestChatLoadDNS -v -timeout 20m
func TestChatLoadDNS(t *testing.T) {
	if os.Getenv("CHAT_LOAD_DNS") == "" {
		t.Skip("set CHAT_LOAD_DNS=1 to run the real-DNS chat load test")
	}
	users := dnsEnvInt("CHAT_USERS", 50)
	perUser := dnsEnvInt("CHAT_MSGS", 10)

	const passphrase = "load-dns-pass"
	const feedDomain = "t.example.com"
	const chatDomain = "chat.example.com"

	// Server keys + chat capability.
	signPub, signKey, err := ed25519.GenerateKey(cryptoRand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ek, err := ecdh.X25519().GenerateKey(cryptoRand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	qk, rk, err := protocol.DeriveKeys(passphrase)
	if err != nil {
		t.Fatal(err)
	}

	limits := protocol.DefaultChatLimits()
	limits.SendPerHour = 60000
	limits.InboxCap = 60000
	limits.PerPairCap = 60000

	feed := server.NewFeed(nil)
	feed.SetSigningKey(signKey)
	feed.SetChatAvailable(true)

	store, err := server.OpenChatStore(filepath.Join(t.TempDir(), "chatload.db"), limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.EnablePeriodicSync(time.Second)

	chat := server.NewChatService(store, ek, qk, limits, []string{chatDomain})
	feed.SetChatInfoPayload(protocol.EncodeChatInfo(chat.Info()))

	// Bind a free loopback UDP port for the DNS server.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go store.RunSync(ctx)

	dnsSrv := server.NewDNSServer(addr, feedDomain, feed, qk, rk, protocol.DefaultMaxPadding, nil, false, "", nil, false)
	if err := dnsSrv.SetReportFile(filepath.Join(t.TempDir(), "rep.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := dnsSrv.SetChatService(chat); err != nil {
		t.Fatal(err)
	}
	go dnsSrv.ListenAndServe(ctx)

	mkClient := func() *ChatClient {
		f, err := NewFetcher(feedDomain, passphrase, []string{addr})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.SetServerPublicKey(base64.RawURLEncoding.EncodeToString(signPub)); err != nil {
			t.Fatal(err)
		}
		f.SetActiveResolvers([]string{addr})
		f.SetScatter(1)
		f.SetTimeout(3 * time.Second)
		f.SetNoiseDisabled(true)
		f.Start(ctx)
		seed, _ := protocol.GenerateSeed()
		id, _ := NewChatIdentity(seed)
		return NewChatClient(f, id)
	}

	clients := make([]*ChatClient, users)
	for i := range clients {
		clients[i] = mkClient()
	}

	// Wait for the server to accept queries (capability discovery succeeds).
	ready := false
	for i := 0; i < 60; i++ {
		pctx, c := context.WithTimeout(ctx, time.Second)
		_, err := clients[0].EnsureInfo(pctx)
		c()
		if err == nil {
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("DNS server did not become ready")
	}

	// Register every client (handshake over real DNS), sequentially.
	regStart := time.Now()
	for i, c := range clients {
		rctx, cc := context.WithTimeout(ctx, 10*time.Second)
		if err := c.Register(rctx, nil); err != nil {
			cc()
			t.Fatalf("register client %d: %v", i, err)
		}
		cc()
	}
	t.Logf("registered %d clients over DNS in %s", users, time.Since(regStart).Round(time.Millisecond))

	const text = "load over real DNS — یک پیام آزمایشی"
	lat := make([][]time.Duration, users)
	var okMsgs, errMsgs int64

	// Send phase: each user sends perUser messages to one peer (a real
	// conversation: seq 1..perUser).
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < users; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			peer := clients[(i+1)%users].Identity().Addr
			lat[i] = make([]time.Duration, 0, perUser)
			for m := 0; m < perUser; m++ {
				sctx, sc := context.WithTimeout(ctx, 30*time.Second)
				t0 := time.Now()
				_, err := clients[i].SendMessage(sctx, peer, uint32(m+1), text, nil)
				sc()
				if err != nil {
					atomic.AddInt64(&errMsgs, 1)
				} else {
					atomic.AddInt64(&okMsgs, 1)
					lat[i] = append(lat[i], time.Since(t0))
				}
			}
		}(i)
	}
	wg.Wait()
	sendElapsed := time.Since(start)

	// Receive phase: each user drains its inbox (poll + decrypt + ack).
	var rMsgs, rErr int64
	rstart := time.Now()
	var wg2 sync.WaitGroup
	for i := 0; i < users; i++ {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			for round := 0; round < perUser+2; round++ {
				fctx, fc := context.WithTimeout(ctx, 30*time.Second)
				msgs, err := clients[i].FetchInbox(fctx, nil)
				if err != nil {
					fc()
					atomic.AddInt64(&rErr, 1)
					return
				}
				if len(msgs) == 0 {
					fc()
					return
				}
				maxSeq := map[[protocol.AddressSize]byte]uint32{}
				for _, mm := range msgs {
					if mm.Seq > maxSeq[mm.From] {
						maxSeq[mm.From] = mm.Seq
					}
				}
				for from, seq := range maxSeq {
					_ = clients[i].Ack(fctx, from, seq)
				}
				fc()
				atomic.AddInt64(&rMsgs, int64(len(msgs)))
			}
		}(i)
	}
	wg2.Wait()
	rElapsed := time.Since(rstart)

	var all []time.Duration
	for _, s := range lat {
		all = append(all, s...)
	}
	sort.Slice(all, func(a, b int) bool { return all[a] < all[b] })
	pct := func(p float64) time.Duration {
		if len(all) == 0 {
			return 0
		}
		idx := int(p * float64(len(all)))
		if idx >= len(all) {
			idx = len(all) - 1
		}
		return all[idx]
	}

	t.Logf("==== chat load (transport: REAL DNS over loopback UDP) ====")
	t.Logf("users=%d msgs/user=%d  addr=%s", users, perUser, addr)
	t.Logf("SEND   : %d ok / %d err in %s → %.0f msgs/s", okMsgs, errMsgs, sendElapsed.Round(time.Millisecond), float64(okMsgs)/sendElapsed.Seconds())
	t.Logf("         end-to-end send latency p50=%s p95=%s p99=%s",
		pct(0.50).Round(time.Millisecond), pct(0.95).Round(time.Millisecond), pct(0.99).Round(time.Millisecond))
	t.Logf("RECEIVE: %d msgs fetched+acked / %d err in %s → %.0f msgs/s", rMsgs, rErr, rElapsed.Round(time.Millisecond), float64(rMsgs)/rElapsed.Seconds())

	if errMsgs > 0 || rErr > 0 {
		t.Fatalf("hard errors: send=%d receive=%d", errMsgs, rErr)
	}
}
