// ===== STATE =====
var selectedChannel = 0, channels = [], eventSource = null, autoRefreshTimer = null, telegramLoggedIn = false, logVisible = false;
// previousMsgIDs is kept for the "no_new_messages" toast on refresh.
// previousContentHashes drives the channel-list NEW badge — robust across
// both Telegram (monotonic IDs) and X accounts (CRC32-hashed snowflake
// IDs that aren't ordered). Both maps are persisted to localStorage so
// they survive page reload and the user keeps seeing badges.
var serverNextFetch = 0, nextFetchInterval = null, previousMsgIDs = loadSeenMap('thefeed_seen_ids'), previousContentHashes = loadSeenMap('thefeed_seen_hashes'), currentMsgTexts = [];
function loadSeenMap(storageKey) {
  try {
    var raw = localStorage.getItem(storageKey);
    if (!raw) return {};
    var parsed = JSON.parse(raw);
    return (parsed && typeof parsed === 'object') ? parsed : {};
  } catch (e) { return {}; }
}
function saveSeenMap(storageKey, m) {
  try { localStorage.setItem(storageKey, JSON.stringify(m)); } catch (e) { }
}
function rememberSeen(name, lastID, contentHash) {
  if (!name) return;
  previousMsgIDs[name] = lastID || 0;
  previousContentHashes[name] = contentHash || 0;
  saveSeenMap('thefeed_seen_ids', previousMsgIDs);
  saveSeenMap('thefeed_seen_hashes', previousContentHashes);
  pushSeen(name, lastID || 0, contentHash || 0);
}
// ----- server-side seen-state -----
// The seen maps used to live only in localStorage, which the WebView wipes
// whenever the loopback port changes between launches (Android/iOS), so the
// unread counts reset to nonsense. We mirror them to disk via /api/seen.
var seenServerSynced = false;
// seenLocalOnly is set when the backend reports shared/multi-user mode
// (--shared). Then seen-state stays in this browser's localStorage and is
// never read from or written to the server, so users sharing one backend
// don't clear each other's unread counts.
var seenLocalOnly = false;
// syncSeenFromServer pulls the disk copy once at startup and lets it win
// over localStorage (the server copy survives the loopback port changing).
async function syncSeenFromServer() {
  if (seenServerSynced) return;
  seenServerSynced = true;
  var d;
  try {
    var r = await fetch('/api/seen');
    if (!r.ok) return;
    d = await r.json();
  } catch (e) { return; }
  if (d && d.shared) {
    // Shared/multi-user backend: keep per-browser localStorage as-is — don't
    // wipe it, don't merge server state, don't push (pushSeen* also bail).
    seenLocalOnly = true;
    return;
  }
  // One-time cleanup: older builds kept seen-state only in localStorage. We
  // deliberately do NOT migrate that to the server (it's origin-scoped and
  // can be stale) — we just drop it so it can't seed the server with wrong
  // counts. The server is the source of truth from here on; with nothing
  // stored, channels re-baseline to "all read" on next load, like Telegram.
  // TODO(v1): remove this one-time localStorage cleanup once every client
  // has launched at least once on a build that persists seen-state to disk.
  try {
    if (!localStorage.getItem('thefeed_seen_cleared')) {
      localStorage.removeItem('thefeed_seen_ids');
      localStorage.removeItem('thefeed_seen_hashes');
      localStorage.setItem('thefeed_seen_cleared', '1');
      previousMsgIDs = {};
      previousContentHashes = {};
    }
  } catch (e) { }
  var sIds = (d && d.seenIds) || {}, sH = (d && d.seenHashes) || {};
  for (var k in sIds) previousMsgIDs[k] = sIds[k];
  for (var k2 in sH) previousContentHashes[k2] = sH[k2];
  saveSeenMap('thefeed_seen_ids', previousMsgIDs);
  saveSeenMap('thefeed_seen_hashes', previousContentHashes);
}
// pushSeen persists a single channel's read marker (fire-and-forget).
function pushSeen(name, id, hash) {
  if (!name || seenLocalOnly) return;
  try {
    fetch('/api/seen', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: name, id: id || 0, hash: hash || 0 }), keepalive: true
    }).catch(function () { });
  } catch (e) { }
}
// pushSeenBulk seeds new channel baselines server-side. The server only
// fills entries it lacks, so a real read marker is never overwritten by a
// baseline.
function pushSeenBulk(ids, hashes) {
  if (seenLocalOnly) return;
  try {
    fetch('/api/seen', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ seenIds: ids, seenHashes: hashes })
    }).catch(function () { });
  } catch (e) { }
}
var appVersion = '', latestVersion = '';
var profiles = null, activeProfileId = '', editingProfileId = null, resolverScanHint = '', resolverScanHealthy = 0, resolverScanDone = 0, resolverScanTotal = 0;
var currentMaxMsgID = 0;
var currentMaxTimestamp = 0;
var newMsgScrollDone = false;
// Per-channel state for the "new messages" separator. The separator is
// kept visible across re-renders for NEW_MSG_STICKY_MS by deferring the
// lastSeen-timestamp commit, so users actually have time to notice the
// new content before it gets marked seen.
var NEW_MSG_STICKY_MS = 10000; // how long the "new messages" tag stays
var newMsgSepLastSeen = {};    // ch name → lastSeenTs the current sep represents
var newMsgSepCommitTimer = {}; // ch name → setTimeout handle for deferred commit
var refreshingChannels = {};

// ===== MOBILE NAV =====
var mobileQuery = window.matchMedia('(max-width: 768px)');
var chatIsOpen = false;
// Desktop: chat is always laid out alongside the sidebar.
// Mobile: chat is only visible when .chat-open is set.
function _chatPanelVisible() {
  return !mobileQuery.matches || document.getElementById('app').classList.contains('chat-open');
}

function openChat() {
  chatIsOpen = true;
  if (mobileQuery.matches) {
    document.getElementById('app').classList.add('chat-open');
    history.pushState({ view: 'chat' }, '');
  }
}
function openSidebar() {
  chatIsOpen = false;
  document.getElementById('app').classList.remove('chat-open');
}
window.addEventListener('popstate', function () {
  if (mobileQuery.matches && document.getElementById('app').classList.contains('chat-open')) {
    openSidebar();
  }
});
document.addEventListener('visibilitychange', function () {
  if (!document.hidden && mobileQuery.matches && chatIsOpen) {
    document.getElementById('app').classList.add('chat-open');
  }
});
function filterChannels() {
  var q = document.getElementById('channelSearch').value.toLowerCase();
  document.querySelectorAll('.ch-item').forEach(function (el) {
    var hay = (el.dataset.name + ' ' + (el.dataset.label || '')).toLowerCase();
    el.style.display = hay.includes(q) ? 'flex' : 'none';
  });
}

// ===== INIT =====
async function init() {
  loadTheme();
  applyLang();
  await loadFontSize();
  loadBgImage();
  connectSSE();
  refreshResolversBadge();
  // Populate profilePicCache so the channel list renders avatars
  // without a per-item probe.
  loadProfilePicState().catch(function () { });
  // Quietly ask GitHub for the latest published client version. Runs in
  // the background so a slow github.com response can't delay startup —
  // if there's an update, the dialog shows up a few seconds later.
  // Skipped on iOS: App Store / TestFlight handles updates there.
  if (typeof IOS === 'undefined') {
    checkGitHubUpdate(false).catch(function () { });
  } else {
    var ghBtn = document.getElementById('checkGitHubBtn');
    if (ghBtn) ghBtn.style.display = 'none';
  }
  try {
    var r = await fetch('/api/status'); var st = await r.json();
    await loadProfiles();
    if (!st.configured) { openProfiles(); return }
    checkAndShowSavedResolversPrompt(st);
    telegramLoggedIn = !!st.telegramLoggedIn;
    serverNextFetch = st.nextFetch || 0;
    latestVersion = st.latestVersion || '';
    renderLatestVersion();
    updateNextFetchDisplay();
    await loadChannels();
    // Land on the channel list; don't auto-open the first channel.
    if (!channels || channels.length === 0) {
      showInitProgress(); await doRefresh();
    } else {
      // Mobile: make sure the sidebar is visible so the user can pick.
      openSidebar();
      document.getElementById('messages').innerHTML = '<div class="empty-state"><p>' + (t('select_channel_hint') || '') + '</p></div>';
    }
    startAutoRefresh();
  } catch (e) { }
}

// ===== FONT SIZE =====
async function loadFontSize() {
  try {
    var r = await fetch('/api/settings'); var s = await r.json();
    if (s.fontSize >= 11 && s.fontSize <= 22) {
      document.documentElement.style.setProperty('--font-size', s.fontSize + 'px');
      document.getElementById('fontSizeSlider').value = s.fontSize;
      document.getElementById('fontSizeVal').textContent = s.fontSize;
    }
    if (s.debug) document.getElementById('cfgDebug').checked = true;
    if (s.theme && (s.theme === 'dark' || s.theme === 'light')) {
      localStorage.setItem('thefeed_theme', s.theme);
      document.documentElement.setAttribute('data-theme', s.theme);
      applyThemeButtons();
    }
    if (s.lang && (s.lang === 'fa' || s.lang === 'en')) {
      lang = s.lang;
      localStorage.setItem('thefeed_lang', s.lang);
      applyLang();
    }
    if (s.version) { appVersion = s.version; renderAppVersion(s.version, s.commit); }
    // Sync the server-persisted "don't show scan prompt" flag
    // into localStorage. Android picks a new 127.0.0.1 port on
    // each launch, so localStorage alone wouldn't survive
    // restart — the server-side flag is the source of truth.
    if (s.scanPromptOff === true) {
      localStorage.setItem('thefeed_scan_prompt_off', '1');
    } else if (s.scanPromptOff === false) {
      localStorage.removeItem('thefeed_scan_prompt_off');
    }
    // Populate pinned channels from the server response (per-profile).
    pinnedChannels = new Set();
    if (Array.isArray(s.pinnedChannels)) {
      for (var pi = 0; pi < s.pinnedChannels.length; pi++) {
        var pn = String(s.pinnedChannels[pi] || '').replace(/^@/, '').trim();
        if (pn) pinnedChannels.add(pn);
      }
    }
    renderLatestVersion();
  } catch (e) { }
}

function renderAppVersion(v, commit) {
  var vEl = document.getElementById('appVersionEl');
  if (!vEl) return;
  if (!v) { vEl.textContent = '-'; return; }
  vEl.textContent = v + (commit && commit !== 'unknown' ? ' (' + commit.slice(0, 7) + ')' : '');
}

function renderLatestVersion() {
  var vEl = document.getElementById('latestVersionEl');
  if (vEl) vEl.textContent = latestVersion || '-';
}

function normalizeVersion(v) {
  if (!v) return '';
  v = String(v).trim().replace(/^v/i, '');
  return v;
}

function compareSemver(a, b) {
  a = normalizeVersion(a); b = normalizeVersion(b);
  if (!a || !b || a === 'dev' || b === 'dev') return 0;
  var as = a.split('.'); var bs = b.split('.');
  var n = Math.max(as.length, bs.length);
  for (var i = 0; i < n; i++) {
    var ai = parseInt(as[i] || '0', 10); if (isNaN(ai)) ai = 0;
    var bi = parseInt(bs[i] || '0', 10); if (isNaN(bi)) bi = 0;
    if (ai > bi) return 1;
    if (ai < bi) return -1;
  }
  return 0;
}

function maybeWarnNewVersion() {
  if (!latestVersion || !appVersion) return;
  if (compareSemver(latestVersion, appVersion) <= 0) return;
  var seenKey = 'thefeed_seen_update_' + normalizeVersion(latestVersion);
  if (localStorage.getItem(seenKey) === '1') return;
  localStorage.setItem(seenKey, '1');
  showToast(t('update_available').replace('{v}', latestVersion));
  addLogLine('Warning: ' + t('update_available').replace('{v}', latestVersion));
}
function previewFontSize(v) { document.documentElement.style.setProperty('--font-size', v + 'px'); document.getElementById('fontSizeVal').textContent = v }

// ===== THEME =====
function loadTheme() {
  var t = localStorage.getItem('thefeed_theme') || 'dark';
  document.documentElement.setAttribute('data-theme', t);
}
function setTheme(t) {
  localStorage.setItem('thefeed_theme', t);
  document.documentElement.setAttribute('data-theme', t);
  applyThemeButtons();
  fetch('/api/settings', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ theme: t }) }).catch(function () { });
}
function applyThemeButtons() {
  var cur = localStorage.getItem('thefeed_theme') || 'dark';
  var d = document.getElementById('themeDark');
  var l = document.getElementById('themeLight');
  if (d) d.classList.toggle('active-theme', cur === 'dark');
  if (l) l.classList.toggle('active-theme', cur === 'light');
}

// ===== LAST SEEN MESSAGES =====
function channelName(num) {
  var ch = channels[num - 1];
  return (ch && (ch.Name || ch.name)) || '';
}
function getLastSeenTimestamp(name) {
  if (!name) return 0;
  try { return parseInt(localStorage.getItem('thefeed_seen_ts_' + name)) || 0 } catch (e) { return 0 }
}
function setLastSeenTimestamp(name, ts) {
  if (!name) return;
  try { localStorage.setItem('thefeed_seen_ts_' + name, ts) } catch (e) { }
}

// ===== RESCAN PROMPT =====
// Resolves true → skip rescan. Honors scanPromptOff.
function askRescan(count) {
  return new Promise(function (resolve) {
    if (localStorage.getItem('thefeed_scan_prompt_off') === '1') { resolve(true); return; }
    if (!count || count <= 0) { resolve(true); return; }
    var msg = t('rescan_prompt_msg').replace('{n}', count);
    var overlay = document.createElement('div');
    overlay.className = 'modal-overlay active';
    overlay.innerHTML =
      '<div class="modal" style="max-width:380px">'
      + '<h2 style="margin-top:0">' + esc(t('rescan_prompt_title')) + '</h2>'
      + '<p style="font-size:13px;color:var(--text-dim);margin-bottom:16px;line-height:1.6">' + esc(msg) + '</p>'
      + '<div style="display:flex;justify-content:flex-end;margin-bottom:10px">'
      + '<button class="btn btn-flat" id="rescanPromptNever" style="font-size:11px;padding:4px 10px;color:var(--text-dim)">' + esc(t('dont_show_again')) + '</button>'
      + '</div>'
      + '<div class="modal-actions">'
      + '<button class="btn btn-outline" id="rescanPromptYes">' + esc(t('rescan_prompt_yes')) + '</button>'
      + '<button class="btn btn-primary" id="rescanPromptSkip">' + esc(t('rescan_prompt_skip')) + '</button>'
      + '</div></div>';
    document.body.appendChild(overlay);
    var done = function (skip) {
      if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
      resolve(skip);
    };
    document.getElementById('rescanPromptSkip').onclick = function () { done(true) };
    document.getElementById('rescanPromptYes').onclick = function () { done(false) };
    document.getElementById('rescanPromptNever').onclick = function () {
      localStorage.setItem('thefeed_scan_prompt_off', '1');
      try {
        fetch('/api/settings', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ scanPromptOff: true })
        });
      } catch (e) { }
      done(true);
    };
  });
}
function showRescanPrompt(count) { return askRescan(count); } // legacy alias
function showConfirmDialog(msg, yesText, noText) {
  return new Promise(function (resolve) {
    var overlay = document.createElement('div');
    overlay.className = 'modal-overlay active';
    overlay.innerHTML = '<div class="modal" style="max-width:380px"><p style="font-size:13px;color:var(--text);margin-bottom:16px;line-height:1.6">' + esc(msg) + '</p><div class="modal-actions"><button class="btn btn-flat" id="confirmNo">' + esc(noText || t('cancel')) + '</button><button class="btn btn-primary" id="confirmYes">' + esc(yesText || t('ok')) + '</button></div></div>';
    document.body.appendChild(overlay);
    document.getElementById('confirmNo').onclick = function () { document.body.removeChild(overlay); resolve(false) };
    document.getElementById('confirmYes').onclick = function () { document.body.removeChild(overlay); resolve(true) };
  });
}

// showInputDialog is a themed window.prompt replacement. Returns
// the trimmed input string on confirm, or null on cancel/escape.
// Uses the existing modal-overlay pattern so it inherits the
// app's theme — no more browser-native prompt with default
// chrome that ignores dark mode.
function showInputDialog(opts) {
  opts = opts || {};
  return new Promise(function (resolve) {
    var overlay = document.createElement('div');
    overlay.className = 'modal-overlay active';
    var titleHtml = opts.title ? '<h2 style="margin-top:0;margin-bottom:8px;font-size:16px">' + esc(opts.title) + '</h2>' : '';
    var msgHtml = opts.message ? '<p style="font-size:13px;color:var(--text-dim);margin:0 0 12px;line-height:1.6">' + esc(opts.message) + '</p>' : '';
    overlay.innerHTML =
      '<div class="modal" style="max-width:380px">'
      + titleHtml + msgHtml
      + '<input type="text" id="inputDialogField" maxlength="' + (opts.maxLength || 64) + '"'
      + ' value="' + esc(opts.value || '') + '"'
      + ' placeholder="' + esc(opts.placeholder || '') + '"'
      + ' autocomplete="off" spellcheck="false"'
      + ' style="width:100%;padding:9px 12px;background:var(--bg);color:var(--text);border:1px solid var(--border);border-radius:8px;font-size:13px;box-sizing:border-box;font-family:inherit">'
      + '<div class="modal-actions">'
      + '<button class="btn btn-flat" id="inputDialogCancel">' + esc(opts.cancelText || t('cancel') || 'Cancel') + '</button>'
      + '<button class="btn btn-primary" id="inputDialogOk">' + esc(opts.okText || t('ok') || 'OK') + '</button>'
      + '</div></div>';
    document.body.appendChild(overlay);
    var field = document.getElementById('inputDialogField');
    var done = function (val) {
      document.removeEventListener('keydown', onKey);
      if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
      resolve(val);
    };
    document.getElementById('inputDialogCancel').onclick = function () { done(null); };
    document.getElementById('inputDialogOk').onclick = function () {
      var v = (field.value || '').trim();
      done(v || null);
    };
    var onKey = function (e) {
      if (e.key === 'Escape') { done(null); }
      else if (e.key === 'Enter') {
        var v = (field.value || '').trim();
        done(v || null);
      }
    };
    document.addEventListener('keydown', onKey);
    // Focus & select after the modal is in the DOM so iOS WebView
    // also gets the soft keyboard up.
    setTimeout(function () { try { field.focus(); field.select(); } catch (e) { } }, 30);
  });
}

// showInfoDialog is the one-button cousin of showConfirmDialog: a small
// modal with a message and a single OK button. Used for explanatory
// bits like "this file is too large for the server cache".
function showInfoDialog(msg, okText) {
  return new Promise(function (resolve) {
    var overlay = document.createElement('div');
    overlay.className = 'modal-overlay active';
    overlay.innerHTML = '<div class="modal" style="max-width:380px"><p style="font-size:13px;color:var(--text);margin-bottom:16px;line-height:1.6;white-space:pre-line">' + esc(msg) + '</p><div class="modal-actions"><button class="btn btn-primary" id="infoOk">' + esc(okText || t('ok') || 'OK') + '</button></div></div>';
    document.body.appendChild(overlay);
    document.getElementById('infoOk').onclick = function () { document.body.removeChild(overlay); resolve(true) };
  });
}

