// ===== SETTINGS =====
function openSettings() {
  renderLatestVersion();
  applyThemeButtons();
  renderPatternGrid();
  switchSettingsTab('display');
  document.getElementById('settingsModal').classList.add('active');
  try { initAndroidSettings(); } catch (e) { console.error('initAndroidSettings error:', e); }
  // Reflect current localStorage flag on every open — toggling it
  // away in the prompt itself should be picked up next time
  // settings is opened.
  var promptEl = document.getElementById('cfgShowScanPrompt');
  if (promptEl) promptEl.checked = localStorage.getItem('thefeed_scan_prompt_off') !== '1';
  // Load server-side settings: profile-pics toggle + the connection
  // fields (queryMode/rateLimit/scatter/timeout, formerly per-profile,
  // now global on ProfileList).
  fetch('/api/settings').then(function (r) { return r.ok ? r.json() : null; }).then(function (s) {
    if (!s) return;
    var pp = document.getElementById('cfgProfilePics');
    if (pp) pp.checked = !!s.profilePicsEnabled;
    var qm = document.getElementById('cfgQueryMode');
    if (qm) qm.value = s.queryMode || 'single';
    var rl = document.getElementById('cfgRateLimit');
    if (rl) rl.value = s.rateLimit || 10;
    var sc = document.getElementById('cfgScatter');
    if (sc) sc.value = s.scatter || 6;
    var to = document.getElementById('cfgTimeout');
    if (to) to.value = s.timeout || 10;
    var rcs = document.getElementById('cfgResolverCacheShare');
    if (rcs) rcs.checked = !!s.resolverCacheShare;
  }).catch(function () { });
}

function switchSettingsTab(tab) {
  var tabs = ['display', 'connection', 'storage', 'backup', 'about'];
  tabs.forEach(function (name) {
    var btn = document.getElementById('settingsTabBtn' + name.charAt(0).toUpperCase() + name.slice(1));
    var panel = document.getElementById('settingsPanel' + name.charAt(0).toUpperCase() + name.slice(1));
    if (btn) {
      btn.classList.toggle('active', name === tab);
      if (name === tab) btn.scrollIntoView({ behavior: 'smooth', block: 'nearest', inline: 'nearest' });
    }
    if (panel) panel.style.display = name === tab ? '' : 'none';
  });
  if (tab === 'storage') loadCacheStats();
}

function toggleHelp(id) {
  var el = document.getElementById(id);
  if (!el) return;
  el.style.display = el.style.display === 'none' ? '' : 'none';
}

// ===== PROFILE PICTURES =====
// Cached lowercase usernames so renderChannels can decide whether
// to overlay an <img> over the initial-letter circle.
var profilePicCache = { enabled: false, users: {} };
var profilePicsPollTimer = null;
// SSE throttle state — server fires one event per stored avatar.
var profilePicsReloadTimer = null;
var profilePicsLastReloadAt = 0;

function loadProfilePicState() {
  // no-store so SSE reloads see fresh data, not a cached entry.
  return fetch('/api/profile-pics', { cache: 'no-store' }).then(function (r) { return r.ok ? r.json() : null; }).then(function (d) {
    if (!d) return;
    profilePicCache.enabled = !!d.enabled;
    profilePicCache.users = {};
    (d.users || []).forEach(function (u) { profilePicCache.users[u.toLowerCase()] = true; });
    try { renderChannels(); } catch (e) { }
  }).catch(function () { });
}

function setProfilePicsEnabled(enabled) {
  // Toggle only controls the DNS path; cached GitHub avatars keep
  // displaying either way.
  fetch('/api/settings', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ profilePicsEnabled: !!enabled })
  }).then(function () {
    profilePicCache.enabled = !!enabled;
    if (enabled) profilePicsRefresh();
  }).catch(function () { });
}

function profilePicsRefresh() {
  var prog = document.getElementById('profilePicsProgress');
  if (prog) prog.textContent = t('profile_pics_starting') || 'Starting…';
  fetch('/api/profile-pics/refresh', { method: 'POST' }).then(function (r) {
    if (!r.ok) {
      if (prog) prog.textContent = t('profile_pics_failed') || 'Failed';
      return;
    }
    // Poll progress until inactive.
    if (profilePicsPollTimer) clearInterval(profilePicsPollTimer);
    profilePicsPollTimer = setInterval(function () {
      fetch('/api/profile-pics/progress').then(function (r) { return r.ok ? r.json() : null; }).then(function (p) {
        if (!p) return;
        if (prog) {
          if (p.active) {
            var label = (t('profile_pics_progress') || 'Refreshing avatars: {n}/{m}').replace('{n}', p.done || 0).replace('{m}', p.total || 0);
            prog.textContent = label + (p.username ? '  (@' + p.username + ')' : '');
          } else {
            prog.textContent = (t('profile_pics_done') || 'Done').replace('{n}', p.done || 0);
            clearInterval(profilePicsPollTimer);
            profilePicsPollTimer = null;
            loadProfilePicState();
          }
        }
      });
    }, 800);
  }).catch(function () { });
}

// Toggle the "show startup scan prompt" preference. Persists
// both client-side (localStorage for fast boot) and server-side
// (so Android keeps it across launches even when the WebView
// origin port changes).
function setShowScanPrompt(enabled) {
  if (enabled) {
    localStorage.removeItem('thefeed_scan_prompt_off');
    // Clear the per-session "already shown" flag so the prompt
    // re-appears on the next reload — without this, re-enabling
    // does nothing until the browser tab is closed.
    sessionStorage.removeItem('thefeed_scan_prompt_shown');
  } else {
    localStorage.setItem('thefeed_scan_prompt_off', '1');
  }
  try {
    fetch('/api/settings', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ scanPromptOff: !enabled })
    });
  } catch (e) { }
}
function closeSettings() { document.getElementById('settingsModal').classList.remove('active') }
async function saveSettings() {
  var fs = parseInt(document.getElementById('fontSizeSlider').value) || 14;
  var dbg = document.getElementById('cfgDebug').checked;
  var qm = document.getElementById('cfgQueryMode').value;
  var rl = parseInt(document.getElementById('cfgRateLimit').value, 10);
  var sc = parseInt(document.getElementById('cfgScatter').value);
  var to = parseFloat(document.getElementById('cfgTimeout').value);
  var body = { fontSize: fs, debug: dbg, queryMode: qm };
  if (rl > 0) body.rateLimit = rl;
  if (sc > 0) body.scatter = sc;
  if (to > 0) body.timeout = to;
  body.resolverCacheShare = document.getElementById('cfgResolverCacheShare').checked;
  try { await fetch('/api/settings', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }) } catch (e) { }
  closeSettings();
}

// ===== ANDROID SETTINGS =====

function initAndroidSettings() {
  if (typeof Android === 'undefined') return;
  document.getElementById('androidPasswordSection').style.display = '';
  // Show correct password section
  try {
    var hasPw = Android.hasPassword();
    document.getElementById('passwordSetSection').style.display = hasPw ? 'none' : '';
    document.getElementById('passwordRemoveSection').style.display = hasPw ? '' : 'none';
    if (!hasPw) document.getElementById('passwordRemovePrompt').style.display = 'none';
  } catch (e) { }
}

function showPasswordRemovePrompt() {
  document.getElementById('passwordRemovePrompt').style.display = '';
  document.getElementById('appPasswordCurrent').value = '';
  document.getElementById('passwordRemoveError').style.display = 'none';
}

function hidePasswordRemovePrompt() {
  document.getElementById('passwordRemovePrompt').style.display = 'none';
}

function setAppPassword() {
  if (typeof Android === 'undefined') return;
  var pw = document.getElementById('appPasswordInput').value;
  var confirm = document.getElementById('appPasswordConfirm').value;
  var errEl = document.getElementById('passwordError');
  errEl.style.display = 'none';
  if (!pw) { errEl.textContent = t('password_empty'); errEl.style.display = ''; return }
  if (pw !== confirm) { errEl.textContent = t('password_mismatch'); errEl.style.display = ''; return }
  try {
    var ok = Android.setPassword(pw);
    if (ok) {
      showToast(t('password_set_ok'));
      document.getElementById('appPasswordInput').value = '';
      document.getElementById('appPasswordConfirm').value = '';
      document.getElementById('passwordSetSection').style.display = 'none';
      document.getElementById('passwordRemoveSection').style.display = '';
    }
  } catch (e) { }
}

async function removeAppPassword() {
  if (typeof Android === 'undefined') return;
  var pw = document.getElementById('appPasswordCurrent').value;
  var errEl = document.getElementById('passwordRemoveError');
  errEl.style.display = 'none';
  if (!pw) { errEl.textContent = t('password_empty'); errEl.style.display = ''; return }
  if (!(await showConfirmDialog(t('password_remove_confirm')))) return;
  try {
    var ok = Android.removePassword(pw);
    if (ok) {
      showToast(t('password_removed'));
      document.getElementById('appPasswordCurrent').value = '';
      document.getElementById('passwordSetSection').style.display = '';
      document.getElementById('passwordRemoveSection').style.display = 'none';
      document.getElementById('passwordRemovePrompt').style.display = 'none';
    } else {
      errEl.textContent = t('password_wrong');
      errEl.style.display = '';
    }
  } catch (e) { }
}

function changeAppPassword() {
  if (typeof Android === 'undefined') return;
  var pw = document.getElementById('appPasswordCurrent').value;
  var errEl = document.getElementById('passwordRemoveError');
  errEl.style.display = 'none';
  if (!pw) { errEl.textContent = t('password_empty'); errEl.style.display = ''; return }
  try {
    var ok = Android.checkPassword(pw);
    if (!ok) {
      errEl.textContent = t('password_wrong');
      errEl.style.display = '';
      return;
    }
    // Remove old and show set section for new password
    Android.removePassword(pw);
    document.getElementById('appPasswordCurrent').value = '';
    document.getElementById('passwordRemoveSection').style.display = 'none';
    document.getElementById('passwordRemovePrompt').style.display = 'none';
    document.getElementById('passwordSetSection').style.display = '';
    showToast(t('password_enter_new'));
  } catch (e) { }
}

// checkGitHubUpdate hits /api/update/github (which reads the latest
// GitHub Release tag for sartoopjj/thefeed) and prompts the user
// with a download link tailored to their platform.
// `manual=true` shows a toast on "no update", `manual=false` stays silent.
async function checkGitHubUpdate(manual) {
  try {
    var r = await fetch('/api/update/github');
    if (!r.ok) {
      if (manual) showToast(t('update_check_failed') || 'Update check failed');
      return;
    }
    var data = await r.json();
    if (!data || !data.latest) return;
    latestVersion = data.latest;
    renderLatestVersion();
    if (data.hasUpdate && data.downloadURL) {
      if (!manual) {
        // Server-stored skip survives per-port localStorage wipes.
        var skipped = '';
        try {
          var sx = new XMLHttpRequest();
          sx.open('GET', '/api/settings', false);
          sx.send();
          if (sx.status === 200) skipped = JSON.parse(sx.responseText).skipUpdateVersion || '';
        } catch (e) { }
        if (!skipped) skipped = localStorage.getItem('thefeed_skip_gh_update_' + normalizeVersion(data.latest)) === '1' ? data.latest : '';
        if (normalizeVersion(skipped) === normalizeVersion(data.latest)) return;
      }
      showUpdateDialog(data.latest, data.downloadURL);
    } else if (manual) {
      showToast((t('version_up_to_date') || 'Up to date: {v}').replace('{v}', data.latest));
    }
  } catch (e) {
    if (manual) showToast(e.message || t('update_check_failed') || 'Update check failed');
  }
}

function showUpdateDialog(newVersion, url) {
  var msg = (t('update_available') || 'New version available: {v}').replace('{v}', newVersion);
  var hint = t('update_download_hint') || 'Download the new version below.';
  var dl = t('update_download_btn') || 'Download';
  var later = t('update_later_btn') || 'Later';
  var skip = t('update_skip_btn') || "Don't show again";
  var skipKey = 'thefeed_skip_gh_update_' + normalizeVersion(newVersion);
  var overlay = document.createElement('div');
  overlay.className = 'modal-overlay active';
  overlay.innerHTML = '<div class="modal" style="max-width:380px">'
    + '<h2 style="margin-top:0">' + esc(msg) + '</h2>'
    + '<p style="font-size:13px;color:var(--text-dim);margin-bottom:12px;line-height:1.6">' + esc(hint) + '</p>'
    + '<p style="font-size:11px;color:var(--text-dim);margin-bottom:16px;word-break:break-all"><code>' + esc(url) + '</code></p>'
    + '<div class="modal-actions" style="flex-wrap:wrap;gap:6px">'
    + '  <button class="btn btn-flat" id="updateSkip">' + esc(skip) + '</button>'
    + '  <button class="btn btn-flat" id="updateLater">' + esc(later) + '</button>'
    + '  <button class="btn btn-primary" id="updateDownload">' + esc(dl) + '</button>'
    + '</div></div>';
  document.body.appendChild(overlay);
  var dismiss = function () { if (overlay.parentNode) overlay.parentNode.removeChild(overlay); };
  var persistSkip = function () {
    try { localStorage.setItem(skipKey, '1'); } catch (e) { }
    fetch('/api/settings', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ skipUpdateVersion: newVersion })
    }).catch(function () { });
  };
  document.getElementById('updateLater').onclick = dismiss;
  document.getElementById('updateSkip').onclick = function () { persistSkip(); dismiss(); };
  document.getElementById('updateDownload').onclick = function () {
    runUpdateDownload(newVersion, overlay);
  };
}

// runUpdateDownload swaps the update dialog's body for a progress
// bar, streams the asset through /api/update/download, and on
// completion triggers a same-page Save As via a Blob URL.
async function runUpdateDownload(newVersion, overlay) {
  var modal = overlay && overlay.querySelector('.modal');
  var progressHTML = ''
    + '<h2 style="margin-top:0">' + esc((t('update_downloading') || 'Downloading {v}…').replace('{v}', newVersion)) + '</h2>'
    + '<div style="background:var(--bg-soft,#222);height:10px;border-radius:5px;overflow:hidden;margin-bottom:8px">'
    + '  <div id="updateProgressBar" style="background:var(--accent,#4caf50);height:100%;width:0%;transition:width .2s"></div>'
    + '</div>'
    + '<div id="updateProgressText" style="font-size:12px;color:var(--text-dim);text-align:center">0 / ?</div>'
    + '<div class="modal-actions" style="margin-top:14px;justify-content:flex-end">'
    + '  <button class="btn btn-flat" id="updateCancel">' + esc(t('cancel') || 'Cancel') + '</button>'
    + '</div>';
  if (modal) modal.innerHTML = progressHTML;

  var controller = new AbortController();
  document.getElementById('updateCancel').onclick = function () {
    controller.abort();
    if (overlay && overlay.parentNode) overlay.parentNode.removeChild(overlay);
  };

  var bar = document.getElementById('updateProgressBar');
  var txt = document.getElementById('updateProgressText');

  var fmtBytes = function (n) {
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    return (n / 1024 / 1024).toFixed(2) + ' MB';
  };

  try {
    var resp = await fetch('/api/update/download?version=' + encodeURIComponent(newVersion), {
      signal: controller.signal,
    });
    if (!resp.ok) {
      var errText = await resp.text();
      throw new Error(errText || ('HTTP ' + resp.status));
    }
    var total = parseInt(resp.headers.get('Content-Length') || '0', 10);
    var filename = resp.headers.get('X-Download-Filename') || ('thefeed-' + newVersion);

    var blob;
    if (resp.body && resp.body.getReader) {
      var reader = resp.body.getReader();
      var chunks = [];
      var received = 0;
      while (true) {
        var step = await reader.read();
        if (step.done) break;
        chunks.push(step.value);
        received += step.value.length;
        if (total > 0) {
          var pct = Math.min(100, (received / total) * 100);
          if (bar) bar.style.width = pct.toFixed(1) + '%';
          if (txt) txt.textContent = fmtBytes(received) + ' / ' + fmtBytes(total)
            + ' (' + pct.toFixed(0) + '%)';
        } else {
          if (txt) txt.textContent = fmtBytes(received);
        }
      }
      blob = new Blob(chunks, { type: 'application/octet-stream' });
    } else {
      // Fallback for old WebViews without ReadableStream. No
      // progress — just wait for the whole response, then save.
      if (txt) txt.textContent = (t('update_downloading_no_progress') || 'Downloading (no progress on this browser)…');
      if (bar) bar.style.width = '100%';
      blob = await resp.blob();
    }

    if (androidBridge && androidBridge.saveMedia) {
      // Android WebView ignores <a download> for blob URLs — route
      // through the native bridge so the file lands in Downloads/.
      if (txt) txt.textContent = (t('update_saving') || 'Saving to Downloads…');
      var b64 = await blobToBase64(blob);
      androidBridge.saveMedia(b64, 'application/octet-stream', filename);
    } else {
      var blobURL = URL.createObjectURL(blob);
      var a = document.createElement('a');
      a.href = blobURL;
      a.download = filename;
      document.body.appendChild(a); a.click(); a.remove();
      setTimeout(function () { URL.revokeObjectURL(blobURL); }, 60000);
    }

    if (txt) txt.textContent = (t('update_saved') || 'Saved') + ': ' + filename;
    try {
      var sk = 'thefeed_skip_gh_update_' + normalizeVersion(newVersion);
      localStorage.setItem(sk, '1');
    } catch (e) { }
    fetch('/api/settings', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ skipUpdateVersion: newVersion })
    }).catch(function () { });

    setTimeout(function () {
      if (overlay && overlay.parentNode) overlay.parentNode.removeChild(overlay);
    }, 1500);
  } catch (e) {
    if (e && e.name === 'AbortError') return;
    if (txt) {
      txt.style.color = 'var(--danger,#e53935)';
      txt.textContent = (t('update_download_failed') || 'Download failed') + ': ' + (e.message || e);
    }
    showToast((t('update_download_failed') || 'Download failed') + ': ' + (e.message || e));
  }
}

async function checkLatestVersion() {
  var btn = document.getElementById('checkVersionBtn');
  var prevText = btn ? btn.textContent : '';
  if (btn) {
    btn.disabled = true;
    btn.textContent = t('checking_version');
  }
  try {
    var r = await fetch('/api/version-check', { method: 'POST' });
    var text = await r.text();
    var data = {};
    try { data = JSON.parse(text) } catch (e) { }
    if (!r.ok) {
      showToast(text || t('version_check_failed'));
      return;
    }
    latestVersion = data.latestVersion || '';
    renderLatestVersion();
    if (!latestVersion) {
      showToast(t('version_check_failed'));
      return;
    }
    if (compareSemver(latestVersion, appVersion) > 0) maybeWarnNewVersion();
    else showToast(t('version_up_to_date').replace('{v}', latestVersion));
  } catch (e) {
    showToast(e.message || t('version_check_failed'));
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.textContent = prevText || t('check_now');
    }
  }
}
// ===== STORAGE / CACHE BUDGET =====

function fmtBytes(n) {
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
  return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB';
}

var _storageLabelMap = {
  'media': 'storage_media',
  'telemirror': 'storage_telemirror',
  'saved-media': 'storage_saved_media',
  'messages': 'storage_messages',
};
var _storageIconMap = {
  'media': '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" style="width:22px;height:22px"><rect x="3" y="3" width="18" height="18" rx="2"/><circle cx="8.5" cy="8.5" r="1.5"/><path d="m21 15-5-5L5 21"/></svg>',
  'telemirror': '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" style="width:22px;height:22px"><path d="m22 2-7 20-4-9-9-4z"/><path d="M22 2 11 13"/></svg>',
  'saved-media': '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" style="width:22px;height:22px"><path d="m19 21-7-4-7 4V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2v16z"/></svg>',
  'messages': '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" style="width:22px;height:22px"><path d="M7.9 20A9 9 0 1 0 4 16.1L2 22z"/></svg>',
};
var _storageColorMap = {
  'media': '#3b82f6',
  'telemirror': '#8b5cf6',
  'saved-media': '#f59e0b',
  'messages': '#10b981',
};

function loadCacheStats() {
  fetch('/api/cache/stats').then(function (r) { return r.ok ? r.json() : null; }).then(function (d) {
    if (!d) return;
    var el = document.getElementById('storageTotalEl');
    if (el) el.textContent = fmtBytes(d.total || 0);
    var inp = document.getElementById('storageBudgetInput');
    if (inp) inp.value = d.budgetMB || 1024;
    var list = document.getElementById('storageCacheList');
    if (!list) return;
    var total = d.total || 1;
    var html = '';
    (d.caches || []).forEach(function (c) {
      var canDelete = c.id !== 'saved-media';
      var label = _storageLabelMap[c.id] ? t(_storageLabelMap[c.id]) : c.label;
      var ico = _storageIconMap[c.id] || '';
      var clr = _storageColorMap[c.id] || 'var(--text-dim)';
      var pct = total > 0 ? Math.max(1, Math.round((c.bytes / total) * 100)) : 0;
      if (c.bytes === 0) pct = 0;
      html += '<div style="display:flex;align-items:center;gap:12px;padding:12px 0;border-bottom:1px solid var(--border)">'
        + '<div style="width:40px;height:40px;border-radius:10px;background:' + clr + '18;color:' + clr + ';display:flex;align-items:center;justify-content:center;flex-shrink:0">' + ico + '</div>'
        + '<div style="flex:1;min-width:0">'
        + '<div style="font-size:14px;color:var(--text);margin-bottom:3px">' + esc(label) + '</div>'
        + '<div style="height:4px;border-radius:2px;background:var(--border);overflow:hidden"><div style="height:100%;width:' + pct + '%;background:' + clr + ';border-radius:2px"></div></div>'
        + '</div>'
        + '<div style="display:flex;align-items:center;gap:6px;flex-shrink:0">'
        + '<span dir="ltr" style="font-size:13px;color:var(--text-dim)">' + fmtBytes(c.bytes) + '</span>'
        + (canDelete ? '<button class="btn btn-flat btn-sm" onclick="clearOneCache(\'' + escAttr(c.id) + '\')" style="color:var(--danger,#e74c3c);padding:4px 6px;font-size:11px;line-height:0">'
          + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" style="width:16px;height:16px"><polyline points="3,6 5,6 21,6"/><path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/><path d="M10 11v6"/><path d="M14 11v6"/></svg></button>' : '')
        + '</div></div>';
    });
    list.innerHTML = html;
  }).catch(function () { });
}

function clearOneCache(which) {
  fetch('/api/cache/clear-one', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ which: which })
  }).then(function (r) {
    if (r.ok) {
      showToast(t('cache_cleared') || 'Cleared');
      loadCacheStats();
    }
  }).catch(function () { });
}

function saveCacheBudget() {
  var inp = document.getElementById('storageBudgetInput');
  var mb = parseInt(inp && inp.value, 10);
  if (!mb || mb < 50) { showToast(t('storage_budget_min') || 'Minimum 50 MB'); return; }
  fetch('/api/cache/budget', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ mb: mb })
  }).then(function (r) {
    if (r.ok) {
      showToast(t('saved') || 'Saved');
      loadCacheStats();
    }
  }).catch(function () { });
}

function toggleResolverBankInfo() {
  var panel = document.getElementById('resolverBankInfoPanel');
  var btn = document.getElementById('resolverBankInfoBtn');
  if (!panel || !btn) return;
  var open = panel.hasAttribute('hidden');
  if (open) panel.removeAttribute('hidden');
  else panel.setAttribute('hidden', '');
  btn.setAttribute('aria-expanded', open ? 'true' : 'false');
}

// ── Backup & Restore ──

function doBackupExport() {
  var pw = document.getElementById('bkExportPw').value;
  if (!pw) { showToast(t('backup_password_required') || 'Enter a password'); return; }
  var sections = [];
  if (document.getElementById('bkProfiles').checked) sections.push('profiles');
  if (document.getElementById('bkChat').checked) sections.push('chat');
  if (document.getElementById('bkSaved').checked) sections.push('saved');
  if (document.getElementById('bkMedia').checked) sections.push('savedMedia');
  if (document.getElementById('bkBgImage').checked) sections.push('bgImage');
  if (!sections.length) { showToast(t('backup_select_section') || 'Select at least one section'); return; }

  var btn = document.getElementById('bkExportBtn');
  btn.disabled = true;
  var origText = btn.textContent;
  btn.textContent = '...';

  fetch('/api/backup/export', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password: pw, sections: sections })
  }).then(function (r) {
    if (r.status === 423) throw new Error(t('backup_saved_locked') || 'Unlock saved messages first');
    if (!r.ok) return r.text().then(function (t) { throw new Error(t); });
    return r.blob();
  }).then(function (blob) {
    triggerDownload(blob, 'thefeed-backup.tfbak');
    if (!(window.Android && window.Android.saveMedia)) {
      showToast(t('backup_exported') || 'Backup exported');
    }
  }).catch(function (e) {
    showToast(e.message || 'Export failed');
  }).finally(function () {
    btn.disabled = false;
    btn.textContent = origText;
    document.getElementById('bkExportPw').value = '';
  });
}

function doBackupPreview() {
  var fileEl = document.getElementById('bkImportFile');
  var pw = document.getElementById('bkImportPw').value;
  if (!fileEl.files.length) { showToast(t('backup_select_file') || 'Select a backup file'); return; }
  if (!pw) { showToast(t('backup_password_required') || 'Enter a password'); return; }

  var fd = new FormData();
  fd.append('file', fileEl.files[0]);
  fd.append('password', pw);

  fetch('/api/backup/preview', { method: 'POST', body: fd })
    .then(function (r) {
      if (r.status === 401) throw new Error(t('backup_wrong_password') || 'Wrong password');
      if (!r.ok) return r.text().then(function (t) { throw new Error(t); });
      return r.json();
    })
    .then(function (d) { renderBackupPreview(d); })
    .catch(function (e) { showToast(e.message || 'Preview failed'); });
}

function renderBackupPreview(d) {
  var el = document.getElementById('bkPreviewResult');
  if (!el) return;
  var inputs = document.getElementById('bkImportInputs');
  if (inputs) inputs.style.display = 'none';

  var html = '<div style="padding:12px;background:var(--bg);border-radius:8px;border:1px solid var(--border)">';

  if (d.createdAt) {
    var dt = new Date(d.createdAt * 1000);
    html += '<div style="font-size:12px;color:var(--text-dim);margin-bottom:8px">' + esc(dt.toLocaleString()) + '</div>';
  }

  var items = [];
  if (d.profiles) {
    items.push({ id: 'profiles', label: t('backup_profiles') || 'Profiles & Settings',
      detail: d.profiles.count + ' — ' + (d.profiles.names || []).join(', ') });
  }
  if (d.chat) {
    var parts = [];
    if (d.chat.identity) parts.push(t('backup_identity') || 'Identity');
    if (d.chat.contacts) parts.push(d.chat.contacts + ' ' + (t('backup_contacts') || 'contacts'));
    if (d.chat.threads) parts.push(d.chat.threads + ' ' + (t('backup_threads') || 'threads'));
    if (d.chat.messages) parts.push(d.chat.messages + ' ' + (t('backup_msgs') || 'messages'));
    items.push({ id: 'chat', label: t('backup_chat') || 'Chat', detail: parts.join(', ') });
  }
  if (d.saved) {
    items.push({ id: 'saved', label: t('backup_saved') || 'Saved Messages',
      detail: d.saved.count + ' ' + (t('backup_items') || 'items') });
  }
  if (d.savedMedia) {
    items.push({ id: 'savedMedia', label: t('backup_media') || 'Saved Media',
      detail: d.savedMedia.count + ' ' + (t('backup_files') || 'files') + ' (' + fmtBytes(d.savedMedia.bytes) + ')' });
  }
  if (d.bgImage) {
    items.push({ id: 'bgImage', label: t('backup_bg') || 'Background Image', detail: fmtBytes(d.bgImage.bytes) });
  }

  items.forEach(function (it) {
    html += '<label style="display:flex;align-items:flex-start;gap:8px;padding:6px 0;font-size:13px;color:var(--text);border-bottom:1px solid var(--border)">'
      + '<input type="checkbox" class="bk-restore-section" value="' + it.id + '" checked style="margin-top:2px">'
      + '<span style="flex:1;min-width:0"><span style="display:block">' + esc(it.label) + '</span>'
      + '<span style="display:block;font-size:11px;color:var(--text-dim);word-break:break-word">' + esc(it.detail) + '</span></span>'
      + '</label>';
  });

  html += '</div>';
  html += '<div style="display:flex;gap:8px;margin-top:10px">'
    + '<button class="btn btn-outline" onclick="hideBackupPreview()" style="flex:1">'
    + (t('cancel') || 'Cancel') + '</button>'
    + '<button class="btn btn-primary" id="bkRestoreBtn" onclick="doBackupRestore()" style="flex:1">'
    + (t('backup_restore_btn') || 'Restore') + '</button>'
    + '</div>';

  el.innerHTML = html;
  el.style.display = '';
}

function hideBackupPreview() {
  var el = document.getElementById('bkPreviewResult');
  if (el) { el.style.display = 'none'; el.innerHTML = ''; }
  var inputs = document.getElementById('bkImportInputs');
  if (inputs) inputs.style.display = '';
}

async function doBackupRestore() {
  var fileEl = document.getElementById('bkImportFile');
  var pw = document.getElementById('bkImportPw').value;
  if (!fileEl.files.length || !pw) return;

  var checks = document.querySelectorAll('.bk-restore-section:checked');
  var sections = [];
  for (var i = 0; i < checks.length; i++) sections.push(checks[i].value);
  if (!sections.length) { showToast(t('backup_select_section') || 'Select at least one section'); return; }

  if (!(await showConfirmDialog(t('backup_restore_confirm') || 'This will overwrite current data. Continue?'))) return;

  var btn = document.getElementById('bkRestoreBtn');
  if (btn) { btn.disabled = true; btn.textContent = '...'; }

  var fd = new FormData();
  fd.append('file', fileEl.files[0]);
  fd.append('password', pw);
  fd.append('sections', JSON.stringify(sections));

  fetch('/api/backup/restore', { method: 'POST', body: fd })
    .then(function (r) {
      if (r.status === 401) throw new Error(t('backup_wrong_password') || 'Wrong password');
      if (r.status === 423) throw new Error(t('backup_saved_locked') || 'Unlock saved messages first');
      if (!r.ok) return r.text().then(function (t) { throw new Error(t); });
      return r.json();
    })
    .then(function () {
      showToast(t('backup_restored') || 'Backup restored');
      setTimeout(function () { location.reload(); }, 1000);
    })
    .catch(function (e) {
      showToast(e.message || 'Restore failed');
      if (btn) { btn.disabled = false; btn.textContent = t('backup_restore_btn') || 'Restore'; }
    });
}

