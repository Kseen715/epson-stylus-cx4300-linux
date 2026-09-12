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

  progressPollMs: 500,                // how often to poll /api/progress during a scan
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
// Platen size in those units (8.5 x 11.7 inch); the server confirms it in
// /api/status and in the X-Area-* headers of every response.
const BED = { w: 5100, h: 7020 };

const el = (id) => document.getElementById(id);

const state = {
  // The preview and the crop drawn on it. A scan never touches these, so the
  // selection survives and can be adjusted and re-scanned.
  preview: null,   // {img, area:{x,y,w,h}, unitsPerPx}
  sel: null,       // {x, y, w, h} in canvas pixels
  drag: null,
  result: null,    // {url, name, w, h}
  view: 'preview',
  busy: false,
};

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
    const s = await (await fetch('/api/status')).json();
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
    showError(s.error || '');
  } catch (e) {
    el('status').textContent = 'cannot reach the escan server';
  }
}

// ---------- view switching ----------

function setView(name) {
  if (name === 'preview' && !state.preview) return;
  if (name === 'result' && !state.result) return;
  state.view = name;
  const isPreview = name === 'preview';
  el('canvas').hidden = !isPreview;
  el('resultImg').hidden = isPreview;
  el('placeholder').hidden = !!(state.preview || state.result);
  el('viewPreview').classList.toggle('active', isPreview);
  el('viewResult').classList.toggle('active', !isPreview);
  el('viewNote').textContent = isPreview
    ? (state.sel ? 'drag to adjust the area' : 'drag to choose an area')
    : 'the preview and your selection are kept';
}

function refreshViewButtons() {
  el('viewPreview').disabled = !state.preview;
  el('viewResult').disabled = !state.result;
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
// an assumed preview resolution.
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

function updateReadout() {
  const u = selectionUnits();
  if (!u) {
    el('selMm').textContent = 'whole bed';
    el('selPx').textContent = '—';
    el('selPos').textContent = '—';
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
  };
  c.addEventListener('mousedown', start);
  window.addEventListener('mousemove', move);
  window.addEventListener('mouseup', end);
  c.addEventListener('touchstart', start, { passive: false });
  c.addEventListener('touchmove', move, { passive: false });
  c.addEventListener('touchend', end);
})();

// ---------- progress ----------

let pollTimer = null;
function startPolling() {
  stopPolling();
  pollTimer = setInterval(async () => {
    try {
      const p = await (await fetch('/api/progress')).json();
      el('bar').style.width = (p.percent || 0).toFixed(1) + '%';
      const mb = (v) => (v / (1024 * 1024)).toFixed(1);
      el('progressText').textContent = p.total > 0
        ? `${p.stage} — ${mb(p.done)} / ${mb(p.total)} MB (${(p.percent || 0).toFixed(0)}%)`
        : (p.stage || 'working…');
    } catch (e) { /* transient; the scan request carries the real error */ }
  }, CONFIG.progressPollMs);
}
function stopPolling() { if (pollTimer) { clearInterval(pollTimer); pollTimer = null; } }

// ---------- scanning ----------

async function requestScan(url, body, { asPreview }) {
  showError('');
  setBusy(true);
  startPolling();
  try {
    const r = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body || {}),
    });
    if (!r.ok) {
      let msg = `HTTP ${r.status}`;
      try { msg = (await r.json()).error || msg; } catch (e) {}
      showError(msg);
      return;
    }
    const blob = await r.blob();
    const objUrl = URL.createObjectURL(blob);
    const img = new Image();
    await new Promise((res, rej) => { img.onload = res; img.onerror = rej; img.src = objUrl; });

    const num = (h, d) => parseInt(r.headers.get(h), 10) || d;
    const dpi = num('X-Scan-DPI', 0);
    const secs = (num('X-Elapsed-Ms', 0) / 1000).toFixed(1);
    const fw = num('X-Full-Width', img.naturalWidth);
    const fh = num('X-Full-Height', img.naturalHeight);
    el('lastRun').textContent = `${dpi} dpi · ${fw}×${fh} px · ${secs}s`;

    if (asPreview) {
      // A new preview replaces the old one, so the stale selection goes too.
      if (state.preview) URL.revokeObjectURL(state.preview.url);
      state.preview = {
        img, url: objUrl,
        area: {
          x: num('X-Area-X', 0), y: num('X-Area-Y', 0),
          w: num('X-Area-W', BED.w), h: num('X-Area-H', BED.h),
        },
        unitsPerPx: num('X-Area-W', BED.w) / img.naturalWidth,
      };
      state.sel = null;
      const c = el('canvas');
      c.width = img.naturalWidth;
      c.height = img.naturalHeight;
      drawStage();
      updateReadout();
      refreshViewButtons();
      setView('preview');
      el('resultBox').innerHTML = state.result
        ? el('resultBox').innerHTML
        : 'Preview only — nothing saved. Drag on the image to choose an area.';
    } else {
      // Keep the preview and its selection; show the scan alongside it.
      if (state.result) URL.revokeObjectURL(state.result.url);
      const name = r.headers.get('X-Saved-Name');
      const path = r.headers.get('X-Saved-Path');
      const href = name ? '/api/file/' + encodeURIComponent(name) : objUrl;
      state.result = { url: objUrl, name, w: fw, h: fh };

      el('resultImg').src = objUrl;
      el('thumb').src = objUrl;
      el('thumbLink').href = href;
      el('thumbLink').hidden = false;

      el('resultBox').innerHTML = `Scanned <strong>${fw}×${fh}</strong> px in ${secs}s` +
        (path ? `<br><span class="muted">saved to</span><br><code>${path}</code>` : '') +
        (fw > img.naturalWidth
          ? '<br><span class="muted">shown scaled down; the saved file is full size</span>'
          : '');

      const dl = el('download');
      dl.href = href;
      dl.download = name || 'cx4300-scan.png';
      dl.hidden = false;

      refreshViewButtons();
      // Stay on the preview so the selection can be adjusted and re-scanned;
      // the result is one click away and thumbnailed in this panel.
      setView('preview');
    }
  } catch (e) {
    showError(String(e));
  } finally {
    stopPolling();
    setBusy(false);
    refreshStatus();
  }
}

// ---------- wiring ----------

const previewDpi = () => parseInt(el('previewDpi').value, 10) || CONFIG.previewDpi;
const scanDpi = () => parseInt(el('dpi').value, 10) || CONFIG.scanDpi;

el('btnPreview').addEventListener('click', () =>
  requestScan('/api/preview', { dpi: previewDpi(), full: true }, { asPreview: true }));

el('btnPreviewSel').addEventListener('click', () => {
  const u = selectionUnits();
  if (!u) return;
  requestScan('/api/preview', { dpi: previewDpi(), ...u }, { asPreview: true });
});

el('btnScanFull').addEventListener('click', () =>
  requestScan('/api/scan', { dpi: scanDpi(), full: true }, { asPreview: false }));

el('btnScanSel').addEventListener('click', () => {
  const u = selectionUnits();
  if (!u) return;
  requestScan('/api/scan', { dpi: scanDpi(), ...u }, { asPreview: false });
});

el('btnClear').addEventListener('click', () => {
  state.sel = null; drawStage(); updateReadout(); setView('preview');
});

el('dpi').addEventListener('change', updateReadout);
el('viewPreview').addEventListener('click', () => setView('preview'));
el('viewResult').addEventListener('click', () => setView('result'));

el('btnReset').addEventListener('click', async () => {
  setBusy(true);
  try {
    const r = await fetch('/api/reset', { method: 'POST' });
    if (!r.ok) showError((await r.json()).error || `HTTP ${r.status}`);
  } catch (e) { showError(String(e)); }
  finally { setBusy(false); refreshStatus(); }
});

refreshViewButtons();
refreshStatus();
