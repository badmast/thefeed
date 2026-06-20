// ===== LOG =====
function debugLog(line) {
  var el = document.getElementById('cfgDebug');
  if (el && el.checked) addLogLine(line);
}
function addLogLine(line) {
  var el = document.getElementById('logPanel');
  var div = document.createElement('div');
  var cls = 'inf';
  if (typeof line === 'string') {
    // Handle structured resolver scan events — show progress bar, suppress from log
    if (line.includes('RESOLVER_SCAN ')) { updateResolverScanDisplay(line); return }
    if (line.includes('SERVER_FETCH_WAIT ')) { updateServerFetchDisplay(line); return }
    if (line.includes('Error:') || line.includes('error') || line.includes('Invalid passphrase')) cls = 'err';
    else if (line.includes('Warning:')) cls = 'warn';
    else if (line.includes('OK') || line.includes('success') || line.includes('done')) cls = 'ok';
    else if (line.match(/\d+%/)) { cls = 'prog'; updateProgressDisplay(line); return }
  }
  div.className = 'log-line ' + cls; div.textContent = line;
  el.appendChild(div); el.scrollTop = el.scrollHeight;
  while (el.children.length > 200) el.removeChild(el.firstChild);
  // Mirror into the in-chat log overlay if it's open (it's above the modal).
  if (typeof chatLogLive === 'function') chatLogLive(line, cls);
}

function updateServerFetchDisplay(line) {
  var panel = document.getElementById('progressPanel');
  var item = document.getElementById('prog-server-fetch');
  // SERVER_FETCH_WAIT start <totalSec>
  var startMatch = line.match(/SERVER_FETCH_WAIT start (\d+)/);
  if (startMatch) {
    var total = parseInt(startMatch[1]);
    if (!item) {
      item = document.createElement('div'); item.id = 'prog-server-fetch'; item.className = 'progress-item';
      item.innerHTML = '<button class="progress-close" onclick="this.parentNode.remove()" title="Dismiss">&times;</button><div class="progress-label"></div><div class="progress-bar"><div class="progress-fill" style="width:0%;transition:width 1s linear"></div></div>';
      panel.insertBefore(item, panel.firstChild);
    }
    item.dataset.total = total;
    item.querySelector('.progress-label').textContent = t('server_fetch_wait') + ' — ' + total + 's';
    item.querySelector('.progress-fill').style.width = '0%';
    item.dataset.lastUpdate = Date.now();
    return;
  }
  if (!item) return;
  // SERVER_FETCH_WAIT tick <remaining>/<total>
  var tickMatch = line.match(/SERVER_FETCH_WAIT tick (\d+)\/(\d+)/);
  if (tickMatch) {
    var remaining = parseInt(tickMatch[1]), total2 = parseInt(tickMatch[2]);
    var pct = Math.round(((total2 - remaining) / total2) * 100);
    item.querySelector('.progress-label').textContent = t('server_fetch_wait') + ' — ' + remaining + 's';
    item.querySelector('.progress-fill').style.width = pct + '%';
    item.dataset.lastUpdate = Date.now();
    return;
  }
  // SERVER_FETCH_WAIT done
  if (line.includes('SERVER_FETCH_WAIT done')) {
    item.querySelector('.progress-label').textContent = t('loading');
    item.querySelector('.progress-fill').style.width = '100%';
    setTimeout(function () { if (item.parentNode) item.parentNode.removeChild(item) }, 1200);
  }
}

function ensureResolverScanItem() {
  var item = document.getElementById('prog-resolvers');
  if (!item) {
    var panel = document.getElementById('progressPanel');
    // Remove the generic init loading bar when resolver scan takes over
    var initItem = document.getElementById('prog-init'); if (initItem) initItem.remove();
    item = document.createElement('div'); item.id = 'prog-resolvers'; item.className = 'progress-item';
    item.innerHTML = '<button class="progress-close" onclick="this.parentNode.remove()" title="Dismiss">&times;</button><div class="progress-label"></div><div class="progress-bar"><div class="progress-fill" style="width:0%"></div></div>';
    panel.insertBefore(item, panel.firstChild);
  }
  return item;
}

function updateResolverScanDisplay(line) {
  var panel = document.getElementById('progressPanel');
  var item = document.getElementById('prog-resolvers');
  // RESOLVER_SCAN start N
  var startMatch = line.match(/RESOLVER_SCAN start (\d+)/);
  if (startMatch) {
    var total = parseInt(startMatch[1]);
    resolverScanDone = 0; resolverScanHealthy = 0; resolverScanTotal = total;
    item = ensureResolverScanItem();
    item.dataset.total = total;
    item.querySelector('.progress-label').textContent = t('scanning_resolvers') + ' 0/' + total;
    item.querySelector('.progress-fill').style.width = '0%';
    item.dataset.lastUpdate = Date.now();
    resolverScanHint = t('scanning_resolvers') + '... <button onclick="jumpToLog()" style="background:none;border:none;cursor:pointer;font-size:13px;vertical-align:middle;padding:0 2px">' + icon('log') + '</button>';
    var hintEl = document.getElementById('no-ch-hint'); if (hintEl) hintEl.innerHTML = resolverScanHint;
    return;
  }
  // If the item was removed (e.g. SSE reconnect cleared the panel) but scan
  // is still in progress, re-create it so progress updates keep showing.
  if (!item) item = ensureResolverScanItem();
  // RESOLVER_SCAN progress D/T healthy=H
  var progMatch = line.match(/RESOLVER_SCAN progress (\d+)\/(\d+)(?: healthy=(\d+))?/);
  if (progMatch) {
    var done = parseInt(progMatch[1]), tot = parseInt(progMatch[2]), hlthy = progMatch[3] !== undefined ? parseInt(progMatch[3]) : null;
    // Use authoritative values from the structured message.
    resolverScanDone = done; resolverScanTotal = tot;
    if (hlthy !== null) resolverScanHealthy = hlthy;
    var pct = Math.round((done / tot) * 100);
    var label = t('scanning_resolvers') + ' ' + done + '/' + tot + ' \u2713' + resolverScanHealthy;
    item.querySelector('.progress-label').textContent = label;
    item.querySelector('.progress-fill').style.width = pct + '%';
    item.dataset.lastUpdate = Date.now();
    resolverScanHint = t('scanning_resolvers') + ' (' + done + '/' + tot + ', \u2713' + resolverScanHealthy + ')' + ' <button onclick="jumpToLog()" style="background:none;border:none;cursor:pointer;font-size:13px;vertical-align:middle;padding:0 2px">' + icon('log') + '</button>';
    var hintEl = document.getElementById('no-ch-hint'); if (hintEl) hintEl.innerHTML = resolverScanHint;
    return;
  }
  // RESOLVER_SCAN cancelled
  if (line.includes('RESOLVER_SCAN cancelled')) {
    resolverScanHint = '';
    if (item && item.parentNode) item.parentNode.removeChild(item);
    return;
  }
  // RESOLVER_SCAN done K/T
  var doneMatch = line.match(/RESOLVER_SCAN done (\d+)\/(\d+)/);
  if (doneMatch) {
    var healthy = parseInt(doneMatch[1]), total2 = parseInt(doneMatch[2]);
    resolverScanDone = total2; resolverScanHealthy = healthy; resolverScanTotal = total2;
    item.querySelector('.progress-label').textContent = t('scanning_resolvers') + ': ' + healthy + '/' + total2 + ' active';
    item.querySelector('.progress-fill').style.width = '100%';
    resolverScanHint = '';
    // Remove init loading bar if it's still around
    var initItem = document.getElementById('prog-init'); if (initItem) initItem.remove();
    var hintEl = document.getElementById('no-ch-hint'); if (hintEl) hintEl.innerHTML = t('no_channels_hint') + ' <button onclick="jumpToLog()" style="background:none;border:none;cursor:pointer;font-size:13px;vertical-align:middle;padding:0 2px">' + icon('log') + '</button> ' + t('no_channels_hint2');
    setTimeout(function () { if (item.parentNode) item.parentNode.removeChild(item) }, 2000);
    // Scan is done — reload the list in case the SSE 'update' event was dropped.
    // Don't auto-open a channel: a programmatic open racing a user Back caused
    // the hang. If nothing is open yet (first-run loading pane), land on the list.
    setTimeout(function () {
      loadChannels().then(function () {
        if (mobileQuery.matches && selectedChannel === 0 &&
          document.getElementById('app').classList.contains('chat-open')) {
          feedBack();
        }
      });
    }, 3000);
    refreshResolversBadge();
  }
}

var progressSilencedUntil = 0;
function updateProgressDisplay(line) {
  // Silence briefly across a profile switch — log events from the
  // previous profile's in-flight fetch keep arriving until the old
  // fetcher's context cancellation lands.
  if (Date.now() < progressSilencedUntil) return;
  var match = line.match(/Channel\s+(\d+)/); if (!match) return;
  var channelNum = parseInt(match[1]);
  var fracMatch = line.match(/\((\d+)\/(\d+)\)/);
  var completed = fracMatch ? parseInt(fracMatch[1]) : 0;
  var total = fracMatch ? parseInt(fracMatch[2]) : 0;
  var pct = line.match(/(\d+)%/); var percent = pct ? parseInt(pct[1]) : 0;
  var ch = channels[channelNum - 1];
  var chName = ch && (ch.Name || ch.name) || '';
  var label = (fracMatch ? (completed + '/' + total) : ('Channel ' + channelNum)) + (chName ? ' (' + chName + ')' : '');
  var panel = document.getElementById('progressPanel');
  var item = document.getElementById('prog-ch-' + channelNum);
  var fetchBar = document.getElementById('prog-fetch-ch-' + channelNum); if (fetchBar) fetchBar.remove();
  if (!item) {
    item = document.createElement('div'); item.id = 'prog-ch-' + channelNum; item.className = 'progress-item';
    item.innerHTML = '<button class="progress-close" onclick="this.parentNode.remove()" title="' + esc(t('dismiss') || 'Dismiss') + '">&times;</button><div class="progress-label"></div><div class="progress-bar"><div class="progress-fill" style="width:0%"></div></div>';
    panel.appendChild(item);
  }
  item.querySelector('.progress-label').textContent = label;
  item.querySelector('.progress-fill').style.width = percent + '%';
  // Mark last-update time so stale items can be cleaned
  item.dataset.lastUpdate = Date.now();
  if (percent >= 100) setTimeout(function () { if (item.parentNode) item.parentNode.removeChild(item) }, 800);
}

// Periodically remove stale progress items that stopped updating (missed 'done' event)
setInterval(function () {
  var now = Date.now();
  var items = document.getElementById('progressPanel').querySelectorAll('.progress-item');
  for (var i = 0; i < items.length; i++) {
    var lu = parseInt(items[i].dataset.lastUpdate || '0');
    if (lu > 0 && now - lu > 30000) items[i].remove();
  }
}, 10000);

function toggleLog() {
  logVisible = !logVisible;
  var p = document.getElementById('logPanel'); var ic = document.getElementById('logToggleIcon');
  p.classList.toggle('hidden', !logVisible);
  ic.innerHTML = logVisible ? icon('chevronDown') : icon('chevronRight');
}
function openLog() {
  if (logVisible) return;
  logVisible = true;
  document.getElementById('logPanel').classList.remove('hidden');
  document.getElementById('logToggleIcon').innerHTML = icon('chevronDown');
}
function jumpToLog() {
  openChat();
  openLog();
  setTimeout(function () { document.getElementById('logPanel').scrollIntoView({ behavior: 'smooth', block: 'end' }) }, 300);
}

// ===== REFRESH =====
function showInitProgress() {
  document.getElementById('progressPanel').innerHTML = '';
  var p = document.getElementById('progressPanel');
  p.innerHTML = '<div class="progress-item" id="prog-init" data-last-update="' + Date.now() + '"><button class="progress-close" onclick="this.parentNode.remove()" title="Dismiss">&times;</button><div class="progress-label">' + t('loading') + '</div><div class="progress-bar"><div class="progress-fill" style="width:30%;animation:prog-pulse 1.5s ease-in-out infinite"></div></div></div>';
  if (mobileQuery.matches) { openChat(); }
}
function startAutoRefresh() { if (autoRefreshTimer) return; autoRefreshTimer = setInterval(function () { if (selectedChannel > 0) doRefresh(true) }, 600000) }
function updateNextFetchDisplay() {
  if (nextFetchInterval) clearInterval(nextFetchInterval);
  var el = document.getElementById('nextFetchTimer');
  var info = document.getElementById('nextFetchInfoBtn');
  if (!serverNextFetch) { el.textContent = ''; if (info) info.style.display = 'none'; return }
  if (info) { info.style.display = ''; info.title = t('next_fetch_info') }
  function tick() {
    var now = Math.floor(Date.now() / 1000), d = serverNextFetch - now;
    if (d <= 0) {
      el.textContent = '';
      return
    }
    var m = Math.floor(d / 60), s = d % 60; el.textContent = m + ':' + (s < 10 ? '0' : '') + s
  }
  tick(); nextFetchInterval = setInterval(tick, 1000);
}
// Channel number for which the user explicitly clicked Refresh.
// The "no new messages" toast fires only for that channel; auto-
// refresh ticks and channel-open refreshes leave it at 0 so the
// toast stays silent.
var manualRefreshChannel = 0;

async function doRefreshUI() {
  var btn = document.getElementById('refreshBtn');
  btn.style.animation = 'spin .8s linear';
  showToast(t('refreshing'));
  if (selectedChannel > 0) {
    var ch = channels[selectedChannel - 1];
    var name = (ch && (ch.Name || ch.name)) || 'Channel ' + selectedChannel;
    showChannelFetchProgress(selectedChannel, name);
    refreshingChannels[selectedChannel] = true;
    manualRefreshChannel = selectedChannel;
  }
  doRefresh(false);
  setTimeout(function () { btn.style.animation = '' }, 800);
}
async function doRefresh(quiet) {
  try {
    var url = '/api/refresh';
    if (selectedChannel > 0) url += '?channel=' + selectedChannel;
    if (quiet) url += (url.includes('?') ? '&' : '?') + 'quiet=1';
    await fetch(url, { method: 'POST' });
    if (!quiet && selectedChannel > 0) setTimeout(function () { loadChannels(); if (_chatPanelVisible()) loadMessages(selectedChannel) }, 3000);
  } catch (e) { }
}

