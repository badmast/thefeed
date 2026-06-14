package server

import (
	"context"
	"crypto/rand"
	"fmt"
	"hash/crc32"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// loadClient is one simulated user with its own session and counters. Each
// runs on its own goroutine, so its mutable fields need no locking.
type loadClient struct {
	c   simClient
	ref [protocol.ChatSelectorSize]byte
	ks  [protocol.KeySize]byte
	ctr uint32 // session op counter
	seq uint32 // outbound message seq (global per sender)
}

func loadEnvInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// loadRegister runs a register handshake without t.Fatal (goroutine-safe).
func loadRegister(svc *ChatService, qk [protocol.KeySize]byte, ekPub []byte, c simClient) (ref [protocol.ChatSelectorSize]byte, ks [protocol.KeySize]byte, err error) {
	eph, _ := protocol.GenerateEphemeralKey()
	ks, _ = protocol.ChatSessionKey(eph, ekPub, protocol.ChatProtocolVersion, qk)
	var tag [protocol.ChatSelectorSize]byte
	_, _ = rand.Read(tag[:])
	protocol.ChatMarkHandshakeSelector(&tag)
	rec, e := protocol.EncodeRegisterEnvelope(c.id, c.enc.PublicKey().Bytes(), uint32(time.Now().Unix()))
	if e != nil {
		return ref, ks, e
	}
	sealedBoot := protocol.SealChat(ks, tag[:], protocol.ChatBootstrapCounter(), rec)
	stream := protocol.BuildChatHandshakeStream(eph.PublicKey().Bytes(), protocol.ChatProtocolVersion, protocol.ChatHandshakeRegister, sealedBoot)
	n := (len(stream) + protocol.ChatCellPayloadSize - 1) / protocol.ChatCellPayloadSize
	var last []byte
	for i := 0; i < n; i++ {
		start := i * protocol.ChatCellPayloadSize
		end := start + protocol.ChatCellPayloadSize
		if end > len(stream) {
			end = len(stream)
		}
		chunk := make([]byte, protocol.ChatCellPayloadSize)
		copy(chunk, stream[start:end])
		last = svc.HandleCell(tag, uint32(i), chunk, simDomain, time.Now())
	}
	st, body, e := protocol.OpenChatResponse(ks, tag, protocol.ChatBootstrapCounter(), last)
	if e != nil {
		return ref, ks, e
	}
	if st != protocol.ChatStatusOK || len(body) < 4+protocol.ChatSelectorSize {
		return ref, ks, fmt.Errorf("register st=%d", st)
	}
	copy(ref[:], body[4:4+protocol.ChatSelectorSize])
	return ref, ks, nil
}

func loadOp(svc *ChatService, lc *loadClient, plain []byte) (byte, []byte, error) {
	pad := protocol.ChatCellPlainSize
	if protocol.ChatPlainOp(plain) != protocol.ChatOpFrag {
		pad += protocol.ChatCellJitter(svc.queryKey, lc.ref, lc.ctr)
	}
	payload, _ := protocol.SealChatCellPayloadN(lc.ks, lc.ref, lc.ctr, plain, pad)
	resp := svc.HandleCell(lc.ref, lc.ctr, payload, simDomain, time.Now())
	st, body, err := protocol.OpenChatResponse(lc.ks, lc.ref, lc.ctr, resp)
	lc.ctr++
	return st, body, err
}

// loadDrain runs one receive cycle for a user: STATUS, FETCH every block of
// every waiting message, then ACK each sender up to its highest seq. Returns
// how many messages were fetched.
func loadDrain(svc *ChatService, lc *loadClient) (int, error) {
	st, body, err := loadOp(svc, lc, protocol.BuildChatStatusPlain())
	if err != nil {
		return 0, err
	}
	if st != protocol.ChatStatusOK || len(body) < 7 {
		return 0, fmt.Errorf("status st=%d", st)
	}
	count := int(body[6])
	if count == 0 {
		return 0, nil
	}
	const entryLen = protocol.AddressSize + 4 + 2 + 1
	maxSeq := map[[protocol.AddressSize]byte]uint32{}
	received := 0
	for i := 0; i < count; i++ {
		off := 7 + i*entryLen
		if off+entryLen > len(body) {
			break
		}
		var src [protocol.AddressSize]byte
		copy(src[:], body[off:])
		seq := binaryBE32(body[off+protocol.AddressSize:])
		blocks := body[off+protocol.AddressSize+6]
		for blk := uint8(0); blk < blocks; blk++ {
			if st, _, err := loadOp(svc, lc, protocol.BuildChatFetchPlain(protocol.ChatPeerHandle(src), seq, blk)); err != nil || st != protocol.ChatStatusOK {
				return received, fmt.Errorf("fetch st=%d err=%v", st, err)
			}
		}
		received++
		if seq > maxSeq[src] {
			maxSeq[src] = seq
		}
	}
	for src, seq := range maxSeq {
		h := protocol.ChatPeerHandle(src)
		if st, _, err := loadOp(svc, lc, protocol.BuildChatAckPlain(h, seq, [protocol.ChatReceiptMACSize]byte{})); err != nil || st != protocol.ChatStatusOK {
			return received, fmt.Errorf("ack st=%d err=%v", st, err)
		}
	}
	return received, nil
}

func binaryBE32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// TestChatLoad ramps concurrent users × messages against an in-process service
// and reports throughput / latency / peak sessions. Opt-in (it is not part of
// the default suite):
//
//	CHAT_LOAD=1 CHAT_USERS=500 CHAT_MSGS=20 go test ./internal/server/ -run TestChatLoad -v -timeout 20m
func TestChatLoad(t *testing.T) {
	if os.Getenv("CHAT_LOAD") == "" {
		t.Skip("set CHAT_LOAD=1 to run the chat load test")
	}
	users := loadEnvInt("CHAT_USERS", 200)
	perUser := loadEnvInt("CHAT_MSGS", 20)

	// Generous limits so quotas don't mask throughput.
	limits := protocol.DefaultChatLimits()
	limits.SendPerHour = 60000
	limits.InboxCap = 60000
	limits.PerPairCap = 60000
	store, err := OpenChatStore(t.TempDir()+"/chatload.db", limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	// Match the production default (periodic group durability) so the numbers
	// reflect shipped behavior. CHAT_SYNC_MS overrides the interval.
	syncMS := loadEnvInt("CHAT_SYNC_MS", 1000)
	store.EnablePeriodicSync(time.Duration(syncMS) * time.Millisecond)
	syncCtx, syncCancel := context.WithCancel(context.Background())
	go store.RunSync(syncCtx)
	t.Cleanup(syncCancel)
	ek, _ := protocol.GenerateEphemeralKey()
	qk, _, _ := protocol.DeriveKeys("svc-pass")
	svc := NewChatService(store, ek, qk, limits, []string{simDomain})
	ekPub := ek.PublicKey().Bytes()

	// Pre-register all users sequentially (handshake once each).
	clients := make([]*loadClient, users)
	for i := 0; i < users; i++ {
		c := newSimClient(t)
		ref, ks, err := loadRegister(svc, qk, ekPub, c)
		if err != nil {
			t.Fatalf("register user %d: %v", i, err)
		}
		clients[i] = &loadClient{c: c, ref: ref, ks: ks}
	}
	t.Logf("registered %d users", users)

	// Sample peak live sessions during the run.
	var peakSessions int64
	stopSampler := make(chan struct{})
	go func() {
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stopSampler:
				return
			case <-tk.C:
				if s := svc.StatsSnapshot()["sessions"]; s > atomic.LoadInt64(&peakSessions) {
					atomic.StoreInt64(&peakSessions, s)
				}
			}
		}
	}()

	const body = "load test message — یک پیام آزمایشی برای سنجش بار"
	var totalMsgs, okMsgs, backpressure, errs, totalQueries int64
	lat := make([][]time.Duration, users)

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < users; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lc := clients[i]
			lat[i] = make([]time.Duration, 0, perUser)
			for m := 0; m < perUser; m++ {
				peer := clients[(i+1+m)%users] // spread across recipients
				lc.seq++
				contentKey, _ := protocol.ChatContentKey(lc.c.enc, peer.c.enc.PublicKey().Bytes(), lc.c.addr, peer.c.addr, lc.seq)
				kss, _ := protocol.ChatServerSharedKey(lc.c.enc, ekPub, lc.c.enc.PublicKey().Bytes(), ekPub)
				env, _ := protocol.EncodeChatMessage(contentKey, kss, lc.c.addr, peer.c.addr, lc.seq, body)

				t0 := time.Now()
				q := int64(1) // send-start
				st, _, err := loadOp(svc, lc, protocol.BuildChatSendStartPlain(peer.c.addr, uint16(len(env))))
				if err == nil && st == protocol.ChatStatusOK {
					chunks := protocol.SplitChunks(env, protocol.ChatDataChunkSize)
					for ci, ch := range chunks {
						d, _ := protocol.BuildChatDataPlain(uint8(ci), ch)
						loadOp(svc, lc, d)
					}
					q += int64(len(chunks)) + 1 // data chunks + fin
					st, _, err = loadOp(svc, lc, protocol.BuildChatFinPlain(crc32.ChecksumIEEE(env)))
				}
				atomic.AddInt64(&totalQueries, q)
				atomic.AddInt64(&totalMsgs, 1)
				switch {
				case err != nil:
					atomic.AddInt64(&errs, 1)
				case st == protocol.ChatStatusOK:
					atomic.AddInt64(&okMsgs, 1)
					lat[i] = append(lat[i], time.Since(t0))
				default: // quota / inbox-full / pair-quota = backpressure, not an error
					atomic.AddInt64(&backpressure, 1)
				}
			}
		}(i)
	}
	wg.Wait()
	sendElapsed := time.Since(start)

	pcts := func(lat [][]time.Duration) (p50, p95, p99 time.Duration, n int) {
		var all []time.Duration
		for _, s := range lat {
			all = append(all, s...)
		}
		sort.Slice(all, func(a, b int) bool { return all[a] < all[b] })
		n = len(all)
		at := func(p float64) time.Duration {
			if n == 0 {
				return 0
			}
			idx := int(p * float64(n))
			if idx >= n {
				idx = n - 1
			}
			return all[idx]
		}
		return at(0.50), at(0.95), at(0.99), n
	}

	// ---- receive phase: every user drains its inbox (STATUS + FETCH + ACK) ----
	rlat := make([][]time.Duration, users)
	var rMsgs, rErr int64
	rstart := time.Now()
	var wg2 sync.WaitGroup
	for i := 0; i < users; i++ {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			lc := clients[i]
			for round := 0; round < perUser+2; round++ {
				t0 := time.Now()
				n, err := loadDrain(svc, lc)
				if err != nil {
					atomic.AddInt64(&rErr, 1)
					return
				}
				if n == 0 {
					return
				}
				rlat[i] = append(rlat[i], time.Since(t0))
				atomic.AddInt64(&rMsgs, int64(n))
			}
		}(i)
	}
	wg2.Wait()
	rElapsed := time.Since(rstart)
	close(stopSampler)

	sp50, sp95, sp99, _ := pcts(lat)
	rp50, rp95, rp99, _ := pcts(rlat)

	t.Logf("==== chat load (transport: IN-PROCESS — no DNS/network; this is the server-side ceiling) ====")
	t.Logf("users=%d msgs/user=%d", users, perUser)
	t.Logf("SEND   : %d ok / %d bp / %d err in %s → %.0f msgs/s  (~%.0f queries/s)",
		okMsgs, backpressure, errs, sendElapsed.Round(time.Millisecond),
		float64(okMsgs)/sendElapsed.Seconds(), float64(atomic.LoadInt64(&totalQueries))/sendElapsed.Seconds())
	t.Logf("         send-cycle latency p50=%s p95=%s p99=%s",
		sp50.Round(time.Microsecond), sp95.Round(time.Microsecond), sp99.Round(time.Microsecond))
	t.Logf("RECEIVE: %d msgs fetched+acked / %d err in %s → %.0f msgs/s",
		rMsgs, rErr, rElapsed.Round(time.Millisecond), float64(rMsgs)/rElapsed.Seconds())
	t.Logf("         drain-cycle latency p50=%s p95=%s p99=%s",
		rp50.Round(time.Microsecond), rp95.Round(time.Microsecond), rp99.Round(time.Microsecond))
	t.Logf("peak live sessions=%d  accounts=%d", atomic.LoadInt64(&peakSessions), svc.StatsSnapshot()["accounts"])

	if errs > 0 || rErr > 0 {
		t.Fatalf("hard errors during load: send=%d receive=%d", errs, rErr)
	}
}
