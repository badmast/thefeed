// ===== Chat messenger =====
// Standalone E2E messenger over the chat sub-domains. All markup is rendered
// here into #chatModal; state comes from /api/chat/*.

var chatState = {
  open: false,
  view: 'list', // 'list' | 'thread'
  peer: null,
  info: null,      // { exists, address, backedUp, serverCount }
  avail: null,     // { available, reason, limits, servers, needsCreate } | null = checking
  quota: null,     // { remaining, resetUnix } last-known send quota
  availChecking: false, // an availability probe is in flight
  threads: [],
  contacts: {},
  pinned: {},        // addr -> true
  pendingServer: {}, // addr -> serverKey, for a new chat before its first send binds it
  peerStatus: {},    // addr -> {accepted, delivered, emojis}
  sending: false,
  sendProg: null,   // {done,total} of the in-flight send, so a mid-send thread
                    // re-render can repaint the progress bar it destroys
  polling: false,
  fgTimer: null,    // foreground poll while the messenger is open
  cdTimer: null,    // 1s ticker for the "next check in Ns" countdown
  nextPollAt: 0,    // epoch ms of the next foreground poll
  vvBound: false,   // visualViewport listener attached
  notifyReady: false,
  promptCb: null,    // pending chatPrompt callback
  renderPending: false, // a poll/status render was deferred while a popup was open
  guideAfterAvail: false, // show "checking servers" + open the server sheet once availability resolves
  draft: '', // current compose text — single source of truth (see chatRenderThread)
  curServer: '', // server the open conversation currently sends via
  safetyOpen: false, // inline safety-code explainer expanded
  connecting: false, // "connecting…" indicator currently shown for the in-flight send
  sendConnTimer: null // delayed reveal of the "connecting…" indicator
};

// First-run server guidance: while NO chat server is enabled, every messenger
// open probes all configs (with a "checking servers" popup) and then opens the
// server sheet so the user actually turns one on — instead of landing on the
// "pick a server" dead-end and forgetting. The "is a server enabled" truth is
// server-side (info.anyEnabled), NOT localStorage — Android changes the
// loopback port between launches, which wipes localStorage but not the on-disk
// enabled set. The probe is retried a few times so a momentary resolver outage
// doesn't make every server look chatless.
var CHAT_AVAIL_RETRIES = 4;      // probe attempts while guiding (resolvers may be flaky)
var CHAT_AVAIL_RETRY_MS = 2500;  // wait between probe attempts

// While the messenger is OPEN, poll the server this often for new messages and
// delivery (✓✓) updates. When the messenger is closed the Go background loop
// polls every 3 min. Each poll is ~1 query when the inbox is empty.
var CHAT_FG_POLL_MS = 15000;

function chatT(key) { return t(key) || key }

function chatName(addr) {
  if (chatState.contacts[addr]) return chatState.contacts[addr];
  return addr.slice(0, 6) + '…' + addr.slice(-4);
}

// chatDir: UI base direction (rtl for Persian) — for lines starting with an LTR value.
function chatDir() { return (typeof lang !== 'undefined' && lang === 'fa') ? 'rtl' : 'ltr'; }

// chatFmtBidi fills a "{key}" template, escaping literals and <bdi>-wrapping
// values so an LTR value doesn't scramble RTL text.
function chatFmtBidi(template, vars) {
  return String(template).replace(/\{(\w+)\}|[^{]+/g, function (seg, key) {
    if (key != null) return '<bdi>' + esc(vars[key] != null ? String(vars[key]) : '') + '</bdi>';
    return esc(seg);
  });
}

// chatAvatarHTML renders an initial avatar (coloured circle + first letter;
// colour derived from the name, so stable per contact).
function chatAvatarColor(s) {
  var palette = ['#e57373', '#f06292', '#ba68c8', '#9575cd', '#7986cb',
    '#64b5f6', '#4fc3f7', '#4dd0e1', '#4db6ac', '#81c784',
    '#aed581', '#dce775', '#ffd54f', '#ffb74d', '#ff8a65'];
  var h = 0;
  for (var i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  return palette[h % palette.length];
}
function chatAvatarHTML(name, size) {
  size = size || 36;
  var disp = (name || '').trim() || '?';
  return '<div class="chat-avatar" style="width:' + size + 'px;height:' + size +
    'px;font-size:' + Math.round(size * 0.45) + 'px;background:' + chatAvatarColor(disp) + '">' +
    esc(disp.charAt(0).toUpperCase()) + '</div>';
}

// chatServerLabel maps a server key to its display name (operator-set, defaults
// to the domain).
function chatServerLabel(key) {
  if (!key) return '';
  var list = (chatState.avail && chatState.avail.servers) || [];
  for (var i = 0; i < list.length; i++) {
    if (list[i].key === key) return list[i].name || list[i].domain || key;
  }
  return key;
}

function chatFmtTime(ts) {
  if (!ts) return '';
  var d = new Date(ts * 1000);
  var locale = (typeof lang !== 'undefined' && lang === 'fa') ? 'fa-IR' : undefined;
  return d.toLocaleTimeString(locale, { hour: '2-digit', minute: '2-digit' });
}

// chatFmtDate renders a day label for the thread date separators — Jalali for
// Persian, Gregorian otherwise, matching the feed (media.js).
function chatFmtDate(ts) {
  if (!ts) return '';
  var d = new Date(ts * 1000);
  var fa = (typeof lang !== 'undefined' && lang === 'fa');
  var opts = { year: 'numeric', month: 'long', day: 'numeric' };
  if (fa) opts.calendar = 'persian';
  try {
    return d.toLocaleDateString(fa ? 'fa-IR' : undefined, opts);
  } catch (e) {
    return d.toLocaleDateString();
  }
}

// ===== open / close + hardware-back history =====
// The messenger is a full-screen modal with two single-pane views (list and
// thread). To make the Android/iOS hardware back button — and browser back —
// navigate thread→list→close instead of dropping out of the app, each layer
// pushes a history entry; the popstate handler unwinds them one at a time.
// Mirrors the telemirror modal pattern. Explicit close/back route through
// history too (history.go/back) so the button and the hardware key take the
// exact same path and the stack stays balanced.
var chatHistoryPushed = false; // messenger modal layer pushed
var chatThreadPushed = false;  // conversation (thread) layer pushed
var chatSuppressPopstate = 0;  // swallow the popstates our own history.go fires

function openMessenger() {
  document.getElementById('chatModal').classList.add('active');
  chatState.open = true;
  if (!chatHistoryPushed) {
    try { history.pushState({ view: 'chatMessenger' }, ''); chatHistoryPushed = true; } catch (e) { }
  }
  chatRenderList();
  chatLoadInfo();
  chatLoadThreads();
  chatRequestNotifyPermission();
  chatStartForegroundTimer();
  chatBindViewport();
}

// chatTeardown hides the modal and stops the timers. It does NOT touch history
// — the caller decides whether to unwind (explicit close) or not (popstate has
// already popped us).
function chatTeardown() {
  chatStopForegroundTimer();
  chatUnbindViewport();
  chatState.open = false;
  chatState.view = 'list';
  chatState.peer = null;
  document.getElementById('chatModal').classList.remove('active');
}

// closeMessenger is the explicit close (the list-view back arrow, the
// not-configured banner). Tear down, then pop whatever history layers we
// pushed, suppressing the popstates that the unwind fires.
function closeMessenger() {
  var steps = (chatHistoryPushed ? 1 : 0) + (chatThreadPushed ? 1 : 0);
  chatHistoryPushed = false;
  chatThreadPushed = false;
  chatTeardown();
  if (steps > 0) {
    chatSuppressPopstate += steps;
    try { history.go(-steps); } catch (e) { }
  }
}

// Hardware / browser back: unwind one layer at a time — thread→list, then
// close the modal. History already popped the entry, so we just sync the UI.
window.addEventListener('popstate', function () {
  if (chatSuppressPopstate > 0) { chatSuppressPopstate--; return; }
  if (chatThreadPushed) {
    chatThreadPushed = false;
    chatShowList();
    return;
  }
  if (chatHistoryPushed) {
    chatHistoryPushed = false;
    chatTeardown();
  }
});

function chatStopForegroundTimer() {
  if (chatState.fgTimer) { clearInterval(chatState.fgTimer); chatState.fgTimer = null; }
  if (chatState.cdTimer) { clearInterval(chatState.cdTimer); chatState.cdTimer = null; }
}

// chatStartForegroundTimer keeps the open messenger live: every CHAT_FG_POLL_MS
// it pulls new incoming messages and, in a conversation, re-checks delivery
// (✓✓). It runs in both the list and thread views. A 1s ticker drives the
// "next check in Ns" countdown.
function chatStartForegroundTimer() {
  chatStopForegroundTimer();
  chatState.nextPollAt = Date.now() + CHAT_FG_POLL_MS;
  chatState.fgTimer = setInterval(chatForegroundPoll, CHAT_FG_POLL_MS);
  chatState.cdTimer = setInterval(chatUpdateCountdown, 1000);
  chatUpdateCountdown();
}

function chatForegroundPoll() {
  if (!chatState.open) { chatStopForegroundTimer(); return; }
  chatState.nextPollAt = Date.now() + CHAT_FG_POLL_MS;
  // If chat is currently unavailable (e.g. the server was rebooting), keep
  // re-probing so the banner clears itself when the server returns — the user
  // shouldn't have to leave and re-enter to recover.
  if (chatState.info && chatState.info.exists && !chatAvailable()) chatCheckAvailability();
  var peer = chatState.peer;
  if (chatState.view === 'thread' && peer) chatFetchPeerStatus(peer);
  fetch('/api/chat/poll', { method: 'POST' }).then(function (r) { return r.json(); }).then(function (d) {
    chatHidePollProgress();
    if (d && d.ok && d.got > 0) {
      chatNotifyNew(d.got);
      chatLoadThreads();
      if (chatState.view === 'thread' && chatState.peer === peer) chatRenderThread();
    }
  }).catch(function () { chatHidePollProgress(); });
}

function chatUpdateCountdown() {
  var el = document.getElementById('chatNextPoll');
  if (!el) return;
  var secs = Math.max(0, Math.ceil((chatState.nextPollAt - Date.now()) / 1000));
  el.textContent = chatT('chat_next_check').replace('{n}', secs);
}

// chatBindViewport keeps the input above the on-screen keyboard on mobile:
// without this, focusing the textarea shifts the whole fixed modal. We resize
// the modal to the visual viewport (which shrinks when the keyboard opens) so
// only the keyboard area is hidden and the input stays visible.
function chatBindViewport() {
  var vv = window.visualViewport;
  if (!vv || chatState.vvBound) return;
  vv.addEventListener('resize', chatViewportFit);
  vv.addEventListener('scroll', chatViewportFit);
  chatState.vvBound = true;
  chatViewportFit();
}

function chatUnbindViewport() {
  var vv = window.visualViewport;
  if (vv && chatState.vvBound) {
    vv.removeEventListener('resize', chatViewportFit);
    vv.removeEventListener('scroll', chatViewportFit);
  }
  chatState.vvBound = false;
  var m = document.getElementById('chatModal');
  if (m) { m.style.height = ''; m.style.top = ''; m.style.bottom = ''; }
}

function chatViewportFit() {
  var vv = window.visualViewport;
  var m = document.getElementById('chatModal');
  if (!vv || !m) return;
  m.style.top = vv.offsetTop + 'px';
  m.style.height = vv.height + 'px';
  m.style.bottom = 'auto';
}

// ---- new-message notification (foreground only) ----
// True background push isn't possible here: there's no push server, and the
// transport is censorship-resistant DNS. So we notify only while the app is
// open — a sound, plus a Web Notification when the tab/conversation isn't
// focused (works on desktop browsers and the Android WebView in foreground;
// iOS has no background mode for this either).

function chatRequestNotifyPermission() {
  try {
    if (typeof Notification !== 'undefined' && Notification.permission === 'default') {
      Notification.requestPermission().then(function () { chatState.notifyReady = true; });
    } else {
      chatState.notifyReady = true;
    }
  } catch (e) { }
}

// chatSoundOff reports whether the user muted the new-message sound (persisted).
function chatSoundOff() {
  try { return localStorage.getItem('chatSoundOff') === '1'; } catch (e) { return false; }
}

// chatToggleSound flips and persists the mute setting.
function chatToggleSound() {
  try { localStorage.setItem('chatSoundOff', chatSoundOff() ? '0' : '1'); } catch (e) { }
  showToast(chatT(chatSoundOff() ? 'chat_sound_off' : 'chat_sound_on'));
  if (chatState.view === 'list') chatRenderList(); else if (chatState.view === 'thread') chatRenderThread();
}

function chatBeep() {
  if (chatSoundOff()) return;
  try {
    var Ctx = window.AudioContext || window.webkitAudioContext;
    if (!Ctx) return;
    var ac = new Ctx();
    var o = ac.createOscillator(), g = ac.createGain();
    o.type = 'sine'; o.frequency.value = 880;
    g.gain.value = 0.06;
    o.connect(g); g.connect(ac.destination);
    o.start();
    setTimeout(function () { o.stop(); ac.close(); }, 150);
  } catch (e) { }
}

function chatNotifyNew(n) {
  // Skip if the user is actively looking at the conversation list/thread.
  var focused = chatState.open && !document.hidden;
  chatBeep();
  if (focused) return;
  try {
    if (typeof Notification !== 'undefined' && Notification.permission === 'granted') {
      new Notification(chatT('chat_btn'), { body: chatT('chat_new_messages').replace('{n}', n) });
    }
  } catch (e) { }
}

// chatLoadInfo fetches the LOCAL identity (instant — no DNS) so the address
// renders immediately, then kicks off the slow availability check.
async function chatLoadInfo() {
  try {
    var r = await fetch('/api/chat/info');
    chatState.info = await r.json();
  } catch (e) { chatState.info = null }
  if (chatState.open && chatState.view === 'list') chatRenderList();
  // No identity yet → show the opt-in prompt, don't probe servers (and don't
  // create anything) until the user enables the messenger.
  if (chatState.info && chatState.info.exists) {
    // Guide to the server sheet whenever no server is enabled yet — re-checked
    // from server-side truth every open (not localStorage), so it keeps asking
    // until the user finally turns one on, then goes quiet.
    chatState.guideAfterAvail = !chatState.info.anyEnabled;
    chatCheckAvailability();
  }
}

// chatEnableMessenger is the explicit opt-in: create the local identity, then
// load availability so the user can choose which servers to turn on.
async function chatEnableMessenger() {
  try {
    var r = await fetch('/api/chat/enable', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: 'create' })
    });
    if (!r.ok) { showToast(chatT('chat_err_generic')); return; }
  } catch (e) { showToast(chatT('chat_err_generic')); return; }
  chatState.avail = null;
  chatLoadInfo();
}

// chatCheckAvailability does the DNS capability probe in the background. The
// overlap guard lets the foreground timer call it repeatedly (to self-heal a
// rebooting server) without stacking slow probes.
async function chatCheckAvailability() {
  if (chatState.availChecking) return;
  chatState.availChecking = true;
  var guiding = chatState.guideAfterAvail;
  chatState.guideAfterAvail = false;
  if (guiding) chatShowCheckingServers();
  // While guiding, retry a few times: if the resolvers are momentarily down
  // every server looks chatless, so one failed probe must not be the verdict.
  var attempts = guiding ? CHAT_AVAIL_RETRIES : 1;
  try {
    for (var i = 0; i < attempts; i++) {
      try {
        var r = await fetch('/api/chat/availability');
        chatState.avail = await r.json();
      } catch (e) { chatState.avail = { available: false, reason: 'chat_err_generic' }; }
      if (!chatState.open) break;                 // user left — stop probing
      if (chatAvailServers().length > 0) break;   // found a chat-capable server
      if (i + 1 < attempts) {
        await new Promise(function (res) { setTimeout(res, CHAT_AVAIL_RETRY_MS); });
        if (!chatState.open) break;
      }
    }
  } finally { chatState.availChecking = false; }
  if (guiding) {
    chatHideCheckingServers();
    // Chat-capable servers exist but none enabled → open the toggles so the
    // user actually turns one on. If none are reachable, fall through: the list
    // shows the unreachable banner (with its Retry button), and the next open
    // probes again.
    if (chatState.open && chatAvailServers().length > 0 && chatUsableServers().length === 0) {
      chatServerSheet();
    }
  }
  if (!chatState.open) return;
  if (chatState.view === 'list') chatRenderList();
  else chatRenderThread();
}

// chatShowCheckingServers / chatHideCheckingServers: the first-run "probing all
// configs" popup. It uses the .chat-sheet-overlay class so the render guard
// keeps it (and focus) intact while the probe runs.
function chatShowCheckingServers() {
  if (document.getElementById('chatCheckingOverlay')) return;
  var ov = document.createElement('div');
  ov.className = 'chat-sheet-overlay';
  ov.id = 'chatCheckingOverlay';
  ov.innerHTML = '<div class="chat-sheet chat-checking-card">' +
    '<div class="chat-spinner" aria-hidden="true"></div>' +
    '<div class="chat-checking-title">' + esc(chatT('chat_checking_servers')) + '</div>' +
    '<div class="chat-sheet-hint">' + esc(chatT('chat_checking_servers_sub')) + '</div></div>';
  document.getElementById('chatModal').appendChild(ov);
}
function chatHideCheckingServers() {
  var o = document.getElementById('chatCheckingOverlay');
  if (o) o.remove();
}

function chatAvailable() { return !!(chatState.avail && chatState.avail.available) }

function chatRetryAvailability() {
  chatState.avail = null;       // back to "checking…"
  if (chatState.view === 'list') chatRenderList();
  chatCheckAvailability();
}

async function chatLoadThreads() {
  try {
    var r = await fetch('/api/chat/threads');
    chatState.threads = (await r.json()) || [];
    chatState.pinned = {};
    chatState.threads.forEach(function (th) { if (th.pinned) chatState.pinned[th.addr] = true });
    var rc = await fetch('/api/chat/contacts');
    var contacts = (await rc.json()) || [];
    chatState.contacts = {};
    contacts.forEach(function (c) { chatState.contacts[c.addr] = c.name });
  } catch (e) { }
  chatUpdateBadge();
  if (chatState.open && chatState.view === 'list') chatRenderList();
}

function chatUpdateBadge() {
  var total = 0;
  (chatState.threads || []).forEach(function (th) { total += th.unread || 0 });
  var b = document.getElementById('chatSidebarBadge');
  if (!b) return;
  b.textContent = total;
  b.className = 'chat-sidebar-badge' + (total > 0 ? ' on' : '');
}

function chatQuotaLine() {
  var q = chatState.quota;
  if (q && q.remaining != null) {
    var reset = q.resetUnix ? chatFmtTime(q.resetUnix) : '';
    return chatT('chat_quota').replace('{n}', q.remaining).replace('{t}', reset);
  }
  // Before the first send/poll the live count is unknown — show the cap so the
  // per-hour limit is always visible.
  var lim = chatState.avail && chatState.avail.limits;
  if (lim && lim.sendPerHour) return chatT('chat_quota_cap').replace('{n}', lim.sendPerHour);
  return '';
}

// ---- thread list view ----

// chatRenderIntro is the one-time opt-in screen: no keys exist yet and no
// server has been probed. Enabling creates the local identity.
function chatRenderIntro() {
  var html = '';
  html += '<div class="chat-topbar">';
  html += '<button class="chat-back" onclick="closeMessenger()" aria-label="close">' + icon('arrowLeft') + '</button>';
  html += '<div class="chat-topbar-title"><div class="chat-peer-name">' + esc(chatT('chat_title')) +
    ' <span class="chat-exp-tag">' + esc(chatT('chat_experimental')) + '</span></div></div>';
  html += '<button class="chat-icon-btn" title="' + escAttr(chatT('chat_log')) + '" onclick="chatOpenLog()">' + icon('log') + '</button>';
  html += '</div>';
  html += '<div class="chat-body" id="chatBody">';
  html += '<div class="chat-card chat-intro">' +
    '<div class="chat-intro-title">' + esc(chatT('chat_enable_title')) + '</div>' +
    '<div class="chat-myaddr-hint">' + esc(chatT('chat_enable_desc')) + '</div>' +
    '<div class="chat-banner-actions"><button class="chat-btn-primary" onclick="chatEnableMessenger()">' +
    esc(chatT('chat_enable_btn')) + '</button></div></div>';
  html += '</div>';
  document.getElementById('chatModal').innerHTML = html;
}

// chatOverlayOpen reports whether any chat popup (action sheet, rename prompt,
// log) is on screen. They're all appended into #chatModal and share the
// .chat-sheet-overlay class. The periodic poll / status refresh must NOT
// rebuild the list/thread innerHTML while one is open — that would wipe the
// popup and steal keyboard focus. Such a render is deferred (renderPending)
// and flushed when the popup closes.
function chatOverlayOpen() {
  return !!document.querySelector('#chatModal .chat-sheet-overlay');
}

// chatFlushPendingRender re-renders the current view if a render was deferred
// while a popup was open. Deferred a tick so a close-then-reopen (e.g. a sheet
// refreshing itself) doesn't trigger a wasted render.
function chatFlushPendingRender() {
  setTimeout(function () {
    if (!chatState.renderPending || !chatState.open || chatOverlayOpen()) return;
    chatState.renderPending = false;
    if (chatState.view === 'thread') chatRenderThread();
    else chatRenderList();
  }, 0);
}

function chatRenderList() {
  if (chatOverlayOpen()) { chatState.renderPending = true; return; }
  chatState.renderPending = false;
  chatState.view = 'list';
  var info = chatState.info || {};
  var avail = chatState.avail; // null = still checking

  // Not opted in yet → consent screen (no keys created, no servers probed).
  if (info.exists === false) { chatRenderIntro(); return; }

  var html = '';
  html += '<div class="chat-topbar">';
  html += '<button class="chat-back" onclick="closeMessenger()" aria-label="close">' + icon('arrowLeft') + '</button>';
  // The title doubles as the server picker: tap it (or the antenna button) to
  // see and toggle which servers the messenger is on for.
  var sub = avail ? chatT('chat_servers_active').replace('{n}', chatUsableServers().length) : chatT('chat_checking');
  html += '<div class="chat-topbar-title chat-servers-link" onclick="chatServerSheet()">' +
    '<div class="chat-peer-name">' + esc(chatT('chat_title')) +
    ' <span class="chat-exp-tag">' + esc(chatT('chat_experimental')) + '</span></div>' +
    '<div class="chat-peer-sub">' + esc(sub) + ' ' + icon('chevronDown') + '</div></div>';
  html += '<button class="chat-icon-btn" title="' + escAttr(chatT('chat_manage_servers')) + '" onclick="chatServerSheet()">' + icon('antenna') + '</button>';
  html += '<button class="chat-icon-btn' + (chatSoundOff() ? ' chat-muted' : '') + '" title="' +
    escAttr(chatT(chatSoundOff() ? 'chat_sound_off' : 'chat_sound_on')) + '" onclick="chatToggleSound()">' + icon('music') + '</button>';
  html += '<button class="chat-icon-btn" title="' + escAttr(chatT('chat_log')) + '" onclick="chatOpenLog()">' + icon('log') + '</button>';
  if (chatAvailable()) {
    html += '<button id="chatRefreshBtn" class="chat-icon-btn' + (chatState.polling ? ' spinning' : '') + '" ' +
      (chatState.polling ? 'disabled ' : '') + 'title="' + escAttr(chatT('chat_refresh')) + '" onclick="chatPollNow()">' + icon('refresh') + '</button>';
  }
  html += '</div>';
  if (chatAvailable()) html += '<div class="chat-nextpoll-bar"><span id="chatNextPoll" class="chat-next-poll"></span></div>';

  html += '<div class="chat-body" id="chatBody">';

  // Status banner: distinct states (checking / pick-a-server / need-key /
  // disabled / ok).
  if (avail === null) {
    html += '<div class="chat-banner chat-banner-info">' + esc(chatT('chat_checking')) + '</div>';
  } else if (!avail.available) {
    if (chatAvailServers().length > 0) {
      // Reachable servers exist but none is enabled — guide the opt-in (warn
      // colour so it reads as "action needed", not a neutral hint).
      html += '<div class="chat-banner chat-banner-warn">' + esc(chatT('chat_no_enabled_servers')) +
        ' <div class="chat-banner-actions"><button class="chat-btn-soft" onclick="chatServerSheet()">' +
        esc(chatT('chat_manage_servers')) + '</button></div></div>';
    } else {
      var reason = avail.reason || 'chat_err_disabled';
      html += '<div class="chat-banner chat-banner-warn">' + esc(chatT(reason));
      if (reason === 'chat_err_need_key' || reason === 'chat_err_no_key') {
        html += ' <div class="chat-banner-actions"><button class="chat-btn-soft" onclick="closeMessenger();openProfiles&&openProfiles()">' +
          esc(chatT('chat_edit_config')) + '</button></div>';
      } else if (reason === 'chat_err_unreachable') {
        html += ' <div class="chat-banner-actions"><button class="chat-btn-soft" onclick="chatRetryAvailability()">' +
          esc(chatT('chat_retry')) + '</button></div>';
      }
      html += '</div>';
    }
  }

  // My address card (always shown once identity is known).
  if (info.address) {
    html += '<div class="chat-card chat-myaddr">' +
      '<div class="chat-myaddr-label">' + esc(chatT('chat_my_address')) + '</div>' +
      '<div class="chat-myaddr-row"><code class="chat-addr">' + esc(info.address) + '</code>' +
      '<button class="chat-btn-soft" onclick="chatCopy(\'' + escAttr(info.address) + '\')">' + esc(chatT('chat_copy')) + '</button>' +
      '<button class="chat-btn-soft" onclick="chatShowRecovery()">' + esc(chatT('chat_recovery_title')) + '</button></div>';
    if (!info.backedUp) {
      html += '<div class="chat-myaddr-hint">' + esc(chatT('chat_backup_hint')) + '</div>';
    }
    html += '</div>';
  }

  html += '<div id="chatRecoveryBox" style="display:none"></div>';

  // Compose row only when chat is actually usable.
  if (chatAvailable()) {
    html += '<div class="chat-card chat-add-row">' +
      '<input id="chatAddAddr" placeholder="' + escAttr(chatT('chat_add_contact_ph')) + '" autocapitalize="off" autocomplete="off" spellcheck="false">' +
      '<button class="chat-btn-primary" onclick="chatStartNew()">' + esc(chatT('chat_new_chat')) + '</button></div>';
  }

  if (!chatState.threads.length) {
    if (chatAvailable()) html += '<div class="chat-empty">' + esc(chatT('chat_no_threads')) + '</div>';
  } else {
    html += '<div class="chat-threads">';
    chatState.threads.forEach(function (th) {
      var nm = th.name || chatName(th.addr);
      var pinMark = th.pinned ? ('<span class="chat-pin-mark">' + icon('pinned') + '</span> ') : '';
      html += '<div class="chat-thread-item" onclick="openChatThread(\'' + escAttr(th.addr) + '\')">' +
        chatAvatarHTML(nm, 38) +
        '<div class="chat-thread-main"><div class="chat-thread-name">' + pinMark + esc(nm) + '</div>' +
        '<div class="chat-thread-last">' + esc(th.lastText || '') + '</div></div>' +
        (th.unread ? '<span class="chat-unread-badge">' + th.unread + '</span>' : '') +
        '<span class="chat-thread-time">' + chatFmtTime(th.lastTs) + '</span>' +
        '<button class="chat-icon-btn chat-row-menu" title="" onclick="event.stopPropagation();chatRowMenu(\'' + escAttr(th.addr) + '\',' + (th.pinned ? 'true' : 'false') + ')">' + icon('more') + '</button>' +
        '</div>';
    });
    html += '</div>';
  }
  html += '<div class="chat-progress-wrap"><div class="chat-progress" id="chatPollProgress"><div class="chat-progress-fill" id="chatPollProgressFill"></div></div><span class="chat-progress-label" id="chatPollProgressLabel"></span></div>';
  html += '</div>';

  document.getElementById('chatModal').innerHTML = html;
  chatUpdateCountdown();
}

function chatCopy(v) {
  var ok = function () { showToast(chatT('chat_copied')); };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(v).then(ok, function () { if (chatCopyFallback(v)) ok(); });
    return;
  }
  // WebViews (and non-secure contexts) can lack the async clipboard API.
  if (chatCopyFallback(v)) ok();
}
function chatCopyFallback(v) {
  try {
    var ta = document.createElement('textarea');
    ta.value = v;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '-1000px';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    var done = document.execCommand('copy');
    document.body.removeChild(ta);
    return done;
  } catch (e) { return false; }
}

// chatCopyMsg copies a bubble's text, read from the DOM (never inlined into the
// handler) so message content with quotes/newlines can't break or inject.
function chatCopyMsg(btn) {
  var msg = btn.closest ? btn.closest('.chat-msg') : null;
  if (!msg) return;
  var txt = msg.querySelector('.chat-msg-text');
  if (txt) chatCopy(txt.textContent);
}

async function chatShowRecovery() {
  var box = document.getElementById('chatRecoveryBox');
  if (!box) return;
  if (box.style.display === 'block') { box.style.display = 'none'; return; }
  try {
    var r = await fetch('/api/chat/seed');
    var d = await r.json();
    box.style.display = 'block';
    box.innerHTML = '<div class="chat-card chat-recovery"><b>' + esc(chatT('chat_recovery_title')) + '</b>' +
      '<div class="chat-myaddr-hint">' + esc(chatT('chat_recovery_hint')) + '</div>' +
      '<div class="chat-recovery-code">' + esc(d.recovery) + '</div>' +
      '<div class="chat-banner-actions">' +
      '<button class="chat-btn-soft" onclick="chatCopy(\'' + escAttr(d.recovery) + '\')">' + esc(chatT('chat_copy')) + '</button>' +
      '<button class="chat-btn-soft" onclick="chatMarkBackedUp()">' + esc(chatT('chat_backup_saved')) + '</button>' +
      '<button class="chat-btn-soft" onclick="chatImportPrompt()">' + esc(chatT('chat_import')) + '</button>' +
      '</div></div>';
  } catch (e) { }
}

async function chatMarkBackedUp() {
  await fetch('/api/chat/seed', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'backedup' }) });
  if (chatState.info) chatState.info.backedUp = true;
  chatRenderList();
}

function chatImportPrompt() {
  chatPrompt(chatT('chat_import'), '', chatT('chat_import_ph'), async function (code) {
    if (!code || !code.trim()) return;
    var r = await fetch('/api/chat/seed', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'import', recovery: code.trim() }) });
    if (r.ok) {
      showToast(chatT('chat_imported'));
      chatState.avail = null;
      chatLoadInfo();
    } else {
      showToast(chatT('chat_import_invalid'));
    }
  });
}

// chatAvailServers returns the chat-capable servers (from the availability
// probe) that are reachable right now (whether or not the user enabled them).
function chatAvailServers() {
  var list = (chatState.avail && chatState.avail.servers) || [];
  return list.filter(function (s) { return s.available });
}

// chatUsableServers returns servers that are both reachable AND turned on by
// the user — the ones you can actually send through.
function chatUsableServers() {
  return chatAvailServers().filter(function (s) { return s.enabled });
}

// chatServerSheet lists every chat-capable server with its state and an on/off
// toggle, so the user controls exactly which servers learn their address.
function chatServerSheet() {
  chatCloseMenu();
  var servers = (chatState.avail && chatState.avail.servers) || [];
  var sheet = document.createElement('div');
  sheet.className = 'chat-sheet-overlay';
  sheet.id = 'chatSheet';
  sheet.onclick = function (e) { if (e.target === sheet) chatCloseMenu(); };
  var items = '<div class="chat-sheet-title">' + esc(chatT('chat_servers_title')) + '</div>' +
    '<div class="chat-sheet-hint">' + esc(chatT('chat_servers_hint')) + '</div>';
  if (!servers.length) {
    items += '<div class="chat-sheet-hint">' + esc(chatT('chat_checking')) + '</div>';
  }
  servers.forEach(function (sv) {
    var label = sv.name ? (sv.name + ' — ' + sv.domain) : sv.domain;
    var state = !sv.available ? chatT('chat_server_unreachable')
      : (sv.enabled ? chatT('chat_server_on') : chatT('chat_server_off'));
    var action = sv.enabled ? chatT('chat_server_turn_off') : chatT('chat_server_turn_on');
    items += '<div class="chat-server-row">' +
      '<div class="chat-server-info"><div class="chat-server-name">' + esc(label) + '</div>' +
      '<div class="chat-server-state' + (sv.enabled ? ' on' : '') + (sv.available ? '' : ' off') + '">' + esc(state) + '</div></div>' +
      '<button class="chat-btn-soft" ' + (sv.available ? '' : 'disabled ') +
      'onclick="chatToggleServer(\'' + escAttr(sv.key) + '\',' + (sv.enabled ? 'false' : 'true') + ')">' +
      esc(action) + '</button></div>';
  });
  items += '<button class="chat-sheet-item chat-sheet-cancel" onclick="chatCloseMenu()">' + esc(chatT('cancel')) + '</button>';
  sheet.innerHTML = '<div class="chat-sheet">' + items + '</div>';
  document.getElementById('chatModal').appendChild(sheet);
}

// chatToggleServer turns the messenger on/off for one server. Turning it on
// publishes the user's address to that server (and only that server).
async function chatToggleServer(key, on) {
  try {
    var r = await fetch('/api/chat/enable', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: 'server', server: key, on: on })
    });
    if (!r.ok) { showToast(chatT('chat_err_generic')); return; }
  } catch (e) { showToast(chatT('chat_err_generic')); return; }
  // Reflect the change locally so the sheet updates instantly, then re-probe.
  var list = (chatState.avail && chatState.avail.servers) || [];
  list.forEach(function (s) { if (s.key === key) s.enabled = on; });
  if (chatState.avail) chatState.avail.available = chatUsableServers().length > 0;
  chatServerSheet();
  if (chatState.view === 'list') chatRenderList();
  chatCheckAvailability();
}

function chatStartNew() {
  var inp = document.getElementById('chatAddAddr');
  var addr = (inp.value || '').trim().toLowerCase();
  if (!addr) return;
  if (!/^[a-z2-7]{20}$/.test(addr)) { showToast(chatT('chat_bad_address')); return }
  if (addr === (chatState.info && chatState.info.address)) { showToast(chatT('chat_bad_address')); return }
  var servers = chatUsableServers();
  if (!servers.length) { chatServerSheet(); return; } // turn on a server first
  if (servers.length === 1) {
    chatState.pendingServer[addr] = servers[0].key;
    openChatThread(addr);
    return;
  }
  // Multiple chat servers: ask which one to start the conversation on.
  chatServerPicker(addr, servers);
}

// chatServerPicker shows an action sheet of chat servers for a new chat.
function chatServerPicker(addr, servers) {
  chatCloseMenu();
  var sheet = document.createElement('div');
  sheet.className = 'chat-sheet-overlay';
  sheet.id = 'chatSheet';
  sheet.onclick = function (e) { if (e.target === sheet) chatCloseMenu(); };
  var items = '<div class="chat-sheet-title">' + esc(chatT('chat_pick_server')) + '</div>';
  servers.forEach(function (sv) {
    var label = (sv.name ? sv.name + ' — ' : '') + sv.domain;
    items += '<button class="chat-sheet-item" onclick="chatPickServer(\'' + escAttr(addr) + '\',\'' + escAttr(sv.key) + '\')">' +
      esc(label) + '</button>';
  });
  items += '<button class="chat-sheet-item chat-sheet-cancel" onclick="chatCloseMenu()">' + esc(chatT('cancel')) + '</button>';
  sheet.innerHTML = '<div class="chat-sheet">' + items + '</div>';
  document.getElementById('chatModal').appendChild(sheet);
}

function chatPickServer(addr, key) {
  chatCloseMenu();
  chatState.pendingServer[addr] = key;
  openChatThread(addr);
}

// ---- conversation view ----

async function openChatThread(addr) {
  // Opening a conversation dismisses any open sheet (e.g. the server picker),
  // so the render guard doesn't defer this user-initiated navigation.
  var s = document.getElementById('chatSheet');
  if (s) s.remove();
  chatState.view = 'thread';
  chatState.peer = addr;
  chatState.draft = ''; // each conversation starts with an empty compose box
  chatState.safetyOpen = false; // collapse the safety explainer per conversation
  if (!chatThreadPushed) {
    try { history.pushState({ view: 'chatThread' }, ''); chatThreadPushed = true; } catch (e) { }
  }
  await chatRenderThread();
  // ✓✓ + safety emojis arrive async (it's a DNS round trip). The foreground
  // timer (started on open) keeps refreshing while the conversation is open.
  chatFetchPeerStatus(addr);
}

// chatThreadRefresh is the manual ⟳ in the conversation header: poll the
// inbox now and re-check delivery status.
async function chatThreadRefresh() {
  var peer = chatState.peer;
  if (!peer) return;
  chatFetchPeerStatus(peer);
  await chatPollNow();
}

// chatToggleSafety expands/collapses the safety-code explainer (state-backed so
// a re-render keeps it open).
function chatToggleSafety(el) {
  chatState.safetyOpen = !chatState.safetyOpen;
  if (el) el.classList.toggle('open', chatState.safetyOpen);
}

// chatRowMenu shows an action sheet (pin/unpin + delete) for a list row.
function chatRowMenu(addr, pinned) {
  chatCloseMenu();
  var nm = chatState.contacts[addr] || chatName(addr);
  var sheet = document.createElement('div');
  sheet.className = 'chat-sheet-overlay';
  sheet.id = 'chatSheet';
  sheet.onclick = function (e) { if (e.target === sheet) chatCloseMenu(); };
  sheet.innerHTML =
    '<div class="chat-sheet">' +
    '<div class="chat-sheet-title">' + esc(nm) + '</div>' +
    '<button class="chat-sheet-item" onclick="chatPin(\'' + escAttr(addr) + '\',' + (pinned ? 'false' : 'true') + ')">' +
    icon('pinned') + ' ' + esc(chatT(pinned ? 'chat_unpin' : 'chat_pin')) + '</button>' +
    '<button class="chat-sheet-item chat-sheet-danger" onclick="chatDelete(\'' + escAttr(addr) + '\')">' +
    icon('delete') + ' ' + esc(chatT('chat_delete')) + '</button>' +
    '<button class="chat-sheet-item chat-sheet-cancel" onclick="chatCloseMenu()">' + esc(chatT('cancel')) + '</button>' +
    '</div>';
  document.getElementById('chatModal').appendChild(sheet);
}

function chatCloseMenu() {
  var s = document.getElementById('chatSheet');
  if (s) s.remove();
  chatFlushPendingRender();
}

// chatPrompt is an in-app replacement for the browser's prompt() (which looks
// out of place and shows the page URL). cb receives the entered string, or is
// not called on cancel.
function chatPrompt(title, value, placeholder, cb) {
  chatPromptClose();
  var ov = document.createElement('div');
  ov.className = 'chat-sheet-overlay chat-prompt-overlay';
  ov.id = 'chatPromptOverlay';
  ov.onclick = function (e) { if (e.target === ov) chatPromptClose(); };
  ov.innerHTML = '<div class="chat-prompt">' +
    '<div class="chat-prompt-title">' + esc(title) + '</div>' +
    '<input id="chatPromptInput" class="chat-prompt-input" dir="auto" value="' + escAttr(value || '') + '" placeholder="' + escAttr(placeholder || '') + '">' +
    '<div class="chat-prompt-actions">' +
    '<button class="chat-btn-soft" onclick="chatPromptClose()">' + esc(chatT('cancel')) + '</button>' +
    '<button class="chat-btn-primary" onclick="chatPromptOK()">' + esc(chatT('ok')) + '</button>' +
    '</div></div>';
  document.getElementById('chatModal').appendChild(ov);
  chatState.promptCb = cb;
  var inp = document.getElementById('chatPromptInput');
  if (inp) {
    try { inp.focus(); inp.select(); } catch (e) { }
    inp.onkeydown = function (e) {
      if (e.key === 'Enter') { e.preventDefault(); chatPromptOK(); }
      else if (e.key === 'Escape') { chatPromptClose(); }
    };
  }
}

function chatPromptOK() {
  var inp = document.getElementById('chatPromptInput');
  var v = inp ? inp.value : '';
  var cb = chatState.promptCb;
  chatPromptClose();
  if (cb) cb(v);
}

function chatPromptClose() {
  chatState.promptCb = null;
  var o = document.getElementById('chatPromptOverlay');
  if (o) o.remove();
  chatFlushPendingRender();
}

async function chatPin(addr, pin) {
  chatCloseMenu();
  await fetch('/api/chat/thread', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ peer: addr, action: pin ? 'pin' : 'unpin' }) });
  chatState.pinned[addr] = pin;
  chatLoadThreads();
}

async function chatDelete(addr) {
  chatCloseMenu();
  if (!confirm(chatT('chat_delete_confirm'))) return;
  await fetch('/api/chat/thread', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ peer: addr, action: 'delete' }) });
  showToast(chatT('chat_deleted'));
  chatBackToList();
}

// chatClearMessages wipes the message history of a conversation but keeps the
// conversation itself (contact, server, seq counters, ✓/✓✓). Local-only.
async function chatClearMessages(addr) {
  chatCloseMenu();
  if (!confirm(chatT('chat_clear_confirm'))) return;
  await fetch('/api/chat/thread', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ peer: addr, action: 'clear' }) });
  showToast(chatT('chat_cleared'));
  if (chatState.view === 'thread' && chatState.peer === addr) chatRenderThread();
  chatLoadThreads();
}

// chatThreadMenu is the ⋮ overflow in the conversation header: clear just the
// messages (keep the chat), or delete the whole conversation.
function chatThreadMenu() {
  var addr = chatState.peer;
  if (!addr) return;
  chatCloseMenu();
  var nm = chatState.contacts[addr] || chatName(addr);
  var sheet = document.createElement('div');
  sheet.className = 'chat-sheet-overlay';
  sheet.id = 'chatSheet';
  sheet.onclick = function (e) { if (e.target === sheet) chatCloseMenu(); };
  sheet.innerHTML =
    '<div class="chat-sheet">' +
    '<div class="chat-sheet-title">' + esc(nm) + '</div>' +
    '<button class="chat-sheet-item" onclick="chatClearMessages(\'' + escAttr(addr) + '\')">' +
    icon('eraser') + ' ' + esc(chatT('chat_clear')) + '</button>' +
    '<button class="chat-sheet-item chat-sheet-danger" onclick="chatDelete(\'' + escAttr(addr) + '\')">' +
    icon('delete') + ' ' + esc(chatT('chat_delete')) + '</button>' +
    '<button class="chat-sheet-item chat-sheet-cancel" onclick="chatCloseMenu()">' + esc(chatT('cancel')) + '</button>' +
    '</div>';
  document.getElementById('chatModal').appendChild(sheet);
}

async function chatRenderThread() {
  var addr = chatState.peer;
  if (!addr) return;
  // A popup is open (rename prompt, menu, …): don't rebuild the thread — defer.
  if (chatOverlayOpen()) { chatState.renderPending = true; return; }
  chatState.renderPending = false;
  // Preserve the compose draft, caret and focus across re-renders: the 15s
  // foreground poll and the ✓✓ status refresh both re-render the thread, and
  // rebuilding innerHTML would otherwise wipe whatever the user is typing.
  var prevInput = document.getElementById('chatInput');
  // Draft is read from chatState (single source of truth), NOT the DOM: a
  // background render (poll / status) that started before a send and resolves
  // after it would otherwise restore the just-sent text, leaving it stuck in
  // the box. chatState.draft is cleared synchronously on send, so any
  // out-of-order render now restores the correct (empty) value.
  var draft = chatState.draft || '';
  var hadFocus = !!prevInput && document.activeElement === prevInput;
  var selStart = prevInput ? prevInput.selectionStart : null;
  var selEnd = prevInput ? prevInput.selectionEnd : null;
  // Don't yank the user to the bottom on a background refresh if they've
  // scrolled up to read history.
  var prevBody = document.getElementById('chatMsgsBody');
  var keepScroll = prevBody && (prevBody.scrollHeight - prevBody.scrollTop - prevBody.clientHeight > 40);
  var prevScroll = prevBody ? prevBody.scrollTop : 0;
  var firstRender = prevInput == null;
  var data = { msgs: [] };
  try {
    var r = await fetch('/api/chat/messages?peer=' + encodeURIComponent(addr) + '&markRead=1');
    data = await r.json();
  } catch (e) { }
  var msgs = data.msgs || []; // server may omit / null for an empty thread
  // Ticks: prefer the live peer-status probe, but fall back to the counters
  // persisted in the thread file so ✓/✓✓ survive an app restart instead of
  // regressing to 🕓 until the first DNS status round trip completes.
  var st = chatState.peerStatus[addr] ||
    { accepted: data.accepted || 0, delivered: data.delivered || 0 };
  var nm = data.name || chatName(addr);
  var threadServer = data.server || chatState.pendingServer[addr] || '';
  chatState.curServer = threadServer; // server this conversation currently sends via
  // hasOut: have we replied yet? (gates the stranger warning)
  var hasOut = false;
  for (var mi0 = 0; mi0 < msgs.length; mi0++) {
    if (msgs[mi0].dir === 'out') { hasOut = true; break; }
  }

  var html = '';
  html += '<div class="chat-topbar">';
  html += '<button class="chat-back" onclick="chatBackToList()" aria-label="back">' + icon('arrowLeft') + '</button>';
  // Avatar + tappable name → contact panel. Single line so it centers with the avatar.
  html += '<div class="chat-topbar-title" onclick="chatContactInfo()" style="cursor:pointer">' +
    chatAvatarHTML(nm, 36) +
    '<div class="chat-topbar-name-wrap"><div class="chat-peer-name">' + esc(nm) + '</div></div></div>';
  html += '<button id="chatRefreshBtn" class="chat-icon-btn' + (chatState.polling ? ' spinning' : '') + '" ' +
    (chatState.polling ? 'disabled ' : '') + 'title="' + escAttr(chatT('chat_refresh')) + '" onclick="chatThreadRefresh()">' + icon('refresh') + '</button>';
  html += '<button class="chat-icon-btn" title="' + escAttr(chatT('chat_menu')) +
    '" aria-label="' + escAttr(chatT('chat_menu')) + '" onclick="chatThreadMenu()">' + icon('more') + '</button>';
  html += '</div>';
  html += '<div class="chat-nextpoll-bar"><span id="chatNextPoll" class="chat-next-poll"></span>' +
    '<div class="chat-progress chat-progress-inline" id="chatRecvProgress"><div class="chat-progress-fill" id="chatRecvProgressFill"></div></div>' +
    '<span class="chat-progress-label" id="chatRecvProgressLabel"></span></div>';

  html += '<div class="chat-body" id="chatMsgsBody">';
  // E2E safety code at the top; tap to expand the explainer.
  if (st.emojis) {
    html += '<div class="chat-sysmsg chat-safety-sep' + (chatState.safetyOpen ? ' open' : '') +
      '" onclick="chatToggleSafety(this)">' +
      '<span dir="auto">' + esc(st.emojis.join(' ')) + ' · ' + esc(chatT('chat_safety_code')) + '</span>' +
      '<div class="chat-safety-explain" dir="auto">' + esc(chatT('chat_safety_explain')) + '</div></div>';
  }
  if (!msgs.length) {
    html += '<div class="chat-empty">' + esc(chatT('chat_no_messages')) + '</div>';
  }
  // Stranger warning: this thread was started by the peer and the user has
  // neither replied nor named the contact — anyone holding the address can
  // write, so warn before the user trusts links or shares anything private.
  // Replying or naming the contact clears it (the user has decided who it is).
  if (msgs.length && !hasOut && !data.name && !chatState.contacts[addr]) {
    html += '<div class="chat-banner chat-banner-warn" dir="auto">' + esc(chatT('chat_stranger_warn')) + '</div>';
  }
  // peerServer: the latest server the peer messaged from (≠ threadServer ⇒ moved).
  var peerServer = '', peerTS = 0;
  for (var pj = msgs.length - 1; pj >= 0; pj--) {
    if (msgs[pj].dir === 'in' && msgs[pj].server) { peerServer = msgs[pj].server; peerTS = msgs[pj].ts || 0; break; }
  }
  var lastDate = '', lastServer = '';
  msgs.forEach(function (m) {
    // Day separator: insert a centered date chip whenever the day changes, so a
    // multi-day conversation reads correctly (Jalali for fa, Gregorian for en).
    var dateStr = chatFmtDate(m.ts);
    if (dateStr && dateStr !== lastDate) {
      html += '<div class="chat-date-sep"><span dir="auto">' + esc(dateStr) + '</span></div>';
      lastDate = dateStr;
    }
    // Separator where the conversation's server changed.
    if (m.server && lastServer && m.server !== lastServer) {
      html += '<div class="chat-sysmsg"><span dir="auto">' +
        chatFmtBidi(chatT('chat_server_changed'), { s: chatServerLabel(m.server) }) + '</span></div>';
    }
    if (m.server) lastServer = m.server;
    var ticks = '';
    if (m.dir === 'out') {
      if (st.delivered >= m.seq) ticks = ' ✓✓';
      else if (st.accepted >= m.seq) ticks = ' ✓';
      else ticks = ' 🕓';
    }
    html += '<div class="chat-msg ' + (m.dir === 'in' ? 'in' : 'out') + '" dir="auto">' +
      '<span class="chat-msg-text">' + esc(m.text) + '</span>' +
      '<span class="chat-msg-meta">' +
      '<button type="button" class="chat-msg-copy"' +
      ' onclick="event.stopPropagation();chatCopyMsg(this)">' + esc(chatT('chat_copy')) + '</button>' +
      '<span class="chat-msg-time">' + chatFmtTime(m.ts) + esc(ticks) + '</span></span></div>' +
      '<div class="chat-clearfix"></div>';
  });
  // Peer moved to a server we don't send through, after our own last switch →
  // offer to follow (server-verified).
  if (peerServer && threadServer && peerServer !== threadServer && peerTS > (data.serverSetAt || 0)) {
    var plabel = chatServerLabel(peerServer);
    html += '<div class="chat-switch-banner" dir="' + chatDir() + '">' +
      '<div class="chat-switch-text">' +
      chatFmtBidi(chatT('chat_peer_switched'), { n: nm, s: plabel }) +
      '</div><button class="chat-btn-primary chat-switch-btn" onclick="chatSwitchServer(\'' +
      escAttr(peerServer) + '\')">' +
      chatFmtBidi(chatT('chat_switch_reply'), { s: plabel }) + '</button></div>';
  }
  html += '</div>';

  html += '<div class="chat-footer">';
  var ql = chatQuotaLine();
  if (ql) html += '<div class="chat-quota" id="chatQuotaLine">' + esc(ql) + '</div>';
  html += '<div class="chat-progress-wrap"><div class="chat-progress" id="chatSendProgress"><div class="chat-progress-fill" id="chatSendProgressFill"></div></div><span class="chat-progress-label" id="chatSendProgressLabel"></span></div>';
  html += '<div class="chat-input-row">' +
    '<textarea id="chatInput" dir="auto" placeholder="' + escAttr(chatT('chat_send_ph')) + '" rows="1" oninput="chatInputResize()" onkeydown="chatInputKey(event)"></textarea>' +
    '<button class="chat-btn-primary" id="chatSendBtn" onclick="sendChatMessage()">' + esc(chatT('chat_send')) + '</button></div>';
  html += '<div class="chat-charcount" id="chatCharCount"></div>';
  html += '</div>';

  // A popup may have opened during the await above — don't wipe it.
  if (chatOverlayOpen()) { chatState.renderPending = true; return; }
  document.getElementById('chatModal').innerHTML = html;
  var body = document.getElementById('chatMsgsBody');
  if (body) body.scrollTop = keepScroll ? prevScroll : body.scrollHeight;
  // Restore the in-progress draft + caret before resizing the input.
  var inp = document.getElementById('chatInput');
  if (inp) {
    inp.value = draft;
    if (draft && selStart != null) { try { inp.setSelectionRange(selStart, selEnd); } catch (e) { } }
  }
  // A send in flight survives this re-render: the compose row was just rebuilt,
  // so re-disable the input (chatInputResize already disables the button while
  // sending) and repaint the live send-progress bar this render destroyed.
  if (chatState.sending) {
    if (inp) inp.disabled = true;
    if (chatState.sendProg && chatState.sendProg.done >= 1) {
      chatShowProgress('chatSendProgress', chatState.sendProg.done, chatState.sendProg.total, '↑');
    } else if (chatState.connecting) {
      chatShowProgress('chatSendProgress', 0, 1, '↑'); // keep the "connecting…" bar
    }
  }
  chatInputResize();
  chatUpdateCountdown();
  // Auto-focus on first open, or keep focus the user already had — but a
  // background refresh must NOT grab focus (it would re-pop the mobile keyboard).
  if (inp && (firstRender || hadFocus)) {
    try { inp.focus({ preventScroll: true }); } catch (e) { inp.focus(); }
  }
}

// chatMaxBytes is the server's per-message byte cap (from ChatInfo), or a safe
// default until availability is known.
function chatMaxBytes() {
  var lim = chatState.avail && chatState.avail.limits;
  return (lim && lim.maxMsgBytes) ? lim.maxMsgBytes : 500;
}

function chatByteLen(s) {
  try { return new TextEncoder().encode(s).length; } catch (e) { return s.length; }
}

// chatInputResize grows the textarea and updates the byte counter, disabling
// send when the message exceeds the server's limit (Persian chars are 2 bytes,
// so we count bytes, not characters).
function chatInputResize() {
  var inp = document.getElementById('chatInput');
  var cc = document.getElementById('chatCharCount');
  var btn = document.getElementById('chatSendBtn');
  if (!inp) return;
  chatState.draft = inp.value; // keep the source-of-truth draft in sync as the user types
  inp.style.height = 'auto';
  inp.style.height = Math.min(inp.scrollHeight, 120) + 'px';
  var max = chatMaxBytes();
  var used = chatByteLen(inp.value || '');
  var remaining = max - used;
  var over = remaining < 0;
  if (cc) {
    // Twitter-style remaining counter: appears as you approach the limit and
    // shows how many more bytes fit (negative + red when over).
    if (used >= max * 0.6) {
      cc.textContent = remaining + '';
      cc.className = 'chat-charcount show' + (over ? ' over' : (remaining <= max * 0.1 ? ' near' : ''));
    } else {
      cc.className = 'chat-charcount';
      cc.textContent = '';
    }
  }
  if (btn) btn.disabled = over || used === 0 || chatState.sending;
}

function chatInputKey(e) {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    sendChatMessage();
  }
}

// chatShowList switches the thread view back to the conversation list. It does
// NOT touch history — chatBackToList and the popstate handler manage that.
function chatShowList() {
  chatState.view = 'list';
  chatState.peer = null;
  chatRenderList();
  chatLoadThreads();
}

// chatBackToList is the in-thread back arrow. Route through history so the
// button and the hardware back key take the same path (popstate → chatShowList).
function chatBackToList() {
  if (chatThreadPushed) { try { history.back(); } catch (e) { } return; }
  chatShowList();
}

// chatContactInfo is the contact panel: rename, copy account ID, change server.
function chatContactInfo() {
  var addr = chatState.peer;
  if (!addr) return;
  chatCloseMenu();
  var nm = chatState.contacts[addr] || chatName(addr);
  var st = chatState.peerStatus[addr] || {};
  var sheet = document.createElement('div');
  sheet.className = 'chat-sheet-overlay';
  sheet.id = 'chatSheet';
  sheet.onclick = function (e) { if (e.target === sheet) chatCloseMenu(); };
  var h = '<div class="chat-sheet">' +
    '<div class="chat-info-head">' + chatAvatarHTML(nm, 56) +
    '<div class="chat-sheet-title">' + esc(nm) + '</div></div>';
  h += '<div class="chat-info-field"><div class="chat-info-label">' + esc(chatT('chat_account_id')) + '</div>' +
    '<div class="chat-info-value chat-info-id" dir="ltr">' + esc(addr) + '</div>' +
    '<button class="chat-btn-soft" onclick="chatCopy(\'' + escAttr(addr) + '\')">' + esc(chatT('chat_copy')) + '</button></div>';
  // Safety code, next to the address.
  if (st.emojis) {
    h += '<div class="chat-info-field" title="' + escAttr(chatT('chat_safety_explain')) + '">' +
      '<div class="chat-info-label">' + esc(chatT('chat_safety_code')) + '</div>' +
      '<div class="chat-info-value chat-info-safety">' + esc(st.emojis.join(' ')) + '</div></div>';
  }
  h += '<div class="chat-info-field"><div class="chat-info-label">' + esc(chatT('chat_info_server')) + '</div>' +
    '<div class="chat-info-value" dir="auto"><bdi>' + esc(chatServerLabel(chatState.curServer)) + '</bdi></div>' +
    '<button class="chat-btn-soft" onclick="chatCloseMenu();chatSwitchServerSheet()">' + esc(chatT('chat_change_server')) + '</button></div>';
  h += '<button class="chat-sheet-item" onclick="chatContactRename()">' + icon('edit') + ' ' + esc(chatT('chat_rename')) + '</button>';
  h += '<button class="chat-sheet-item chat-sheet-cancel" onclick="chatCloseMenu()">' + esc(chatT('close')) + '</button></div>';
  sheet.innerHTML = h;
  document.getElementById('chatModal').appendChild(sheet);
}

function chatContactRename() {
  chatCloseMenu();
  chatRenamePeer();
}

// chatSwitchServerSheet moves the conversation to another server (verified
// server-side). reasonHtml, if given, is shown as a top warning (bidi-safe HTML).
function chatSwitchServerSheet(reasonHtml) {
  var addr = chatState.peer;
  if (!addr) return;
  chatCloseMenu();
  var servers = chatUsableServers();
  var sheet = document.createElement('div');
  sheet.className = 'chat-sheet-overlay';
  sheet.id = 'chatSheet';
  sheet.onclick = function (e) { if (e.target === sheet) chatCloseMenu(); };
  var items = '<div class="chat-sheet-title">' + esc(chatT('chat_switch_server_title')) + '</div>' +
    (reasonHtml
      ? '<div class="chat-sheet-warn" dir="' + chatDir() + '">' + reasonHtml + '</div>'
      : '<div class="chat-sheet-hint">' + esc(chatT('chat_switch_server_hint')) + '</div>');
  if (!servers.length) {
    items += '<div class="chat-sheet-hint">' + esc(chatT('chat_checking')) + '</div>';
  }
  servers.forEach(function (sv) {
    var label = (sv.name ? sv.name + ' — ' : '') + sv.domain;
    var cur = sv.key === chatState.curServer;
    items += '<button class="chat-sheet-item"' + (cur ? ' disabled' : '') +
      ' onclick="chatSwitchServer(\'' + escAttr(sv.key) + '\')">' + esc(label) +
      (cur ? ' · <span class="chat-server-current">' + esc(chatT('chat_server_current')) + '</span>' : '') + '</button>';
  });
  items += '<button class="chat-sheet-item chat-sheet-cancel" onclick="chatCloseMenu()">' + esc(chatT('cancel')) + '</button>';
  sheet.innerHTML = '<div class="chat-sheet">' + items + '</div>';
  document.getElementById('chatModal').appendChild(sheet);
}

// chatSwitchServer rebinds the conversation to serverKey (backend verifies the
// peer is registered there).
async function chatSwitchServer(serverKey) {
  var addr = chatState.peer;
  if (!addr) return;
  chatCloseMenu();
  showToast(chatT('chat_switch_checking'));
  try {
    var r = await fetch('/api/chat/setserver', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ peer: addr, server: serverKey })
    });
    var d = await r.json();
    if (d.ok) {
      chatState.curServer = d.server;
      delete chatState.pendingServer[addr];
      showToast(chatT('chat_switched_server'));
      if (chatState.view === 'thread' && chatState.peer === addr) chatRenderThread();
    } else {
      var emsg = chatT(d.error || 'chat_err_generic');
      showToast(emsg);
      // Peer not on the chosen server → reopen the picker with the warning.
      if (d.error === 'chat_err_peer_not_on_server') chatSwitchServerSheet(esc(emsg));
    }
  } catch (e) {
    showToast(chatT('chat_err_generic'));
  }
}

function chatRenamePeer() {
  var addr = chatState.peer;
  if (!addr) return;
  chatPrompt(chatT('chat_contact_name'), chatState.contacts[addr] || '', '', function (name) {
    name = (name || '').trim();
    fetch('/api/chat/contacts', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ addr: addr, name: name }) })
      .then(function () {
        if (name) chatState.contacts[addr] = name; else delete chatState.contacts[addr];
        chatRenderThread();
      });
  });
}

async function chatFetchPeerStatus(addr) {
  try {
    var r = await fetch('/api/chat/peer-status?peer=' + encodeURIComponent(addr));
    var d = await r.json();
    if (d.ok) {
      chatState.peerStatus[addr] = d;
      if (d.quota) chatState.quota = d.quota; // live "N sends left this hour"
      if (chatState.view === 'thread' && chatState.peer === addr) chatRenderThread();
    }
  } catch (e) { }
}

async function sendChatMessage() {
  if (chatState.sending) return;
  var inp = document.getElementById('chatInput');
  var btn = document.getElementById('chatSendBtn');
  var text = (inp.value || '').trim();
  if (!text || !chatState.peer) return;
  if (chatByteLen(text) > chatMaxBytes()) {
    showToast(chatT('chat_too_long').replace('{n}', chatMaxBytes()));
    return;
  }

  chatState.sending = true;
  chatState.sendProg = { done: 0, total: 1 };
  chatState.connecting = false;
  btn.disabled = true;
  inp.disabled = true;
  // Reveal "connecting…" only if no block has gone out after a short delay
  // (cold-start session open); warm sends skip straight to the block bar.
  clearTimeout(chatState.sendConnTimer);
  chatState.sendConnTimer = setTimeout(function () {
    if (chatState.sending && (!chatState.sendProg || chatState.sendProg.done < 1)) {
      chatState.connecting = true;
      chatShowProgress('chatSendProgress', 0, 1, '↑'); // done==0 → indeterminate "connecting…"
    }
  }, 700);

  var peerAddr = chatState.peer;
  try {
    var r = await fetch('/api/chat/send', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ peer: peerAddr, text: text, server: chatState.pendingServer[peerAddr] || '' })
    });
    var d = await r.json();
    if (d.ok) {
      // Mark the send finished BEFORE re-rendering so chatRenderThread won't
      // re-disable the row, repaint a stale bar, or restore the sent draft.
      chatState.sending = false;
      chatState.sendProg = null;
      // Clear the draft at the source of truth so the upcoming re-render (and
      // any background render still in flight) restores an empty box instead of
      // the just-sent text. Also clear the live element for instant feedback.
      chatState.draft = '';
      var liveInp = document.getElementById('chatInput');
      if (liveInp) liveInp.value = '';
      if (d.remaining != null) chatState.quota = { remaining: d.remaining, resetUnix: d.resetUnix };
      delete chatState.pendingServer[peerAddr]; // thread is now bound to its server
      await chatRenderThread();
      // Nudge the delivery status: ✓ (accepted) is immediate; ✓✓ lands once
      // the recipient polls. The thread timer keeps refreshing after that.
      var peer = chatState.peer;
      chatFetchPeerStatus(peer);
      setTimeout(function () { if (chatState.peer === peer) chatFetchPeerStatus(peer); }, 3000);
      setTimeout(function () { if (chatState.peer === peer) chatFetchPeerStatus(peer); }, 8000);
    } else {
      var msg = chatT(d.error || 'chat_err_generic');
      if (d.error === 'chat_err_rate_limited' && d.resetUnix) {
        msg += ' (' + chatFmtTime(d.resetUnix) + ')';
      }
      showToast(msg);
      // Not on this server → open the picker with a reason.
      if (d.error === 'chat_err_unknown_recipient' && chatUsableServers().length > 1) {
        chatSwitchServerSheet(chatFmtBidi(chatT('chat_switch_because_absent'),
          { n: chatState.contacts[peerAddr] || chatName(peerAddr), s: chatServerLabel(chatState.curServer) }));
      }
    }
  } catch (e) {
    showToast(chatT('chat_err_generic'));
  } finally {
    chatState.sending = false;
    chatState.sendProg = null;
    clearTimeout(chatState.sendConnTimer);
    chatState.connecting = false;
    var inp2 = document.getElementById('chatInput');
    if (inp2) inp2.disabled = false;
    chatInputResize(); // recompute the send button's enabled state
    chatHideProgress('chatSendProgress');
  }
}

// chatPollNow checks every server for new mail. Rapid taps are ignored while a
// poll is already running (the button shows a spinner + goes disabled), so the
// process never stacks.
async function chatPollNow() {
  if (chatState.polling) return;
  chatState.polling = true;
  chatSetRefreshBusy(true);
  chatState.nextPollAt = Date.now() + CHAT_FG_POLL_MS; // reset the countdown
  chatShowPollProgress(0, 1);
  try {
    var r = await fetch('/api/chat/poll', { method: 'POST' });
    var d = await r.json();
    if (!d.ok) showToast(chatT(d.error || 'chat_err_generic'));
    else if (d.got > 0) showToast(chatT('chat_new_messages').replace('{n}', d.got));
  } catch (e) { }
  chatState.polling = false;
  chatSetRefreshBusy(false);
  chatHidePollProgress();
  chatLoadThreads().then(function () {
    if (chatState.view === 'thread') chatRenderThread();
  });
}

// chatSetRefreshBusy toggles the refresh button's spinner/disabled state (it
// has the same id in both the list and thread headers).
function chatSetRefreshBusy(busy) {
  var b = document.getElementById('chatRefreshBtn');
  if (!b) return;
  b.disabled = busy;
  b.classList.toggle('spinning', busy);
}

// ---- in-chat log viewer ----
// The sidebar #logPanel always receives the server's log lines over SSE, but
// it's hidden behind the full-screen chat modal. So show a log overlay inside
// the modal: seed it from #logPanel's history, then append new lines live.

function chatOpenLog() {
  chatCloseMenu();
  var ov = document.createElement('div');
  ov.className = 'chat-sheet-overlay chat-log-overlay';
  ov.id = 'chatLogOverlay';
  ov.onclick = function (e) { if (e.target === ov) chatCloseLog(); };
  ov.innerHTML = '<div class="chat-log-sheet">' +
    '<div class="chat-log-head"><span>' + esc(chatT('chat_log')) + '</span>' +
    '<button class="chat-icon-btn" aria-label="close" onclick="chatCloseLog()">' + icon('close') + '</button></div>' +
    '<div class="chat-log-body" id="chatLogBody"></div></div>';
  document.getElementById('chatModal').appendChild(ov);
  var body = document.getElementById('chatLogBody');
  var src = document.getElementById('logPanel');
  if (src && body) {
    Array.prototype.forEach.call(src.children, function (c) { body.appendChild(c.cloneNode(true)); });
    body.scrollTop = body.scrollHeight;
  }
}

function chatCloseLog() {
  var o = document.getElementById('chatLogOverlay');
  if (o) o.remove();
  chatFlushPendingRender();
}

// chatLogLive is called by addLogLine (log.js) for each new line so the in-chat
// log stays live while open.
function chatLogLive(line, cls) {
  var body = document.getElementById('chatLogBody');
  if (!body) return;
  var div = document.createElement('div');
  div.className = 'log-line ' + (cls || 'inf');
  div.textContent = line;
  body.appendChild(div);
  body.scrollTop = body.scrollHeight;
  while (body.children.length > 200) body.removeChild(body.firstChild);
}

// ---- progress + SSE glue ----
// arrow: '↑' for uploads (send), '↓' for downloads (receive). The label shows
// the block count so the user sees how many data blocks go up / come down.

function chatShowProgress(id, done, total, arrow) {
  var bar = document.getElementById(id);
  if (!bar) return;
  // Nothing to show, or already complete → hide instead of leaving a full bar
  // stuck on screen. An empty-inbox poll reports done==total==1, which used to
  // stick the "↓ 1/1" bar in the thread list.
  if (!(total > 0) || done >= total) { chatHideProgress(id); return; }
  bar.classList.add('active');
  var fill = document.getElementById(id + 'Fill');
  var label = document.getElementById(id + 'Label');
  // Send at done==0 (still opening the session): show an indeterminate
  // "connecting…" bar instead of a frozen 0/N.
  var connecting = (id === 'chatSendProgress' && done === 0);
  bar.classList.toggle('indeterminate', connecting);
  if (fill) fill.style.width = connecting ? '100%' : (Math.round(done * 100 / total) + '%');
  if (label) {
    label.textContent = connecting
      ? chatT('chat_connecting')
      : ((arrow || '') + ' ' + done + '/' + total + ' ' + chatT('chat_blocks'));
  }
}

function chatHideProgress(id) {
  var bar = document.getElementById(id);
  if (!bar) return;
  bar.classList.remove('active');
  bar.classList.remove('indeterminate');
  var fill = document.getElementById(id + 'Fill');
  if (fill) fill.style.width = '0';
  var label = document.getElementById(id + 'Label');
  if (label) label.textContent = '';
}

// Receive/poll progress can be shown in either view (list has chatPollProgress,
// thread has chatRecvProgress); update whichever is present.
function chatShowPollProgress(done, total) {
  chatShowProgress('chatPollProgress', done, total, '↓');
  chatShowProgress('chatRecvProgress', done, total, '↓');
}
function chatHidePollProgress() {
  chatHideProgress('chatPollProgress');
  chatHideProgress('chatRecvProgress');
}

// Called by sse.js on "chat" events.
function chatOnSSE(data) {
  if (!data || typeof data !== 'object') return;
  if (data.type === 'progress') {
    if (data.op === 'send') {
      chatState.sendProg = { done: data.done, total: data.total };
      // done==0: leave it to the connecting timer; the block bar shows once acked.
      if (data.done >= 1) {
        clearTimeout(chatState.sendConnTimer);
        chatState.connecting = false;
        chatShowProgress('chatSendProgress', data.done, data.total, '↑');
      }
    }
    else if (data.op === 'poll') chatShowPollProgress(data.done, data.total);
    return;
  }
  if (data.type === 'inboxstored') {
    // Messages are stored but not yet acked — render them now so the UI doesn't
    // wait on the ack round trip. Notification is left to the 'inbox' event so
    // it isn't doubled.
    chatLoadThreads().then(function () {
      if (chatState.open && chatState.view === 'thread') chatRenderThread();
    });
    return;
  }
  if (data.type === 'inbox') {
    if (data.got > 0) chatNotifyNew(data.got);
    chatLoadThreads().then(function () {
      if (chatState.open && chatState.view === 'thread') chatRenderThread();
    });
  }
}
