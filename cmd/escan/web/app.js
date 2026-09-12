'use strict';

// The device measures the platen in 1/600 inch units regardless of resolution,
// so selections are converted into those units before being sent.
const UNIT = 600;
const MM_PER_INCH = 25.4;

const el = (id) => document.getElementById(id);
const state = {
  previewDpi: 75,
  bed: { w: 5100, h: 7020 },   // 1/600 inch, replaced by /api/status
  img: null,                   // the preview Image
  sel: null,                   // {x, y, w, h} in preview image pixels
  drag: null,
  busy: false,
};

function unitsFromPreviewPx(px) { return Math.round(px * UNIT / state.previewDpi); }

function showError(msg) {
  const box = el('error');
  if (!msg) { box.hidden = true; return; }
  box.hidden = false;
  box.textContent = msg;
}

function setBusy(busy) {
  state.busy = busy;
  for (const id of ['btnPreview', 'btnScanFull', 'dpi', 'btnReset']) el(id).disabled = busy;
  el('btnScanSel').disabled = busy || !state.sel;
  el('progressWrap').hidden = !busy;
  if (!busy) { el('bar').style.width = '0'; el('progressText').textContent = ''; }
}

async function refreshStatus() {
  try {
    const r = await fetch('/api/status');
    const s = await r.json();
    state.previewDpi = s.previewDpi || 75;
    el('previewDpi').textContent = state.previewDpi;
    el('outDir').textContent = s.outDir || '.';
    state.bed = {
      w: Math.round((s.bedWidthMm / MM_PER_INCH) * UNIT),
      h: Math.round((s.bedHeightMm / MM_PER_INCH) * UNIT),
    };

    const sel = el('dpi');
    if (!sel.options.length) {
      for (const d of (s.dpiOptions || [75, 150, 300, 600])) {
        const o = document.createElement('option');
        o.value = d; o.textContent = d + ' dpi';
        if (d === 150) o.selected = true;
        sel.appendChild(o);
      }
    }

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

function updateReadout() {
  if (!state.sel) {
    el('selMm').textContent = 'whole bed';
    el('selPx').textContent = '—';
    el('selPos').textContent = '—';
    el('btnScanSel').disabled = true;
    el('btnClear').disabled = true;
    return;
  }
  const dpi = parseInt(el('dpi').value, 10) || 150;
  const u = {
    x: unitsFromPreviewPx(state.sel.x), y: unitsFromPreviewPx(state.sel.y),
    w: unitsFromPreviewPx(state.sel.w), h: unitsFromPreviewPx(state.sel.h),
  };
  const mm = (v) => (v / UNIT * MM_PER_INCH).toFixed(1);
  el('selMm').textContent = `${mm(u.w)} × ${mm(u.h)} mm`;
  el('selPx').textContent = `${Math.round(u.w * dpi / UNIT)} × ${Math.round(u.h * dpi / UNIT)} px at ${dpi} dpi`;
  el('selPos').textContent = `${mm(u.x)}, ${mm(u.y)} mm from top-left`;
  el('btnScanSel').disabled = state.busy;
  el('btnClear').disabled = false;
}

// Mouse position in preview-image pixels, which is what the canvas is sized in.
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
    const url2 = URL.createObjectURL(blob);
    const img = new Image();
    await new Promise((res, rej) => { img.onload = res; img.onerror = rej; img.src = url2; });

    if (asPreview) {
      state.sel = null;
      installCanvas(img);
      updateReadout();
    } else {
      installCanvas(img);   // show the result in the same stage
      state.sel = null;
      updateReadout();
      const saved = r.headers.get('X-Saved-Path');
      const w = r.headers.get('X-Image-Width'), h = r.headers.get('X-Image-Height');
      el('resultBox').innerHTML = `Scanned <strong>${w}×${h}</strong> px` +
        (saved ? `<br><span class="muted">saved to</span><br><code>${saved}</code>` : '');
      const dl = el('download');
      dl.href = url2;
      dl.download = (saved ? saved.split(/[\\/]/).pop() : 'cx4300-scan.png');
      dl.hidden = false;
    }
  } catch (e) {
    showError(String(e));
  } finally {
    stopPolling();
    setBusy(false);
    refreshStatus();
  }
}

el('btnPreview').addEventListener('click', () =>
  requestScan('/api/preview', {}, { asPreview: true }));

el('btnScanFull').addEventListener('click', () =>
  requestScan('/api/scan', { dpi: parseInt(el('dpi').value, 10), full: true }, { asPreview: false }));

el('btnScanSel').addEventListener('click', () => {
  if (!state.sel) return;
  requestScan('/api/scan', {
    dpi: parseInt(el('dpi').value, 10),
    x: unitsFromPreviewPx(state.sel.x), y: unitsFromPreviewPx(state.sel.y),
    w: unitsFromPreviewPx(state.sel.w), h: unitsFromPreviewPx(state.sel.h),
  }, { asPreview: false });
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
