'use strict';

// ---------------------------------------------------------------------------
// Defaults and tunables. Everything adjustable lives here; the server's own
// values (see --preview-dpi / --scan-dpi) win where it reports them, and these
// are the fallbacks used before /api/status answers.
// ---------------------------------------------------------------------------
const CONFIG = {
  previewDpi: 75,                     // fallback until the server reports its default
  scanDpi: 300,                       // fallback for the scan menu
  dpiOptions: [75, 150, 300, 600],    // fallback list; the device's real set comes from the server

  reconnectMs: 2000,                  // wait before re-opening a dropped event stream
  minDragPx: 4,                       // a drag shorter than this is a click, not a selection

  // Crop marquee. Grayscale on purpose: the accent is reserved for buttons.
  marquee: {
    dimOutside: 'rgba(0,0,0,0.42)',   // shading over the unselected area
    under: '#ffffff',                 // solid line beneath the dashes
    over: '#000000',                  // dashed line on top
    dash: [7, 5],
    widthDivisor: 700,                // line width = canvas width / this
    tickDivisor: 90,                  // corner tick length = canvas width / this
  },
};

// The device measures the platen in 1/600 inch units regardless of resolution,
// so selections are converted into those units before being sent.
const UNIT = 600;
const MM_PER_INCH = 25.4;

const el = (id) => document.getElementById(id);

// There is one scanner, so the server holds one state - what it is doing, the
// preview, the crop and the last scan - and pushes it to every page over
// /api/events. Nothing below decides what is true; it renders what arrives, so
// a scan started on a phone draws its progress and its result here too. Only
// the drag in progress is local, because it is not a fact about the scanner
// until the finger comes off the glass.
const state = {
  rev: -1,
  snap: null,
  preview: null,       // {id, img, area, unitsPerPx}
  loadingPreview: null,
  resultId: null,
  sel: null,           // {x, y, w, h} in canvas pixels, derived from snap.sel
  drag: null,
  view: 'preview',
  busy: false,
};

// Every call to the server goes through here. The access token is short-lived
// and renewed by the server in passing, so a 401 means the long-lived refresh
// token is gone or expired too: the only thing left is to log in again.
async function api(url, opts) {
  const r = await fetch(url, opts);
  if (r.status === 401) {
    window.location.replace('/login');
    throw new Error('signed out');
  }
  return r;
}

function showError(msg) {
  const box = el('error');
  if (!msg) { box.hidden = true; return; }
  box.hidden = false;
  box.textContent = msg;
}

function setBusy(busy) {
  state.busy = busy;
  for (const id of ['btnPreview', 'btnScanFull', 'dpi', 'previewDpi', 'btnReset']) {
    el(id).disabled = busy;
  }
  const noSel = !state.sel;
  el('btnScanSel').disabled = busy || noSel;
  el('btnPreviewSel').disabled = busy || noSel;
  el('progressWrap').hidden = !busy;
  if (!busy) { el('bar').style.width = '0'; el('progressText').textContent = ''; }
}

function fillSelect(sel, values, chosen) {
  if (sel.options.length) return;
  for (const v of values) {
    const o = document.createElement('option');
    o.value = v; o.textContent = v + ' dpi';
    if (v === chosen) o.selected = true;
    sel.appendChild(o);
  }
}

async function refreshStatus() {
  try {
    const s = await (await api('/api/status')).json();
    const opts = s.dpiOptions || CONFIG.dpiOptions;
    fillSelect(el('previewDpi'), opts, s.previewDpi || CONFIG.previewDpi);
    fillSelect(el('dpi'), opts, s.scanDpi || CONFIG.scanDpi);
    el('outDir').textContent = s.outDir || '.';

    el('status').textContent = s.device
      ? `${s.device}${s.firmware ? ' · ' + s.firmware : ''} · via ${s.backend}`
      : `no scanner detected · via ${s.backend}`;

    el('warning').hidden = !s.warning;
    if (s.warning) el('warning').textContent = s.warning;
    el('btnReset').hidden = !s.canReset;
    el('btnLogout').hidden = !s.auth;
    // A device error from /api/status is worth showing, but not at the cost of
    // wiping a scan error the shared state is carrying.
    if (s.error) showError(s.error);
  } catch (e) {
    el('status').textContent = 'cannot reach the escan server';
  }
}

// ---------- view switching ----------

function setView(name) {
  if (name === 'preview' && !state.preview) return;
  if (name === 'result' && !state.resultId) return;
  state.view = name;
  const isPreview = name === 'preview';
  el('canvas').hidden = !isPreview;
  el('resultImg').hidden = isPreview;
  el('placeholder').hidden = !!(state.preview || state.resultId);
  el('viewPreview').classList.toggle('active', isPreview);
  el('viewResult').classList.toggle('active', !isPreview);
  el('viewNote').textContent = isPreview
    ? (state.sel ? 'drag to adjust the area' : 'drag to choose an area')
    : 'the preview and your selection are kept';
}

function refreshViewButtons() {
  el('viewPreview').disabled = !state.preview;
  el('viewResult').disabled = !state.resultId;
}

// ---------- preview canvas and crop ----------

function drawStage() {
  const c = el('canvas');
  if (!state.preview) return;
  const ctx = c.getContext('2d');
  ctx.drawImage(state.preview.img, 0, 0);
  if (!state.sel) return;

  const { x, y, w, h } = state.sel;
  const M = CONFIG.marquee;
  // Dim everything outside the selection so the crop reads clearly.
  ctx.fillStyle = M.dimOutside;
  ctx.fillRect(0, 0, c.width, y);
  ctx.fillRect(0, y + h, c.width, c.height - y - h);
  ctx.fillRect(0, y, x, h);
  ctx.fillRect(x + w, y, c.width - x - w, h);

  const lw = Math.max(1, Math.round(c.width / M.widthDivisor));
  ctx.lineWidth = lw;
  ctx.setLineDash([]);
  ctx.strokeStyle = M.under;
  ctx.strokeRect(x, y, w, h);
  ctx.strokeStyle = M.over;
  ctx.setLineDash(M.dash);
  ctx.strokeRect(x, y, w, h);
  ctx.setLineDash([]);

  // Corner ticks cut at 45 degrees, matching the chamfers in the chrome.
  const t = Math.max(6, Math.round(c.width / M.tickDivisor));
  ctx.lineWidth = lw * 2;
  for (const [cx, cy, sx, sy] of [
    [x, y, 1, 1], [x + w, y, -1, 1], [x, y + h, 1, -1], [x + w, y + h, -1, -1],
  ]) {
    ctx.beginPath();
    ctx.moveTo(cx + sx * t, cy);
    ctx.lineTo(cx, cy + sy * t);
    ctx.strokeStyle = M.under;
    ctx.stroke();
  }
}

// Selection in device units, derived from the previewed area rather than from
// an assumed preview resolution. This is the form the crop is shared in, and
// the only form that means the same thing on another screen.
function selectionUnits() {
  if (!state.sel || !state.preview) return null;
  const k = state.preview.unitsPerPx;
  const a = state.preview.area;
  return {
    x: Math.round(a.x + state.sel.x * k),
    y: Math.round(a.y + state.sel.y * k),
    w: Math.max(1, Math.round(state.sel.w * k)),
    h: Math.max(1, Math.round(state.sel.h * k)),
  };
}

// The reverse: the shared crop, in canvas pixels for this screen's preview.
function applySharedSelection() {
  if (state.drag) return;   // a finger is on the glass here; it wins locally
  const u = state.snap && state.snap.sel;
  if (!u || !state.preview) { state.sel = null; return; }
  const k = state.preview.unitsPerPx;
  const a = state.preview.area;
  state.sel = {
    x: (u.x - a.x) / k, y: (u.y - a.y) / k,
    w: u.w / k, h: u.h / k,
  };
}

async function shareSelection() {
  try {
    await api('/api/selection', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(selectionUnits()),
    });
  } catch (e) { /* the stream will correct us */ }
}

function updateReadout() {
  const u = selectionUnits();
  if (!u) {
    el('selMm').textContent = 'whole bed';
    el('selPx').textContent = '-';
    el('selPos').textContent = '-';
    el('btnScanSel').disabled = true;
    el('btnPreviewSel').disabled = true;
    el('btnClear').disabled = true;
    return;
  }
  const dpi = parseInt(el('dpi').value, 10) || CONFIG.scanDpi;
  const mm = (v) => (v / UNIT * MM_PER_INCH).toFixed(1);
  const px = (v) => Math.round(v * dpi / UNIT);
  el('selMm').textContent = `${mm(u.w)} × ${mm(u.h)} mm`;
  el('selPx').textContent = `${px(u.w)} × ${px(u.h)} px at ${dpi} dpi`;
  el('selPos').textContent = `${mm(u.x)}, ${mm(u.y)} mm from top-left`;
  el('btnScanSel').disabled = state.busy;
  el('btnPreviewSel').disabled = state.busy;
  el('btnClear').disabled = false;
}

// Mouse position in canvas pixels.
function canvasPos(ev) {
  const c = el('canvas');
  const r = c.getBoundingClientRect();
  const p = ev.touches ? ev.touches[0] : ev;
  return {
    x: Math.max(0, Math.min(c.width, (p.clientX - r.left) * c.width / r.width)),
    y: Math.max(0, Math.min(c.height, (p.clientY - r.top) * c.height / r.height)),
  };
}

// Crop handlers are bound once: the canvas element is never recreated, which is
// what keeps the selection alive across scans.
(function bindCrop() {
  const c = el('canvas');
  const start = (ev) => {
    if (state.busy || !state.preview || state.view !== 'preview') return;
    ev.preventDefault();
    state.drag = canvasPos(ev);
    state.sel = null;
    updateReadout();
  };
  const move = (ev) => {
    if (!state.drag) return;
    ev.preventDefault();
    const p = canvasPos(ev);
    state.sel = {
      x: Math.min(state.drag.x, p.x), y: Math.min(state.drag.y, p.y),
      w: Math.abs(p.x - state.drag.x), h: Math.abs(p.y - state.drag.y),
    };
    drawStage();
    updateReadout();
  };
  const end = () => {
    if (!state.drag) return;
    state.drag = null;
    // A stray click is not a selection.
    if (state.sel && (state.sel.w < CONFIG.minDragPx || state.sel.h < CONFIG.minDragPx)) {
      state.sel = null;
    }
    drawStage();
    updateReadout();
    setView('preview');
    // Only the finished rectangle is shared: the other screens want the crop,
    // not every intermediate frame of the drag.
    shareSelection();
  };
  c.addEventListener('mousedown', start);
  window.addEventListener('mousemove', move);
  window.addEventListener('mouseup', end);
  c.addEventListener('touchstart', start, { passive: false });
  c.addEventListener('touchmove', move, { passive: false });
  c.addEventListener('touchend', end);
})();

// ---------- the shared state ----------

function setLive(live) {
  el('conn').textContent = live ? 'live' : 'reconnecting…';
  el('conn').classList.toggle('muted', live);
}

function render(snap) {
  // A late frame from a dropped stream must not undo a newer one.
  if (snap.rev <= state.rev) return;
  const wasBusy = state.busy;
  state.rev = snap.rev;
  state.snap = snap;

  showError(snap.error || '');
  syncPreview(snap);
  syncResult(snap);
  applySharedSelection();
  drawStage();
  setBusy(snap.busy);
  updateReadout();
  refreshViewButtons();

  if (snap.busy) {
    el('bar').style.width = (snap.percent || 0).toFixed(1) + '%';
    const mb = (v) => (v / (1024 * 1024)).toFixed(1);
    el('progressText').textContent = snap.total > 0
      ? `${snap.stage} - ${mb(snap.done)} / ${mb(snap.total)} MB (${(snap.percent || 0).toFixed(0)}%)`
      : (snap.stage || 'working…');
  }
  if (snap.last) {
    const secs = (snap.last.elapsedMs / 1000).toFixed(1);
    el('lastRun').textContent =
      `${snap.last.dpi} dpi · ${snap.last.fullW}×${snap.last.fullH} px · ${secs}s`;
  }
  // The device line is worth re-reading once the scanner is free again.
  if (wasBusy && !snap.busy) refreshStatus();
}

function syncPreview(snap) {
  const p = snap.preview;
  if (!p) { state.preview = null; return; }
  if (state.preview && state.preview.id === p.id) return;
  if (state.loadingPreview === p.id) return;

  state.loadingPreview = p.id;
  const img = new Image();
  img.onload = () => {
    state.loadingPreview = null;
    // Another preview may have been taken while this one was loading.
    const cur = state.snap && state.snap.preview;
    if (!cur || cur.id !== p.id) { if (cur) syncPreview(state.snap); return; }
    state.preview = {
      id: p.id, img, area: p.area, unitsPerPx: p.area.w / img.naturalWidth,
    };
    const c = el('canvas');
    c.width = img.naturalWidth;
    c.height = img.naturalHeight;
    applySharedSelection();
    drawStage();
    updateReadout();
    refreshViewButtons();
    setView('preview');
  };
  img.onerror = () => { state.loadingPreview = null; };
  img.src = '/api/image/' + encodeURIComponent(p.id);
}

function syncResult(snap) {
  const res = snap.result;
  if (!res) { state.resultId = null; return; }
  if (state.resultId === res.id) return;
  state.resultId = res.id;

  const shown = '/api/image/' + encodeURIComponent(res.id);
  const href = res.savedName ? '/api/file/' + encodeURIComponent(res.savedName) : shown;
  el('resultImg').src = shown;
  el('thumb').src = shown;
  el('thumbLink').href = href;
  el('thumbLink').hidden = false;

  const secs = (res.elapsedMs / 1000).toFixed(1);
  el('resultBox').innerHTML = `Scanned <strong>${res.fullW}×${res.fullH}</strong> px in ${secs}s` +
    (res.savedPath ? `<br><span class="muted">saved to</span><br><code>${res.savedPath}</code>` : '') +
    (res.fullW > res.w
      ? '<br><span class="muted">shown scaled down; the saved file is full size</span>'
      : '');

  const dl = el('download');
  dl.href = href;
  dl.download = res.savedName || 'cx4300-scan.png';
  dl.hidden = false;
}

// The event stream is the only source of state. If it drops, the reconnect
// brings a whole fresh snapshot with it, so nothing has to be replayed - and
// /api/status is called on the way, which is what sends an expired session back
// to the login page.
function connect() {
  const es = new EventSource('/api/events');
  es.onopen = () => setLive(true);
  es.onmessage = (ev) => {
    setLive(true);
    try { render(JSON.parse(ev.data)); } catch (e) { /* not a snapshot */ }
  };
  es.onerror = () => {
    es.close();
    setLive(false);
    refreshStatus();
    setTimeout(connect, CONFIG.reconnectMs);
  };
}

// ---------- scanning ----------

async function requestScan(url, body) {
  showError('');
  try {
    const r = await api(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body || {}),
    });
    if (!r.ok) {
      let msg = `HTTP ${r.status}`;
      try { msg = (await r.json()).error || msg; } catch (e) {}
      showError(msg);
    }
    // Everything else - progress, the image, any failure - arrives on the
    // event stream, here and on every other device.
  } catch (e) {
    showError(String(e));
  }
}

// ---------- wiring ----------

const previewDpi = () => parseInt(el('previewDpi').value, 10) || CONFIG.previewDpi;
const scanDpi = () => parseInt(el('dpi').value, 10) || CONFIG.scanDpi;

el('btnPreview').addEventListener('click', () =>
  requestScan('/api/preview', { dpi: previewDpi(), full: true }));

el('btnPreviewSel').addEventListener('click', () => {
  const u = selectionUnits();
  if (!u) return;
  requestScan('/api/preview', { dpi: previewDpi(), ...u });
});

el('btnScanFull').addEventListener('click', () =>
  requestScan('/api/scan', { dpi: scanDpi(), full: true }));

el('btnScanSel').addEventListener('click', () => {
  const u = selectionUnits();
  if (!u) return;
  requestScan('/api/scan', { dpi: scanDpi(), ...u });
});

el('btnClear').addEventListener('click', () => {
  state.sel = null; drawStage(); updateReadout(); setView('preview');
  shareSelection();
});

el('dpi').addEventListener('change', updateReadout);
el('viewPreview').addEventListener('click', () => setView('preview'));
el('viewResult').addEventListener('click', () => setView('result'));

el('btnReset').addEventListener('click', async () => {
  setBusy(true);
  try {
    const r = await api('/api/reset', { method: 'POST' });
    if (!r.ok) showError((await r.json()).error || `HTTP ${r.status}`);
  } catch (e) { showError(String(e)); }
  finally { setBusy(false); refreshStatus(); }
});

el('btnLogout').addEventListener('click', async () => {
  try { await fetch('/api/logout', { method: 'POST' }); } catch (e) { /* going anyway */ }
  window.location.replace('/login');
});

refreshViewButtons();
refreshStatus();
connect();
