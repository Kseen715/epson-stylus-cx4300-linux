'use strict';

// The device measures the platen in 1/600 inch units regardless of resolution,
// so selections are converted into those units before being sent.
const UNIT = 600;
const MM_PER_INCH = 25.4;

const el = (id) => document.getElementById(id);
const state = {
  img: null,          // the image currently on the canvas
  // The area that image covers, in 1/600 inch, and how many of those units one
  // canvas pixel represents. Taken from the response headers, so the mapping
  // stays correct however much the server shrank the picture.
  area: { x: 0, y: 0, w: 5100, h: 7020 },
  unitsPerPx: 8,
  sel: null,          // {x, y, w, h} in canvas pixels
  drag: null,
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
    const opts = s.dpiOptions || [75, 150, 300, 600];
    fillSelect(el('previewDpi'), opts, s.previewDpi || 75);
    fillSelect(el('dpi'), opts, 150);
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

function drawStage() {
  const c = el('canvas');
  if (!c || !state.img) return;
  const ctx = c.getContext('2d');
  ctx.drawImage(state.img, 0, 0);
  if (!state.sel) return;

  const { x, y, w, h } = state.sel;
  // Dim everything outside the selection so the crop reads clearly.
  ctx.fillStyle = 'rgba(0,0,0,0.42)';
  ctx.fillRect(0, 0, c.width, y);
  ctx.fillRect(0, y + h, c.width, c.height - y - h);
  ctx.fillRect(0, y, x, h);
  ctx.fillRect(x + w, y, c.width - x - w, h);

  ctx.strokeStyle = '#2f6fed';
  ctx.lineWidth = Math.max(2, c.width / 500);
  ctx.strokeRect(x, y, w, h);
}

// Selection in device units, derived from the previewed area rather than from
// an assumed preview resolution.
function selectionUnits() {
  if (!state.sel) return null;
  const k = state.unitsPerPx;
  return {
    x: Math.round(state.area.x + state.sel.x * k),
    y: Math.round(state.area.y + state.sel.y * k),
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
  const dpi = parseInt(el('dpi').value, 10) || 150;
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
function imgPos(ev, c) {
  const r = c.getBoundingClientRect();
  const p = ev.touches ? ev.touches[0] : ev;
  return {
    x: Math.max(0, Math.min(c.width, (p.clientX - r.left) * c.width / r.width)),
    y: Math.max(0, Math.min(c.height, (p.clientY - r.top) * c.height / r.height)),
  };
}

function installCanvas(img) {
  const stage = el('stage');
  stage.innerHTML = '<canvas id="canvas"></canvas>';
  const c = el('canvas');
  c.width = img.naturalWidth;
  c.height = img.naturalHeight;
  state.img = img;
  drawStage();

  const start = (ev) => {
    if (state.busy) return;
    ev.preventDefault();
    state.drag = imgPos(ev, c);
    state.sel = null;
    updateReadout();
  };
  const move = (ev) => {
    if (!state.drag) return;
    ev.preventDefault();
    const p = imgPos(ev, c);
    state.sel = {
      x: Math.min(state.drag.x, p.x), y: Math.min(state.drag.y, p.y),
      w: Math.abs(p.x - state.drag.x), h: Math.abs(p.y - state.drag.y),
    };
    drawStage();
    updateReadout();
  };
  const end = () => {
    state.drag = null;
    // A stray click is not a selection.
    if (state.sel && (state.sel.w < 4 || state.sel.h < 4)) state.sel = null;
    drawStage();
    updateReadout();
  };

  c.addEventListener('mousedown', start);
  window.addEventListener('mousemove', move);
  window.addEventListener('mouseup', end);
  c.addEventListener('touchstart', start, { passive: false });
  c.addEventListener('touchmove', move, { passive: false });
  c.addEventListener('touchend', end);
}

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
  }, 500);
}
function stopPolling() { if (pollTimer) { clearInterval(pollTimer); pollTimer = null; } }

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
    state.area = {
      x: num('X-Area-X', 0), y: num('X-Area-Y', 0),
      w: num('X-Area-W', 5100), h: num('X-Area-H', 7020),
    };
    state.unitsPerPx = state.area.w / img.naturalWidth;

    const dpi = num('X-Scan-DPI', 0);
    const secs = (num('X-Elapsed-Ms', 0) / 1000).toFixed(1);
    const fw = num('X-Full-Width', img.naturalWidth);
    const fh = num('X-Full-Height', img.naturalHeight);
    el('lastRun').textContent = `${dpi} dpi · ${fw}×${fh} px · ${secs}s`;

    state.sel = null;
    installCanvas(img);
    updateReadout();

    if (asPreview) {
      el('resultBox').innerHTML = 'Preview only — nothing saved. ' +
        'Drag on the image to choose an area.';
      el('download').hidden = true;
    } else {
      const name = r.headers.get('X-Saved-Name');
      const path = r.headers.get('X-Saved-Path');
      el('resultBox').innerHTML = `Scanned <strong>${fw}×${fh}</strong> px in ${secs}s` +
        (path ? `<br><span class="muted">saved to</span><br><code>${path}</code>` : '') +
        (fw > img.naturalWidth ? `<br><span class="muted">shown here scaled down; the saved file is full size</span>` : '');
      const dl = el('download');
      if (name) {
        // Serve the full-resolution file from disk rather than the shrunk copy
        // the page is displaying.
        dl.href = '/api/file/' + encodeURIComponent(name);
        dl.download = name;
        dl.hidden = false;
      } else {
        dl.href = objUrl;
        dl.download = 'cx4300-scan.png';
        dl.hidden = false;
      }
    }
  } catch (e) {
    showError(String(e));
  } finally {
    stopPolling();
    setBusy(false);
    refreshStatus();
  }
}

const previewDpi = () => parseInt(el('previewDpi').value, 10) || 75;
const scanDpi = () => parseInt(el('dpi').value, 10) || 150;

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
  state.sel = null; drawStage(); updateReadout();
});

el('dpi').addEventListener('change', updateReadout);

el('btnReset').addEventListener('click', async () => {
  setBusy(true);
  try {
    const r = await fetch('/api/reset', { method: 'POST' });
    if (!r.ok) showError((await r.json()).error || `HTTP ${r.status}`);
  } catch (e) { showError(String(e)); }
  finally { setBusy(false); refreshStatus(); }
});

refreshStatus();
