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
  polling: false,
  fgTimer: null,    // foreground poll while the messenger is open
  cdTimer: null,    // 1s ticker for the "next check in Ns" countdown
  nextPollAt: 0,    // epoch ms of the next foreground poll
  vvBound: false,   // visualViewport listener attached
  notifyReady: false,
  promptCb: null    // pending chatPrompt callback
};

// While the messenger is OPEN, poll the server this often for new messages and
// delivery (✓✓) updates. When the messenger is closed the Go background loop
// polls every 3 min. Each poll is ~1 query when the inbox is empty.
var CHAT_FG_POLL_MS = 15000;

function chatT(key) { return t(key) || key }

function chatName(addr) {
  if (chatState.contacts[addr]) return chatState.contacts[addr];
  return addr.slice(0, 6) + '…' + addr.slice(-4);
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

function openMessenger() {
  document.getElementById('chatModal').classList.add('active');
  chatState.open = true;
  chatRenderList();
  chatLoadInfo();
  chatLoadThreads();
  chatRequestNotifyPermission();
  chatStartForegroundTimer();
  chatBindViewport();
}

function closeMessenger() {
  chatStopForegroundTimer();
  chatUnbindViewport();
  chatState.open = false;
  chatState.view = 'list';
  chatState.peer = null;
  document.getElementById('chatModal').classList.remove('active');
}

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
  if (chatState.info && chatState.info.exists) chatCheckAvailability();
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
  try {
    var r = await fetch('/api/chat/availability');
    chatState.avail = await r.json();
  } catch (e) { chatState.avail = { available: false, reason: 'chat_err_generic' } }
  finally { chatState.availChecking = false; }
  if (!chatState.open) return;
  if (chatState.view === 'list') chatRenderList();
  else chatRenderThread();
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

function chatRenderList() {
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
        '<div class="chat-avatar">' + esc((nm[0] || '?').toUpperCase()) + '</div>' +
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
  navigator.clipboard.writeText(v).then(function () { showToast(t('copied')) }).catch(function () { });
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
  chatState.view = 'thread';
  chatState.peer = addr;
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

// chatSafetyInfo toggles a persistent explainer banner at the top of the
// conversation (a transient toast was easy to miss, especially on touch).
function chatSafetyInfo() {
  var body = document.getElementById('chatMsgsBody');
  if (!body) return;
  var existing = document.getElementById('chatSafetyBanner');
  if (existing) { existing.remove(); return; }
  var st = chatState.peerStatus[chatState.peer] || {};
  var emo = st.emojis ? st.emojis.join(' ') + ' — ' : '';
  var div = document.createElement('div');
  div.id = 'chatSafetyBanner';
  div.className = 'chat-banner chat-banner-info';
  div.textContent = emo + chatT('chat_safety_explain');
  body.insertBefore(div, body.firstChild);
  div.scrollIntoView({ block: 'nearest' });
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
  chatLoadThreads();
}

async function chatRenderThread() {
  var addr = chatState.peer;
  if (!addr) return;
  // Preserve the compose draft, caret and focus across re-renders: the 15s
  // foreground poll and the ✓✓ status refresh both re-render the thread, and
  // rebuilding innerHTML would otherwise wipe whatever the user is typing.
  var prevInput = document.getElementById('chatInput');
  var draft = prevInput ? prevInput.value : null;
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
  var st = chatState.peerStatus[addr] || {};
  var nm = data.name || chatName(addr);
  var threadServer = data.server || chatState.pendingServer[addr] || '';

  var html = '';
  html += '<div class="chat-topbar">';
  html += '<button class="chat-back" onclick="chatBackToList()" aria-label="back">' + icon('arrowLeft') + '</button>';
  html += '<div class="chat-topbar-title" onclick="chatRenamePeer()" style="cursor:pointer">' +
    '<div class="chat-peer-name">' + esc(nm) + ' ' + icon('edit') + '</div>' +
    '<div class="chat-peer-sub">' + esc(addr) + (threadServer ? ' · ' + esc(threadServer) : '') + '</div></div>';
  if (st.emojis) {
    html += '<button type="button" class="chat-safety" title="' + escAttr(chatT('chat_safety_explain')) +
      '" onclick="chatSafetyInfo()">' + st.emojis.join('') + '</button>';
  }
  html += '<button id="chatRefreshBtn" class="chat-icon-btn' + (chatState.polling ? ' spinning' : '') + '" ' +
    (chatState.polling ? 'disabled ' : '') + 'title="' + escAttr(chatT('chat_refresh')) + '" onclick="chatThreadRefresh()">' + icon('refresh') + '</button>';
  html += '</div>';
  html += '<div class="chat-nextpoll-bar"><span id="chatNextPoll" class="chat-next-poll"></span>' +
    '<div class="chat-progress chat-progress-inline" id="chatRecvProgress"><div class="chat-progress-fill" id="chatRecvProgressFill"></div></div>' +
    '<span class="chat-progress-label" id="chatRecvProgressLabel"></span></div>';

  html += '<div class="chat-body" id="chatMsgsBody">';
  if (!msgs.length) {
    html += '<div class="chat-empty">' + esc(chatT('chat_no_messages')) + '</div>';
  }
  var lastDate = '';
  msgs.forEach(function (m) {
    // Day separator: insert a centered date chip whenever the day changes, so a
    // multi-day conversation reads correctly (Jalali for fa, Gregorian for en).
    var dateStr = chatFmtDate(m.ts);
    if (dateStr && dateStr !== lastDate) {
      html += '<div class="chat-date-sep"><span dir="auto">' + esc(dateStr) + '</span></div>';
      lastDate = dateStr;
    }
    var ticks = '';
    if (m.dir === 'out') {
      if (st.delivered >= m.seq) ticks = ' ✓✓';
      else if (st.accepted >= m.seq) ticks = ' ✓';
      else ticks = ' 🕓';
    }
    html += '<div class="chat-msg ' + (m.dir === 'in' ? 'in' : 'out') + '" dir="auto">' +
      '<span class="chat-msg-text">' + esc(m.text) + '</span>' +
      '<span class="chat-msg-meta">' +
      '<button type="button" class="chat-msg-copy" title="' + escAttr(chatT('chat_copy')) +
      '" aria-label="' + escAttr(chatT('chat_copy')) +
      '" onclick="event.stopPropagation();chatCopyMsg(this)">' + icon('copy') + '</button>' +
      '<span class="chat-msg-time">' + chatFmtTime(m.ts) + esc(ticks) + '</span></span></div>' +
      '<div class="chat-clearfix"></div>';
  });
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

  document.getElementById('chatModal').innerHTML = html;
  var body = document.getElementById('chatMsgsBody');
  if (body) body.scrollTop = keepScroll ? prevScroll : body.scrollHeight;
  // Restore the in-progress draft + caret before resizing the input.
  var inp = document.getElementById('chatInput');
  if (inp && draft != null) {
    inp.value = draft;
    if (selStart != null) { try { inp.setSelectionRange(selStart, selEnd); } catch (e) { } }
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

function chatBackToList() {
  chatState.view = 'list';
  chatState.peer = null;
  chatRenderList();
  chatLoadThreads();
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
  btn.disabled = true;
  inp.disabled = true;
  chatShowProgress('chatSendProgress', 0, 1, '↑');

  var peerAddr = chatState.peer;
  try {
    var r = await fetch('/api/chat/send', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ peer: peerAddr, text: text, server: chatState.pendingServer[peerAddr] || '' })
    });
    var d = await r.json();
    if (d.ok) {
      inp.value = '';
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
    }
  } catch (e) {
    showToast(chatT('chat_err_generic'));
  } finally {
    chatState.sending = false;
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
  if (fill && total > 0) fill.style.width = Math.round(done * 100 / total) + '%';
  var label = document.getElementById(id + 'Label');
  if (label) label.textContent = (arrow || '') + ' ' + done + '/' + total + ' ' + chatT('chat_blocks');
}

function chatHideProgress(id) {
  var bar = document.getElementById(id);
  if (!bar) return;
  bar.classList.remove('active');
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
    if (data.op === 'send') chatShowProgress('chatSendProgress', data.done, data.total, '↑');
    else if (data.op === 'poll') chatShowPollProgress(data.done, data.total);
    return;
  }
  if (data.type === 'inbox') {
    if (data.got > 0) chatNotifyNew(data.got);
    chatLoadThreads().then(function () {
      if (chatState.open && chatState.view === 'thread') chatRenderThread();
    });
  }
}
