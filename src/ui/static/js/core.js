// core.js — DOM helpers, i18n, API, toast, modal, helpers, sort
'use strict';


const $ = (sel, ctx) => (ctx || document).querySelector(sel);
const $$ = (sel, ctx) => [...(ctx || document).querySelectorAll(sel)];
const content = $('#content');

// animateCounter tweens an element's text from its currently displayed
// number to `target` over ~600 ms using easeOutQuad. Reads the prior
// value from data-value on the element so consecutive renders animate
// from the last shown number (not a hard snap back to 0). Skips the
// animation entirely if prefers-reduced-motion is set — accessibility
// trumps the aesthetic. `format(n)` lets the caller render bytes,
// percentages, plain integers, etc.
function animateCounter(el, target, format) {
  format = format || (n => String(Math.round(n)));
  if (!el) return;
  const reduce = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  const from = parseFloat(el.getAttribute('data-value') || '0');
  const to = Number.isFinite(target) ? target : 0;
  el.setAttribute('data-value', String(to));
  if (reduce || from === to) { el.textContent = format(to); return; }
  const start = performance.now();
  const dur = 600;
  function tick(now) {
    const t = Math.min(1, (now - start) / dur);
    const eased = 1 - (1 - t) * (1 - t); // easeOutQuad
    const cur = from + (to - from) * eased;
    el.textContent = format(cur);
    if (t < 1) requestAnimationFrame(tick);
    else el.textContent = format(to);
  }
  requestAnimationFrame(tick);
}

// --- i18n ---
// Adding a language: drop a JSON file in /static/i18n/<code>.json with the
// same key shape as en.json, and register the code in `availableLangs`.
// That is the entire contract — no code changes elsewhere.
const availableLangs = ['en', 'de', 'fr'];
const fallbackLang = 'en';
let currentLang = fallbackLang;
const dictionaries = {};

function detectLang() {
  const stored = localStorage.getItem('lang');
  if (stored && availableLangs.includes(stored)) return stored;
  const nav = (navigator.language || '').slice(0, 2).toLowerCase();
  if (availableLangs.includes(nav)) return nav;
  return fallbackLang;
}

async function loadLang(code) {
  if (dictionaries[code]) return dictionaries[code];
  try {
    const resp = await fetch('/static/i18n/' + code + '.json');
    if (!resp.ok) throw new Error('HTTP ' + resp.status);
    dictionaries[code] = await resp.json();
    return dictionaries[code];
  } catch (e) {
    console.warn('i18n: failed to load ' + code, e);
    if (code !== fallbackLang) return loadLang(fallbackLang);
    dictionaries[fallbackLang] = {};
    return dictionaries[fallbackLang];
  }
}

// tr('nav.dashboard') → "Dashboard". Named `tr` (not `t`) because `t` is
// already used as a parameter name for `target` in dozens of lambdas in
// this file. Falls back to en, then to the key itself so a missing
// translation is visible. Optional vars: tr('toast.exportFail', {error: e.message}).
function tr(key, vars) {
  const parts = key.split('.');
  const lookup = (dict) => {
    let v = dict;
    for (const p of parts) {
      if (v == null || typeof v !== 'object') return undefined;
      v = v[p];
    }
    return typeof v === 'string' ? v : undefined;
  };
  let s = lookup(dictionaries[currentLang]);
  if (s == null) s = lookup(dictionaries[fallbackLang]);
  if (s == null) s = key;
  if (vars) {
    s = s.replace(/\{(\w+)\}/g, (_, k) => (vars[k] != null ? vars[k] : '{' + k + '}'));
  }
  return s;
}

window.tr = tr; // expose for inline handlers and easy console debugging

async function setLang(code) {
  if (!availableLangs.includes(code)) code = fallbackLang;
  await loadLang(code);
  currentLang = code;
  localStorage.setItem('lang', code);
  document.documentElement.lang = code;
  applyStaticTranslations();
  if (typeof window._renderPage === 'function') window._renderPage(window._currentPage(), false);
}
window.setLang = setLang;

// Translates anything in the static shell (sidebar, footer) that carries
// a data-i18n attribute. Page-rendered content uses t() inline at render
// time, so it picks up the new language automatically when renderPage()
// re-runs after setLang.
function applyStaticTranslations() {
  $$('[data-i18n]').forEach(el => {
    const key = el.dataset.i18n;
    el.textContent = tr(key);
  });
  $$('[data-i18n-title]').forEach(el => {
    el.title = tr(el.dataset.i18nTitle);
  });
  // Refresh the language picker label.
  const picker = $('#langPicker');
  if (picker) picker.value = currentLang;
}

// --- API helpers ---
async function api(path, opts = {}) {
  // Only set Content-Type on requests that carry a body. Sending it on
  // bodyless GETs/DELETEs is harmless for same-origin but can trigger a
  // CORS preflight (OPTIONS) when an auth proxy sits in front — which
  // some proxies don't handle, breaking the request with a 405.
  const headers = { ...opts.headers };
  if (opts.body) {
    headers['Content-Type'] = headers['Content-Type'] || 'application/json';
  }
  const resp = await fetch(path, { ...opts, headers });
  let data = {};
  try { data = await resp.json(); } catch (_) { /* empty body / non-JSON */ }
  if (!resp.ok) {
    const err = new Error(data.message || resp.statusText || 'request failed');
    // Attach machine-readable error metadata so handlers can pick toast
    // severity, friendly messages, or retry behaviour without parsing
    // the human-readable string.
    err.code = data.code || '';
    err.status = resp.status;
    throw err;
  }
  return data;
}

// Map an API error to a (message, toast-type) pair. Used by call sites
// that want code-driven presentation; sites that don't care can keep
// using toast(err.message, 'error') and behave exactly as before.
const apiErrorMessages = {
  validation:           e => e.message,
  bad_request:          e => e.message,
  conflict:             e => e.message,
  forbidden:            e => e.message || 'Not allowed',
  not_found:            e => e.message || 'Not found',
  method_not_allowed:   e => e.message || 'Operation not allowed',
  server_error:         e => 'Server error: ' + (e.message || 'try again'),
};
function apiErrorToast(err, fallback) {
  const msg = (apiErrorMessages[err.code] || (() => err.message || fallback || 'Request failed'))(err);
  // 5xx (and any unknown ≥500 status) is a server problem — louder.
  // Everything else is "you tried something that didn't work" — same
  // visual weight, but operators learn the distinction over time as
  // codes become actionable in the UI.
  const type = (err.status >= 500 || err.code === 'server_error') ? 'error' : 'error';
  toast(msg, type);
}

// --- Toast ---
function toast(msg, type = 'info') {
  const el = document.createElement('div');
  el.className = 'toast toast-' + type;
  el.textContent = msg;
  $('#toasts').appendChild(el);
  setTimeout(() => el.remove(), 4000);
}

// --- Modal ---
// Tracks the element that opened the modal so we can return focus to it on
// close. Without this, screen-reader users (and keyboard users in general)
// land at the top of the document after every dialog interaction.
let modalReturnFocus = null;
window.openModal = function(title, bodyHTML) {
  modalReturnFocus = document.activeElement;
  $('#modal-title').textContent = title;
  $('#modal-body').innerHTML = bodyHTML;
  $('#modal-overlay').classList.remove('hidden');
  // Focus the first interactive element inside the body. Falls back to the
  // close button so the dialog is always reachable without a mouse.
  const target = $('#modal-body').querySelector(
    'input:not([type=hidden]), select, textarea, button, [tabindex]:not([tabindex="-1"])'
  ) || $('#modal-overlay .modal-close');
  if (target) setTimeout(() => target.focus(), 0);
};
window.closeModal = function() {
  $('#modal-overlay').classList.add('hidden');
  if (modalReturnFocus && typeof modalReturnFocus.focus === 'function') {
    modalReturnFocus.focus();
  }
  modalReturnFocus = null;
};
$('#modal-overlay').addEventListener('click', e => {
  if (e.target === $('#modal-overlay')) closeModal();
});
// Esc closes any visible modal. The tooltip on the close button already
// promises this — without the handler the promise was a lie.
document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && !$('#modal-overlay').classList.contains('hidden')) {
    closeModal();
  }
});

// --- Helpers ---
function fmtCount(n) { return (n != null) ? n.toLocaleString() : '—'; }
function humanBytes(n) {
  if (!n || n === 0) return '0 B';
  const units = ['B','KiB','MiB','GiB','TiB'];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(1)) + ' ' + units[i];
}
function timeAgo(ts) {
  if (!ts) return tr('time.never');
  const d = new Date(ts.replace(/(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z/,
    '$1-$2-$3T$4:$5:$6Z'));
  if (isNaN(d)) return ts;
  const diff = (Date.now() - d.getTime()) / 1000;
  if (diff < 60) return tr('time.justNow');
  if (diff < 3600) return tr('time.minutesAgo', {n: Math.floor(diff/60)});
  if (diff < 86400) return tr('time.hoursAgo', {n: Math.floor(diff/3600)});
  return tr('time.daysAgo', {n: Math.floor(diff/86400)});
}
function escHTML(s) {
  const d = document.createElement('div');
  d.textContent = s || '';
  return d.innerHTML;
}
// escAttr is for HTML attribute *values*. textContent → innerHTML escapes
// `<`, `>`, `&` but NOT `"` or `'` — so escHTML inside a quoted attribute
// is XSS-vulnerable when the value can contain a quote that matches the
// attribute's quoting style. escAttr handles both quoting styles plus
// backtick (template-string boundary).
function escAttr(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;')
    .replace(/`/g, '&#96;');
}
// escJS is for JavaScript string literals embedded inside HTML attributes
// (e.g. onclick="foo('${escJS(name)}')"). escHTML/escAttr produce HTML
// entities, which the JS parser does NOT decode — wrong escape, wrong
// context. We unicode-escape every char that could break out of either
// the JS string or the surrounding HTML attribute. The result is safe
// regardless of whether the attribute uses ' or " quoting.
function escJS(s) {
  // Includes U+2028 LINE SEPARATOR and U+2029 PARAGRAPH SEPARATOR — older
  // engines treat these as line terminators inside string literals.
  return String(s == null ? '' : s).replace(
    /[\\'"<>&\n\r\u2028\u2029`]/g,
    c => '\\u' + c.charCodeAt(0).toString(16).padStart(4, '0')
  );
}
// Render a Failed badge with phase suffix and full error in tooltip.
// Matches the legacy templates' "✗ failed (phase)" + title=error pattern.
function failedBadge(m) {
  const phase = m && m.phase ? ' (' + escHTML(m.phase) + ')' : '';
  const tip = m && (m.error || m.phase) ? escAttr((m.phase ? m.phase + ': ' : '') + (m.error || '')) : '';
  return `<span class="badge badge-failed"${tip ? ' title="' + tip + '"' : ''}>Failed${phase}</span>`;
}

// verificationRow renders the per-source restore-verification status
// line in the source-list cards. Configured-but-not-yet-run shows just
// the mode; if the latest meta has a restoreVerification block we add
// the verdict badge + relative timestamp. Returns "" when verification
// is off or unset so the row is hidden entirely.
function verificationRow(t) {
  const cfgMode = (t.verification && t.verification.mode) || '';
  if (!cfgMode || cfgMode === 'off') return '';
  const rv = t.Latest && t.Latest.restoreVerification;
  let status = `<span class="badge badge-pending" style="font-size:11px">${tr('verification.verdictLabel.configured')}</span>`;
  if (rv) {
    const verdict = rv.verdict || '';
    let cls = 'badge-pending';
    if (verdict === 'match') cls = 'badge-ok';
    else if (verdict === 'mismatch') cls = 'badge-failed';
    const ts = rv.completedAt ? timeAgo(rv.completedAt) : '—';
    const tip = rv.summary ? ' title="' + escAttr(rv.summary) + '"' : '';
    const verdictKey = 'verification.verdictLabel.' + verdict;
    const verdictText = tr(verdictKey) === verdictKey ? verdict : tr(verdictKey);
    status = `<span class="badge ${cls}" style="font-size:11px"${tip}>${escHTML(verdictText)} · ${ts}</span>`;
  }
  return `<div class="detail-row"><span class="key">${tr('verification.verifyOf', {mode: cfgMode})}</span><span class="val">${status}</span></div>`;
}
function truncate(s, n) {
  if (!s) return '';
  return s.length > n ? s.slice(0, n) + '…' : s;
}

function showLoading() {
  content.innerHTML = '<div class="empty-state"><div class="spinner"></div></div>';
}

// --- Sort state per list ---
// Default direction is "desc" because most lists naturally answer
// "what happened most recently?" — newest first.
const sortState = {
  dashboard:    { col: 'lastRun',   dir: 'desc' },
  sources:      { col: 'createdAt', dir: 'desc' },
  destinations: { col: 'createdAt', dir: 'desc' },
  jobs:         { col: 'startTime', dir: 'desc' },
  runs:         { col: 'timestamp', dir: 'desc' },
};
function cmp(a, b) {
  if (a == null && b == null) return 0;
  if (a == null) return 1;   // nulls last regardless of direction
  if (b == null) return -1;
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  return String(a).localeCompare(String(b));
}
function sortBy(arr, getter, dir) {
  const sorted = arr.slice();
  sorted.sort((x, y) => {
    const r = cmp(getter(x), getter(y));
    return dir === 'asc' ? r : -r;
  });
  return sorted;
}
function parseTsCompact(ts) {
  // 20060102T150405Z → epoch ms; null if not parseable
  if (!ts) return null;
  const m = String(ts).match(/^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$/);
  if (!m) {
    const t = Date.parse(ts);
    return isNaN(t) ? null : t;
  }
  return Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6]);
}
function parseTsRFC(ts) { const t = ts ? Date.parse(ts) : NaN; return isNaN(t) ? null : t; }
function sortIndicator(list, col) {
  const s = sortState[list];
  if (!s || s.col !== col) return '<span class="sort-ind">↕</span>';
  return '<span class="sort-ind active">' + (s.dir === 'asc' ? '▲' : '▼') + '</span>';
}
window.toggleSort = function(list, col) {
  const s = sortState[list];
  if (s.col === col) {
    s.dir = s.dir === 'asc' ? 'desc' : 'asc';
  } else {
    s.col = col;
    s.dir = 'desc';
  }
  window._renderPage(window._currentPage(), false);
};
window.setSort = function(list, col, dir) {
  sortState[list] = { col, dir: dir || sortState[list].dir };
  window._renderPage(window._currentPage(), false);
};
window.flipSortDir = function(list) {
  const s = sortState[list];
  s.dir = s.dir === 'asc' ? 'desc' : 'asc';
  window._renderPage(window._currentPage(), false);
};
function renderSortControl(list, options) {
  const s = sortState[list];
  const opts = options.map(([k, lbl]) =>
    `<option value="${k}" ${s.col === k ? 'selected' : ''}>${lbl}</option>`).join('');
  const arrow = s.dir === 'asc' ? '▲' : '▼';
  return `<div class="sort-control">
    <span class="sort-label">Sort:</span>
    <select onchange="setSort('${list}', this.value)">${opts}</select>
    <button class="btn btn-ghost btn-sm sort-dir" onclick="flipSortDir('${list}')" title="Toggle direction">${arrow}</button>
  </div>`;
}
