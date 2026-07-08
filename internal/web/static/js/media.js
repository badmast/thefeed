// ===== DOWNLOADABLE MEDIA =====
var mediaInflight = {};
var mediaBlobURLs = {};
var mediaBlobs = {};

var MEDIA_MAX_CONCURRENT = 1;
var mediaActiveCount = 0;
var mediaQueue = []; // [{ domID, run }]
var mediaQueued = {}; // domID → true

// Content-addressed: keyed by "<size>-<crc>" so identical bytes share a
// single cache entry across messages, channels, and server reboots
// (the dynamic media-channel number isn't part of the key).
var MEDIA_DB_NAME = 'thefeed-media';
var MEDIA_DB_VERSION = 2;
var MEDIA_DB_STORE = 'blobs';
var MEDIA_DB_MAX_AGE_MS = 7 * 24 * 60 * 60 * 1000;
var mediaDBPromise = null;

var _crc32Table = (function () {
  var t = new Uint32Array(256);
  for (var n = 0; n < 256; n++) {
    var c = n;
    for (var k = 0; k < 8; k++) c = (c & 1) ? (0xedb88320 ^ (c >>> 1)) : (c >>> 1);
    t[n] = c >>> 0;
  }
  return t;
})();
function crc32IEEE(uint8Array) {
  var c = 0xffffffff;
  for (var i = 0; i < uint8Array.length; i++) {
    c = (_crc32Table[(c ^ uint8Array[i]) & 0xff] ^ (c >>> 8)) >>> 0;
  }
  return ((c ^ 0xffffffff) >>> 0);
}
async function blobCRC32(blob) {
  var buf = await blob.arrayBuffer();
  return crc32IEEE(new Uint8Array(buf));
}

function mediaDBOpen() {
  if (mediaDBPromise) return mediaDBPromise;
  if (!window.indexedDB) return Promise.resolve(null);
  mediaDBPromise = new Promise(function (resolve) {
    var req = indexedDB.open(MEDIA_DB_NAME, MEDIA_DB_VERSION);
    req.onupgradeneeded = function () {
      var db = req.result;
      // Drop the previous keyspace (channel-keyed entries from v1) so we
      // don't leak orphaned bytes that the new content-addressed lookup
      // can't find.
      if (db.objectStoreNames.contains(MEDIA_DB_STORE)) {
        db.deleteObjectStore(MEDIA_DB_STORE);
      }
      db.createObjectStore(MEDIA_DB_STORE);
    };
    req.onsuccess = function () { resolve(req.result); };
    req.onerror = function () { resolve(null); };
  });
  return mediaDBPromise;
}

function mediaCacheKey(size, crc) { return size + '-' + crc; }

async function mediaForgetCache(card) {
  try {
    var size = card.getAttribute('data-size');
    var crc = card.getAttribute('data-crc');
    if (!size || !crc) return;
    var db = await mediaDBOpen();
    if (!db) return;
    var tx = db.transaction(MEDIA_DB_STORE, 'readwrite');
    tx.objectStore(MEDIA_DB_STORE)['delete'](mediaCacheKey(size, crc));
  } catch (e) { }
}

async function mediaPersistBlob(msgID, card, blob, mime) {
  try {
    var db = await mediaDBOpen();
    if (!db) return;
    var size = card.getAttribute('data-size');
    var crc = card.getAttribute('data-crc');
    if (!size || !crc) return;
    var tx = db.transaction(MEDIA_DB_STORE, 'readwrite');
    tx.objectStore(MEDIA_DB_STORE).put({
      blob: blob,
      mime: mime || '',
      savedAt: Date.now()
    }, mediaCacheKey(size, crc));
  } catch (e) { }
}

async function mediaRestoreFromCache(msgID) {
  var card = document.getElementById('media-' + msgID);
  if (!card) return false;
  var size = card.getAttribute('data-size');
  var crc = card.getAttribute('data-crc');
  if (!size || !crc) return false;
  var db = await mediaDBOpen();
  if (!db) return false;
  try {
    var key = mediaCacheKey(size, crc);
    var tx = db.transaction(MEDIA_DB_STORE, 'readonly');
    var store = tx.objectStore(MEDIA_DB_STORE);
    var rec = await new Promise(function (resolve) {
      var req = store.get(key);
      req.onsuccess = function () { resolve(req.result); };
      req.onerror = function () { resolve(null); };
    });
    if (!rec) return false;
    if (rec.savedAt && Date.now() - rec.savedAt > MEDIA_DB_MAX_AGE_MS) {
      try {
        var d2 = db.transaction(MEDIA_DB_STORE, 'readwrite');
        d2.objectStore(MEDIA_DB_STORE)['delete'](key);
      } catch (e) { }
      return false;
    }
    var blobURL = URL.createObjectURL(rec.blob);
    mediaBlobURLs[msgID] = blobURL;
    mediaBlobs[msgID] = { blob: rec.blob, url: blobURL, mime: rec.mime || '' };
    mediaShowBlob(card, blobURL);
    return true;
  } catch (e) { return false; }
}

function mediaTryRestoreVisibleCards() {
  var cards = document.querySelectorAll('.media-card');
  for (var i = 0; i < cards.length; i++) {
    var card = cards[i];
    var msgID = card.getAttribute('data-msg');
    if (!msgID) continue;
    var entry = mediaBlobs[msgID];
    if (entry) {
      mediaShowBlob(card, entry.url);
      continue;
    }
    if (mediaInflight[msgID]) {
      mediaShowProgress(card);
      continue;
    }
    if (mediaQueued[card.id]) {
      mediaShowQueued(card);
      continue;
    }
    // Restore from the content-addressed IndexedDB cache (keyed by size+crc,
    // not channel — see mediaCacheKey). Applies to both DNS- and GitHub-relay
    // media; a cache miss safely no-ops.
    mediaRestoreFromCache(msgID);
  }
}

// Relay slot indices — match protocol/media.go.
var RELAY_DNS = 0, RELAY_GITHUB = 1;

function parseDownloadableMedia(text, tag) {
  var rest = text.substring(tag.length);
  var nl = rest.indexOf('\n');
  var head = nl >= 0 ? rest.substring(0, nl) : rest;
  var caption = nl >= 0 ? rest.substring(nl + 1) : '';
  head = head.trim();
  var empty = { downloadable: false, dnsAvailable: false, githubAvailable: false, relays: [], size: 0, channel: 0, blocks: 0, crc: '', filename: '', caption: caption };
  if (!head) return empty;
  var parts = head.split(':');
  if (parts.length < 5) {
    empty.caption = rest.replace(/^\n/, '');
    return empty;
  }
  var size = parseInt(parts[0], 10);
  // parts[1] is a comma-separated relay flag list, e.g. "1,0".
  var flagParts = parts[1].split(',');
  var relays = [];
  var flagsOK = true;
  for (var fi = 0; fi < flagParts.length; fi++) {
    var f = flagParts[fi].trim();
    if (f !== '0' && f !== '1') { flagsOK = false; break; }
    relays.push(f === '1');
  }
  if (!flagsOK) {
    empty.caption = rest.replace(/^\n/, '');
    return empty;
  }
  var ch = parseInt(parts[2], 10);
  var blk = parseInt(parts[3], 10);
  var crc = parts[4].toLowerCase();
  if (isNaN(size) || isNaN(ch) || isNaN(blk) || !/^[0-9a-f]+$/.test(crc)) {
    empty.caption = rest.replace(/^\n/, '');
    return empty;
  }
  var filename = parts.length >= 6 ? parts.slice(5).join(':') : '';
  var dnsOK = !!relays[RELAY_DNS] && ch >= 10000 && ch <= 60000 && blk > 0;
  var ghOK = !!relays[RELAY_GITHUB];
  return {
    downloadable: dnsOK || ghOK,
    dnsAvailable: dnsOK,
    githubAvailable: ghOK,
    relays: relays,
    size: size,
    channel: ch,
    blocks: blk,
    crc: crc,
    filename: filename,
    caption: caption
  };
}

function formatBytes(n) {
  if (!n || n < 0) return '0 B';
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  if (n < 1024 * 1024 * 1024) return (n / (1024 * 1024)).toFixed(1) + ' MB';
  return (n / (1024 * 1024 * 1024)).toFixed(2) + ' GB';
}

function mediaTagLabel(tag) {
  return tag.replace(/^\[|\]$/g, '');
}

function mediaIsImageTag(tag) {
  return tag === '[IMAGE]' || tag === '[STICKER]' || tag === '[GIF]';
}

function mediaIsPlayableTag(tag) {
  return tag === '[VIDEO]' || tag === '[AUDIO]' || tag === '[VOICE]';
}

function mediaExtForTag(tag, mime) {
  if (mime) {
    if (/^image\/(jpeg|jpg)$/i.test(mime)) return 'jpg';
    if (/^image\/png$/i.test(mime)) return 'png';
    if (/^image\/webp$/i.test(mime)) return 'webp';
    if (/^image\/gif$/i.test(mime)) return 'gif';
    if (/^video\/mp4$/i.test(mime)) return 'mp4';
    if (/^audio\/(mpeg|mp3)$/i.test(mime)) return 'mp3';
    if (/^audio\/ogg$/i.test(mime)) return 'ogg';
    if (/^application\/pdf$/i.test(mime)) return 'pdf';
  }
  switch (tag) {
    case '[IMAGE]': return 'jpg';
    case '[STICKER]': return 'webp';
    case '[GIF]': return 'gif';
    case '[VIDEO]': return 'mp4';
    case '[AUDIO]': return 'mp3';
  }
  return 'bin';
}

function mediaSummaryLine(tag, parsed) {
  var label = mediaTagLabel(tag);
  var parts = [label];
  if (parsed.size > 0) parts.push(formatBytes(parsed.size));
  if (parsed.blocks > 0) parts.push(parsed.blocks + ' ' + (t('blocks_label') || 'blocks'));
  return parts.join(' · ');
}

function renderDownloadableMedia(tag, parsed, msgID) {
  var label = mediaTagLabel(tag);
  var summary = mediaSummaryLine(tag, parsed);
  var sizeStr = parsed.size > 0 ? formatBytes(parsed.size) : '?';
  var canDownload = parsed.downloadable;
  var domID = 'media-' + msgID;
  var dataAttrs = 'id="' + domID + '" data-tag="' + escAttr(tag)
    + '" data-ch="' + parsed.channel + '" data-blk="' + parsed.blocks
    + '" data-size="' + parsed.size + '" data-crc="' + parsed.crc
    + '" data-dns="' + (parsed.dnsAvailable ? '1' : '0') + '"'
    + ' data-gh="' + (parsed.githubAvailable ? '1' : '0') + '"'
    + ' data-fname="' + escAttr(parsed.filename || '') + '" data-msg="' + msgID + '"';

  if (mediaIsImageTag(tag)) {
    var imageInner;
    if (canDownload) {
      imageInner = '<button class="media-action" onclick="mediaStart(\'' + domID + '\')" type="button" title="' + esc(t('download') || 'Download') + '">'
        + '<span class="media-icon">' + icon('download') + '</span>'
        + '<span class="media-meta" dir="ltr">' + esc(summary) + '</span>'
        + '</button>';
    } else {
      imageInner = '<button class="media-action media-action-blocked" type="button" onclick="mediaShowBlockedReason(\'' + domID + '\')" title="' + esc(t('media_blocked_title') || 'Not available for download') + '">'
        + '<span class="media-icon media-icon-blocked">' + icon('close') + '</span>'
        + '<span class="media-meta" dir="ltr">' + esc(summary) + '</span>'
        + '</button>';
    }
    return '<div class="media-card media-image" ' + dataAttrs + '>'
      + '<div class="media-image-preview" id="' + domID + '-preview">'
      + imageInner
      + '</div>'
      + '</div>';
  }

  // File-style row: tag chip + filename(label) + size + action button
  var btnHTML = canDownload
    ? '<button class="media-file-btn" onclick="mediaStart(\'' + domID + '\')" type="button" title="' + esc(t('download') || 'Download') + '">' + icon('download') + '</button>'
    : '<button class="media-file-btn media-file-btn-blocked" onclick="mediaShowBlockedReason(\'' + domID + '\')" type="button" title="' + esc(t('media_blocked_title') || 'Not available for download') + '">' + icon('close') + '</button>';
  var detail = (parsed.size > 0 ? formatBytes(parsed.size) : '?')
    + (parsed.blocks > 0 ? ' &middot; ' + parsed.blocks + ' ' + (t('blocks_label') || 'blocks') : '');
  var displayName = parsed.filename || label;
  return '<div class="media-card media-file" ' + dataAttrs + '>'
    + '<div class="media-file-icon">' + label.charAt(0) + '</div>'
    + '<div class="media-file-info">'
    + '<div class="media-file-name" dir="ltr">' + esc(displayName) + '</div>'
    + '<div class="media-file-size" id="' + domID + '-text" dir="ltr">' + detail + '</div>'
    + '<div class="media-progress-bar media-progress-bar-thin"><div class="media-progress-fill" id="' + domID + '-fill" style="width:0%"></div></div>'
    + '</div>'
    + '<div class="media-file-actions" id="' + domID + '-actions">' + btnHTML + '</div>'
    + '</div>';
}

function mediaShowBlockedReason(domID) {
  var card = document.getElementById(domID);
  if (!card) return;
  var size = parseInt(card.getAttribute('data-size'), 10) || 0;
  var maxSize = (window._serverMediaMaxBytes || 0);
  var msg;
  if (maxSize > 0 && size > maxSize) {
    msg = (t('media_blocked_too_large') || 'This file is larger than the server\'s configured cache limit ({size} > {limit}).')
      .replace('{size}', formatBytes(size))
      .replace('{limit}', formatBytes(maxSize));
  } else if (maxSize > 0) {
    msg = (t('media_blocked_unavailable') || 'The server didn\'t cache this file. It may exceed the {limit} cache limit, or the upstream fetch failed.')
      .replace('{limit}', formatBytes(maxSize));
  } else {
    msg = t('media_blocked_generic') || 'The server didn\'t cache this file.';
  }
  showInfoDialog(msg);
}

async function mediaStart(domID) {
  var card = document.getElementById(domID);
  if (!card) return;
  var msgID = card.getAttribute('data-msg');
  if (mediaBlobURLs[msgID]) {
    mediaShowBlob(card, mediaBlobURLs[msgID]);
    return;
  }
  if (mediaInflight[msgID]) {
    var ok = await showConfirmDialog(t('cancel_media_msg') || 'Cancel this download?', t('yes') || 'Yes', t('no') || 'No');
    if (!ok) return;
    if (!mediaInflight[msgID]) return;
    try {
      var inf = mediaInflight[msgID];
      if (inf.ctrl) inf.ctrl.abort();
      else if (inf.xhr) inf.xhr.abort();
    } catch (e) { }
    delete mediaInflight[msgID];
    mediaResetCard(card);
    return;
  }
  if (mediaQueued[domID]) {
    var ok2 = await showConfirmDialog(t('cancel_media_msg') || 'Cancel this download?', t('yes') || 'Yes', t('no') || 'No');
    if (!ok2) return;
    mediaDequeue(domID);
    mediaResetCard(card);
    return;
  }
  if (mediaActiveCount >= MEDIA_MAX_CONCURRENT) {
    mediaQueued[domID] = true;
    mediaQueue.push({ domID: domID, run: function () { mediaRunDownload(domID); } });
    mediaShowQueued(card);
    return;
  }
  mediaRunDownload(domID);
}

function mediaDequeue(domID) {
  delete mediaQueued[domID];
  for (var i = 0; i < mediaQueue.length; i++) {
    if (mediaQueue[i].domID === domID) {
      mediaQueue.splice(i, 1);
      return;
    }
  }
}

function mediaPumpQueue() {
  while (mediaActiveCount < MEDIA_MAX_CONCURRENT && mediaQueue.length > 0) {
    var next = mediaQueue.shift();
    delete mediaQueued[next.domID];
    next.run();
  }
}

function mediaShowQueued(card) {
  var domID = card.id;
  var label = t('queued') || 'Queued';
  if (mediaIsImageTag(card.getAttribute('data-tag'))) {
    var preview = document.getElementById(domID + '-preview');
    if (preview) {
      preview.innerHTML = '<button class="media-action media-action-queued" type="button" onclick="mediaStart(\'' + domID + '\')" title="' + esc(t('cancel') || 'Cancel') + '">'
        + '<span class="media-icon">' + icon('loading') + '</span>'
        + '<span class="media-meta" dir="ltr">' + esc(label) + '</span>'
        + '</button>';
    }
  } else {
    var actions = document.getElementById(domID + '-actions');
    if (actions) actions.innerHTML = '<button class="media-file-btn media-file-btn-queued" onclick="mediaStart(\'' + domID + '\')" type="button" title="' + esc(label) + '">' + icon('loading') + '</button>';
  }
}

// GH_MAX_ATTEMPTS = total tries (initial + retries). 2 = 1 initial + 1 retry.
// 404 / 429 skip retry entirely — file genuinely missing or rate-limited.
var GH_MAX_ATTEMPTS = 2, GH_RETRY_DELAY_MS = 500;

async function mediaRunDownload(domID, opts) {
  opts = opts || {};
  var card = document.getElementById(domID);
  if (!card) { mediaPumpQueue(); return; }
  var msgID = card.getAttribute('data-msg');
  var ch = card.getAttribute('data-ch');
  var blk = card.getAttribute('data-blk');
  var size = card.getAttribute('data-size');
  var crc = card.getAttribute('data-crc');
  var fname = card.getAttribute('data-fname') || '';
  var ghAvail = card.getAttribute('data-gh') === '1';
  var dnsAvail = card.getAttribute('data-dns') === '1';
  var source = opts.forceSource || (ghAvail ? 'fast' : 'slow');
  if (!opts.forceSource && source === 'slow' && dnsAvail) {
    var ok = await showConfirmDialog(
      t('media_slow_only') || 'GitHub relay is unavailable for this file. The DNS path is very slow. Download anyway?',
      t('yes') || 'Yes', t('no') || 'No');
    if (!ok) { mediaPumpQueue(); return; }
  }
  var baseUrl = '/api/media/get?ch=' + encodeURIComponent(ch)
    + '&blk=' + encodeURIComponent(blk)
    + '&size=' + encodeURIComponent(size)
    + '&crc=' + encodeURIComponent(crc)
    + (fname ? '&name=' + encodeURIComponent(fname) : '');
  var url = baseUrl + '&source=' + source;
  mediaActiveCount++;
  var attempt = 0;
  debugLog('media: download msg=' + msgID + ' source=' + source + ' size=' + size);

  var ctrl = new AbortController();
  var pollTimer = null;
  var progressShownAt = 0;
  var MIN_PROGRESS_VISIBLE_MS = 350;

  function stopPoll() { if (pollTimer) { clearInterval(pollTimer); pollTimer = null; } }
  function finishSlot() {
    mediaActiveCount = Math.max(0, mediaActiveCount - 1);
    mediaPumpQueue();
  }
  function restartWith(newSource) {
    stopPoll();
    try { ctrl.abort(); } catch (e) { }
    delete mediaInflight[msgID];
    mediaActiveCount = Math.max(0, mediaActiveCount - 1);
    delete mediaProgressState[msgID];
    mediaRunDownload(domID, { forceSource: newSource });
  }
  async function handleRateLimit(resetMin) {
    var minutes = (resetMin && resetMin > 0) ? resetMin : '?';
    showToast((t('media_rate_limited') || 'GitHub rate limit hit. Reset in {n} min — using slow path.').replace('{n}', minutes));
    restartWith('slow');
  }
  async function handleFastFailure(reason, status) {
    attempt++;
    var skipRetry = (status === 404 || status === 429);
    if (!skipRetry && attempt < GH_MAX_ATTEMPTS) {
      await new Promise(function (r) { setTimeout(r, GH_RETRY_DELAY_MS); });
      runOnce();
      return;
    }
    if (skipRetry && source === 'fast') {
      var note = (status === 404)
        ? (t('media_relay_404_fallback') || 'Not in fast relay yet — switching to DNS')
        : (t('media_rate_limited_fallback') || 'Rate limit hit — switching to DNS');
      showToast(note);
      restartWith('slow');
      return;
    }
    if (source === 'fast' && dnsAvail) {
      var ok2 = await showConfirmDialog(
        t('media_relay_fallback') || "Fast relay failed. Try the slow DNS path? Note: this can be very slow.",
        t('yes') || 'Yes', t('no') || 'No');
      if (ok2) { restartWith('slow'); return; }
    }
    mediaShowError(card, reason || (t('media_failed') || 'Download failed'));
    finishSlot();
  }

  async function deliverBlob(blob, headers) {
    var expectedCRC = parseInt(crc, 16);
    var expectedSize = parseInt(size, 10);
    if (blob && !isNaN(expectedSize) && expectedSize > 0 && blob.size !== expectedSize) {
      await mediaForgetCache(card);
      if (source === 'fast') { handleFastFailure(t('media_size_mismatch') || 'Size mismatch'); return; }
      mediaShowError(card, t('media_size_mismatch') || 'Size mismatch');
      return;
    }
    if (!isNaN(expectedCRC) && expectedCRC > 0) {
      try {
        var got = await blobCRC32(blob);
        if (got !== expectedCRC) {
          await mediaForgetCache(card);
          if (source === 'fast') { handleFastFailure(t('media_hash_mismatch') || 'Content hash mismatch'); return; }
          mediaShowError(card, t('media_hash_mismatch') || 'Content hash mismatch');
          return;
        }
      } catch (e) { }
    }
    var mime = (headers && headers.get('Content-Type')) || '';
    var blobURL = URL.createObjectURL(blob);
    mediaBlobURLs[msgID] = blobURL;
    mediaBlobs[msgID] = { blob: blob, url: blobURL, mime: mime };
    mediaShowBlob(card, blobURL);
    mediaPersistBlob(msgID, card, blob, mime);
  }

  // fetch + manual reader so we can finalise the moment we have
  // Content-Length bytes — some censoring proxies hold the
  // connection open after sending the body, which makes XHR /
  // resp.blob() hang at 100 %. Cancelling the reader breaks us
  // out of that wait.
  async function runOnce() {
    try {
      var resp = await fetch(url, { signal: ctrl.signal, cache: 'no-store' });
      if (!resp.ok) {
        stopPoll();
        delete mediaInflight[msgID];
        if (source === 'fast' && resp.status === 429) {
          var resetMin = parseInt(resp.headers.get('X-Relay-Reset-Min') || '0', 10);
          await handleRateLimit(resetMin);
          return;
        }
        if (source === 'fast') { handleFastFailure(resp.statusText || ('HTTP ' + resp.status), resp.status); return; }
        mediaShowError(card, resp.statusText || ('HTTP ' + resp.status));
        finishSlot();
        return;
      }
      var total = parseInt(resp.headers.get('Content-Length') || '0', 10) || (parseInt(size, 10) || 0);
      var blob;
      if (!resp.body || !resp.body.getReader) {
        // Old WebView fallback — accept the original hang risk.
        blob = await resp.blob();
      } else {
        var reader = resp.body.getReader();
        var chunks = [];
        var received = 0;
        while (true) {
          var step = await reader.read();
          if (step.done) break;
          chunks.push(step.value);
          received += step.value.byteLength;
          mediaUpdateProgress(card, received, total);
          if (total > 0 && received >= total) {
            try { await reader.cancel(); } catch (e) { }
            break;
          }
        }
        blob = new Blob(chunks, { type: resp.headers.get('Content-Type') || 'application/octet-stream' });
      }
      stopPoll();
      delete mediaInflight[msgID];
      var totalSize = blob.size || (parseInt(card.getAttribute('data-size'), 10) || 0);
      if (totalSize > 0) mediaUpdateProgress(card, totalSize, totalSize);
      debugLog('media: ok msg=' + msgID + ' source=' + source + ' served-by=' + (resp.headers.get('X-Cache') || '?'));
      var elapsed = Date.now() - progressShownAt;
      var run = function () { deliverBlob(blob, resp.headers).finally(finishSlot); };
      if (progressShownAt > 0 && elapsed < MIN_PROGRESS_VISIBLE_MS) {
        setTimeout(run, MIN_PROGRESS_VISIBLE_MS - elapsed);
      } else {
        run();
      }
    } catch (e) {
      stopPoll();
      delete mediaInflight[msgID];
      if (e && e.name === 'AbortError') { finishSlot(); return; }
      if (source === 'fast') { handleFastFailure((e && e.message) || 'network error'); return; }
      mediaShowError(card, (e && e.message) || 'network error');
      finishSlot();
    }
  }

  mediaProgressState[msgID] = {
    loaded: 0,
    total: parseInt(size, 10) || 0,
    completed: 0,
    blocks: source === 'slow' ? (parseInt(blk, 10) || 0) : 0,
    // Relay (fast) is a single HTTP download, tracked in BYTES — never blocks.
    byteOnly: source === 'fast'
  };

  var pollUrl = '/api/media/progress?ch=' + encodeURIComponent(ch)
    + '&blk=' + encodeURIComponent(blk)
    + '&crc=' + encodeURIComponent(crc);
  pollTimer = setInterval(async function () {
    try {
      var pr = await fetch(pollUrl);
      if (!pr.ok) return;
      var pj = await pr.json();
      if (pj.active === false) return;
      // Relay reports bytes (pj.bytes); DNS reports blocks. Route each to its
      // own monotonic channel so the bar reflects the right unit.
      if (pj.bytes) mediaUpdateProgress(card, pj.completed | 0, pj.total | 0);
      else mediaApplyBlockProgress(card, pj.completed | 0, pj.total | 0);
    } catch (e) { }
  }, source === 'fast' ? 300 : 500);

  mediaInflight[msgID] = { ctrl: ctrl };
  mediaShowProgress(card);
  progressShownAt = Date.now();
  runOnce();
}

// mediaProgressState is the per-download counter. Both xhr.onprogress
// (bytes) and the /api/media/progress poll (blocks) write into it, but
// values only ever increase — so a poll that lags behind the byte
// stream never makes the bar jump backwards.
var mediaProgressState = {};

// mediaApplyBlockProgress updates the size text + fill from the polled
// block counter. Driven by /api/media/progress so the user gets real
// per-block updates, not byte-buffered estimates.
function mediaShowProgress(card) {
  var domID = card.id;
  var totalSize = parseInt(card.getAttribute('data-size'), 10) || 0;
  var blocks = parseInt(card.getAttribute('data-blk'), 10) || 0;
  // Relay (byteOnly) downloads aren't block-based — never show a block count.
  var st0 = mediaProgressState[card.getAttribute('data-msg')];
  var blocksSuffix = (blocks > 0 && !(st0 && st0.byteOnly))
    ? ' &middot; 0/' + blocks + ' ' + (t('blocks_label') || 'blocks') : '';
  var initialText = (totalSize > 0 ? '0 / ' + formatBytes(totalSize) : (t('downloading') || 'Downloading...')) + blocksSuffix;
  card.classList.add('downloading');
  if (mediaIsImageTag(card.getAttribute('data-tag'))) {
    var preview = document.getElementById(domID + '-preview');
    if (preview) {
      preview.innerHTML = '<div class="media-progress-overlay" id="' + domID + '-progress">'
        + '<div class="media-progress-bar"><div class="media-progress-fill" id="' + domID + '-fill" style="width:0%"></div></div>'
        + '<div class="media-progress-text" id="' + domID + '-text" dir="ltr">' + initialText + '</div>'
        + '<button class="media-cancel-btn" onclick="mediaStart(\'' + domID + '\')" type="button" title="' + esc(t('cancel') || 'Cancel') + '">&times;</button>'
        + '</div>';
    }
    var msgIDImg = card.getAttribute('data-msg');
    if (msgIDImg && mediaProgressState[msgIDImg]) mediaRenderProgressState(card);
    return;
  }
  // File row: rebuild the info column from scratch so the fill always
  // starts at 0% (clears stale 100% from a previous successful download)
  // and the cancel button replaces the download button.
  var label = mediaTagLabel(card.getAttribute('data-tag'));
  var displayName = card.getAttribute('data-fname') || label;
  var newInner = '<div class="media-file-icon">' + label.charAt(0) + '</div>'
    + '<div class="media-file-info">'
    + '<div class="media-file-name" dir="ltr">' + esc(displayName) + '</div>'
    + '<div class="media-file-size" id="' + domID + '-text" dir="ltr">' + initialText + '</div>'
    + '<div class="media-progress-bar media-progress-bar-thin"><div class="media-progress-fill" id="' + domID + '-fill" style="width:0%"></div></div>'
    + '</div>'
    + '<div class="media-file-actions" id="' + domID + '-actions">'
    + '<button class="media-file-btn media-file-btn-cancel" onclick="mediaStart(\'' + domID + '\')" type="button" title="' + esc(t('cancel') || 'Cancel') + '">&times;</button>'
    + '</div>';
  card.innerHTML = newInner;
  var msgID = card.getAttribute('data-msg');
  if (msgID && mediaProgressState[msgID]) mediaRenderProgressState(card);
}

// Byte-level update from xhr.onprogress.
function mediaUpdateProgress(card, loaded, total) {
  if (!total || total <= 0) {
    total = parseInt(card.getAttribute('data-size'), 10) || 0;
  }
  var msgID = card.getAttribute('data-msg');
  var s = mediaProgressState[msgID] || { loaded: 0, total: 0, completed: 0, blocks: parseInt(card.getAttribute('data-blk'), 10) || 0 };
  if (loaded > s.loaded) s.loaded = loaded;
  if (total > s.total) s.total = total;
  mediaProgressState[msgID] = s;
  mediaRenderProgressState(card);
}

// Block-level update from /api/media/progress poll.
function mediaApplyBlockProgress(card, completed, total) {
  if (!total || total <= 0) return;
  if (completed < 0) completed = 0;
  if (completed > total) completed = total;
  var msgID = card.getAttribute('data-msg');
  var s = mediaProgressState[msgID] || { loaded: 0, total: parseInt(card.getAttribute('data-size'), 10) || 0, completed: 0, blocks: 0 };
  if (completed > s.completed) s.completed = completed;
  if (total > s.blocks) s.blocks = total;
  mediaProgressState[msgID] = s;
  mediaRenderProgressState(card);
}

// Render the combined "X / Y (P%) · K/N blocks" string from monotonic
// state so the two update sources never fight each other.
function mediaRenderProgressState(card) {
  var msgID = card.getAttribute('data-msg');
  var s = mediaProgressState[msgID];
  if (!s) return;
  // Pick the higher of byte% and block% for the bar so it never goes
  // backwards even if one side races ahead of the other.
  var bytePct = s.total > 0 ? Math.min(100, Math.floor(s.loaded * 100 / s.total)) : 0;
  var blockPct = s.blocks > 0 ? Math.min(100, Math.floor(s.completed * 100 / s.blocks)) : 0;
  var pct = Math.max(bytePct, blockPct);
  var fill = document.getElementById(card.id + '-fill');
  if (fill) {
    var prevPct = parseInt(fill.style.width, 10) || 0;
    if (pct > prevPct) fill.style.width = pct + '%';
  }
  var txt = document.getElementById(card.id + '-text');
  if (!txt) return;
  var head = s.total > 0
    ? formatBytes(s.loaded) + ' / ' + formatBytes(s.total) + ' (' + pct + '%)'
    : (s.loaded > 0 ? formatBytes(s.loaded) : (t('downloading') || 'Downloading...'));
  var blocksSuffix = (s.blocks > 0 && !s.byteOnly)
    ? ' &middot; ' + s.completed + '/' + s.blocks + ' ' + (t('blocks_label') || 'blocks')
    : '';
  txt.innerHTML = head + blocksSuffix;
}

function mediaShowBlob(card, blobURL) {
  var tag = card.getAttribute('data-tag');
  var msgID = card.getAttribute('data-msg');
  var domID = card.id;
  card.classList.remove('downloading');
  var entry = mediaBlobs[msgID];
  var mime = entry && entry.mime ? entry.mime.toLowerCase() : '';
  var isInlineImage = /^image\/(jpeg|jpg|png|gif|webp|avif|bmp)$/.test(mime);
  if (mediaIsImageTag(tag) && isInlineImage) {
    var preview = document.getElementById(domID + '-preview');
    if (!preview) return;
    var alt = esc(mediaTagLabel(tag));
    preview.innerHTML = '<img src="' + escAttr(blobURL) + '" alt="' + alt
      + '" class="media-image-loaded" onclick="mediaOpen(\'' + msgID + '\')"'
      + ' onerror="mediaImageFailed(\'' + msgID + '\')">'
      + '<div class="media-image-actions">' + buildMediaActions(msgID, tag) + '</div>';
    return;
  }
  if (mediaIsImageTag(tag)) {
    mediaConvertImageToFile(card);
  }
  var actions = document.getElementById(domID + '-actions');
  if (actions) actions.innerHTML = buildMediaActions(msgID, tag);
  var fill = document.getElementById(domID + '-fill');
  if (fill) fill.style.width = '100%';
  var txt = document.getElementById(domID + '-text');
  if (txt) txt.innerHTML = formatBytes(parseInt(card.getAttribute('data-size'), 10) || 0) + ' ' + icon('check');
}

function mediaImageFailed(msgID) {
  var card = document.getElementById('media-' + msgID);
  if (!card) return;
  mediaConvertImageToFile(card);
  var actions = document.getElementById(card.id + '-actions');
  if (actions) actions.innerHTML = buildMediaActions(msgID, card.getAttribute('data-tag'));
}

function mediaConvertImageToFile(card) {
  var tag = card.getAttribute('data-tag');
  var domID = card.id;
  var size = parseInt(card.getAttribute('data-size'), 10) || 0;
  var blocks = parseInt(card.getAttribute('data-blk'), 10) || 0;
  var label = mediaTagLabel(tag);
  var detail = (size > 0 ? formatBytes(size) : '?')
    + (blocks > 0 ? ' &middot; ' + blocks + ' ' + (t('blocks_label') || 'blocks') : '');
  card.classList.remove('media-image');
  card.classList.add('media-file');
  card.innerHTML = '<div class="media-file-icon">' + label.charAt(0) + '</div>'
    + '<div class="media-file-info">'
    + '<div class="media-file-name">' + label + '</div>'
    + '<div class="media-file-size" id="' + domID + '-text" dir="ltr">' + detail + '</div>'
    + '<div class="media-progress-bar media-progress-bar-thin"><div class="media-progress-fill" id="' + domID + '-fill" style="width:100%"></div></div>'
    + '</div>'
    + '<div class="media-file-actions" id="' + domID + '-actions"></div>';
}

// Read window.Android once. The native bridge is the WebView wrapper's
// way of getting around blob-URL / Web-Share limitations on Android.
var androidBridge = (typeof window !== 'undefined' && window.Android) ? window.Android : null;

// Single entry point invoked by MainActivity.kt's back-press handler.
// Returns nothing — the JS side resolves the action itself, calling
// back into the bridge for "minimize"/"kill" if the user picks one
// of those from the close-confirmation dialog.
window.handleAndroidBack = async function () {
  // Unwind ONE layer per press, matching the in-app back buttons (which the
  // hardware key can't reach because MainActivity routes here instead of
  // firing popstate). Order: transient overlays first, then the reparented
  // shell sections, then the feed; at the feed root, confirm before closing.
  var de = document.documentElement;
  var app = document.getElementById('app');

  // --- transient overlays (most-nested first) ---
  if (typeof closeMediaLightbox === 'function' && closeMediaLightbox()) return;

  var tmLb = document.getElementById('tmLightbox');
  if (tmLb) {
    if (typeof tmCloseLightbox === 'function') tmCloseLightbox();
    else tmLb.remove();
    return;
  }

  var chatSheet = document.querySelector('.chat-sheet-overlay');
  if (chatSheet && chatSheet.parentNode) { chatSheet.parentNode.removeChild(chatSheet); return; }

  // Link sheets (feed + mirror "open this link?"). Own overlay classes, not
  // .modal-overlay, so they need their own back-button hook.
  var linkSheet = document.getElementById('linkSheetOverlay') || document.getElementById('tmLinkSheet');
  if (linkSheet && linkSheet.parentNode) { linkSheet.parentNode.removeChild(linkSheet); return; }

  // Any open modal dialog (profiles, link sheet, info dialog, close-confirm…).
  var openModal = document.querySelector('.modal-overlay.active');
  if (openModal) { openModal.classList.remove('active'); return; }

  // --- in-app sections + feed channel ---
  // Each section (Mirror/Chat/Resolver/Settings) and the feed channel view
  // pushes a history entry per layer (section → sub-view), and each owns a
  // popstate handler that unwinds exactly ONE layer the right way
  // (sub-view → sidebar → feed). So just step history back and let those run —
  // the same path desktop/iOS take. Calling the section-close helpers directly
  // (the old approach) skipped the sidebar step and jumped straight to the feed.
  var inSection =
    de.classList.contains('tm-open') ||
    de.classList.contains('chat-section') ||
    de.classList.contains('resolver-section') ||
    de.classList.contains('settings-section') ||
    (typeof mobileQuery !== 'undefined' && mobileQuery.matches &&
      app && app.classList.contains('chat-open'));
  if (inSection) {
    try { history.back(); } catch (e) { }
    return;
  }

  // At app root — confirm before closing/killing the process.
  var choice = await showCloseConfirm();
  if (choice === 'background' && androidBridge && androidBridge.minimizeApp) {
    androidBridge.minimizeApp();
  } else if (choice === 'kill' && androidBridge && androidBridge.killApp) {
    androidBridge.killApp();
  }
  // 'cancel' or unknown → stay in the app.
};

function showCloseConfirm() {
  return new Promise(function (resolve) {
    var existing = document.getElementById('closeConfirmModal');
    if (existing) existing.remove();
    var overlay = document.createElement('div');
    overlay.id = 'closeConfirmModal';
    overlay.className = 'modal-overlay active';
    // The floating nav sits at z-index 9300; the base .modal-overlay (100) would
    // render the close prompt underneath it. Lift it above the nav.
    overlay.style.zIndex = '9600';
    overlay.innerHTML =
      '<div class="modal" style="max-width:380px">'
      + '<p style="font-size:14px;color:var(--text);margin-bottom:18px;line-height:1.6">'
      + esc(t('close_confirm') || 'Close thefeed?') + '</p>'
      + '<div class="modal-actions" style="flex-direction:column;gap:8px">'
      + '<button class="btn btn-flat" id="closeCancelBtn">' + esc(t('close_cancel') || "Don't close") + '</button>'
      + '<button class="btn btn-flat" id="closeBgBtn">' + esc(t('close_background') || 'Close, keep running in background') + '</button>'
      + '<button class="btn btn-primary" id="closeKillBtn" style="background:var(--danger,#e74c3c)">' + esc(t('close_kill') || 'Close and stop service') + '</button>'
      + '</div></div>';
    document.body.appendChild(overlay);
    var done = function (choice) {
      if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
      resolve(choice);
    };
    document.getElementById('closeCancelBtn').onclick = function () { done('cancel'); };
    document.getElementById('closeBgBtn').onclick = function () { done('background'); };
    document.getElementById('closeKillBtn').onclick = function () { done('kill'); };
  });
}

function blobToBase64(blob) {
  return new Promise(function (resolve, reject) {
    var fr = new FileReader();
    fr.onload = function () {
      var result = fr.result || '';
      var idx = result.indexOf(',');
      resolve(idx >= 0 ? result.substring(idx + 1) : result);
    };
    fr.onerror = function () { reject(fr.error); };
    fr.readAsDataURL(blob);
  });
}

function buildMediaActions(msgID, tag) {
  var isImage = mediaIsImageTag(tag);
  var isPlayable = mediaIsPlayableTag(tag);
  var canShare = !!androidBridge || !!(navigator.share && navigator.canShare);
  var openTitle = esc(t('media_open') || 'Open');
  var playTitle = esc(t('media_play') || 'Play');
  var saveTitle = esc(t('media_save') || 'Save');
  var shareTitle = esc(t('media_share') || 'Share');
  var html = '';
  if (isPlayable) {
    // Play icon (▶) — on Android Intent.createChooser picks the
    // best system video/audio player; on browser the blob opens in a
    // new tab where the browser's native viewer handles it.
    html += '<button class="media-action-icon" onclick="mediaOpen(\'' + msgID + '\')" title="' + playTitle + '" aria-label="' + playTitle + '">' + icon('play') + '</button>';
  } else if (!isImage) {
    html += '<button class="media-action-icon" onclick="mediaOpen(\'' + msgID + '\')" title="' + openTitle + '" aria-label="' + openTitle + '">' + icon('link') + '</button>';
  }
  html += '<button class="media-action-icon" onclick="mediaSave(\'' + msgID + '\')" title="' + saveTitle + '" aria-label="' + saveTitle + '">' + icon('save') + '</button>';
  if (canShare) {
    html += '<button class="media-action-icon" onclick="mediaShare(\'' + msgID + '\')" title="' + shareTitle + '" aria-label="' + shareTitle + '">' + icon('share') + '</button>';
  }
  return html;
}

async function mediaOpen(msgID) {
  var entry = mediaBlobs[msgID];
  if (!entry) return;
  var card = document.getElementById('media-' + msgID);
  var tag = card ? card.getAttribute('data-tag') : '';
  if (mediaIsImageTag(tag) && entry.mime && /^image\//i.test(entry.mime)) {
    showImageLightbox(entry.url, mediaTagLabel(tag));
    return;
  }
  if (mediaIsPlayableTag(tag)) {
    showMediaPlayer(entry, tag);
    return;
  }
  if (androidBridge && androidBridge.openMedia) {
    var fname = mediaFilenameFor(msgID, tag, entry.mime);
    try {
      var b64 = await blobToBase64(entry.blob);
      androidBridge.openMedia(b64, entry.mime || 'application/octet-stream', fname);
    } catch (e) { }
    return;
  }
  try {
    var a = document.createElement('a');
    a.href = entry.url;
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
    a.style.display = 'none';
    document.body.appendChild(a);
    a.click();
    a.remove();
  } catch (e) { }
}

function showMediaPlayer(entry, tag) {
  var existing = document.getElementById('mediaLightbox');
  if (existing) existing.remove();
  var isAudio = (tag === '[AUDIO]' || tag === '[VOICE]');
  var mime = entry.mime || (isAudio ? 'audio/mpeg' : 'video/mp4');
  var element = isAudio
    ? '<audio class="media-lightbox-audio" controls autoplay src="' + escAttr(entry.url) + '" type="' + escAttr(mime) + '"></audio>'
    : '<video class="media-lightbox-video" controls autoplay playsinline src="' + escAttr(entry.url) + '" type="' + escAttr(mime) + '"></video>';
  var overlay = document.createElement('div');
  overlay.id = 'mediaLightbox';
  overlay.className = 'media-lightbox';
  overlay.innerHTML =
    '<button class="media-lightbox-speed" type="button" aria-label="Playback speed">1x</button>'
    + '<button class="media-lightbox-close" type="button" aria-label="' + esc(t('close') || 'Close') + '">&times;</button>'
    + element;
  var close = function () {
    overlay.removeEventListener('click', onClick);
    document.removeEventListener('keydown', onKey);
    var media = overlay.querySelector('audio,video');
    if (media) { try { media.pause(); } catch (e) { } }
    if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
    mediaLightboxCloser = null;
  };
  var onClick = function (e) {
    if (e.target === overlay || (e.target.classList && e.target.classList.contains('media-lightbox-close'))) {
      close();
    }
  };
  var onKey = function (e) { if (e.key === 'Escape') close(); };
  overlay.addEventListener('click', onClick);
  document.addEventListener('keydown', onKey);
  document.body.appendChild(overlay);
  mediaLightboxCloser = close;

  // Playback-speed cycling. Standard HTML5 playbackRate — supported
  // on every modern WebView, no native bridge needed. Pitch is
  // preserved by default in Chromium / WebKit (preservesPitch=true).
  var speeds = [1, 1.25, 1.5, 2];
  var saved = parseFloat(localStorage.getItem('thefeed_play_speed'));
  var idx = speeds.indexOf(saved);
  if (idx < 0) idx = 0;
  var media = overlay.querySelector('audio,video');
  var speedBtn = overlay.querySelector('.media-lightbox-speed');
  function applySpeed() {
    var s = speeds[idx];
    if (media) {
      try { media.playbackRate = s; } catch (e) { }
      // Webkit needs the prefixed flag for pitch preservation.
      try { media.preservesPitch = true; } catch (e) { }
      try { media.webkitPreservesPitch = true; } catch (e) { }
    }
    if (speedBtn) speedBtn.textContent = (s === Math.floor(s) ? s : s.toFixed(2).replace(/0$/, '')) + 'x';
    localStorage.setItem('thefeed_play_speed', String(s));
  }
  // Re-apply on every fresh `loadeddata` — some WebViews reset the
  // rate when the source becomes ready.
  if (media) media.addEventListener('loadeddata', applySpeed);
  applySpeed();
  if (speedBtn) {
    // Coalesce rapid duplicate clicks. Some Android WebView builds
    // fire click twice per tap (touchend + synthesized 300ms tap),
    // which made one fast tap skip a step (1 → 1.5). 250 ms is well
    // under any human double-tap interval.
    var lastClickAt = 0;
    speedBtn.addEventListener('click', function (e) {
      e.stopPropagation();
      var now = Date.now();
      if (now - lastClickAt < 250) return;
      lastClickAt = now;
      idx = (idx + 1) % speeds.length;
      applySpeed();
    });
  }
}

function mediaFilenameFor(msgID, tag, mime) {
  var card = document.getElementById('media-' + msgID);
  var fname = card ? (card.getAttribute('data-fname') || '') : '';
  if (!fname) {
    var ext = mediaExtForTag(tag, mime);
    fname = (mediaTagLabel(tag || 'FILE') + '-' + msgID + '.' + ext).toLowerCase();
  }
  return fname;
}

// Tracked close handler so the Android back button (and any other
// place outside the lightbox) can dismiss it cleanly.
var mediaLightboxCloser = null;

function closeMediaLightbox() {
  if (mediaLightboxCloser) {
    try { mediaLightboxCloser(); } catch (e) { }
    mediaLightboxCloser = null;
    return true;
  }
  return false;
}

function showImageLightbox(blobURL, alt) {
  var existing = document.getElementById('mediaLightbox');
  if (existing) existing.remove();
  var overlay = document.createElement('div');
  overlay.id = 'mediaLightbox';
  overlay.className = 'media-lightbox';
  overlay.innerHTML = '<button class="media-lightbox-close" type="button" aria-label="' + esc(t('close') || 'Close') + '">&times;</button>'
    + '<img class="media-lightbox-img" src="' + escAttr(blobURL) + '" alt="' + esc(alt || '') + '">';
  var close = function () {
    overlay.removeEventListener('click', onClick);
    document.removeEventListener('keydown', onKey);
    if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
    mediaLightboxCloser = null;
  };
  var onClick = function (e) {
    // Don't dismiss while pinching/dragging — the click event fires
    // on touchend after a multi-touch zoom and would close the box.
    if (zoomBusy) return;
    if (e.target === overlay || (e.target.classList && e.target.classList.contains('media-lightbox-close'))) {
      close();
    }
  };
  var onKey = function (e) { if (e.key === 'Escape') close(); };
  overlay.addEventListener('click', onClick);
  document.addEventListener('keydown', onKey);
  document.body.appendChild(overlay);
  mediaLightboxCloser = close;

  // Pinch-zoom + pan + double-tap on the image.
  var imgEl = overlay.querySelector('.media-lightbox-img');
  var zoomBusy = false;
  attachPinchZoom(imgEl, function (busy) { zoomBusy = busy; });
}

// Two-finger pinch + drag-to-pan + double-tap toggle. Uses
// CSS transform so we don't need a library. busyCb fires true on
// multi-touch start and false a tick after touchend so the click
// close handler can ignore the trailing tap.
function attachPinchZoom(el, busyCb) {
  var scale = 1, startScale = 1;
  var translateX = 0, translateY = 0;
  var startTX = 0, startTY = 0;
  var pinchStartDist = 0;
  var lastTap = 0;
  var lastTouchEnd = 0;
  el.style.touchAction = 'none';
  el.style.transformOrigin = 'center center';
  el.style.transition = 'transform 0.1s ease-out';
  // iOS 13+ ignores user-scalable=no; prevent native pinch-zoom on
  // the overlay so our custom zoom isn't fought by the browser's.
  var overlay = el.parentNode;
  var preventGesture = function (e) { e.preventDefault(); };
  overlay.addEventListener('gesturestart', preventGesture, { passive: false });
  overlay.addEventListener('gesturechange', preventGesture, { passive: false });

  function apply() {
    el.style.transform = 'translate(' + translateX + 'px, ' + translateY + 'px) scale(' + scale + ')';
  }
  function distance(t0, t1) {
    var dx = t0.clientX - t1.clientX;
    var dy = t0.clientY - t1.clientY;
    return Math.hypot(dx, dy);
  }
  function clampPan() {
    if (scale <= 1) { translateX = 0; translateY = 0; return; }
    var maxX = (el.offsetWidth * (scale - 1)) / 2;
    var maxY = (el.offsetHeight * (scale - 1)) / 2;
    translateX = Math.max(-maxX, Math.min(maxX, translateX));
    translateY = Math.max(-maxY, Math.min(maxY, translateY));
  }
  el.addEventListener('touchstart', function (e) {
    if (e.touches.length === 2) {
      e.preventDefault();
      pinchStartDist = distance(e.touches[0], e.touches[1]);
      startScale = scale;
      if (busyCb) busyCb(true);
    } else if (e.touches.length === 1 && scale > 1) {
      startTX = e.touches[0].clientX - translateX;
      startTY = e.touches[0].clientY - translateY;
      if (busyCb) busyCb(true);
    }
  }, { passive: false });
  el.addEventListener('touchmove', function (e) {
    if (e.touches.length === 2 && pinchStartDist > 0) {
      e.preventDefault();
      var d = distance(e.touches[0], e.touches[1]);
      scale = Math.max(1, Math.min(5, startScale * d / pinchStartDist));
      if (scale === 1) { translateX = 0; translateY = 0; }
      apply();
    } else if (e.touches.length === 1 && scale > 1) {
      e.preventDefault();
      translateX = e.touches[0].clientX - startTX;
      translateY = e.touches[0].clientY - startTY;
      clampPan();
      apply();
    }
  }, { passive: false });
  el.addEventListener('touchend', function (e) {
    lastTouchEnd = Date.now();
    setTimeout(function () { if (busyCb) busyCb(false); }, 50);
    pinchStartDist = 0;
    // 2→1 finger: re-seed pan origin so the leftover finger doesn't
    // compute translation against stale startTX/startTY (=0) and snap
    // the image off-screen.
    if (e.touches.length === 1 && scale > 1) {
      startTX = e.touches[0].clientX - translateX;
      startTY = e.touches[0].clientY - translateY;
    } else if (e.touches.length === 0) {
      clampPan();
      apply();
    }
  });
  el.addEventListener('click', function (e) {
    // Ignore clicks that immediately follow a pinch.
    if (Date.now() - lastTouchEnd < 100) { e.stopPropagation(); return; }
    var now = Date.now();
    if (now - lastTap < 300) {
      e.stopPropagation();
      if (scale === 1) {
        scale = 2;
      } else {
        scale = 1; translateX = 0; translateY = 0;
      }
      apply();
    }
    lastTap = now;
  });
}

async function mediaSave(msgID) {
  var entry = mediaBlobs[msgID];
  if (!entry) return;
  var card = document.getElementById('media-' + msgID);
  var tag = card ? card.getAttribute('data-tag') : '';
  var fname = mediaFilenameFor(msgID, tag, entry.mime);
  if (androidBridge && androidBridge.saveMedia) {
    try {
      var b64 = await blobToBase64(entry.blob);
      androidBridge.saveMedia(b64, entry.mime || 'application/octet-stream', fname);
    } catch (e) { }
    return;
  }
  var a = document.createElement('a');
  a.href = entry.url;
  a.download = fname;
  a.style.display = 'none';
  document.body.appendChild(a);
  a.click();
  a.remove();
}

async function mediaShare(msgID) {
  var entry = mediaBlobs[msgID];
  if (!entry) return;
  var card = document.getElementById('media-' + msgID);
  var tag = card ? card.getAttribute('data-tag') : '';
  var fname = mediaFilenameFor(msgID, tag, entry.mime);
  if (androidBridge && androidBridge.shareMedia) {
    try {
      var b64 = await blobToBase64(entry.blob);
      androidBridge.shareMedia(b64, entry.mime || 'application/octet-stream', fname);
    } catch (e) { }
    return;
  }
  var file;
  try {
    file = new File([entry.blob], fname, { type: entry.mime || 'application/octet-stream' });
  } catch (e) {
    mediaSave(msgID);
    return;
  }
  var data = { files: [file], title: fname };
  if (navigator.canShare && !navigator.canShare(data)) {
    mediaSave(msgID);
    return;
  }
  try {
    await navigator.share(data);
  } catch (e) { }
}

function mediaShowError(card, msg) {
  card.classList.remove('downloading');
  var txt = document.getElementById(card.id + '-text');
  if (txt) txt.textContent = (t('media_failed') || 'Download failed') + ': ' + msg;
  var fill = document.getElementById(card.id + '-fill');
  if (fill) fill.style.width = '0%';
}

function mediaResetCard(card) {
  card.classList.remove('downloading');
  var domID = card.id;
  var msgID = card.getAttribute('data-msg');
  var tag = card.getAttribute('data-tag');
  var size = parseInt(card.getAttribute('data-size'), 10) || 0;
  var sizeStr = size > 0 ? formatBytes(size) : '?';
  var label = mediaTagLabel(tag);
  if (mediaIsImageTag(tag)) {
    var preview = document.getElementById(domID + '-preview');
    if (preview) {
      preview.innerHTML = '<button class="media-action" onclick="mediaStart(\'' + domID + '\')" type="button" title="' + esc(t('download') || 'Download') + '">'
        + '<span class="media-icon">' + icon('download') + '</span>'
        + '<span class="media-meta">' + label + ' &middot; ' + sizeStr + '</span>'
        + '</button>';
    }
    return;
  }
  var actions = document.getElementById(domID + '-actions');
  if (actions) {
    actions.innerHTML = '<button class="media-file-btn" onclick="mediaStart(\'' + domID + '\')" type="button" title="' + esc(t('download') || 'Download') + '">' + icon('download') + '</button>';
  }
  var fill = document.getElementById(domID + '-fill');
  if (fill) fill.style.width = '0%';
  var txt = document.getElementById(domID + '-text');
  if (txt) txt.textContent = sizeStr;
}

// Parses leading [IMAGE]/[VIDEO]/[FILE]/etc tags out of `text`.
// Returns {mediaHtml, textHtml} — used by both top-level messages
// and the body of a [REPLY] (so a reply that carries an image
// still renders a downloadable media card instead of dropping it).
function parseTextMedia(text, idBase) {
  var swapNationalFlag = function (s) {
    return linkify(s).replace(/🇮🇷/g, '<img src="/static/lion-sun.svg" alt="\u{1F981}☀️" style="height:1.1em;vertical-align:middle">');
  };
  var mediaTypes = ['[IMAGE]', '[VIDEO]', '[FILE]', '[AUDIO]', '[STICKER]', '[GIF]', '[CONTACT]', '[LOCATION]'];
  var rest = text;
  var cards = [];
  while (true) {
    var matchedTag = null;
    for (var m = 0; m < mediaTypes.length; m++) {
      if (rest.indexOf(mediaTypes[m]) === 0) { matchedTag = mediaTypes[m]; break; }
    }
    if (!matchedTag) break;
    var p = parseDownloadableMedia(rest, matchedTag);
    // Bare "[TAG]<word>" with non-numeric/non-newline next char is
    // caption text, not a media tag. Wire forms are "[TAG]\n..."
    // or "[TAG]<digit>:<dl>:<ch>:<blk>:<crc>".
    var afterTag = rest.charAt(matchedTag.length);
    var isLegacyOrStructured = afterTag === '' || afterTag === '\n' || (afterTag >= '0' && afterTag <= '9');
    if (!isLegacyOrStructured) break;
    cards.push({ tag: matchedTag, parsed: p });
    rest = p.caption;
  }
  var mediaHtml = '';
  var textHtml = '';
  if (cards.length === 0) {
    textHtml = swapNationalFlag(text);
  } else if (cards.length === 1) {
    var c0 = cards[0];
    if (c0.parsed.downloadable || c0.parsed.size > 0) {
      mediaHtml = renderDownloadableMedia(c0.tag, c0.parsed, idBase);
      textHtml = c0.parsed.caption ? swapNationalFlag(c0.parsed.caption) : '';
    } else {
      mediaHtml = '<div class="media-tag">' + c0.tag + '</div>';
      textHtml = swapNationalFlag(text.substring(c0.tag.length).replace(/^\n/, ''));
    }
  } else {
    mediaHtml = '<div class="media-album">';
    for (var k = 0; k < cards.length; k++) {
      mediaHtml += renderDownloadableMedia(cards[k].tag, cards[k].parsed, idBase + '-' + k);
    }
    mediaHtml += '</div>';
    textHtml = rest ? swapNationalFlag(rest) : '';
  }
  return { mediaHtml: mediaHtml, textHtml: textHtml };
}

function renderMessages(msgs, gaps) {
  var el = document.getElementById('messages');
  if (!msgs || !msgs.length) { el.innerHTML = '<div class="empty-state"><p>' + t('no_messages') + '</p><p style="font-size:12px;opacity:.6;margin-top:6px">' + t('no_messages_hint') + '</p></div>'; return }
  // Check if user is near the bottom before re-render (within 150px)
  var wasAtBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 150;
  var prevScrollTop = el.scrollTop; // for restoring scroll on re-render mid-session
  var isFirstRender = el.querySelector('.empty-state') !== null || el.querySelector('.msg') === null;
  msgs.sort(function (a, b) { return (a.Timestamp || a.timestamp || 0) - (b.Timestamp || b.timestamp || 0) });
  var html = '', lastDate = '';
  // Build a lookup: message ID → gap count to insert BEFORE that message.
  var gapBefore = {};
  if (gaps) { for (var g = 0; g < gaps.length; g++) { gapBefore[gaps[g].before_id] = gaps[g].count } }
  // Build a lookup: message ID → message object (for reply previews).
  var msgByID = {};
  for (var mi = 0; mi < msgs.length; mi++) { var mid = msgs[mi].ID || msgs[mi].id; if (mid) msgByID[mid] = msgs[mi] }
  currentMsgTexts = [];
  var dateLocale = lang === 'fa' ? 'fa-IR' : 'en-US';
  var dateOpts = lang === 'fa' ? { year: 'numeric', month: 'long', day: 'numeric', calendar: 'persian' } : { year: 'numeric', month: 'long', day: 'numeric' };
  var lastSeenTs = getLastSeenTimestamp(channelName(selectedChannel));
  var isFirstVisit = lastSeenTs === 0;
  var newMsgSepInserted = false;
  var maxMsgID = 0;
  var maxTimestamp = 0;
  for (var i = 0; i < msgs.length; i++) {
    var msg = msgs[i];
    var id = msg.ID || msg.id;
    var msgTs = msg.Timestamp || msg.timestamp || 0;
    if (id > maxMsgID) maxMsgID = id;
    if (msgTs > maxTimestamp) maxTimestamp = msgTs;
    if (gapBefore[id]) {
      html += '<div class="msg-gap-sep"><span>' + t('missed_messages').replace('{n}', gapBefore[id]) + '</span></div>';
    }
    // New messages separator (timestamp-based for X/Telegram compatibility)
    if (!isFirstVisit && lastSeenTs > 0 && msgTs > lastSeenTs && !newMsgSepInserted) {
      html += '<div class="msg-new-sep" id="newMsgSep"><span>' + t('new_messages') + '</span></div>';
      newMsgSepInserted = true;
    }
    var ts = new Date((msg.Timestamp || msg.timestamp) * 1000);
    var dateStr = ts.toLocaleDateString(dateLocale, dateOpts);
    if (dateStr !== lastDate) { html += '<div class="msg-date-sep"><span dir="auto">' + dateStr + '</span></div>'; lastDate = dateStr }
    var timeStr = ts.toLocaleTimeString(dateLocale, { hour: '2-digit', minute: '2-digit' });
    var text = msg.Text || msg.text || '';
    // Outgoing private-chat messages get a "[ME]\n" sentinel
    // from the server. Strip it, flag the bubble for right-align
    // and a [YOU] label, then continue parsing the rest.
    var isOutgoing = false;
    if (text.indexOf('[ME]\n') === 0) {
      isOutgoing = true;
      text = text.substring('[ME]\n'.length);
    } else if (text === '[ME]') {
      isOutgoing = true;
      text = '';
    }
    currentMsgTexts.push(text);
    var mediaHtml = '', textHtml = linkify(text).replace(/\uD83C\uDDEE\uD83C\uDDF7/g, '<img src="/static/lion-sun.svg" alt="\u{1F981}\u2600\uFE0F" style="height:1.1em;vertical-align:middle">');
    // Check for [REPLY]:ID or [REPLY] format (backward compat: also [REPLY:ID])
    var replyMatch = text.match(/^\[REPLY\](?::(\d+))?/) || text.match(/^\[REPLY:(\d+)\]/);
    if (replyMatch) {
      var replyTag = replyMatch[0];
      var replyId = replyMatch[1] ? parseInt(replyMatch[1]) : 0;
      var replyBody = text.substring(replyTag.length).replace(/^\n/, '');
      // Outgoing-reply: server emits [REPLY]:N\n[ME]\nbody, so we
      // peel [ME] off the inner body too. Without this, replies
      // the user sent themselves would render as incoming.
      if (replyBody.indexOf('[ME]\n') === 0) {
        isOutgoing = true;
        replyBody = replyBody.substring('[ME]\n'.length);
      } else if (replyBody === '[ME]') {
        isOutgoing = true;
        replyBody = '';
      }
      // Build the snippet of the message being replied to (skipped
      // when the target isn't in this batch).
      var previewHtml = '';
      if (replyId > 0 && msgByID[replyId]) {
        var rpText = (msgByID[replyId].Text || msgByID[replyId].text || '').replace(/^\[(?:IMAGE|VIDEO|FILE|AUDIO|STICKER|GIF|POLL|CONTACT|LOCATION|REPLY)[^\]]*\][^\n]*\n?/, '');
        if (rpText.length > 120) rpText = rpText.substring(0, 120) + '…';
        previewHtml = '<div class="reply-preview" onclick="scrollToMsg(' + replyId + ')" title="#' + replyId + '">' + esc(rpText) + '</div>';
      }
      // Parse the body — POLL gets the styled card, otherwise hand
      // off to parseTextMedia so a reply that carries an image
      // (or any other [TAG]) renders a downloadable card instead
      // of dropping the media silently.
      var bodyMediaHtml, bodyTextHtml;
      if (replyBody.indexOf('[POLL]') === 0) {
        var rpPollBody = replyBody.substring('[POLL]'.length).replace(/^\n/, '');
        bodyMediaHtml = '<div class="media-tag">[POLL]</div>';
        bodyTextHtml = renderPollCard(rpPollBody);
      } else {
        var rpParsed = parseTextMedia(replyBody, id);
        bodyMediaHtml = rpParsed.mediaHtml;
        bodyTextHtml = rpParsed.textHtml;
      }
      mediaHtml = '<div class="media-tag reply-tag">[REPLY]</div>' + previewHtml + bodyMediaHtml;
      textHtml = bodyTextHtml;
    } else if (text.indexOf('[POLL]') === 0) {
      // Render poll with styled card
      mediaHtml = '<div class="media-tag">[POLL]</div>';
      var pollBody = text.substring('[POLL]'.length).replace(/^\n/, '');
      textHtml = renderPollCard(pollBody);
    } else {
      var mediaTypes = ['[IMAGE]', '[VIDEO]', '[FILE]', '[AUDIO]', '[STICKER]', '[GIF]', '[CONTACT]', '[LOCATION]'];
      var rest = text;
      var cards = [];
      while (true) {
        var matchedTag = null;
        for (var m = 0; m < mediaTypes.length; m++) {
          if (rest.indexOf(mediaTypes[m]) === 0) { matchedTag = mediaTypes[m]; break; }
        }
        if (!matchedTag) break;
        var p = parseDownloadableMedia(rest, matchedTag);
        // Bare "[TAG]<word>" form (no leading "\n", non-numeric "<word>") is
        // caption text that happens to start with a tag literal — don't
        // treat it as media. The wire forms are either "[TAG]\n..." or
        // "[TAG]<digit>:<dl>:...".
        var afterTag = rest.charAt(matchedTag.length);
        var isLegacyOrStructured = afterTag === '' || afterTag === '\n' || (afterTag >= '0' && afterTag <= '9');
        if (!isLegacyOrStructured) break;
        cards.push({ tag: matchedTag, parsed: p });
        rest = p.caption;
      }
      if (cards.length === 0) { /* no tag matched */ }
      else if (cards.length === 1) {
        var c0 = cards[0];
        if (c0.parsed.downloadable || c0.parsed.size > 0) {
          mediaHtml = renderDownloadableMedia(c0.tag, c0.parsed, id);
          textHtml = c0.parsed.caption
            ? linkify(c0.parsed.caption).replace(/\uD83C\uDDEE\uD83C\uDDF7/g, '<img src="/static/lion-sun.svg" alt="\u{1F981}\u2600\uFE0F" style="height:1.1em;vertical-align:middle">')
            : '';
        } else {
          mediaHtml = '<div class="media-tag">' + c0.tag + '</div>';
          textHtml = linkify(text.substring(c0.tag.length).replace(/^\n/, '')).replace(/\uD83C\uDDEE\uD83C\uDDF7/g, '<img src="/static/lion-sun.svg" alt="\u{1F981}\u2600\uFE0F" style="height:1.1em;vertical-align:middle">');
        }
      } else {
        mediaHtml = '<div class="media-album">';
        for (var k = 0; k < cards.length; k++) {
          var ck = cards[k];
          mediaHtml += renderDownloadableMedia(ck.tag, ck.parsed, id + '-' + k);
        }
        mediaHtml += '</div>';
        textHtml = rest
          ? linkify(rest).replace(/\uD83C\uDDEE\uD83C\uDDF7/g, '<img src="/static/lion-sun.svg" alt="\u{1F981}\u2600\uFE0F" style="height:1.1em;vertical-align:middle">')
          : '';
      }
    }
    var hasMediaCard = mediaHtml.indexOf('media-card') !== -1;
    var msgClasses = 'msg'
      + (isPersian(text) ? ' rtl-msg' : '')
      + (hasMediaCard ? ' has-media' : '')
      + (isOutgoing ? ' msg-outgoing' : '');
    var youTag = isOutgoing ? '<div class="media-tag you-tag">[YOU]</div>' : '';
    html += '<div class="' + msgClasses + '" dir="auto">' + youTag + mediaHtml + textHtml + '<div class="msg-meta"><button class="msg-save-btn" aria-label="' + (t('forward_to_saved') || 'Save') + '" onclick="msgSaveToggle(' + id + ', this)">' + icon('bookmark') + '</button><button class="msg-copy-btn" onclick="copyMsg(' + i + ')">' + t('copy') + '</button><span>#' + id + '</span><span>' + timeStr + '</span></div></div>';
  }
  el.innerHTML = html;
  try { mediaTryRestoreVisibleCards(); } catch (e) { }
  currentMaxMsgID = maxMsgID;
  currentMaxTimestamp = maxTimestamp;
  // On first visit, store max timestamp (no separator shown)
  var chName = channelName(selectedChannel);
  if (isFirstVisit && maxTimestamp > 0) {
    setLastSeenTimestamp(chName, maxTimestamp);
  }

  if (newMsgSepInserted) {
    // The "new messages" separator is showing. Don't commit lastSeen
    // immediately — keep the separator visible across re-renders for a
    // few seconds so the user actually has time to notice it (otherwise
    // the next refresh would dismiss it instantly).
    var prevSepLastSeen = newMsgSepLastSeen[chName];
    var sepIsNew = prevSepLastSeen !== lastSeenTs;
    newMsgSepLastSeen[chName] = lastSeenTs;

    // Only scroll to the separator the FIRST time we see this particular
    // separator state — re-renders that don't introduce newer messages
    // shouldn't yank the user's scroll position around.
    if (sepIsNew) {
      newMsgScrollDone = true;
      setTimeout(function () {
        var sep = document.getElementById('newMsgSep');
        // Instant, not smooth: smooth-scroll animation janks the Android WebView
        // compositor; an instant jump to the separator is reliable.
        if (sep) sep.scrollIntoView({ block: 'start' });
      }, 100);
    } else if (wasAtBottom) {
      // Same separator, but the user is parked at the bottom — keep them
      // at the new bottom so they see the freshest message.
      el.scrollTop = el.scrollHeight;
    } else {
      // Same separator, just a re-render (e.g. another refresh tick or
      // additional new messages within the sticky window). innerHTML reset
      // scrollTop to 0; restore the user's previous position so they
      // aren't teleported away from what they were reading.
      el.scrollTop = prevScrollTop;
    }

    // Defer (or extend) the commit. After the sticky window expires we
    // mark messages as seen so the separator naturally goes away on the
    // next render.
    if (newMsgSepCommitTimer[chName]) clearTimeout(newMsgSepCommitTimer[chName]);
    var commitTs = maxTimestamp;
    newMsgSepCommitTimer[chName] = setTimeout(function () {
      delete newMsgSepCommitTimer[chName];
      delete newMsgSepLastSeen[chName];
      if (commitTs > 0) setLastSeenTimestamp(chName, commitTs);
    }, NEW_MSG_STICKY_MS);
  } else {
    // No new-messages separator. Behave like before: if user is parked at
    // the bottom (or this is the first render), keep them at the bottom.
    if (wasAtBottom && maxTimestamp > 0 && !isFirstVisit) {
      setLastSeenTimestamp(chName, maxTimestamp);
    }
    if (isFirstRender || wasAtBottom) {
      el.scrollTop = el.scrollHeight;
      document.getElementById('scrollDownBtn').classList.remove('visible');
    } else {
      // User had scrolled up and is reading older messages — restore
      // their position so an in-place re-render doesn't yank them to
      // the top (innerHTML resets scrollTop to 0 by default).
      el.scrollTop = prevScrollTop;
    }
  }
  // A jump-to-post highlight may have just been wiped by this re-render
  // (the refresh that selectChannel starts lands seconds after the jump).
  // While the highlight window is open, re-anchor on the post and restart
  // its ring on the fresh element. Doesn't extend the window, so repeated
  // refreshes can't pin the view forever.
  if (_msgHighlight && _msgHighlight.ch === selectedChannel && Date.now() < _msgHighlight.until) {
    var hlEl = findMsgEl(_msgHighlight.id);
    if (hlEl) {
      hlEl.scrollIntoView({ block: 'center' });
      highlightMsgEl(hlEl);
    }
  }
}

