// ===== RESOLVER BANK =====
var currentResolverTab = 'active';
function updateResolversBadge(count, bankCount) {
  var total = bankCount !== undefined ? bankCount : count;
  // Bottom-nav Resolver tab badge: active / total bank resolvers.
  var nb = document.getElementById('navResolverBadge');
  if (nb) {
    nb.textContent = count + '/' + total;
    nb.hidden = false;
    nb.classList.toggle('nav-badge-warn', total > 500 || count === 0);
  }
  var badge = document.getElementById('resolversBadge');
  if (!badge) return;
  badge.textContent = count + ' / ' + total;
  if (total > 500) {
    badge.style.color = 'var(--error, #e74c3c)';
  } else {
    badge.style.color = count > 0 ? 'var(--success, #27ae60)' : 'var(--error, #e74c3c)';
  }
}
async function refreshResolversBadge() {
  try {
    var r = await fetch('/api/resolvers/bank');
    if (!r.ok) return;
    var data = await r.json();
    var activeCount = 0;
    (data.bank || []).forEach(function (b) { if (b.active) activeCount++ });
    updateResolversBadge(activeCount, data.count || 0);
  } catch (e) { }
}
var resolversRefreshTimer = null;
function _buildScoreboardTable(board, showRemove, removeFromBank, removeFnOverride) {
  if (!board.length) return '<div style="color:var(--text-dim)">' + t('no_active_resolvers') + '</div>';
  // Row-card layout — left holds resolver address + a stats line
  // (speed · score · ✅ · ❌), right holds the action buttons.
  // Replaces the old 6-column table that overflowed off the right
  // edge on phone widths and hid the × button.
  var h = '<div class="rb-rows">';
  for (var i = 0; i < board.length; i++) {
    var b = board[i];
    var scoreColor = b.score >= 0.5 ? 'var(--success)' : b.score >= 0.15 ? 'var(--text)' : 'var(--error)';
    var dot = (b.active !== undefined && b.active)
      ? ' <span style="color:var(--success);font-size:10px">' + icon('dotFilled') + '</span>' : '';
    h += '<div class="rb-row">';
    h += '<div class="rb-row-main">';
    h += '<div class="rb-row-addr">' + esc(b.addr) + dot + '</div>';
    h += '<div class="rb-row-stats">';
    h += '<span>' + (b.avgMs > 0 ? Math.round(b.avgMs) + 'ms' : '-') + '</span>';
    h += '<span style="color:' + scoreColor + ';font-weight:600">' + b.score.toFixed(2) + '</span>';
    h += '<span style="color:var(--success)">' + icon('success') + ' ' + b.success + '</span>';
    h += '<span style="color:var(--error)">' + icon('fail') + ' ' + b.failure + '</span>';
    h += '</div></div>';
    if (showRemove) {
      var fn = removeFnOverride || (removeFromBank ? 'removeResolverFromBank' : 'removeResolver');
      h += '<div class="rb-row-actions">';
      if (removeFromBank) {
        h += '<button class="rb-row-btn rb-row-add" onclick="openBankAddPicker(this,\'' + jsStr(b.addr) + '\')" '
          + 'data-i18n-title="add_to_list" title="Add to list" aria-label="Add to list">' + icon('add') + '</button>';
      }
      h += '<button class="rb-row-btn rb-row-del" onclick="' + fn + '(\'' + jsStr(b.addr) + '\')" '
        + 'title="Remove" aria-label="Remove">&times;</button>';
      h += '</div>';
    }
    h += '</div>';
  }
  h += '</div>';
  return h;
}
async function _fetchActiveBoard() {
  var el = document.getElementById('resolverPanelActive');
  try {
    var r = await fetch('/api/resolvers/active');
    if (!r.ok) throw new Error(await r.text());
    var data = await r.json();
    var board = data.scoreboard || [];
    el.innerHTML = _buildScoreboardTable(board, true, false);
    // Sync the Bank tab badge so it stays current while the user
    // sits on the Active view.
    try {
      var br = await fetch('/api/resolvers/bank');
      if (br.ok) {
        var bd = await br.json();
        var bc = document.getElementById('resolverBankCount');
        if (bc) bc.textContent = bd.count || 0;
      }
    } catch (e2) { }
  } catch (e) { el.innerHTML = '<div style="color:var(--error)">' + esc(e.message) + '</div>' }
}
async function _fetchBankBoard() {
  var el = document.getElementById('resolverBankListEl');
  try {
    var r = await fetch('/api/resolvers/bank');
    if (!r.ok) throw new Error(await r.text());
    var data = await r.json();
    var bank = data.bank || [];
    var activeCount = 0;
    bank.forEach(function (b) { if (b.active) activeCount++ });
    var countEl = document.getElementById('resolverBankCount');
    if (countEl) {
      // Show active / total so the user sees how many are currently usable.
      countEl.textContent = activeCount + ' / ' + (data.count || 0);
      countEl.style.color = (data.count || 0) > 500 ? 'var(--error)' : '';
    }
    updateResolversBadge(activeCount, data.count || 0);
    el.innerHTML = _buildScoreboardTable(bank, true, true);
    previewBankCleanup();
  } catch (e) { el.innerHTML = '<div style="color:var(--error)">' + esc(e.message) + '</div>' }
}
function switchResolverTab(tab) {
  currentResolverTab = tab;
  var pa = document.getElementById('resolverPanelActive');
  var pb = document.getElementById('resolverPanelBank');
  if (pa) pa.style.display = tab === 'active' ? '' : 'none';
  if (pb) pb.style.display = tab === 'bank' ? '' : 'none';
  // Tab styling lives on the dynamic strip — re-render so the
  // accent underline tracks the new selection. renderResolverTabs
  // also reads currentResolverTab and decorates the bank tab.
  renderResolverTabs();
  if (tab === 'active') _fetchActiveBoard();
  else _fetchBankBoard();
}
async function openResolversModal() {
  document.getElementById('resolversModal').classList.add('active');
  // Load autoScan from active profile
  var autoScanEl = document.getElementById('bankAutoScan');
  if (autoScanEl && profiles && profiles.profiles && activeProfileId) {
    var p = profiles.profiles.find(function (x) { return x.id === activeProfileId });
    if (p) autoScanEl.checked = p.config.autoScan !== false;
  }
  switchResolverTab('active');
  refreshResolversBadge();
  // Populate the list pill strip at the top of the modal.
  loadResolverLists();
  if (resolversRefreshTimer) clearInterval(resolversRefreshTimer);
  resolversRefreshTimer = setInterval(function () {
    if (!document.getElementById('resolversModal').classList.contains('active')) {
      clearInterval(resolversRefreshTimer); resolversRefreshTimer = null; return;
    }
    if (currentResolverTab === 'active') _fetchActiveBoard();
    else _fetchBankBoard();
  }, 3000);
}
function closeResolversModal() {
  document.getElementById('resolversModal').classList.remove('active');
  if (resolversRefreshTimer) { clearInterval(resolversRefreshTimer); resolversRefreshTimer = null; }
}
function openScannerFromBank() {
  closeResolversModal();
  openScanner();
}
async function doRescanFromBank() {
  closeResolversModal();
  showToast(t('rescan_started'));
  document.getElementById('progressPanel').innerHTML = '';
  showInitProgress();
  try { await fetch('/api/rescan', { method: 'POST' }) } catch (e) { }
  setTimeout(function () { loadChannels().then(function () { if (selectedChannel > 0) loadMessages(selectedChannel) }); refreshResolversBadge(); }, 3000);
}
async function toggleBankAutoScan() {
  var checked = document.getElementById('bankAutoScan').checked;
  if (!profiles || !profiles.profiles || !activeProfileId) return;
  var p = profiles.profiles.find(function (x) { return x.id === activeProfileId });
  if (!p) return;
  var profile = JSON.parse(JSON.stringify(p));
  profile.config.autoScan = checked ? undefined : false;
  try {
    await fetch('/api/profiles', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'update', profile: profile, skipCheck: true }) });
    await loadProfiles();
  } catch (e) { showToast(e.message) }
}
async function removeResolver(addr) {
  try {
    await fetch('/api/resolvers/remove', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ addr: addr }) });
    _fetchActiveBoard();
  } catch (e) { }
}
async function removeResolverFromBank(addr) {
  try {
    await fetch('/api/resolvers/bank', { method: 'DELETE', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ addrs: [addr] }) });
    _fetchBankBoard();
  } catch (e) { }
}
async function resetScoreboard() {
  try {
    await fetch('/api/resolvers/reset-stats', { method: 'POST' });
    if (currentResolverTab === 'active') _fetchActiveBoard();
    else _fetchBankBoard();
  } catch (e) { }
}
function copyResolversList() {
  var panelId = currentResolverTab === 'active' ? 'resolverPanelActive' : 'resolverBankListEl';
  // New row layout uses .rb-row-addr divs in place of <td>.
  var addrs = document.querySelectorAll('#' + panelId + ' .rb-row-addr');
  var lines = [];
  addrs.forEach(function (el) {
    // Strip the trailing active-dot span if present.
    var clone = el.cloneNode(true);
    var dot = clone.querySelector('span');
    if (dot) dot.remove();
    var t = clone.textContent.trim();
    if (t) lines.push(t);
  });
  if (!lines.length) { showToast(t('no_active_resolvers')); return }
  navigator.clipboard.writeText(lines.join('\n')).then(function () { showToast(t('copied')) });
}
async function previewBankCleanup() {
  var val = parseFloat(document.getElementById('bankCleanupSlider').value) || 0.1;
  document.getElementById('bankCleanupValue').textContent = val.toFixed(2);
  try {
    var r = await fetch('/api/resolvers/bank/cleanup', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ minScore: val, dryRun: true }) });
    if (!r.ok) return;
    var data = await r.json();
    // dir="auto" picks RTL from the Persian text (the pane is otherwise forced
    // LTR, which pushed the leading count to the wrong side); <bdi> isolates the
    // LTR numbers so they sit at their logical place in the sentence.
    var prev = document.getElementById('bankCleanupPreview');
    prev.dir = 'auto';
    prev.innerHTML = '<bdi style="color:var(--error)">' + data.removed + '</bdi> ' + t('would_be_removed') + ', <bdi style="color:var(--success)">' + data.remaining + '</bdi> ' + t('would_remain');
  } catch (e) { }
}
async function doBankCleanup() {
  var val = parseFloat(document.getElementById('bankCleanupSlider').value) || 0.1;
  try {
    var r = await fetch('/api/resolvers/bank/cleanup', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ minScore: val }) });
    if (!r.ok) { showToast('Cleanup failed'); return }
    var data = await r.json();
    showToast(t('removed') + ': ' + data.removed + ', ' + t('remaining') + ': ' + data.remaining);
    _fetchBankBoard();
    refreshResolversBadge();
  } catch (e) { showToast(e.message) }
}
async function addResolversToBank() {
  var text = document.getElementById('bankAddResolvers').value.trim();
  var resolvers = text.split(/[\n,;\s]+/).map(function (s) { return s.trim() }).filter(Boolean);
  if (!resolvers.length) return;
  try {
    var r = await fetch('/api/resolvers/bank', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ resolvers: resolvers }) });
    if (!r.ok) { showToast('Add failed'); return }
    var data = await r.json();
    showToast(t('added') + ': ' + data.added);
    document.getElementById('bankAddResolvers').value = '';
    _fetchBankBoard();
    refreshResolversBadge();
  } catch (e) { showToast(e.message) }
}

// ===== RESOLVER SECTION (Scanner + Bank merged into the unified shell) =====
// Reparent pattern mirrors Mirror/Chat: the named lists / Bank / Scanner entry
// and actions live in #resolverSidebar; the boards + scanner form live in
// #resolverPane. The dense bank/scanner DOM is MOVED out of the old modals once
// (resolverReparentOnce) so all existing render logic keeps working unchanged.

var resolverView = 'active';        // 'active' | 'bank' | 'scanner' | 'list'
var resolverViewedList = '';        // name of the list shown in the pane (view ≠ activate)
var resolverMenuOpen = null;        // name of the list whose ⋮ menu is open
var _resolverReparented = false;
var _resolverRefreshTimer = null;
var resolverSectionOpen = false;
var resolverHistoryPushed = false;
var resolverSuppressPopstate = 0;
var resolverPanePushed = false;     // mobile: a list/scanner pane is open over the sidebar

function resolverReparentOnce() {
  if (_resolverReparented) return;
  var content = document.getElementById('resolverContent');
  if (!content) return;
  // Boards view: the active + bank panels relocated from the old bank modal.
  var boards = document.createElement('div');
  boards.id = 'resolverBoardsView';
  var pa = document.getElementById('resolverPanelActive');
  var pb = document.getElementById('resolverPanelBank');
  [pa, pb].forEach(function (el) {
    if (!el) return;
    // Drop the modal's inline max-height/overflow — the pane (.tm-content)
    // scrolls now, so a nested scrollbox would double-scroll.
    el.style.maxHeight = '';
    el.style.overflowY = '';
    boards.appendChild(el);
  });
  // Drop the modal's auto-scan toggle row — it's rendered in the sidebar now
  // (keeping it would duplicate the #bankAutoScan id). Target the modal copy
  // specifically so we never remove the sidebar-rendered one.
  var auto = document.querySelector('#resolversModal #bankAutoScan');
  if (auto) { var row = auto.closest('div'); if (row) row.remove(); }
  content.appendChild(boards);
  // The Bank "+" popover (#bankAddMenu) was anchored inside the old modal —
  // move it into the pane (a positioned, visible ancestor) so it shows.
  var pane = document.getElementById('resolverPane');
  var addMenu = document.getElementById('bankAddMenu');
  if (pane && addMenu) pane.appendChild(addMenu);
  // Scanner view: move the scanner modal's inner content wholesale.
  var scanWrap = document.createElement('div');
  scanWrap.id = 'resolverScannerView';
  scanWrap.style.display = 'none';
  var scanInner = document.querySelector('#scannerModal > .modal');
  if (scanInner) { while (scanInner.firstChild) scanWrap.appendChild(scanInner.firstChild); }
  content.appendChild(scanWrap);
  _resolverReparented = true;
}

function resolverUpdateTitle() {
  var el = document.getElementById('resolverPaneTitle');
  if (!el) return;
  if (resolverView === 'scanner') el.textContent = t('scanner_title') || 'Scanner';
  else if (resolverView === 'bank') el.textContent = t('resolver_tab_bank') || 'Bank';
  else if (resolverView === 'list') el.textContent = resolverViewedList || (t('resolver_active_list') || 'Active');
  else {
    var sel = ((rlState && rlState.lists) || []).find(function (l) { return l.selected; });
    el.textContent = sel ? sel.name : (t('resolver_active_list') || 'Active');
  }
}

// resolverShowView swaps the content pane between the boards (active/bank) and
// the scanner. swapMobile=true also slides to the pane on mobile (a sidebar tap).
function resolverShowView(view, swapMobile) {
  resolverView = view;
  resolverViewedList = '';
  var pane = document.getElementById('resolverPane');
  if (pane) pane.dataset.view = view; // CSS hides the topbar copy in scanner view
  var boards = document.getElementById('resolverBoardsView');
  var scan = document.getElementById('resolverScannerView');
  var pl = document.getElementById('resolverPanelList');
  if (pl) pl.style.display = 'none';
  if (view === 'scanner') {
    if (boards) boards.style.display = 'none';
    if (scan) scan.style.display = '';
    try { if (typeof populateScanProfileSelect === 'function') populateScanProfileSelect(); } catch (e) { }
    try { if (typeof pollScannerOnce === 'function') pollScannerOnce(); } catch (e) { }
  } else {
    if (scan) scan.style.display = 'none';
    if (boards) boards.style.display = '';
    switchResolverTab(view); // 'active' | 'bank' — toggles panels + fetches
  }
  resolverUpdateTitle();
  renderResolverSidebar();
  if (swapMobile) resolverEnterPane();
}

// resolverViewList VIEWS a list's resolvers in the pane WITHOUT activating it —
// view and activate are separate now. Activation is the "Activate" button.
function resolverViewList(name) {
  resolverView = 'list';
  resolverViewedList = name;
  var pane = document.getElementById('resolverPane');
  if (pane) pane.dataset.view = 'list';
  var boards = document.getElementById('resolverBoardsView');
  var scan = document.getElementById('resolverScannerView');
  if (scan) scan.style.display = 'none';
  if (boards) boards.style.display = '';
  var pa = document.getElementById('resolverPanelActive');
  var pb = document.getElementById('resolverPanelBank');
  if (pa) pa.style.display = 'none';
  if (pb) pb.style.display = 'none';
  var pl = document.getElementById('resolverPanelList');
  if (!pl) { pl = document.createElement('div'); pl.id = 'resolverPanelList'; if (boards) boards.appendChild(pl); }
  pl.style.display = '';
  pl.innerHTML = '<div style="color:var(--text-dim);padding:8px">…</div>';
  resolverRenderListBoard(name, pl);
  resolverUpdateTitle();
  renderResolverSidebar();
  resolverEnterPane();
}

async function resolverRenderListBoard(name, el) {
  var bankMap = {};
  try { var r = await fetch('/api/resolvers/bank'); if (r.ok) { (await r.json()).bank.forEach(function (b) { bankMap[b.addr] = b; }); } } catch (e) { }
  var addrs = [];
  try { var lr = await fetch('/api/resolvers/lists?include=resolvers'); if (lr.ok) { var li = ((await lr.json()).lists || []).find(function (l) { return l.name === name; }); if (li) addrs = li.resolvers || []; } } catch (e) { }
  var board = addrs.map(function (a) { var b = bankMap[a] || {}; return { addr: a, score: (b.score != null ? b.score : 0), avgMs: (b.avgMs || 0), success: (b.success || 0), failure: (b.failure || 0), active: b.active }; });
  board.sort(function (a, b) { return b.score - a.score; });
  if (!board.length) { el.innerHTML = '<div style="color:var(--text-dim);padding:8px">' + esc(t('no_active_resolvers') || 'No resolvers') + '</div>'; return; }
  // Show a remove (×) per resolver so the user can empty the list.
  el.innerHTML = _buildScoreboardTable(board, true, false, 'resolverRemoveFromList');
}

// resolverRemoveFromList removes a resolver from the currently-viewed list.
async function resolverRemoveFromList(addr) {
  var name = resolverViewedList;
  if (!name || !addr) return;
  try {
    var r = await fetch('/api/resolvers/lists/remove', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: name, resolvers: [addr] })
    });
    if (!r.ok) { showToast(await r.text() || t('remove_failed') || 'Remove failed'); return; }
    var pl = document.getElementById('resolverPanelList');
    if (pl) resolverRenderListBoard(name, pl); // re-render the list view
    try { await loadResolverLists(); } catch (e) { } // refresh counts/sidebar
  } catch (e) { showToast(e.message || 'remove failed'); }
}

// resolverSelectList ACTIVATES a list (makes it the one the fetcher uses). It
// does NOT open/view the pane — activating shouldn't navigate; the user stays
// where they are and just sees the green "Active" badge move.
function resolverSelectList(name) {
  resolverMenuOpen = null;
  selectResolverList(name); // switches the fetcher's active list
  renderResolverSidebar();  // move the ACTIVE badge
}

async function renderResolverSidebar() {
  var sb = document.getElementById('resolverSidebar');
  if (!sb) return;
  // Reparent first (idempotent): removes the static modal #bankAutoScan so the
  // toggle we render below isn't a duplicate id. renderResolverSidebar can run
  // (via renderResolverTabs on profile-load / SSE) before the section is opened.
  resolverReparentOnce();
  var lists = (rlState && rlState.lists) || [];
  var bankCount = 0, activeCount = 0, bankMap = {};
  try {
    var r = await fetch('/api/resolvers/bank');
    if (r.ok) { var d = await r.json(); bankCount = d.count || 0; (d.bank || []).forEach(function (b) { bankMap[b.addr] = b; if (b.active) activeCount++; }); }
  } catch (e) { }
  // Each list's resolver addresses; scores/latency come from the bank map.
  var listResolvers = {};
  try {
    var lr = await fetch('/api/resolvers/lists?include=resolvers');
    if (lr.ok) { var ld = await lr.json(); (ld.lists || []).forEach(function (l) { listResolvers[l.name] = l.resolvers || []; }); }
  } catch (e) { }
  // Auto-scan state from the active profile (default on).
  var autoOn = true;
  try {
    if (typeof profiles !== 'undefined' && profiles && profiles.profiles && typeof activeProfileId !== 'undefined' && activeProfileId) {
      var ap = profiles.profiles.find(function (x) { return x.id === activeProfileId; });
      if (ap) autoOn = ap.config.autoScan !== false;
    }
  } catch (e) { }

  // Safe to drop into a single-quoted JS string inside a double-quoted HTML
  // attribute (onclick="f('NAME')"): HTML-escape, then JS-escape \ and ', then
  // HTML-escape " so it can't break the attribute. Guards against list names
  // with quotes (esc()/escAttr() alone don't make a value JS-safe).
  function jsName(s) {
    return esc(String(s)).replace(/\\/g, '\\\\').replace(/'/g, "\\'").replace(/"/g, '&quot;');
  }

  function scoredRow(b) {
    var c = b.score >= 0.5 ? 'var(--success)' : b.score >= 0.15 ? 'var(--text)' : 'var(--error)';
    return '<div class="rs-preview-row"><span class="rs-pv-addr">' + esc(b.addr) + '</span>'
      + (b.avgMs > 0 ? '<span class="rs-pv-ms">' + Math.round(b.avgMs) + 'ms</span>' : '')
      + '<span style="color:' + c + ';font-weight:600">' + (b.score != null ? b.score.toFixed(2) : '-') + '</span></div>';
  }

  var h = '<div class="rs-head">' + esc(t('nav_resolver') || 'Resolver') + '</div>';

  var chev = '<span class="rs-chevron" aria-hidden="true">' + icon('chevron') + '</span>';
  // View-highlight only matters while the pane is actually visible (desktop, or
  // mobile after a tap). On the mobile sidebar it's cleared so nothing looks
  // "stuck" selected when you come back.
  var paneOpen = !(typeof mobileQuery !== 'undefined' && mobileQuery.matches) ||
    (document.getElementById('app') && document.getElementById('app').classList.contains('chat-open'));

  // 1) Find more resolvers (scanner) — the entry point that discovers new ones.
  // .rs-scanning spins the icon while a scan is running (window.scanActive).
  h += '<button class="rs-item rs-item-lg rs-item-find' + (window.scanActive ? ' rs-scanning' : '') + '" onclick="resolverShowView(\'scanner\',true)">'
    + '<span class="rs-ic">' + icon('navScanner') + '</span>'
    + '<span class="rs-name-wrap"><span class="rs-name">' + esc(t('resolver_find_more') || 'Find more resolvers') + '</span>'
    + '<span class="rs-sub">' + esc(t('resolver_find_more_sub') || '') + '</span></span>' + chev + '</button>';

  // 2) Bank — the master pool. Count = active / total.
  h += '<div class="rs-label">' + esc(t('resolver_tab_bank') || 'Bank') + '</div>';
  h += '<button class="rs-item rs-item-bordered" onclick="resolverShowView(\'bank\',true)">'
    + '<span class="rs-ic">' + icon('bank') + '</span>'
    + '<span class="rs-name-wrap"><span class="rs-name">' + esc(t('resolver_tab_bank') || 'Bank') + '</span>'
    + '<span class="rs-sub">' + esc(t('resolver_bank_sub') || '') + '</span></span>'
    + '<span class="rs-count">' + activeCount + ' / ' + bankCount + '</span>' + chev + '</button>';

  // 3) Lists — each in its own box: name + a ⋮ menu (rename/delete) + a preview
  // of its top-scoring resolvers; click the box to open it fully in the pane.
  h += '<div class="rs-label">' + esc(t('resolver_lists_label') || 'Active list') + '</div>';
  lists.forEach(function (li) {
    var sel = !!li.selected;
    var nm = esc(li.name);       // display (HTML text)
    var nmJs = jsName(li.name);  // safe for onclick JS-string args
    var addrs = listResolvers[li.name] || [];
    var scored = addrs.map(function (a) { var b = bankMap[a] || {}; return { addr: a, score: (b.score != null ? b.score : 0), avgMs: (b.avgMs || 0) }; });
    scored.sort(function (a, b) { return b.score - a.score; });
    var top = scored.slice(0, 3);
    // Click the box to VIEW the list in the pane (doesn't change what's active).
    // The green "Active" badge marks the in-use list; "Activate" switches to it.
    var viewing = (resolverView === 'list' && resolverViewedList === li.name) || (sel && resolverView === 'active');
    h += '<div class="rs-listbox' + (viewing && paneOpen ? ' viewing' : '') + '" onclick="resolverViewList(\'' + nmJs + '\')">';
    h += '<div class="rs-list-headrow">';
    // Stable layout: [icon][name][count] then a fixed trailing slot holding
    // either the green "Active" badge OR the "Activate" button, then ⋮. This
    // keeps the count/⋮ from shifting when a list is activated.
    h += '<div class="rs-list-head">'
      + '<span class="rs-ic">' + icon('antenna') + '</span>'
      + '<span class="rs-name">' + nm + '</span>'
      + '<span class="rs-count">' + (li.count || 0) + '</span></div>';
    h += '<div class="rs-list-trail">';
    if (sel) {
      h += '<span class="rs-active-badge">' + esc(t('resolver_active_badge') || 'Active') + '</span>';
    } else {
      h += '<button class="rs-activate-btn" onclick="event.stopPropagation();resolverSelectList(\'' + nmJs + '\')">' + esc(t('resolver_activate') || 'Activate') + '</button>';
    }
    h += '</div>';
    h += '<button class="rs-list-menu-btn" title="' + esc(t('manage_lists') || 'More') + '" aria-label="' + esc(t('manage_lists') || 'More') + '" onclick="event.stopPropagation();resolverToggleListMenu(\'' + nmJs + '\')">' + icon('more') + '</button>';
    h += '</div>';
    if (resolverMenuOpen === li.name) {
      h += '<div class="rs-list-tools">'
        + '<button class="rs-tool" onclick="event.stopPropagation();resolverRenameList(\'' + nmJs + '\')">' + icon('edit') + ' ' + esc(t('rename') || 'Rename') + '</button>'
        + '<button class="rs-tool rs-tool-del" onclick="event.stopPropagation();resolverDeleteList(\'' + nmJs + '\')">' + icon('delete') + ' ' + esc(t('delete') || 'Delete') + '</button>'
        + '</div>';
    }
    if (top.length) {
      h += '<div class="rs-list-resolvers">';
      top.forEach(function (b) { h += scoredRow(b); });
      if ((li.count || 0) > top.length) {
        h += '<div class="rs-seeall">'
          + esc((t('resolver_see_all') || 'See all ({n})').replace('{n}', li.count || 0)) + '</div>';
      }
      h += '</div>';
    } else {
      h += '<div class="rs-empty">' + esc(t('no_active_resolvers') || 'No resolvers') + '</div>';
    }
    h += '</div>'; // .rs-listbox
  });
  h += '<button class="rs-item rs-item-add rs-item-bordered" onclick="resolverNewListPrompt()">'
    + '<span class="rs-ic">' + icon('add') + '</span><span class="rs-name">' + esc(t('resolver_new_empty') || 'New empty list') + '</span></button>';

  // Auto-scan toggle + a "Details" button explaining what the hourly check does.
  h += '<div class="rs-toggle"><label><input type="checkbox" id="bankAutoScan"' + (autoOn ? ' checked' : '') + ' onchange="toggleBankAutoScan()"> '
    + esc(t('resolver_autoscan') || 'Automatic hourly check') + '</label>'
    + '<button type="button" class="rs-info-link" onclick="resolverAutoScanInfo()">' + esc(t('resolver_autoscan_details') || 'Details') + '</button>'
    + '</div>';
  h += '<div class="rs-actions">'
    + '<button class="btn btn-outline btn-sm" onclick="doResolverRescan()">' + esc(t('rescan') || 'Rescan') + '</button>'
    + '<button class="btn btn-outline btn-sm" onclick="resetScoreboard()">' + esc(t('reset_scoreboard') || 'Reset') + '</button>'
    + '</div>';
  sb.innerHTML = h;
}

// resolverAutoScanInfo explains the hourly auto-check in a one-button dialog.
function resolverAutoScanInfo() {
  showInfoDialog(
    t('resolver_autoscan_info') ||
    'Once an hour the bank resolvers are re-checked and the best-scoring ones replace the currently active resolvers.',
    t('ok') || 'OK');
}

// resolverNewListPrompt creates a brand-new EMPTY list (NOT a copy of the
// current active resolvers) — the user fills it from the bank afterwards.
async function resolverNewListPrompt() {
  var name = await showInputDialog({
    title: t('resolver_new_empty') || 'New empty list',
    message: t('resolver_list_name_prompt') || 'Name this list:',
    placeholder: t('resolver_list_name_ph') || 'List name',
    maxLength: 32,
  });
  if (!name) return;
  try {
    var r = await fetch('/api/resolvers/lists/save', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: name, mode: 'empty' })
    });
    if (r.status === 409) { showToast(t('resolver_list_exists') || 'A list with that name already exists'); return; }
    if (!r.ok) { showToast(await r.text() || t('save_failed') || 'Save failed'); return; }
    rlState = await r.json();
    renderResolverTabs();
    renderResolverSidebar();
    showToast((t('resolver_list_saved') || 'Saved "{n}"').replace('{n}', name));
  } catch (e) { showToast(e.message || 'save failed'); }
}

// resolverToggleListMenu opens/closes the ⋮ rename/delete menu for a list.
function resolverToggleListMenu(name) {
  resolverMenuOpen = (resolverMenuOpen === name) ? null : name;
  renderResolverSidebar();
}

// resolverRenameList / resolverDeleteList act on a list BY NAME (the ⋮ menu is
// per-list, so we don't rely on which list is currently selected).
async function resolverRenameList(name) {
  resolverMenuOpen = null;
  var newName = await showInputDialog({
    title: t('rename') || 'Rename',
    message: (t('resolver_list_rename_prompt') || 'New name for "{n}"').replace('{n}', name),
    value: name, maxLength: 32,
  });
  if (!newName || newName === name) { renderResolverSidebar(); return; }
  try {
    var r = await fetch('/api/resolvers/lists/rename', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: name, newName: newName })
    });
    if (!r.ok) { showToast(await r.text() || t('rename_failed') || 'Rename failed'); return; }
    rlState = await r.json();
    renderResolverTabs();
  } catch (e) { showToast(e.message || 'rename failed'); }
}

async function resolverDeleteList(name) {
  resolverMenuOpen = null;
  var ok = await showConfirmDialog(
    (t('resolver_list_delete_confirm') || 'Delete "{n}"?').replace('{n}', name),
    t('delete') || 'Delete', t('cancel') || 'Cancel');
  if (!ok) { renderResolverSidebar(); return; }
  try {
    var r = await fetch('/api/resolvers/lists', {
      method: 'DELETE', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: name })
    });
    if (!r.ok) { showToast(await r.text() || t('delete_failed') || 'Delete failed'); return; }
    rlState = await r.json();
    renderResolverTabs();
    try { if (resolverView === 'active') _fetchActiveBoard(); } catch (e) { }
  } catch (e) { showToast(e.message || 'delete failed'); }
}

// Rescan: trigger the re-check and show the REAL determinate progress in
// #progressPanel (now floated into the resolver section via CSS, driven by the
// same SSE/log path the feed uses). It auto-hides when the panel empties.
async function doResolverRescan() {
  showToast(t('rescan_started'));
  // Show the determinate progress placeholder IN PLACE (the SSE/log path fills
  // it with real progress). We deliberately do NOT call the feed's
  // showInitProgress() — on mobile that calls openChat(), which would slide the
  // pane in and "open" whatever list was last viewed. Rescan must stay put.
  var pp = document.getElementById('progressPanel');
  if (pp) {
    pp.innerHTML = '<div class="progress-item" id="prog-init" data-last-update="' + Date.now()
      + '"><button class="progress-close" onclick="this.parentNode.remove()" title="Dismiss">&times;</button>'
      + '<div class="progress-label">' + (t('loading') || 'Loading…') + '</div>'
      + '<div class="progress-bar"><div class="progress-fill" style="width:30%;animation:prog-pulse 1.5s ease-in-out infinite"></div></div></div>';
  }
  // The progress bar sits below the scrollable sidebar; scroll the list to the
  // bottom so the Rescan/Reset buttons stay visible right above it (the progress
  // shrinks the scroll area, otherwise the buttons fall just below the fold).
  var sb = document.getElementById('resolverSidebar');
  if (sb) sb.scrollTop = sb.scrollHeight;
  try { await fetch('/api/rescan', { method: 'POST' }); } catch (e) { }
  setTimeout(function () {
    if (resolverView === 'active') _fetchActiveBoard(); else if (resolverView === 'bank') _fetchBankBoard();
    renderResolverSidebar();
    refreshResolversBadge();
  }, 3000);
}

// resolverEnterPane: on mobile, slide to the content pane AND push a history
// entry so hardware-back returns to the sidebar (not all the way to the feed).
// We key the back off this flag — NOT the .chat-open class — because core.js's
// popstate handler clears .chat-open before ours runs, so a class check misses.
function resolverEnterPane() {
  if (typeof mobileQuery === 'undefined' || !mobileQuery.matches) return;
  var app = document.getElementById('app'); if (app) app.classList.add('chat-open');
  if (!resolverPanePushed) {
    try { history.pushState({ view: 'resolverPane' }, ''); resolverPanePushed = true; } catch (e) { }
  }
}

// resolverShowSidebar: mobile "back" — slide from the pane back to the sidebar,
// then re-render so the view-highlight clears (nothing looks "stuck" selected).
function resolverShowSidebar() {
  var app = document.getElementById('app'); if (app) app.classList.remove('chat-open');
  // Consume the pane history entry so the stack stays in sync with the UI.
  if (resolverPanePushed) { resolverPanePushed = false; resolverSuppressPopstate++; try { history.go(-1); } catch (e) { } }
  renderResolverSidebar();
}

async function openResolverSection(adopt) {
  resolverReparentOnce();
  // Float the rescan progress in the SIDEBAR column (not .chat-area, which is
  // off-screen on mobile while the user is on the list) so it's always visible.
  var _pp = document.getElementById('progressPanel');
  var _col = document.getElementById('sidebar');
  if (_pp && _col && _pp.parentElement !== _col) _col.appendChild(_pp);
  document.documentElement.classList.add('resolver-section');
  resolverSectionOpen = true;
  if (adopt) {
    resolverHistoryPushed = true;
  } else if (!resolverHistoryPushed) {
    try { history.pushState({ view: 'resolverSection' }, ''); resolverHistoryPushed = true; } catch (e) { }
  }
  // Mobile opens on the sidebar (list first); desktop shows both, pane on active.
  var app = document.getElementById('app'); if (app) app.classList.remove('chat-open');
  // Await the named lists so the sidebar's first paint already has them.
  try { await loadResolverLists(); } catch (e) { }
  renderResolverSidebar();
  resolverShowView(resolverView || 'active', false);
  if (_resolverRefreshTimer) clearInterval(_resolverRefreshTimer);
  _resolverRefreshTimer = setInterval(function () {
    if (!document.documentElement.classList.contains('resolver-section')) {
      clearInterval(_resolverRefreshTimer); _resolverRefreshTimer = null; return;
    }
    if (resolverView === 'active') _fetchActiveBoard();
    else if (resolverView === 'bank') _fetchBankBoard();
    renderResolverSidebar();
  }, 3000);
}

function resolverTeardown() {
  resolverSectionOpen = false;
  resolverPanePushed = false;
  document.documentElement.classList.remove('resolver-section');
  var app = document.getElementById('app'); if (app) app.classList.remove('chat-open');
  if (_resolverRefreshTimer) { clearInterval(_resolverRefreshTimer); _resolverRefreshTimer = null; }
  if (typeof scanPollTimer !== 'undefined' && scanPollTimer) { clearInterval(scanPollTimer); scanPollTimer = null; }
  // Return #progressPanel to the feed (.chat-area), before the log-toggle.
  var pp = document.getElementById('progressPanel');
  var ca = document.querySelector('.chat-area');
  if (pp && ca && pp.parentElement !== ca) {
    var lt = ca.querySelector(':scope > .log-toggle');
    if (lt) ca.insertBefore(pp, lt); else ca.appendChild(pp);
  }
}

// Section switch (→ Mirror/Chat): tear down WITHOUT history.go (avoids the
// cross-section popstate cross-talk). Returns whether we owned a history entry.
function resolverCloseForNav() {
  var had = resolverHistoryPushed;
  resolverHistoryPushed = false;
  resolverTeardown();
  return had;
}

// Explicit close (topbar back / hardware back from the section root).
function closeResolverSection() {
  var steps = resolverHistoryPushed ? 1 : 0;
  resolverHistoryPushed = false;
  resolverTeardown();
  if (steps > 0) { resolverSuppressPopstate += steps; try { history.go(-steps); } catch (e) { } }
}

window.addEventListener('popstate', function () {
  if (resolverSuppressPopstate > 0) { resolverSuppressPopstate--; return; }
  // Mobile: back from a list/scanner pane returns to the sidebar (consume the
  // pane entry). Key off our own flag — core.js's popstate handler clears
  // .chat-open before ours runs, so a class check here would always miss and we
  // would wrongly tear the section down (showing the feed under the nav tab).
  if (resolverPanePushed) {
    resolverPanePushed = false;
    var app = document.getElementById('app'); if (app) app.classList.remove('chat-open');
    renderResolverSidebar();
    return;
  }
  if (resolverHistoryPushed) { resolverHistoryPushed = false; resolverTeardown(); }
});

// Redirect the old modal entry points into the section so every caller (nav,
// settings deep-link, saved-resolvers, scanner) lands in one place.
window.openResolverSection = openResolverSection;
window.openResolversModal = function () { openResolverSection(); resolverShowView('active', false); };
window.closeResolversModal = function () { closeResolverSection(); };
window.openScanner = function () { openResolverSection(); resolverShowView('scanner', true); };
window.closeScanner = function () {
  if (typeof scanPollTimer !== 'undefined' && scanPollTimer) { clearInterval(scanPollTimer); scanPollTimer = null; }
  resolverShowView('active', false);
};

