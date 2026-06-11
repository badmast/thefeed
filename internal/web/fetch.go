package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
)

// nextFetchDeadline returns the Time when the server will next fetch from Telegram.
// Returns zero value if nextFetch is not set or has already passed.
func (s *Server) nextFetchDeadline() time.Time {
	s.mu.RLock()
	nf := s.nextFetch
	s.mu.RUnlock()
	if nf == 0 {
		return time.Time{}
	}
	t := time.Unix(int64(nf), 0)
	if time.Until(t) <= 0 {
		return time.Time{} // already passed
	}
	return t
}

// waitForServerFetch blocks until the server's Telegram fetch is likely complete
// (nextFetch + 30 s), emitting a countdown progress event each second so the UI
// can render a live progress bar. Returns true on completion, false if ctx cancelled.
func (s *Server) waitForServerFetch(ctx context.Context, nf uint32) bool {
	const serverFetchDuration = 30 * time.Second
	deadline := time.Unix(int64(nf), 0).Add(serverFetchDuration)
	totalWait := time.Until(deadline)
	if totalWait <= 0 {
		totalWait = serverFetchDuration
	}
	totalSec := int(totalWait.Seconds()) + 1

	s.addLog(fmt.Sprintf("SERVER_FETCH_WAIT start %d", totalSec))

	timer := time.NewTimer(totalWait)
	defer timer.Stop()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			s.addLog("SERVER_FETCH_WAIT done")
			return false
		case <-timer.C:
			s.addLog("SERVER_FETCH_WAIT done")
			return true
		case <-ticker.C:
			remaining := int((totalWait - time.Since(start)).Seconds())
			if remaining < 0 {
				remaining = 0
			}
			s.addLog(fmt.Sprintf("SERVER_FETCH_WAIT tick %d/%d", remaining, totalSec))
		}
	}
}

func (s *Server) refreshMetadataOnly() {
	// Don't fetch before resolver scanning has found at least one healthy resolver.
	// The onFirstDone callback in startCheckerThenRefresh is the canonical first trigger.
	s.mu.RLock()
	fetcherEarly := s.fetcher
	s.mu.RUnlock()
	if fetcherEarly != nil && len(fetcherEarly.Resolvers()) == 0 {
		s.addLog("Waiting for resolver scan to complete...")
		return
	}

	// Cancel any in-progress metadata refresh and start a new cancellable one.
	const metaKey = 0
	s.refreshMu.Lock()
	if prev, ok := s.refreshCancels[metaKey]; ok {
		prev()
	}

	s.mu.RLock()
	basectx := s.fetcherCtx
	fetcher := s.fetcher
	cache := s.cache
	s.mu.RUnlock()

	if fetcher == nil || basectx == nil {
		delete(s.refreshCancels, metaKey)
		s.refreshMu.Unlock()
		return
	}

	// Child context: cancelled either by the next refresh call or by a config change.
	ctx, cancel := context.WithCancel(basectx)
	s.refreshCancels[metaKey] = cancel
	s.refreshMu.Unlock()
	defer func() {
		cancel()
		s.refreshMu.Lock()
		delete(s.refreshCancels, metaKey)
		s.refreshMu.Unlock()
	}()

	s.addLog(fmt.Sprintf("Fetching metadata... (%d active resolvers)", len(fetcher.Resolvers())))

	// If the server's next Telegram fetch is imminent (within 15 s), wait for it first.
	if dl := s.nextFetchDeadline(); !dl.IsZero() && time.Until(dl) < 15*time.Second {
		s.mu.RLock()
		nf := s.nextFetch
		s.mu.RUnlock()
		if !s.waitForServerFetch(ctx, nf) {
			return
		}
	}

	meta, err := fetcher.FetchMetadata(ctx)
	if err != nil {
		if ctx.Err() != nil {
			s.addLog("Refresh cancelled")
			return
		}
		// Detect invalid passphrase from crypto errors
		errStr := err.Error()
		if strings.Contains(errStr, "integrity check failed") || strings.Contains(errStr, "message authentication failed") || strings.Contains(errStr, "cipher") {
			s.addLog("Error: Invalid passphrase — check your encryption key in Settings")
		} else {
			s.addLog(fmt.Sprintf("Error: %v", err))
		}
		return
	}

	channels := meta.Channels
	if cache != nil {
		if cached := cache.GetAllTitles(); len(cached) > 0 {
			for i := range channels {
				if t := cached[channels[i].Name]; t != "" {
					channels[i].DisplayName = t
				}
			}
		}
	}

	s.mu.Lock()
	s.channels = channels
	s.telegramLoggedIn = meta.TelegramLoggedIn
	s.chatAvailable = meta.ChatAvailable
	s.nextFetch = meta.NextFetch
	s.metaFetchedAt = time.Now()
	s.mu.Unlock()

	if cache != nil {
		_ = cache.PutMetadata(meta)
	}
	s.saveChannelsCache(channels, meta.NextFetch)

	s.broadcast("event: update\ndata: \"channels\"\n\n")

	needsFetch := false
	for _, ch := range channels {
		if ch.DisplayName == "" {
			needsFetch = true
			break
		}
	}
	if needsFetch {
		go s.ensureTitlesFetched(basectx)
	}

	go s.maybeRefreshProfilePics(basectx)
}

// maybeRefreshProfilePics fires a refresh when GitHub relay is up or
// the user has opted into the DNS path. No-op otherwise; hub coalesces.
func (s *Server) maybeRefreshProfilePics(parentCtx context.Context) {
	s.mu.RLock()
	hub := s.profilePics
	fetcher := s.fetcher
	rc := s.relayInfo
	s.mu.RUnlock()
	if hub == nil || fetcher == nil {
		return
	}
	dnsAllowed := s.profilePicsEnabled()
	githubLikelyUp := false
	if rc != nil {
		ctx, cancel := context.WithTimeout(parentCtx, 5*time.Second)
		info, err := rc.get(ctx, fetcher)
		cancel()
		if err == nil && info.GitHubRepo != "" {
			githubLikelyUp = true
		}
	}
	if !dnsAllowed && !githubLikelyUp {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	onStored := func(string) {
		s.broadcast("event: update\ndata: \"profile-pics\"\n\n")
	}
	if err := hub.refresh(ctx, fetcher, dnsAllowed, s.fetchFromGitHubRelayBytes, onStored); err == nil {
		s.broadcast("event: update\ndata: \"profile-pics\"\n\n")
	}
}

// ensureTitlesFetched fetches channel display names from TitlesChannel in the background.
// At most one fetch runs at a time; errors impose a 5-minute backoff so that an
// outdated server does not cause endless retries.
func (s *Server) ensureTitlesFetched(ctx context.Context) {
	s.titlesMu.Lock()
	if s.titlesLoading || time.Now().Before(s.titlesBackoffUntil) {
		s.titlesMu.Unlock()
		return
	}
	s.titlesLoading = true
	s.titlesMu.Unlock()

	defer func() {
		s.titlesMu.Lock()
		s.titlesLoading = false
		s.titlesMu.Unlock()
	}()

	s.mu.RLock()
	fetcher := s.fetcher
	cache := s.cache
	s.mu.RUnlock()

	if fetcher == nil {
		return
	}

	titles, err := fetcher.FetchTitles(ctx)
	if err != nil && ctx.Err() == nil {
		s.titlesMu.Lock()
		s.titlesBackoffUntil = time.Now().Add(5 * time.Minute)
		s.titlesMu.Unlock()
		return
	}
	if len(titles) == 0 {
		// Server doesn't support TitlesChannel or has no titles yet; back off.
		s.titlesMu.Lock()
		s.titlesBackoffUntil = time.Now().Add(5 * time.Minute)
		s.titlesMu.Unlock()
		return
	}

	if cache != nil {
		for name, title := range titles {
			_ = cache.PutTitle(name, title)
		}
	}

	s.mu.Lock()
	channels := s.channels
	updated := false
	for i := range channels {
		if t, ok := titles[channels[i].Name]; ok && t != "" && channels[i].DisplayName != t {
			channels[i].DisplayName = t
			updated = true
		}
	}
	s.channels = channels
	nextFetch := s.nextFetch
	s.mu.Unlock()

	if updated {
		s.saveChannelsCache(channels, nextFetch)
		s.broadcast("event: update\ndata: \"channels\"\n\n")
	}
}

func (s *Server) refreshChannel(channelNum int) {
	// Prevent duplicate fetches for the same channel
	s.refreshMu.Lock()
	if _, running := s.refreshCancels[channelNum]; running {
		s.refreshMu.Unlock()
		return
	}

	s.mu.RLock()
	basectx := s.fetcherCtx
	fetcher := s.fetcher
	cache := s.cache
	s.mu.RUnlock()

	if fetcher == nil || basectx == nil {
		s.refreshMu.Unlock()
		return
	}

	ctx, cancel := context.WithCancel(basectx)
	s.refreshCancels[channelNum] = cancel
	s.refreshMu.Unlock()
	defer func() {
		cancel()
		s.refreshMu.Lock()
		delete(s.refreshCancels, channelNum)
		s.refreshMu.Unlock()
	}()

	// Use the cached in-memory metadata if it is fresh enough (< metaCacheTTL, default 30 sec).
	// This avoids a redundant metadata DNS fetch for every channel refresh.
	// If the metadata is stale (or was never fetched), fetch it from DNS now.
	s.mu.RLock()
	ttl := s.metaCacheTTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	// Cap TTL at the time remaining until the server's next Telegram fetch.
	// If nextFetch is sooner than our TTL the cached metadata may already be stale.
	if nf := s.nextFetch; nf > 0 {
		if rem := time.Until(time.Unix(int64(nf), 0)); rem > 0 && rem < ttl {
			ttl = rem
		}
	}
	cachedChannels := s.channels
	cachedAge := time.Since(s.metaFetchedAt)
	s.mu.RUnlock()

	var meta *protocol.Metadata
	if len(cachedChannels) > 0 && cachedAge < ttl {
		// Build a lightweight Metadata from the cached fields to keep the rest of the
		// function unchanged.
		s.mu.RLock()
		meta = &protocol.Metadata{
			Channels:         s.channels,
			TelegramLoggedIn: s.telegramLoggedIn,
			NextFetch:        s.nextFetch,
		}
		s.mu.RUnlock()
	} else {
		var err error
		meta, err = fetcher.FetchMetadata(ctx)
		if err != nil {
			if ctx.Err() != nil {
				s.addLog("Refresh cancelled")
				return
			}
			errStr := err.Error()
			if strings.Contains(errStr, "integrity check failed") || strings.Contains(errStr, "message authentication failed") || strings.Contains(errStr, "cipher") {
				s.addLog("Error: Invalid passphrase — check your encryption key in Settings")
			} else {
				s.addLog(fmt.Sprintf("Error: %v", err))
			}
			return
		}
		channels := meta.Channels
		if cache != nil {
			if cached := cache.GetAllTitles(); len(cached) > 0 {
				for i := range channels {
					if t := cached[channels[i].Name]; t != "" {
						channels[i].DisplayName = t
					}
				}
			}
		}
		meta.Channels = channels
		s.mu.Lock()
		s.channels = channels
		s.telegramLoggedIn = meta.TelegramLoggedIn
		s.chatAvailable = meta.ChatAvailable
		s.nextFetch = meta.NextFetch
		s.metaFetchedAt = time.Now()
		s.mu.Unlock()
		if cache != nil {
			_ = cache.PutMetadata(meta)
		}
		s.saveChannelsCache(channels, meta.NextFetch)
		s.broadcast("event: update\ndata: \"channels\"\n\n")
		needsFetch := false
		for _, ch := range channels {
			if ch.DisplayName == "" {
				needsFetch = true
				break
			}
		}
		if needsFetch {
			go s.ensureTitlesFetched(basectx)
		}
	}

	channels := meta.Channels
	if channelNum < 1 || channelNum > len(channels) {
		s.addLog(fmt.Sprintf("Warning: channel %d is not available", channelNum))
		return
	}

	ch := channels[channelNum-1]

	// Skip refresh if the last message ID and content hash haven't changed
	// AND we already have messages stored for this channel.
	s.mu.RLock()
	prevID := s.lastMsgIDs[channelNum]
	prevHash := s.lastHashes[channelNum]
	prevMsgs := s.messages[channelNum]
	s.mu.RUnlock()
	if prevID > 0 && ch.LastMsgID == prevID && ch.ContentHash == prevHash && len(prevMsgs) > 0 {
		s.addLog(fmt.Sprintf("Channel %s: no changes (last ID: %d)", ch.Name, prevID))
		s.broadcast(fmt.Sprintf("event: update\ndata: {\"type\":\"no_changes\",\"channel\":%d}\n\n", channelNum))
		return
	}

	blockCount := int(ch.Blocks)
	if blockCount <= 0 {
		s.mu.Lock()
		s.messages[channelNum] = nil
		s.lastMsgIDs[channelNum] = ch.LastMsgID
		s.lastHashes[channelNum] = ch.ContentHash
		s.mu.Unlock()
		s.addLog(fmt.Sprintf("Updated %s: 0 messages", ch.Name))
		s.broadcast(fmt.Sprintf("event: update\ndata: {\"type\":\"messages\",\"channel\":%d}\n\n", channelNum))
		return
	}

	// Wrap the context with a deadline at the server's next Telegram fetch.
	// If the server starts fetching during our block download we cancel early,
	// wait for the fresh data to land, then restart this channel fetch.
	fetchCtx := ctx
	var fetchCancel context.CancelFunc
	var fetchNF uint32
	if dl := s.nextFetchDeadline(); !dl.IsZero() {
		s.mu.RLock()
		fetchNF = s.nextFetch
		s.mu.RUnlock()

		// If the server's next refresh is within 15 seconds, wait for it rather
		// than risking a block-version race (metadata says N blocks but the
		// server regenerates them mid-download).
		if time.Until(dl) < 15*time.Second {
			s.addLog("Server refresh imminent — waiting before fetching blocks")
			if !s.waitForServerFetch(ctx, fetchNF) {
				return
			}
			// Re-fetch metadata after the server refresh to get fresh block counts.
			freshMeta, freshErr := fetcher.FetchMetadata(ctx)
			if freshErr != nil {
				if ctx.Err() != nil {
					s.addLog("Refresh cancelled")
					return
				}
				s.addLog(fmt.Sprintf("Channel %s error refreshing metadata: %v", ch.Name, freshErr))
				return
			}
			s.mu.Lock()
			s.channels = freshMeta.Channels
			s.telegramLoggedIn = freshMeta.TelegramLoggedIn
			s.chatAvailable = freshMeta.ChatAvailable
			s.nextFetch = freshMeta.NextFetch
			s.metaFetchedAt = time.Now()
			s.mu.Unlock()
			if cache != nil {
				_ = cache.PutMetadata(freshMeta)
			}
			s.saveChannelsCache(freshMeta.Channels, freshMeta.NextFetch)
			if channelNum < 1 || channelNum > len(freshMeta.Channels) {
				return
			}
			ch = freshMeta.Channels[channelNum-1]
			blockCount = int(ch.Blocks)
			if blockCount <= 0 {
				return
			}
		}

		// Refresh the deadline after potential wait.
		dl = s.nextFetchDeadline()
		if !dl.IsZero() {
			fetchCtx, fetchCancel = context.WithDeadline(ctx, dl)
			defer fetchCancel()
		}
	}

	// Fetch blocks with content-hash verification.  On hash mismatch (the
	// server regenerated blocks between our metadata fetch and block fetch)
	// re-fetch metadata and retry up to 2 times.
	const maxHashRetries = 2
	var msgs []protocol.Message
	var err error
	for attempt := 0; ; attempt++ {
		msgs, err = fetcher.FetchChannelVerified(fetchCtx, channelNum, blockCount, ch.ContentHash)
		if err == nil {
			break
		}
		if errors.Is(err, client.ErrContentHashMismatch) && attempt < maxHashRetries {
			s.addLog(fmt.Sprintf("Channel %s: block-version race detected, re-fetching metadata (attempt %d/%d)", ch.Name, attempt+1, maxHashRetries))
			freshMeta, freshErr := fetcher.FetchMetadata(ctx)
			if freshErr != nil {
				s.addLog(fmt.Sprintf("Channel %s error refreshing metadata: %v", ch.Name, freshErr))
				return
			}
			s.mu.Lock()
			s.channels = freshMeta.Channels
			s.telegramLoggedIn = freshMeta.TelegramLoggedIn
			s.chatAvailable = freshMeta.ChatAvailable
			s.nextFetch = freshMeta.NextFetch
			s.metaFetchedAt = time.Now()
			s.mu.Unlock()
			if cache != nil {
				_ = cache.PutMetadata(freshMeta)
			}
			s.saveChannelsCache(freshMeta.Channels, freshMeta.NextFetch)
			if channelNum < 1 || channelNum > len(freshMeta.Channels) {
				return
			}
			ch = freshMeta.Channels[channelNum-1]
			blockCount = int(ch.Blocks)
			if blockCount <= 0 {
				return
			}
			continue // retry with fresh metadata
		}
		if errors.Is(err, client.ErrExtraBlockInvalid) {
			// The server signature didn't match — messages were rejected as a
			// possible tamper. Surface a toast (client localises the key).
			s.addLog(fmt.Sprintf("Channel %s: signature INVALID — messages rejected (check the server key)", ch.Name))
			s.broadcast("event: toast\ndata: \"signature_invalid_toast\"\n\n")
			return
		}
		if fetchCancel != nil && fetchCtx.Err() == context.DeadlineExceeded {
			// nextFetch fired mid-download — wait for the server, then re-fetch.
			fetchCancel()
			if s.waitForServerFetch(ctx, fetchNF) {
				go s.refreshChannel(channelNum)
			}
			return
		}
		if ctx.Err() != nil {
			s.addLog("Refresh cancelled")
			return
		}
		s.addLog(fmt.Sprintf("Channel %s error: %v", ch.Name, err))
		return
	}

	s.mu.Lock()
	s.messages[channelNum] = msgs
	// Only store the metadata IDs when we actually received messages.
	// If the fetch returned 0 messages but the channel has content (LastMsgID > 0),
	// keep the old IDs so the next refresh will try a full fetch instead of skipping.
	if len(msgs) > 0 || ch.LastMsgID == 0 {
		s.lastMsgIDs[channelNum] = ch.LastMsgID
		s.lastHashes[channelNum] = ch.ContentHash
	}
	s.mu.Unlock()

	if cache != nil {
		if result, mergeErr := cache.MergeAndPut(ch.Name, msgs); mergeErr == nil {
			// Replace the in-memory store with the full merged history.
			s.mu.Lock()
			s.messages[channelNum] = result.Messages
			s.mu.Unlock()
		}
	}

	s.addLog(fmt.Sprintf("Updated %s: %d messages", ch.Name, len(msgs)))
	s.broadcast(fmt.Sprintf("event: update\ndata: {\"type\":\"messages\",\"channel\":%d}\n\n", channelNum))
}
