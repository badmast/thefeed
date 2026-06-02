package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
	"github.com/sartoopjj/thefeed/internal/version"
)

//go:embed static
var staticFS embed.FS

// Config holds the client configuration saved in the data directory.
type Config struct {
	Domain    string   `json:"domain"`
	Key       string   `json:"key"`
	Resolvers []string `json:"resolvers"`
	QueryMode string   `json:"queryMode"`
	RateLimit float64  `json:"rateLimit"`
	// Timeout is the per-query DNS timeout in seconds (0 = default 15 s).
	// Also used as the resolver health-check probe timeout.
	Timeout float64 `json:"timeout,omitempty"`
	// Scatter is the number of resolvers queried simultaneously per DNS block request
	// (0 or 1 = sequential, 4 = default parallel pair).
	Scatter int `json:"scatter,omitempty"`
	// AutoScan enables hourly automatic resolver health-check scans.
	// nil means enabled (default); set to a false pointer to disable.
	AutoScan *bool `json:"autoScan,omitempty"`
}

// Profile wraps a Config with a user-chosen nickname and a unique ID.
type Profile struct {
	ID                 string   `json:"id"`
	Nickname           string   `json:"nickname"`
	Config             Config   `json:"config"`
	AutoUpdate         []string `json:"autoUpdate,omitempty"`
	AutoUpdateInterval int      `json:"autoUpdateInterval,omitempty"`
	// PinnedChannels stores the user's pinned channel names (stripped of @).
	// Pinned channels render at the top of each section in the sidebar.
	PinnedChannels []string `json:"pinnedChannels,omitempty"`
}

const (
	// minAutoUpdateInterval is the floor — never tick faster than once per
	// minute, even if the user sets something silly. The DNS path is
	// expensive and the server's own fetch cycle is much longer.
	minAutoUpdateInterval = 60 * time.Second
	// serverFetchSettleDelay is how long after nextFetch we wait before
	// asking the server for fresh data — gives it time to process the
	// upstream Telegram fetch and have a coherent metadata snapshot.
	serverFetchSettleDelay = 30 * time.Second
	// autoUpdateStartupDelay defers the first tick so the initial metadata
	// + resolver checks have a chance to land before we start polling.
	autoUpdateStartupDelay = 30 * time.Second
)

// SavedResolverScore stores persistent resolver performance data.
type SavedResolverScore struct {
	Success int64 `json:"success"`
	Failure int64 `json:"failure"`
	TotalMs int64 `json:"totalMs"`
}

// ActiveList is a named subset of the resolver bank that the user can
// switch between — e.g., "Home", "Office", "Mobile". The currently
// selected list is what the fetcher uses; switching to a different list
// hot-swaps the active resolvers without rescanning.
type ActiveList struct {
	Name      string   `json:"name"`
	Resolvers []string `json:"resolvers"`
	LastUsed  int64    `json:"lastUsed,omitempty"`
}

// ProfileList is the on-disk structure for profiles.json.
type ProfileList struct {
	Active   string    `json:"active"` // ID of active profile
	Profiles []Profile `json:"profiles"`
	// FontSize stores user's preferred font size (0 = default 14).
	FontSize int    `json:"fontSize,omitempty"`
	Debug    bool   `json:"debug,omitempty"`
	Theme    string `json:"theme,omitempty"`
	Lang     string `json:"lang,omitempty"`
	// SkipUpdateVersion is the latest release the user dismissed.
	SkipUpdateVersion string `json:"skipUpdateVersion,omitempty"`
	// ResolverBank is the shared pool of DNS resolvers used by all profiles.
	ResolverBank []string `json:"resolverBank,omitempty"`
	// ResolverScores stores accumulated performance data for bank resolvers.
	ResolverScores map[string]*SavedResolverScore `json:"resolverScores,omitempty"`
	// ActiveLists holds named resolver subsets so the user can keep one
	// list per situation (home, office, mobile data) and switch by name
	// instead of rescanning. The currently-selected list is named in
	// SelectedList; its resolvers are what the fetcher uses.
	//
	// Migration: legacy installs without ActiveLists are upgraded on
	// first load — their last_scan.json (or current bank) becomes a
	// single list named "Default".
	ActiveLists  []ActiveList `json:"activeLists,omitempty"`
	SelectedList string       `json:"selectedList,omitempty"`
	// ScanPromptOff suppresses the startup "scan resolvers?" prompt.
	// Persisted server-side so it survives Android's per-launch port
	// changes (each launch picks a fresh port → different localStorage
	// origin → flag was lost on every restart).
	ScanPromptOff bool `json:"scanPromptOff,omitempty"`

	// ProfilePicsEnabled enables fetching avatars over DNS when the
	// GitHub relay can't serve them. Off by default.
	ProfilePicsEnabled bool `json:"profilePicsEnabled,omitempty"`

	// Connection settings (formerly per-profile). 0 = use default.
	QueryMode string  `json:"queryMode,omitempty"` // "single" or "double"
	RateLimit float64 `json:"rateLimit,omitempty"` // parallel block fetches
	Scatter   int     `json:"scatter,omitempty"`   // resolvers per block
	Timeout   float64 `json:"timeout,omitempty"`   // DNS query timeout (s)

	// ResolverCacheShare gates the shared-resolver-cache feature. Pointer
	// (not plain bool) so we can tell "user never set it" (nil → default
	// on) apart from "user explicitly disabled" (false). New installs and
	// upgrades from before this field existed get the feature on; an
	// explicit opt-out persists across restarts.
	ResolverCacheShare *bool `json:"resolverCacheShare,omitempty"`

	// SeenIDs maps channel name → last-seen message ID. It's the baseline
	// for the per-channel unread-count badge. Persisted server-side so the
	// counts survive the client's loopback port changing (which wipes the
	// WebView localStorage these markers used to live in).
	SeenIDs map[string]int64 `json:"seenIds,omitempty"`
	// SeenHashes maps channel name → last-seen content hash, used for the
	// X/Twitter "NEW" badge where message IDs aren't sequential.
	SeenHashes map[string]int64 `json:"seenHashes,omitempty"`
}

// ShareEnabled returns whether the shared-resolver-cache feature is on.
// Default is on; only an explicit *false disables it.
func (pl *ProfileList) ShareEnabled() bool {
	if pl == nil || pl.ResolverCacheShare == nil {
		return true
	}
	return *pl.ResolverCacheShare
}

// Defaults for the connection settings exposed on the settings page.
const (
	defaultQueryMode = "single"
	defaultRateLimit = 10.0
	defaultScatter   = 6
	defaultTimeout   = 10.0
)

// connectionSettings returns the effective values from pl, substituting
// defaults for any zero field. Centralises the fallback so every place
// that builds a fetcher reads the same numbers.
func connectionSettings(pl *ProfileList) (queryMode string, rateLimit float64, scatter int, timeout float64) {
	queryMode = defaultQueryMode
	rateLimit = defaultRateLimit
	scatter = defaultScatter
	timeout = defaultTimeout
	if pl == nil {
		return
	}
	if pl.QueryMode != "" {
		queryMode = pl.QueryMode
	}
	if pl.RateLimit > 0 {
		rateLimit = pl.RateLimit
	}
	if pl.Scatter > 0 {
		scatter = pl.Scatter
	}
	if pl.Timeout > 0 {
		timeout = pl.Timeout
	}
	return
}

// lastScanData is the on-disk structure for last_scan.json.
type lastScanData struct {
	Resolvers []string `json:"resolvers"`
	ScannedAt int64    `json:"scannedAt"`
}

// channelsCacheEntry is one profile's startup snapshot.
type channelsCacheEntry struct {
	Channels  []protocol.ChannelInfo `json:"channels"`
	NextFetch uint32                 `json:"nextFetch"`
	SavedAt   int64                  `json:"savedAt"`
}

// channelsCacheFile maps profile ID → snapshot.
type channelsCacheFile map[string]*channelsCacheEntry

// Server is the web UI server for thefeed client.
type Server struct {
	dataDir  string
	port     int
	host     string
	password string // admin password; empty means no auth
	// sharedBackend marks a multi-user deployment (e.g. --host 0.0.0.0 with
	// --shared). The UI then keeps unread/seen state per-browser in
	// localStorage instead of the shared server profiles.json, so connected
	// users don't clear each other's unread counts.
	sharedBackend bool

	mu               sync.RWMutex
	config           *Config
	fetcher          *client.Fetcher
	cache            *client.Cache
	channels         []protocol.ChannelInfo
	messages         map[int][]protocol.Message
	telegramLoggedIn bool
	nextFetch        uint32
	latestVersion    string
	lastMsgIDs       map[int]uint32 // last seen message IDs per channel
	lastHashes       map[int]uint32 // last seen content hashes per channel

	// checker is the active resolver health-checker; set by initFetcher.
	checker *client.ResolverChecker

	// metaFetchedAt is when channels/nextFetch were last fetched from DNS.
	// refreshChannel reuses the in-memory metadata when it is younger than metaCacheTTL.
	metaFetchedAt time.Time
	metaCacheTTL  time.Duration

	// fetcherCtx/fetcherCancel control the lifetime of the active fetcher's
	// background goroutines (rate limiter, noise, resolver checker).
	// They are cancelled and recreated each time the config changes.
	fetcherCtx    context.Context
	fetcherCancel context.CancelFunc

	// refreshMu / refreshCancels allow a new refresh to cancel an in-progress one.
	// Each channel gets its own cancel func so concurrent channel refreshes are allowed.
	// Key 0 is reserved for metadata-only refreshes.
	refreshMu      sync.Mutex
	refreshCancels map[int]context.CancelFunc

	logMu    sync.RWMutex
	logLines []string

	sseMu   sync.Mutex
	clients map[chan string]struct{}

	stopRefresh chan struct{}

	scanner *client.ResolverScanner

	// titlesMu guards the background title-fetch state.
	// Only one goroutine fetches titles at a time; errors impose a 5-minute backoff.
	titlesMu           sync.Mutex
	titlesLoading      bool
	titlesBackoffUntil time.Time

	// dlMu guards dlProgress. Active media downloads register their block
	// counter here so the frontend can poll /api/media/progress and show
	// per-block updates instead of waiting for byte chunks.
	dlMu       sync.Mutex
	dlProgress map[string]*mediaDLProgress

	// relayInfo caches the latest answer from RelayInfoChannel so the fast
	// media path doesn't pay a DNS round trip per file.
	relayInfo *relayCache

	// mediaCache is a disk-backed store for downloaded media bytes so that
	// multiple devices on the same network share a single DNS-tunnelled
	// fetch. Entries expire after 7 days.
	mediaCache *mediaDiskCache

	// rescanReplaceList: set by handleRescan, consumed (and cleared)
	// by the next SetOnScanDone callback so an explicit user rescan
	// overwrites the selected list instead of just topping it up.
	rescanFlagMu      sync.Mutex
	rescanReplaceList bool

	// profilesMu serialises read-modify-write cycles on profiles.json.
	profilesMu sync.Mutex

	// Optional, removable backup feed (Telegram-via-Translate proxy).
	telemirror *telemirrorHub

	// Optional per-channel profile pictures cache.
	profilePics *profilePicsHub
}

// New creates a new web server.
func New(dataDir string, port int, host string, password string) (*Server, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	// Remove stale cache files on every startup, even before a config is loaded.
	go func() {
		if c, err := client.NewCache(filepath.Join(dataDir, "cache")); err == nil {
			_ = c.Cleanup()
		}
	}()

	scanner := client.NewResolverScanner()

	mediaCache, mcErr := newMediaDiskCache(filepath.Join(dataDir, "media-cache"), 7*24*time.Hour)
	if mcErr != nil {
		log.Printf("Warning: media disk cache disabled: %v", mcErr)
	}

	s := &Server{
		dataDir:        dataDir,
		port:           port,
		host:           host,
		password:       password,
		messages:       make(map[int][]protocol.Message),
		clients:        make(map[chan string]struct{}),
		refreshCancels: make(map[int]context.CancelFunc),
		lastMsgIDs:     make(map[int]uint32),
		lastHashes:     make(map[int]uint32),
		scanner:        scanner,
		mediaCache:     mediaCache,
		dlProgress:     make(map[string]*mediaDLProgress),
		relayInfo:      newRelayCache(),
		profilePics:    newProfilePicsHub(dataDir),
	}
	// Set up telemirror with an onUpdate hook so background refreshes
	// push an SSE event; the frontend re-fetches the active channel
	// when it sees the matching event.
	s.telemirror = newTelemirrorHub(dataDir, func(username string) {
		s.broadcast("event: update\ndata: \"telemirror:" + username + "\"\n\n")
	})

	if mediaCache != nil {
		go mediaCache.Cleanup()
		go s.runMediaCacheSweep()
	}

	// Migrate per-profile resolvers into the shared bank on first run.
	s.migrateResolverBank()

	cfg, err := s.loadConfig()
	if err == nil {
		s.config = cfg
		if err := s.initFetcher(); err != nil {
			log.Printf("Warning: could not initialize fetcher: %v", err)
		}
	} else {
		// config.json missing — try to bootstrap from the active profile
		if pl, plErr := s.loadProfiles(); plErr == nil && pl.Active != "" {
			for _, p := range pl.Profiles {
				if p.ID == pl.Active {
					_ = s.saveConfig(&p.Config)
					s.config = &p.Config
					if err := s.initFetcher(); err != nil {
						log.Printf("Warning: could not initialize fetcher from profile: %v", err)
					}
					break
				}
			}
		}
	}

	if cc := s.loadChannelsCache(); cc != nil {
		s.channels = cc.Channels
		s.nextFetch = cc.NextFetch
	}

	return s, nil
}

// Run starts the web server, binding to s.host:s.port.
func (s *Server) Run() error { return s.serve(nil) }

// Serve runs the web server on an already-bound listener. Used by the
// mobile entry where the listener is opened first to discover the
// kernel-assigned port.
func (s *Server) Serve(ln net.Listener) error { return s.serve(ln) }

func (s *Server) serve(ln net.Listener) error {
	mux := http.NewServeMux()

	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/channels", s.handleChannels)
	mux.HandleFunc("/api/messages/", s.handleMessages)
	mux.HandleFunc("/api/refresh", s.handleRefresh)
	mux.HandleFunc("/api/rescan", s.handleRescan)
	mux.HandleFunc("/api/send", s.handleSend)
	mux.HandleFunc("/api/admin", s.handleAdmin)
	mux.HandleFunc("/api/events", s.handleSSE)
	mux.HandleFunc("/api/profiles", s.handleProfiles)
	mux.HandleFunc("/api/profiles/switch", s.handleProfileSwitch)
	mux.HandleFunc("/api/auto-update", s.handleAutoUpdate)
	mux.HandleFunc("/api/auto-update/toggle", s.handleAutoUpdateToggle)
	mux.HandleFunc("/api/pinned-channels/toggle", s.handlePinnedChannelToggle)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/seen", s.handleSeen)
	mux.HandleFunc("/api/version-check", s.handleVersionCheck)
	mux.HandleFunc("/api/update/github", s.handleGitHubUpdateCheck)
	mux.HandleFunc("/api/update/download", s.handleUpdateDownload)
	mux.HandleFunc("/api/cache/clear", s.handleClearCache)
	mux.HandleFunc("/api/bg-image", s.handleBgImage)
	mux.HandleFunc("/api/resolvers/apply-saved", s.handleApplySavedResolvers)
	mux.HandleFunc("/api/resolvers/active", s.handleActiveResolvers)
	mux.HandleFunc("/api/resolvers/remove", s.handleRemoveResolver)
	mux.HandleFunc("/api/resolvers/reset-stats", s.handleResetResolverStats)
	mux.HandleFunc("/api/resolvers/bank", s.handleResolverBank)
	mux.HandleFunc("/api/resolvers/bank/cleanup", s.handleResolverBankCleanup)
	mux.HandleFunc("/api/scanner/start", s.handleScannerStart)
	mux.HandleFunc("/api/scanner/stop", s.handleScannerStop)
	mux.HandleFunc("/api/scanner/pause", s.handleScannerPause)
	mux.HandleFunc("/api/scanner/resume", s.handleScannerResume)
	mux.HandleFunc("/api/scanner/progress", s.handleScannerProgress)
	mux.HandleFunc("/api/scanner/apply", s.handleScannerApply)
	mux.HandleFunc("/api/scanner/presets", s.handleScannerPresets)
	mux.HandleFunc("/api/resolvers/lists", s.handleResolverLists)
	mux.HandleFunc("/api/resolvers/lists/select", s.handleResolverListSelect)
	mux.HandleFunc("/api/resolvers/lists/save", s.handleResolverListSave)
	mux.HandleFunc("/api/resolvers/lists/rename", s.handleResolverListRename)
	mux.HandleFunc("/api/resolvers/lists/add", s.handleResolverListAdd)
	// Media (image/file) downloader: assembles a binary blob from a media
	// channel and streams it back. See internal/web/media.go for the param
	// contract.
	mux.HandleFunc("/api/media/get", s.handleMediaGet)
	mux.HandleFunc("/api/media/progress", s.handleMediaProgress)
	// Optional telemirror feature — see internal/telemirror/.
	mux.HandleFunc("/api/telemirror/channels", s.telemirror.handleChannels)
	mux.HandleFunc("/api/telemirror/channel/", s.telemirror.handleChannel)
	mux.HandleFunc("/api/telemirror/img", s.telemirror.handleImg)
	mux.HandleFunc("/api/telemirror/avatar/", s.telemirror.handleAvatar)
	mux.HandleFunc("/api/telemirror/older/", s.telemirror.handleOlder)
	// Profile-pics cache + control endpoints.
	mux.HandleFunc("/api/profile-pics/", s.profilePics.handleProfilePic)
	mux.HandleFunc("/api/profile-pics", s.handleProfilePicsList)
	mux.HandleFunc("/api/profile-pics/refresh", s.handleProfilePicsRefresh)
	mux.HandleFunc("/api/profile-pics/progress", s.handleProfilePicsProgress)
	mux.HandleFunc("/", s.handleIndex)

	// Listen on the specified host (default 127.0.0.1)
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	log.Printf("thefeed client %s", version.Version)
	fmt.Printf("\n  Open in browser: http://%s:%d\n\n", s.host, s.port)

	if s.fetcher != nil {
		// Boot-time fast path for the resolver list:
		//   1. The user's currently-selected named list (instant, no
		//      probing) — populated by the migration on first load.
		//   2. last_scan.json (legacy fast path for old installs).
		//   3. Full health-check scan.
		if applied := s.applySelectedList(); applied {
			s.checker.StartPeriodic(s.fetcherCtx)
			go s.refreshMetadataOnly()
		} else if ls := s.loadLastScan(); ls != nil && len(ls.Resolvers) > 0 {
			// Pin the runtime pool to the saved scan and propagate
			// those resolvers into the selected list and bank when
			// either is empty — without this, the user sees
			// counts of "0" in the UI even though the fetcher is
			// happily using the saved resolvers.
			s.fetcher.UpdateResolverPool(ls.Resolvers)
			s.fetcher.SetActiveResolvers(ls.Resolvers)
			s.persistLastScanToProfiles(ls.Resolvers)
			s.checker.StartPeriodic(s.fetcherCtx)
			go s.refreshMetadataOnly()
		} else {
			s.startCheckerThenRefresh()
		}
	}

	var handler http.Handler = mux
	if s.password != "" {
		pw := s.password
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, pass, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(pw)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="thefeed"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			mux.ServeHTTP(w, r)
		})
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
		// ReadHeaderTimeout protects against slow-loris on the request
		// header. The body itself can be large (Telegram send-message
		// uploads), and the response can be slow (DNS-tunneled media
		// streams take many minutes for multi-block files in censored
		// networks). So zero out ReadTimeout/WriteTimeout and bound the
		// idle period on the connection itself.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       30 * time.Minute,
	}
	if ln != nil {
		return srv.Serve(ln)
	}
	return srv.ListenAndServe()
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *Server) initFetcher() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Cancel goroutines from the previous fetcher configuration.
	// This also cancels any in-progress manual rescan (via the context chain).
	// Preserve resolver stats across fetcher re-creation (e.g. profile switch).
	var prevStats map[string][3]int64
	if s.fetcher != nil {
		prevStats = s.fetcher.ExportStats()
		// Persist accumulated stats before destroying the old fetcher.
		s.persistResolverScores(prevStats)
	}
	if s.fetcherCancel != nil {
		s.fetcherCancel()
	}

	cfg := s.config
	if cfg == nil {
		return fmt.Errorf("no config")
	}

	// Load the shared resolver bank and preferences from profiles.json.
	var bankResolvers []string
	var debug bool
	var savedScores map[string]*SavedResolverScore
	if pl, plErr := s.loadProfiles(); plErr == nil {
		debug = pl.Debug
		if len(pl.ResolverBank) > 0 {
			bankResolvers = pl.ResolverBank
		}
		savedScores = pl.ResolverScores
	}

	// Use resolver bank; fall back to per-profile resolvers for backward compat.
	resolvers := cfg.Resolvers
	if len(bankResolvers) > 0 {
		resolvers = bankResolvers
	}

	cacheDir := filepath.Join(s.dataDir, "cache")
	cache, err := client.NewCache(cacheDir)
	if err != nil {
		return fmt.Errorf("create cache: %w", err)
	}

	fetcher, err := client.NewFetcher(cfg.Domain, cfg.Key, resolvers)
	if err != nil {
		return fmt.Errorf("create fetcher: %w", err)
	}

	// Restore resolver stats: prefer in-memory stats from the previous fetcher,
	// fall back to persisted scores for fresh starts.
	if prevStats != nil {
		fetcher.ImportStats(prevStats)
	} else if len(savedScores) > 0 {
		m := make(map[string][3]int64)
		for addr, ss := range savedScores {
			key := addr
			if !strings.Contains(key, ":") {
				key += ":53"
			}
			m[key] = [3]int64{ss.Success, ss.Failure, ss.TotalMs}
		}
		fetcher.ImportStats(m)
	}

	// Connection settings are global (live on ProfileList). Defaults
	// apply when fields are zero/empty.
	pl, _ := s.loadProfiles()
	qm, rl, sc, to := connectionSettings(pl)
	if qm == "double" {
		fetcher.SetQueryMode(protocol.QueryMultiLabel)
	}
	fetcher.SetDebug(debug)
	s.scanner.SetDebug(debug)
	if rl > 0 {
		fetcher.SetRateLimit(rl)
	}
	if sc > 1 {
		fetcher.SetScatter(sc)
	}
	timeout := time.Duration(to * float64(time.Second))
	fetcher.SetTimeout(timeout)
	fetcher.SetCacheShare(pl.ShareEnabled())

	fetcher.SetLogFunc(func(msg string) {
		s.addLog(msg)
	})

	// Create a shared context for this fetcher's lifetime.
	ctx, cancel := context.WithCancel(context.Background())
	s.fetcherCtx = ctx
	s.fetcherCancel = cancel

	// Start rate limiter and noise goroutines.
	fetcher.Start(ctx)

	// Initialise resolver health-checker; start it (with initial scan → then refresh)
	// via startCheckerThenRefresh, called by every initFetcher call site.
	checker := client.NewResolverChecker(fetcher, timeout)
	checker.SetLogFunc(func(msg string) {
		s.addLog(msg)
	})
	checker.SetOnScanDone(func(healthy []string) {
		if len(healthy) > 0 {
			s.saveLastScan(healthy)
		}
		s.persistScanResultsToList(healthy)
	})
	// nil means enabled (the default); only an explicit false pointer disables it.
	autoScan := cfg.AutoScan == nil || *cfg.AutoScan
	checker.SetAutoScan(autoScan)
	s.checker = checker

	s.fetcher = fetcher
	s.cache = cache
	go cache.Cleanup() // remove channel files not updated in 7 days

	// Goroutine dies with fetcherCtx, so a profile switch / config change
	// stops it cleanly.
	go s.runAutoUpdateLoop(ctx)
	return nil
}
