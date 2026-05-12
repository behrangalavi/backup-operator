// backup-operator SPA
(function() {
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
  if (typeof renderPage === 'function') renderPage(currentPage(), false);
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
  const resp = await fetch(path, {
    headers: { 'Content-Type': 'application/json', ...opts.headers },
    ...opts
  });
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

// --- SSE ---
// Maps each server-sent event type to the pages whose visible state
// actually depends on the changed resource. Events for pages the user
// is not currently viewing are dropped — every renderer re-fetches on
// navigation, so the user lands on fresh data the next time they visit.
// Without this map, every CRUD action on any object re-rendered the
// current page, even when it had nothing to do with the change (e.g.
// editing a destination re-rendered the Audit log).
const sseEventPages = {
  source_created:      ['dashboard', 'sources', 'audit'],
  source_updated:      ['dashboard', 'sources', 'target', 'audit'],
  source_deleted:      ['dashboard', 'sources', 'audit'],
  destination_created: ['dashboard', 'destinations', 'audit'],
  destination_updated: ['dashboard', 'destinations', 'audit'],
  destination_deleted: ['dashboard', 'destinations', 'audit'],
  backup_triggered:    ['dashboard', 'jobs', 'target', 'audit'],
  settings_updated:    ['settings', 'audit'],
  age_keys_updated:    ['age-keys', 'audit'],
  // Emitted by the server when a background storage probe finishes
  // refreshing the per-destination meta cache. The render path served
  // stale data instantly; this event closes the loop with fresh data.
  refresh:             ['dashboard', 'destinations', 'target'],
  // Emitted by the JobWatcher controller on every K8s Job state
  // transition in the backup namespace (create, pod-start, complete,
  // fail, delete). Dashboard + Jobs page + Target detail all care.
  job_state_change:    ['dashboard', 'jobs', 'target'],
  // Emitted by MetricsRefresher when a per-target latest meta.json
  // timestamp differs from the previous tick — i.e. a new backup has
  // landed on a destination. Repaints the same surfaces that consume
  // run history.
  meta_changed:        ['dashboard', 'jobs', 'target'],
};

// scheduleSSERender coalesces a burst of events into a single render. The
// 200 ms window is below the eye's flicker-fusion threshold (~16-50 ms
// for hard transitions, far higher for content change), so the user
// perceives no extra delay; under load it folds N rapid events into one
// expensive renderPage call.
let sseRenderTimer = null;
function scheduleSSERender() {
  if (sseRenderTimer) return;
  sseRenderTimer = setTimeout(() => {
    sseRenderTimer = null;
    renderPage(currentPage(), false);
  }, 200);
}

// handleSSEEvent is the single entry point for routed SSE events.
// For "the server knows something new" events (meta_changed) we
// invalidate the slow-probe cache so the next render re-fetches
// instead of serving stale data with a refreshing-dot. job_state_change
// is handled by the natural per-render /api/jobs fetch — no cache to
// invalidate, the fast Promise.all picks up fresh state automatically.
function handleSSEEvent(eventType) {
  if (eventType === 'meta_changed') {
    _fleetSummary.lastFetch = 0;
    refreshFleetSummary();
  }
  const pages = sseEventPages[eventType] || [];
  if (pages.indexOf(currentPage()) !== -1) {
    scheduleSSERender();
  }
}

let eventSource = null;
function connectSSE() {
  if (eventSource) eventSource.close();
  eventSource = new EventSource('/api/events');
  const dot = $('.status-dot');
  const txt = $('.status-text');

  eventSource.addEventListener('connected', () => {
    dot.className = 'status-dot connected';
    txt.textContent = tr('status.live');
  });
  // Each event re-renders only when the current page actually depends on
  // the changed resource. Other events are dropped — page renderers
  // always re-fetch on navigation, so a user who lands on the affected
  // page later still sees fresh data.
  eventSource.addEventListener('refresh', () => {
    const page = currentPage();
    if (page === 'dashboard' || page === 'jobs') scheduleSSERender();
  });
  Object.keys(sseEventPages).forEach(ev => {
    eventSource.addEventListener(ev, () => handleSSEEvent(ev));
  });
  eventSource.onerror = () => {
    dot.className = 'status-dot error';
    txt.textContent = tr('status.disconnected');
    setTimeout(connectSSE, 5000);
  };
}

// --- Router ---
function currentPage() {
  const hash = location.hash.slice(2) || 'dashboard';
  return hash.split('/')[0] || 'dashboard';
}
function currentParam() {
  const parts = (location.hash.slice(2) || '').split('/');
  return parts.length > 1 ? parts.slice(1).join('/') : null;
}

window.addEventListener('hashchange', () => renderPage(currentPage()));

function renderPage(page, loading = true) {
  $$('.nav-link').forEach(a => {
    a.classList.toggle('active', a.dataset.page === page);
  });
  // Stop the per-second progress-bar tick whenever we leave the Jobs page.
  // renderJobs re-arms it as needed.
  if (page !== 'jobs' && jobProgressTimer) {
    clearInterval(jobProgressTimer);
    jobProgressTimer = null;
  }
  switch(page) {
    case 'dashboard': renderDashboard(loading); break;
    case 'sources': renderSources(loading); break;
    case 'destinations': renderDestinations(loading); break;
    case 'jobs': renderJobs(loading); break;
    case 'target': renderTargetDetail(currentParam(), loading); break;
    case 'alerts': renderAlerts(loading); break;
    case 'audit': renderAudit(loading); break;
    case 'age-keys': renderAgeKeys(loading); break;
    case 'settings': renderSettings(loading); break;
    default: renderDashboard(loading);
  }
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
  const tip = m && (m.error || m.phase) ? escHTML((m.phase ? m.phase + ': ' : '') + (m.error || '')) : '';
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
  renderPage(currentPage(), false);
};
window.setSort = function(list, col, dir) {
  sortState[list] = { col, dir: dir || sortState[list].dir };
  renderPage(currentPage(), false);
};
window.flipSortDir = function(list) {
  const s = sortState[list];
  s.dir = s.dir === 'asc' ? 'desc' : 'asc';
  renderPage(currentPage(), false);
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

// --- Dashboard ---
// Cached slow-probe results so SSE-triggered re-renders don't re-fire the
// probes on every refresh tick. TTL of 60 s comfortably outlives the SSE
// broadcast cadence (10 s) and Bridge's typical page-navigation pattern;
// stale-by-up-to-a-minute destination-health is fine for a dashboard.
// A separate _slowFetchInFlight guard prevents the "kick off probe → not
// done yet → next refresh kicks off another probe in parallel" storm
// that pinned an unreachable destination's 8 s timeout on every render.
const SLOW_PROBE_TTL_MS = 60000;
let _slowProbes = { health: [], consistency: [], lastFetch: 0 };
let _slowFetchInFlight = false;

// Last-good cache for the dashboard's fast Promise.all so a transient
// API failure during an SSE-triggered refresh doesn't reset the stats
// cards to 0/0/0/0/0 and yank the targets table away. Each entry is
// keyed by URL because all three fast endpoints share the same
// rejection behaviour. Updated only on a successful response.
const _fastDataCache = { '/api/targets': null, '/api/destinations': null, '/api/jobs': null };
async function fastDataAllSettled(urls) {
  const results = await Promise.allSettled(urls.map(u => api(u)));
  return urls.map((u, i) => {
    if (results[i].status === 'fulfilled' && results[i].value != null) {
      _fastDataCache[u] = results[i].value;
      return results[i].value;
    }
    // Failed or null — fall back to last good (may itself be null on
    // a cold start, which renderers handle via `|| []`).
    return _fastDataCache[u];
  });
}

// chartOrLoading is the standard pattern for chart cards on the
// dashboard: while the underlying data is being fetched AND nothing
// is cached yet, render a spinner in place of the empty-state. Once
// any data exists (even stale), render that data — a background
// refresh can update it later via SSE without yanking the chart away.
// A separate chart-refreshing dot in the header signals "fresher data
// on the way" when refresh is in-flight but stale data is shown.
function chartOrLoading(data, inFlight, renderFn) {
  const empty = !data ||
                (Array.isArray(data) && data.length === 0) ||
                (typeof data === 'object' && !Array.isArray(data) && Object.keys(data).length === 0);
  if (empty && inFlight) {
    return `<div class="chart-loading"><div class="spinner"></div><div class="hint">${tr('chart.loading') || 'loading…'}</div></div>`;
  }
  return renderFn();
}
function refreshingDot(inFlight, hasData) {
  return (inFlight && hasData) ? '<span class="chart-refreshing" title="refreshing in background"></span>' : '';
}

// Fleet-summary cache (heatmap + storage + anomalies + durations +
// verification daily pass-rate). All five datasets share a single
// backend pass and one endpoint — reading meta files once and emitting
// projections is much cheaper than separate endpoints each iterating
// the same files.
let _fleetSummary = {
  heatmap: [], storage: [], anomalies: [],
  durations: [], verificationDaily: [],
  lastFetch: 0,
};
let _fleetSummaryInFlight = false;
async function refreshFleetSummary() {
  if (_fleetSummaryInFlight) return;
  _fleetSummaryInFlight = true;
  // Don't poison the cache on transient API failure. The previous
  // `.catch(() => ({}))` pattern wrote empty arrays AND updated
  // lastFetch, so the next 60 s of renders showed empty charts even
  // though the actual data was unchanged. Preserve the last good
  // values; leaving lastFetch alone means the next user-initiated
  // render retries immediately instead of waiting out the full TTL.
  //
  // CRITICAL: the post-fetch renderDashboard call MUST happen after
  // _fleetSummaryInFlight is reset, or chartOrLoading sees
  // (empty data + inFlight=true) and renders a permanent spinner.
  let success = false;
  try {
    let r;
    try {
      r = await api('/api/dashboard/heatmap?days=30');
    } catch(e) {
      return;
    }
    if (!r || typeof r !== 'object') return;
    _fleetSummary = {
      heatmap:           r.heatmap           || [],
      storage:           r.storage           || [],
      anomalies:         r.anomalies         || [],
      durations:         r.durations         || [],
      verificationDaily: r.verificationDaily || [],
      lastFetch:         Date.now(),
    };
    success = true;
  } finally {
    _fleetSummaryInFlight = false;
  }
  if (success && currentPage() === 'dashboard') renderDashboard(false);
}
async function refreshSlowProbes() {
  if (_slowFetchInFlight) return;
  _slowFetchInFlight = true;
  // Same render-after-inFlight-reset rule as refreshFleetSummary —
  // otherwise the post-fetch repaint sees inFlight=true and shows
  // a stuck loading state.
  let success = false;
  try {
    // Promise.allSettled so a transient failure on ONE endpoint
    // doesn't blow away the other's good response. If both failed,
    // preserve the previous cache entirely.
    const [hRes, cRes] = await Promise.allSettled([
      api('/api/destination-health'),
      api('/api/consistency-check'),
    ]);
    const hOk = hRes.status === 'fulfilled';
    const cOk = cRes.status === 'fulfilled';
    if (!hOk && !cOk) return;
    _slowProbes = {
      health:      hOk ? (hRes.value || []) : _slowProbes.health,
      consistency: cOk ? (cRes.value || []) : _slowProbes.consistency,
      lastFetch:   Date.now(),
    };
    success = true;
  } finally {
    _slowFetchInFlight = false;
  }
  if (success && currentPage() === 'dashboard') renderDashboard(false);
}

// renderTargetSparkline shows the target's last 7 daily statuses as
// a compact strip of 7 coloured cells. Sourced from the fleet
// heatmap (last 7 entries of the 30-day row). Returns '' if the
// target has no row in the heatmap yet — that's the "brand-new
// source, no runs" case. Inline SVG so it sits next to the target
// name without disturbing the table layout.
function renderTargetSparkline(targetName) {
  const row = (_fleetSummary.heatmap || []).find(r => r.target === targetName);
  if (!row || !row.days || row.days.length === 0) return '';
  const days = row.days.slice(-7);
  const cellW = 8, cellH = 12, gap = 2;
  const W = days.length * (cellW + gap) - gap;
  const colorByStatus = {
    ok:     'var(--success, #10b981)',
    failed: 'var(--danger, #ef4444)',
    mixed:  'var(--warning, #f59e0b)',
    none:   'var(--bg-input, #2a2a2a)',
  };
  const cells = days.map((c, i) => {
    const x = i * (cellW + gap);
    const fill = colorByStatus[c.status] || colorByStatus.none;
    const tt = `${c.day}: ${c.runs > 0 ? c.runs + ' run(s), ' + c.status : 'no run'}`;
    return `<rect x="${x}" y="0" width="${cellW}" height="${cellH}" rx="2" fill="${fill}"><title>${escAttr(tt)}</title></rect>`;
  }).join('');
  return `<svg viewBox="0 0 ${W} ${cellH}" width="${W}" height="${cellH}" class="target-sparkline" aria-label="7-day status">${cells}</svg>`;
}

// buildHourlyActivity buckets jobs into 24 hour-of-day slots ending
// now. Each bucket holds counts by status so the hero strip can pick
// a colour. Bars are oldest-left, newest-right — matches how everyone
// reads a timeline. Pre-computed once per render so the SVG output is
// pure and the counter animations stay deterministic.
function buildHourlyActivity(jobs) {
  const now = Date.now();
  const buckets = Array.from({length: 24}, () => ({ ok: 0, failed: 0, running: 0 }));
  for (const j of jobs || []) {
    const t = parseTsRFC(j.startTime);
    if (!t) continue;
    const hoursAgo = Math.floor((now - t) / 3600000);
    if (hoursAgo < 0 || hoursAgo >= 24) continue;
    const idx = 23 - hoursAgo;
    if (j.status === 'running') buckets[idx].running++;
    else if (j.status === 'failed') buckets[idx].failed++;
    else buckets[idx].ok++;
  }
  return buckets;
}

// _heroLastShown persists the last-displayed value of each hero
// counter across renders. Without this, every re-render rebuilds the
// DOM (content.innerHTML = ...), the data-value attribute is wiped,
// and animateCounter reads 0 → tweens 0→N on every SSE tick. The
// user sees the panel "load again and again". With this cache we
// seed the new DOM with the last-known value so the animation runs
// 0→N exactly once (on first paint), then stays put until the value
// actually changes.
const _heroLastShown = { runs24h: 0, failed24h: 0, runningNow: 0, destinations: 0 };

// renderHeroPanel is the full-bleed status-at-a-glance card at the
// top of the dashboard. Replaces the five flat stat cards with:
//   - a status header that's green / amber / red based on 24h health
//   - four big metrics (runs, failures, running, destinations)
//   - a 24-bar hourly activity strip so the visitor sees the
//     fleet's rhythm without scrolling
// Numbers are wrapped in data-counter spans so animateCounter can
// tween them when SSE updates land.
function renderHeroPanel(targets, dests, jobs) {
  const buckets = buildHourlyActivity(jobs);
  const runs24h = buckets.reduce((s, b) => s + b.ok + b.failed + b.running, 0);
  const failed24h = buckets.reduce((s, b) => s + b.failed, 0);
  const runningNow = jobs.filter(j => j.status === 'running').length;
  const destCount = dests.length;
  const srcCount = targets.length;

  // Status tier: red if any 24h failure, amber if no recent run for
  // an active target (fleet quiet), green otherwise. The "quiet"
  // signal is "0 runs in 24h while there's >0 active targets" — that
  // shouts misconfig louder than a clean green.
  let statusKey = 'healthy';
  let statusMsg = tr('hero.status.healthy');
  let statusSub = tr('hero.status.healthySub', {sources: srcCount, dests: destCount});
  if (failed24h > 0) {
    statusKey = 'danger';
    statusMsg = tr('hero.status.failures', {count: failed24h});
    statusSub = tr('hero.status.failuresSub');
  } else if (srcCount > 0 && runs24h === 0) {
    statusKey = 'warn';
    statusMsg = tr('hero.status.quiet');
    statusSub = tr('hero.status.quietSub');
  }

  // 24h activity strip: each bar's height proportional to that hour's
  // total runs; colour = dominant status. Empty hours render a thin
  // baseline so the strip's shape stays readable.
  const maxRuns = Math.max(1, ...buckets.map(b => b.ok + b.failed + b.running));
  const stripW = 280, stripH = 36, barW = (stripW / 24) - 2;
  const stripBars = buckets.map((b, i) => {
    const total = b.ok + b.failed + b.running;
    const h = total === 0 ? 2 : 4 + (total / maxRuns) * (stripH - 4);
    const x = i * (stripW / 24) + 1;
    const y = stripH - h;
    let cls = 'hero-strip-bar';
    if (total === 0) cls += ' hero-strip-bar-empty';
    else if (b.failed > 0) cls += ' hero-strip-bar-failed';
    else if (b.running > 0) cls += ' hero-strip-bar-running';
    else cls += ' hero-strip-bar-ok';
    const tt = `${23-i}h ago: ${total} run(s)${b.failed > 0 ? `, ${b.failed} failed` : ''}${b.running > 0 ? `, ${b.running} running` : ''}`;
    return `<rect x="${x.toFixed(1)}" y="${y.toFixed(1)}" width="${barW.toFixed(1)}" height="${h.toFixed(1)}" rx="1" class="${cls}"><title>${escAttr(tt)}</title></rect>`;
  }).join('');

  return `<div class="hero hero-status-${statusKey}">
    <div class="hero-header">
      <span class="hero-pulse"></span>
      <div>
        <div class="hero-status-msg">${escHTML(statusMsg)}</div>
        <div class="hero-status-sub">${escHTML(statusSub)}</div>
      </div>
    </div>
    <div class="hero-metrics">
      <div class="hero-metric">
        <div class="hero-metric-value" data-counter="runs24h" data-value="${_heroLastShown.runs24h}">${_heroLastShown.runs24h}</div>
        <div class="hero-metric-label">${tr('hero.metric.runs24h')}</div>
      </div>
      <div class="hero-metric ${failed24h > 0 ? 'hero-metric-bad' : ''}">
        <div class="hero-metric-value" data-counter="failed24h" data-value="${_heroLastShown.failed24h}">${_heroLastShown.failed24h}</div>
        <div class="hero-metric-label">${tr('hero.metric.failed24h')}</div>
      </div>
      <div class="hero-metric ${runningNow > 0 ? 'hero-metric-running' : ''}">
        <div class="hero-metric-value" data-counter="runningNow" data-value="${_heroLastShown.runningNow}">${_heroLastShown.runningNow}</div>
        <div class="hero-metric-label">${tr('hero.metric.running')}${runningNow > 0 ? ' <span class="hero-metric-pulse"></span>' : ''}</div>
      </div>
      <div class="hero-metric">
        <div class="hero-metric-value" data-counter="destinations" data-value="${_heroLastShown.destinations}">${_heroLastShown.destinations}</div>
        <div class="hero-metric-label">${tr('hero.metric.destinations')}</div>
      </div>
    </div>
    <div class="hero-activity">
      <svg viewBox="0 0 ${stripW} ${stripH}" class="hero-strip" preserveAspectRatio="none" aria-label="24-hour activity">
        ${stripBars}
      </svg>
      <div class="hero-activity-label">${tr('hero.activity.label')}</div>
    </div>
  </div>`;
}

async function renderDashboard(loading = true) {
  if (loading) showLoading();
  let targets = [], dests = [], jobs = [];
  // Render as soon as the fast K8s-API calls return. The slow endpoints
  // (destination-health, consistency-check) dial every storage backend;
  // an unreachable destination would otherwise stall the dashboard
  // behind its 8 s probe timeout × N destinations on every refresh.
  // fastDataAllSettled falls back to the last good value per URL so a
  // transient failure on /api/jobs doesn't reset stats to 0/0/0/0/0.
  const got = await fastDataAllSettled(['/api/targets', '/api/destinations', '/api/jobs']);
  targets = got[0] || [];
  dests   = got[1] || [];
  jobs    = got[2] || [];

  const healthEntries = _slowProbes.health;
  const consistencyIssues = _slowProbes.consistency;

  // Kick off a slow-probe refresh only on user-initiated renders (page
  // navigation, manual reload) when the cache is stale. SSE-driven
  // re-renders (loading=false) reuse whatever's cached — otherwise an
  // unreachable destination's 8 s timeout × N dests would fire every
  // 10 s SSE tick.
  if (loading && Date.now() - _slowProbes.lastFetch > SLOW_PROBE_TTL_MS) refreshSlowProbes();
  if (loading && Date.now() - _fleetSummary.lastFetch > SLOW_PROBE_TTL_MS) refreshFleetSummary();
  // Storage Donut on the dashboard reads _destStatsCache, which was
  // only refreshed by the destinations page — a user landing directly
  // on /dashboard saw a permanently empty donut. Trigger the refresh
  // from here too; the in-flight guard prevents duplicate fetches if
  // both pages were ever rendered in quick succession.
  if (loading && Date.now() - _destStatsCache.lastFetch > SLOW_PROBE_TTL_MS) refreshDestStats();

  const ok = targets.filter(t => t.Latest && !t.Latest.status?.includes('fail')).length;
  const failed = targets.filter(t => t.Latest?.status === 'failed').length;
  const running = jobs.filter(j => j.status === 'running').length;

  const dashGetters = {
    name:     t => (t.Name || '').toLowerCase(),
    dbType:   t => t.DBType || '',
    schedule: t => t.Schedule || '',
    status:   t => !t.Latest ? 2 : (t.Latest.status === 'failed' ? 0 : 1), // failed first asc, ok mid, none last
    lastRun:  t => parseTsCompact(t.Latest && t.Latest.timestamp),
    size:     t => (t.Latest && !t.Latest.status?.includes('fail')) ? (t.Latest.encryptedSizeBytes || 0) : null,
    createdAt: t => parseTsRFC(t.CreatedAt),
  };
  const ds = sortState.dashboard;
  const sortedTargets = sortBy(targets, dashGetters[ds.col] || dashGetters.lastRun, ds.dir);

  content.innerHTML = `
    <div class="page-header">
      <div><h1>${tr('page.dashboard.title')}</h1><div class="subtitle">${tr('page.dashboard.subtitle')}</div></div>
    </div>
    ${renderHeroPanel(targets, dests, jobs)}
    <div class="chart-grid-2">
      <div class="chart-card">
        <h3>${tr('chart.storageDonut.title')}${refreshingDot(_destStatsInFlight, _destStatsCache.stats.length > 0)} <span class="chart-card-sub">${tr('chart.storageDonut.sub')}</span></h3>
        ${chartOrLoading(_destStatsCache.stats, _destStatsInFlight, () => renderStorageDonut(_destStatsCache.stats))}
      </div>
      <div class="chart-card">
        <h3>${tr('chart.nextRunGantt.title')} <span class="chart-card-sub">${tr('chart.nextRunGantt.sub')}</span></h3>
        ${renderNextRunGantt(targets)}
      </div>
    </div>
    <div class="chart-card" style="margin-bottom:16px">
      <h3>${tr('chart.fleetHeatmap.title')}${refreshingDot(_fleetSummaryInFlight, _fleetSummary.heatmap.length > 0)} <span class="chart-card-sub">${tr('chart.fleetHeatmap.sub')}</span></h3>
      ${chartOrLoading(_fleetSummary.heatmap, _fleetSummaryInFlight, () => renderFleetHeatmap(_fleetSummary.heatmap))}
    </div>
    <div class="chart-grid-2">
      <div class="chart-card">
        <h3>${tr('chart.storageGrowth.title')}${refreshingDot(_fleetSummaryInFlight, _fleetSummary.storage.length > 0)} <span class="chart-card-sub">${tr('chart.storageGrowth.sub')}</span></h3>
        ${chartOrLoading(_fleetSummary.storage, _fleetSummaryInFlight, () => renderStorageGrowth(_fleetSummary.storage))}
      </div>
      <div class="chart-card">
        <h3>${tr('chart.anomalyStream.title')}${refreshingDot(_fleetSummaryInFlight, _fleetSummary.anomalies.length > 0)} <span class="chart-card-sub">${tr('chart.anomalyStream.sub')}</span></h3>
        ${chartOrLoading(_fleetSummary.anomalies, _fleetSummaryInFlight, () => renderAnomalyStream(_fleetSummary.anomalies))}
      </div>
    </div>
    <div class="chart-grid-2">
      <div class="chart-card">
        <h3>${tr('chart.durationDist.title')}${refreshingDot(_fleetSummaryInFlight, _fleetSummary.durations.length > 0)} <span class="chart-card-sub">${tr('chart.durationDist.sub')}</span></h3>
        ${chartOrLoading(_fleetSummary.durations, _fleetSummaryInFlight, () => renderDurationDistribution(_fleetSummary.durations))}
      </div>
      <div class="chart-card">
        <h3>${tr('chart.verifyTrend.title')}${refreshingDot(_fleetSummaryInFlight, _fleetSummary.verificationDaily.length > 0)} <span class="chart-card-sub">${tr('chart.verifyTrend.sub')}</span></h3>
        ${chartOrLoading(_fleetSummary.verificationDaily, _fleetSummaryInFlight, () => renderVerificationTrend(_fleetSummary.verificationDaily))}
      </div>
    </div>
    ${renderStorageByDestination(targets, dests)}
    <div class="table-card">
      <div class="table-card-header">
        <h2>${tr('page.dashboard.targets')}</h2>
        <button class="btn btn-primary btn-sm" onclick="location.hash='#/sources';openSourceForm()" title="Create a new backup source — opens a form that generates a labelled Kubernetes Secret">+ ${tr('buttons.addSource')}</button>
      </div>
      ${targets.length === 0 ? `<div class="empty-state"><h3>${tr('page.dashboard.noTargets')}</h3><p>${tr('page.dashboard.noTargetsHint')}</p></div>` : `
      <table>
        <thead><tr>
          <th class="num row-num">#</th>
          <th class="sortable" onclick="toggleSort('dashboard','name')">${tr('table.target')}${sortIndicator('dashboard','name')}</th>
          <th class="sortable" onclick="toggleSort('dashboard','dbType')">${tr('table.type')}${sortIndicator('dashboard','dbType')}</th>
          <th class="sortable" onclick="toggleSort('dashboard','schedule')">${tr('table.schedule')}${sortIndicator('dashboard','schedule')}</th>
          <th class="sortable" onclick="toggleSort('dashboard','status')">${tr('table.status')}${sortIndicator('dashboard','status')}</th>
          <th class="sortable" onclick="toggleSort('dashboard','lastRun')">${tr('table.lastRun')}${sortIndicator('dashboard','lastRun')}</th>
          <th class="num sortable" onclick="toggleSort('dashboard','size')">${tr('table.size')}${sortIndicator('dashboard','size')}</th>
          <th class="sortable" onclick="toggleSort('dashboard','createdAt')">${tr('table.createdAt')}${sortIndicator('dashboard','createdAt')}</th>
          <th>${tr('table.destinations')}</th><th></th>
        </tr></thead>
        <tbody>${sortedTargets.map((t, i) => `<tr>
          <td class="num row-num">${i + 1}</td>
          <td>
            <a href="#/target/${escAttr(t.Name)}" style="color:var(--accent);font-weight:600">${escHTML(t.Name)}</a>
            <div class="target-sparkline-wrap" title="${escAttr(tr('hero.sparkline.title') || 'last 7 days')}">${renderTargetSparkline(t.Name)}</div>
          </td>
          <td><span class="badge badge-${t.DBType}">${t.DBType}</span></td>
          <td>
            <code style="font-size:12px;background:var(--bg-input);padding:2px 6px;border-radius:4px">${escHTML(t.Schedule)}</code>${t.Suspended ? ` <span class="badge badge-warn" style="margin-left:4px" title="Scheduled runs are paused; manual triggers still work">${tr('badge.paused')}</span>` : ''}
            ${!t.Suspended && t.nextRun ? `<div style="font-size:11px;color:var(--text-muted);margin-top:2px">${escHTML(fmtNextRun(t.nextRun))}</div>` : ''}
          </td>
          <td>${t.Latest ? (t.Latest.status === 'failed'
            ? failedBadge(t.Latest)
            : `<span class="badge badge-ok">${tr('badge.ok')}</span>`)
            : `<span class="badge badge-pending">${tr('badge.noRuns')}</span>`}</td>
          <td style="color:var(--text-muted);font-size:12px">${t.Latest ? timeAgo(t.Latest.timestamp) : tr('time.never')}</td>
          <td class="num" style="font-size:12px">${t.Latest && !t.Latest.status?.includes('fail') ? humanBytes(t.Latest.encryptedSizeBytes) : '—'}</td>
          <td style="color:var(--text-muted);font-size:12px">${t.CreatedAt ? timeAgo(t.CreatedAt) : '—'}</td>
          <td>${(t.Destinations || []).map(d => `<span class="badge badge-sftp" style="margin:1px">${escHTML(d)}</span>`).join('')}</td>
          <td style="white-space:nowrap">
            <button class="btn btn-ghost btn-sm" onclick="triggerBackup('${escJS(t.Name)}')" title="Trigger a manual backup run now (creates a one-off Job from the CronJob template)">&#9654;</button>
            ${t.Suspended
              ? `<button class="btn btn-ghost btn-sm" style="color:var(--success)" onclick="toggleSourceSuspend('${escJS(t.SecretName)}','${escJS(t.Name)}',false)" title="Resume scheduled runs">&#9655;&#9655;</button>`
              : `<button class="btn btn-ghost btn-sm" style="color:var(--warning)" onclick="toggleSourceSuspend('${escJS(t.SecretName)}','${escJS(t.Name)}',true)" title="Pause scheduled runs — sets backup.mogenius.io/suspended=true. Existing dumps kept; manual triggers still work.">&#10074;&#10074;</button>`}
            <button class="btn btn-ghost btn-sm" onclick="openSourceForm('${escJS(t.SecretName)}')" title="Edit this source's connection details and schedule">&#9998;</button>
            <button class="btn btn-ghost btn-sm" style="color:var(--danger)" onclick="deleteSource('${escJS(t.SecretName)}','${escJS(t.Name)}')" title="Delete this source — the CronJob is cascaded; existing dumps in storage are kept">&#10005;</button>
          </td>
        </tr>`).join('')}</tbody>
      </table>`}
    </div>
    ${consistencyIssues.length > 0 ? `
    <div class="table-card" style="border-left:3px solid var(--danger)">
      <div class="table-card-header"><h2 style="color:var(--danger)">${tr('page.dashboard.consistency')}</h2></div>
      <p style="padding:0 16px;color:var(--text-muted);font-size:13px;margin:0 0 8px">${tr('consistency.hint')}</p>
      <table>
        <thead><tr><th class="num row-num">#</th><th>${tr('table.target')}</th><th>${tr('table.timestamp')}</th><th>${tr('table.presentIn')}</th><th>${tr('table.missingFrom')}</th></tr></thead>
        <tbody>${consistencyIssues.slice(0, 20).map((ci, i) => `<tr>
          <td class="num row-num">${i + 1}</td>
          <td><strong>${escHTML(ci.target)}</strong></td>
          <td style="font-size:12px">${escHTML(ci.timestamp)}</td>
          <td>${(ci.presentIn||[]).map(d => `<span class="badge badge-ok" style="margin:1px">${escHTML(d)}</span>`).join('')}</td>
          <td>${(ci.missingFrom||[]).map(d => `<span class="badge badge-failed" style="margin:1px">${escHTML(d)}</span>`).join('')}</td>
        </tr>`).join('')}</tbody>
      </table>
      ${consistencyIssues.length > 20 ? `<p style="padding:8px 16px;color:var(--text-muted);font-size:12px">${tr('consistency.andMore', {count: consistencyIssues.length - 20})}</p>` : ''}
    </div>` : ''}
    ${healthEntries.length > 0 && dests.length > 1 ? `
    <div class="table-card">
      <div class="table-card-header"><h2>${tr('page.dashboard.health')}</h2></div>
      <table>
        <thead><tr>
          <th class="num row-num">#</th>
          <th>${tr('table.target')}</th>
          ${[...new Set(healthEntries.map(h => h.destination))].map(d => `<th style="text-align:center">${escHTML(d)}</th>`).join('')}
        </tr></thead>
        <tbody>${(() => {
          const destNames = [...new Set(healthEntries.map(h => h.destination))];
          const targetNames = [...new Set(healthEntries.map(h => h.target))];
          const lookup = {};
          healthEntries.forEach(h => { lookup[h.target + '@' + h.destination] = h; });
          return targetNames.map((t, i) => `<tr>
            <td class="num row-num">${i + 1}</td>
            <td><a href="#/target/${escAttr(t)}" style="color:var(--accent);font-weight:600">${escHTML(t)}</a></td>
            ${destNames.map(d => {
              const h = lookup[t + '@' + d];
              if (!h) return '<td style="text-align:center"><span class="badge" style="background:var(--bg-input);color:var(--text-muted)">N/A</span></td>';
              const badge = h.status === 'ok' ? 'badge-ok' : h.status === 'failed' ? 'badge-failed' : h.status === 'missing' ? 'badge-pending' : 'badge-failed';
              const label = h.status === 'ok' ? tr('badge.ok') : h.status === 'failed' ? tr('badge.failed') : h.status === 'missing' ? tr('badge.noData') : tr('badge.unreachable');
              const tip = h.error ? ' title="' + escAttr(h.error) + '"' : '';
              return '<td style="text-align:center"><span class="badge ' + badge + '"' + tip + '>' + label + '</span>' +
                (h.latestRun ? '<div style="font-size:10px;color:var(--text-muted)">' + timeAgo(h.latestRun) + '</div>' : '') +
                renderScrubChip(h) + '</td>';
            }).join('')}
          </tr>`).join('');
        })()}</tbody>
      </table>
    </div>` : ''}`;

  // Animate hero counters from the previously-displayed value to the
  // new one. _heroLastShown holds what was actually on screen before
  // the re-render — without that cache, every SSE tick wiped the
  // data-value attribute and tweened 0→N over and over. Computed
  // values mirror the renderHeroPanel logic above.
  const buckets24h = buildHourlyActivity(jobs);
  const targets24h = {
    runs24h:      buckets24h.reduce((s, b) => s + b.ok + b.failed + b.running, 0),
    failed24h:    buckets24h.reduce((s, b) => s + b.failed, 0),
    runningNow:   jobs.filter(j => j.status === 'running').length,
    destinations: dests.length,
  };
  for (const key of Object.keys(targets24h)) {
    animateCounter($(`[data-counter="${key}"]`), targets24h[key]);
    _heroLastShown[key] = targets24h[key];
  }
}

// --- Sources ---
async function renderSources(loading = true) {
  if (loading) showLoading();
  let targets = [];
  try { targets = (await api('/api/targets')) || []; } catch(e) { toast(e.message, 'error'); }

  const srcGetters = {
    createdAt: t => parseTsRFC(t.CreatedAt),
    name:      t => (t.Name || '').toLowerCase(),
    lastRun:   t => parseTsCompact(t.Latest && t.Latest.timestamp),
    dbType:    t => t.DBType || '',
  };
  const ss = sortState.sources;
  const sortedTargets = sortBy(targets, srcGetters[ss.col] || srcGetters.createdAt, ss.dir);

  content.innerHTML = `
    <div class="page-header">
      <div><h1>${tr('page.sources.title')}</h1><div class="subtitle">${tr('page.sources.subtitle')}</div></div>
      <div style="display:flex;gap:8px;align-items:center">
        ${targets.length > 0 ? renderSortControl('sources', [
          ['createdAt', tr('table.createdAt')],['name', tr('common.name')],['lastRun', tr('table.lastRun')],['dbType', tr('common.type')],
        ]) : ''}
        <button class="btn btn-primary" onclick="openSourceForm()" title="Create a new backup source — opens a form that generates a labelled Kubernetes Secret">+ ${tr('buttons.addSource')}</button>
      </div>
    </div>
    ${targets.length === 0 ? `
    <div class="empty-state">
      <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M21 12c0 1.66-4 3-9 3s-9-1.34-9-3"/><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"/></svg>
      <h3>${tr('page.sources.empty')}</h3>
      <p>${tr('page.sources.emptyHint')}</p>
      <button class="btn btn-primary" onclick="openSourceForm()" title="Create your first backup source">+ ${tr('buttons.addSource')}</button>
    </div>` : `
    <div style="display:grid;grid-template-columns:repeat(auto-fill,minmax(340px,1fr));gap:16px">
      ${sortedTargets.map(t => `
      <div class="detail-card" style="cursor:pointer" onclick="location.hash='#/target/${escJS(t.Name)}'">
        <div style="display:flex;justify-content:space-between;align-items:start;margin-bottom:12px">
          <div>
            <div style="font-weight:600;font-size:15px;color:var(--text-heading)">${escHTML(t.Name)}</div>
            <span class="badge badge-${t.DBType}" style="margin-top:6px">${t.DBType}</span>
            ${t.Suspended ? `<span class="badge badge-warn" style="margin-top:6px;margin-left:4px" title="Scheduled runs are paused; manual triggers still work">${tr('badge.paused')}</span>` : ''}
          </div>
          ${t.Latest ? (t.Latest.status === 'failed'
            ? failedBadge(t.Latest)
            : `<span class="badge badge-ok">${tr('badge.ok')}</span>`)
            : `<span class="badge badge-pending">${tr('badge.noRuns')}</span>`}
        </div>
        <div class="detail-row"><span class="key">${tr('table.schedule')}</span><code class="val">${escHTML(t.Schedule)}${t.Suspended ? ' <span style="color:var(--warning);font-weight:600">(' + tr('buttons.pause').toLowerCase() + ')</span>' : ''}</code></div>
        ${!t.Suspended && t.nextRun ? `<div class="detail-row"><span class="key">${tr('target.nextRun') || 'Nächster Lauf'}</span><span class="val" style="color:var(--text-muted)">${escHTML(fmtNextRun(t.nextRun))}</span></div>` : ''}
        <div class="detail-row"><span class="key">${tr('table.lastRun')}</span><span class="val">${t.Latest ? timeAgo(t.Latest.timestamp) : tr('common.none')}</span></div>
        ${t.Latest && t.Latest.status === 'failed' && t.Latest.error ? `
        <div class="detail-row" style="align-items:flex-start"><span class="key">${tr('table.error')}</span><span class="val" style="color:var(--danger);font-size:12px;word-break:break-word" title="${escAttr(t.Latest.error)}">${escHTML(truncate(t.Latest.error, 140))}</span></div>` : ''}
        <div class="detail-row"><span class="key">${tr('table.createdAt')}</span><span class="val">${t.CreatedAt ? timeAgo(t.CreatedAt) : '—'}</span></div>
        <div class="detail-row"><span class="key">${tr('table.destinations')}</span><span class="val">${(t.Destinations||[]).join(', ') || tr('common.all').toLowerCase()}</span></div>
        ${verificationRow(t)}
        <div style="display:flex;gap:6px;margin-top:12px;justify-content:flex-end">
          <button class="btn btn-ghost btn-sm" onclick="event.stopPropagation();triggerBackup('${escJS(t.Name)}')" title="Trigger a manual backup run now (creates a one-off Job from the CronJob template; pause-state ignored for manual triggers)">&#9654; ${tr('buttons.run')}</button>
          ${t.Suspended
            ? `<button class="btn btn-ghost btn-sm" style="color:var(--success)" onclick="event.stopPropagation();toggleSourceSuspend('${escJS(t.SecretName)}','${escJS(t.Name)}',false)" title="Resume scheduled runs — the reconciler clears Spec.Suspend on the managed CronJob within seconds">${tr('buttons.resume')}</button>`
            : `<button class="btn btn-ghost btn-sm" style="color:var(--warning)" onclick="event.stopPropagation();toggleSourceSuspend('${escJS(t.SecretName)}','${escJS(t.Name)}',true)" title="Pause scheduled runs — sets backup.mogenius.io/suspended=true on the source Secret. Existing dumps and config are kept; manual Run still works.">${tr('buttons.pause')}</button>`}
          <button class="btn btn-ghost btn-sm" onclick="event.stopPropagation();openSourceForm('${escJS(t.SecretName)}')" title="Edit this source's connection details and schedule">${tr('common.edit')}</button>
          <button class="btn btn-ghost btn-sm" style="color:var(--danger)" onclick="event.stopPropagation();deleteSource('${escJS(t.SecretName)}','${escJS(t.Name)}')" title="Delete this source — the managed CronJob is removed via OwnerReference; existing dumps in storage are kept">${tr('common.delete')}</button>
        </div>
      </div>`).join('')}
    </div>`}`;
}

// --- Source Form ---

// Lazy-loaded cluster capabilities. Cached for the page session so
// every source-form open does not pay a SubjectAccessReview round-trip.
// Cleared by the SSE 'refresh' tick path is unnecessary — RBAC changes
// require a Helm upgrade and a page reload anyway.
let _clusterCapabilitiesCache = null;
async function getClusterCapabilities() {
  if (_clusterCapabilitiesCache) return _clusterCapabilitiesCache;
  try {
    _clusterCapabilitiesCache = await api('/api/cluster/capabilities');
  } catch (e) {
    _clusterCapabilitiesCache = { phase2Allowed: false, reason: 'capability check failed: ' + e.message };
  }
  return _clusterCapabilitiesCache;
}

// Phase-2 modes spawn an ephemeral DB pod and therefore need pods/create
// in the worker SA's namespace. Phase-1 (stream-validate) does not.
function isPhase2VerificationMode(mode) {
  return mode === 'schema-only' || mode === 'sample' || mode === 'full';
}

// Render the warning banner inside the open source form when the user
// picks Phase-2 but the cluster currently won't allow it. We do not
// block save — the source secret is still useful (RBAC may flip on
// later, or the user may know the schedule won't run until a Helm
// upgrade lands). The banner just stops the silent "pods is forbidden"
// dead end.
function refreshPhase2RBACWarning(formEl, caps) {
  const select = formEl.elements['restoreVerificationMode'];
  const banner = formEl.querySelector('#phase2-rbac-warning');
  if (!select || !banner || !caps) return;
  const phase2 = isPhase2VerificationMode(select.value);
  if (!phase2 || caps.phase2Allowed) {
    banner.style.display = 'none';
    return;
  }
  banner.style.display = 'block';
  banner.innerHTML = '⚠ ' + escHTML(tr('banner.phase2RBAC')) + (caps.reason ? ' ' + escHTML(caps.reason) : '');
}

window.openSourceForm = function(secretName) {
  const isEdit = !!secretName;
  const title = isEdit ? tr('form.source.editTitle') : tr('form.source.createTitle');

  let formHTML = `<form id="sourceForm" onsubmit="submitSourceForm(event, '${secretName || ''}')">
    <div class="form-row">
      <div class="form-group"><label>${tr('form.source.label.name')} *</label>
        <input name="name" required placeholder="${tr('form.source.placeholder.name')}" ${isEdit ? 'disabled' : ''}></div>
      <div class="form-group"><label>${tr('form.source.label.dbType')} *</label>
        <select name="dbType" required>
          <option value="">${tr('form.source.placeholder.selectType')}</option>
          <option value="postgres">PostgreSQL</option>
          <option value="mysql">MySQL</option>
          <option value="mariadb">MariaDB</option>
          <option value="mongo">MongoDB</option>
          <option value="redis">Redis</option>
        </select></div>
    </div>
    <div class="form-row">
      <div class="form-group"><label>${tr('form.source.label.host')} *</label><input name="host" required placeholder="${tr('form.source.placeholder.host')}"></div>
      <div class="form-group"><label>${tr('form.source.label.port')}</label><input name="port" placeholder="${tr('form.source.placeholder.port')}"></div>
    </div>
    <div class="form-row">
      <div class="form-group"><label>${tr('form.source.label.database')}</label><input name="database" placeholder="${tr('form.source.placeholder.database')}"></div>
      <div class="form-group"><label>${tr('form.source.label.schedule')}</label>
        <input name="schedule" placeholder="${tr('form.source.placeholder.schedule')}">
        <div class="hint">${tr('form.source.hint.schedule')}</div></div>
    </div>
    <div class="form-row">
      <div class="form-group"><label>${tr('form.source.label.jitter')}</label>
        <input name="jitterMinutes" placeholder="${tr('form.source.placeholder.jitter')}">
        <div class="hint">${tr('form.source.hint.jitter')}</div></div>
    </div>
    <div class="form-row">
      <div class="form-group"><label>${tr('form.source.label.username')}</label>
        <input name="username">
        <div class="hint">${tr('form.source.hint.username')}</div></div>
      <div class="form-group"><label>${tr('form.source.label.password')}</label><input name="password" type="password" placeholder="${isEdit ? tr('form.source.placeholder.passwordEdit') : ''}"></div>
    </div>
    <div class="form-section"><h4>${tr('form.source.section.retention')}</h4>
      <div class="form-row">
        <div class="form-group"><label>${tr('form.source.label.retentionDays')}</label><input name="retentionDays" placeholder="${tr('form.source.placeholder.retentionDays')}"></div>
        <div class="form-group"><label>${tr('form.source.label.minKeep')}</label><input name="minKeep" placeholder="${tr('form.source.placeholder.minKeep')}"></div>
      </div>
      <div class="form-row">
        <div class="form-group"><label>${tr('form.source.label.destinations')}</label>
          <div id="destinationsPicker" class="multi-select-list">
            <div class="multi-select-empty">${tr('common.loading')}</div>
          </div>
          <input type="hidden" name="destinations" value="">
          <div class="hint">${tr('form.source.label.destinationsHint')}</div></div>
        <div class="form-group"><label>${tr('form.source.label.anonymize')}</label>
          <select name="anonymizeTables"><option value="">${tr('common.no')}</option><option value="true">${tr('common.yes')}</option></select></div>
      </div>
    </div>
    <div class="form-section"><h4>${tr('form.source.section.verification')}</h4>
      <div class="hint" style="margin-bottom:12px">${tr('form.source.hint.verifyIntro')}</div>
      <div id="phase2-rbac-warning" style="display:none;margin-bottom:12px;padding:10px 12px;border-left:3px solid var(--warning);background:var(--warning-bg);color:var(--warning);font-size:12px;border-radius:4px"></div>
      <div class="form-row">
        <div class="form-group"><label>${tr('form.source.label.verificationMode')}</label>
          <select name="restoreVerificationMode">
            <option value="">${tr('form.source.verifyMode.off')}</option>
            <option value="off">${tr('common.off')}</option>
            <option value="stream-validate">${tr('form.source.verifyMode.streamValidate')}</option>
            <option value="schema-only">${tr('form.source.verifyMode.schemaOnly')}</option>
            <option value="sample">${tr('form.source.verifyMode.sample')}</option>
            <option value="full">${tr('form.source.verifyMode.full')}</option>
          </select>
          <div class="hint">${tr('form.source.hint.verifyMode')}</div></div>
        <div class="form-group"><label>${tr('form.source.label.verificationInterval')}</label>
          <input name="restoreVerificationInterval" placeholder="${tr('form.source.placeholder.interval')}">
          <div class="hint">${tr('form.source.hint.verifyInterval')}</div></div>
      </div>
      <div class="form-row">
        <div class="form-group"><label>${tr('form.source.label.verificationImage')}</label>
          <input name="verificationImage" placeholder="${tr('form.source.placeholder.image')}">
          <div class="hint">${tr('form.source.hint.verifyImage')}</div></div>
        <div class="form-group"><label>${tr('form.source.label.verificationVolumeSize')}</label>
          <input name="verificationVolumeSize" placeholder="${tr('form.source.placeholder.volumeSize')}">
          <div class="hint">${tr('form.source.hint.verifyVolumeSize')}</div></div>
      </div>
    </div>
    <div class="form-section"><h4>${tr('form.source.section.analysis')}</h4>
      <div class="hint" style="margin-bottom:12px">${tr('form.source.hint.analysisIntro')}</div>
      <div class="form-row">
        <div class="form-group"><label>${tr('form.source.label.analyzer')}</label>
          <select name="analyzerEnabled">
            <option value="">${tr('form.source.select.defaultOn')}</option>
            <option value="true">${tr('form.source.select.enabled')}</option>
            <option value="false">${tr('form.source.select.disabled')}</option>
          </select>
          <div class="hint">${tr('form.source.hint.analyzer')}</div></div>
        <div class="form-group"><label>${tr('form.source.label.emptyDumpCheck')}</label>
          <select name="emptyDumpCheck">
            <option value="">${tr('form.source.select.defaultOn')}</option>
            <option value="true">${tr('form.source.select.enabled')}</option>
            <option value="false">${tr('form.source.select.disabled')}</option>
          </select>
          <div class="hint">${tr('form.source.hint.emptyDumpCheck')}</div></div>
      </div>
      <div class="form-row">
        <div class="form-group"><label>${tr('form.source.label.rowDropThreshold')}</label>
          <input name="rowDropThreshold" placeholder="${tr('form.source.placeholder.rowDrop')}">
          <div class="hint">${tr('form.source.hint.rowDropThreshold')}</div></div>
        <div class="form-group"><label>${tr('form.source.label.sizeDropThreshold')}</label>
          <input name="sizeDropThreshold" placeholder="${tr('form.source.placeholder.sizeDrop')}">
          <div class="hint">${tr('form.source.hint.sizeDropThreshold')}</div></div>
      </div>
    </div>
    <div class="form-actions">
      <button type="button" class="btn btn-secondary" onclick="closeModal()" title="Discard changes and close this dialog">${tr('common.cancel')}</button>
      <button type="submit" class="btn btn-primary" title="${isEdit ? 'Save the modified source Secret — the operator reconciles changes within seconds' : 'Create the source Secret — the operator generates a CronJob within seconds'}">${isEdit ? tr('common.update') : tr('common.create')}</button>
    </div>
  </form>`;

  openModal(title, formHTML);

  // Hook the Phase-2 RBAC warning. Capabilities load asynchronously —
  // when the answer arrives we evaluate against the current dropdown
  // value, and we also re-evaluate on every change so the banner
  // appears the moment a user picks schema-only / sample / full while
  // the cluster has the flag off.
  const formEl = document.getElementById('sourceForm');
  if (formEl) {
    const modeSelect = formEl.elements['restoreVerificationMode'];
    getClusterCapabilities().then(caps => refreshPhase2RBACWarning(formEl, caps));
    if (modeSelect) {
      modeSelect.addEventListener('change',
        () => getClusterCapabilities().then(caps => refreshPhase2RBACWarning(formEl, caps)));
    }
  }

  // In create mode we have no preselection to wait for — populate immediately.
  // In edit mode the source fetch below kicks off the picker so we don't fire
  // two fetches and risk the empty-preselection response overwriting the real
  // one if it arrives second.
  if (!isEdit) loadDestinationsPicker('');

  if (isEdit) {
    api('/api/sources/' + secretName).then(src => {
      const f = $('#sourceForm');
      f.name.value = src.name || '';
      f.dbType.value = src.dbType || '';
      f.host.value = src.host || '';
      f.port.value = src.port || '';
      f.database.value = src.database || '';
      f.schedule.value = src.schedule || '';
      f.jitterMinutes.value = src.jitterMinutes || '';
      f.username.value = src.username || '';
      f.retentionDays.value = src.retentionDays || '';
      f.minKeep.value = src.minKeep || '';
      loadDestinationsPicker(src.destinations || '');
      if (src.anonymizeTables === 'true') f.anonymizeTables.value = 'true';
      // Annotation values come back as the literal string the user wrote
      // ("true" / "false" / ""). Empty == "use default" — leave the select on
      // its first option so the next save doesn't pin a value the user
      // didn't intentionally choose.
      f.analyzerEnabled.value = (src.analyzerEnabled === 'true' || src.analyzerEnabled === 'false') ? src.analyzerEnabled : '';
      f.emptyDumpCheck.value = (src.emptyDumpCheck === 'true' || src.emptyDumpCheck === 'false') ? src.emptyDumpCheck : '';
      f.rowDropThreshold.value = src.rowDropThreshold || '';
      f.sizeDropThreshold.value = src.sizeDropThreshold || '';
      f.restoreVerificationMode.value = src.restoreVerificationMode || '';
      f.restoreVerificationInterval.value = src.restoreVerificationInterval || '';
      f.verificationImage.value = src.verificationImage || '';
      f.verificationVolumeSize.value = src.verificationVolumeSize || '';
      // Programmatic value-set does not fire a change event, so the
      // Phase-2 banner needs an explicit nudge after edit-mode populates.
      getClusterCapabilities().then(caps => refreshPhase2RBACWarning(f, caps));
    }).catch(e => toast(tr('toast.loadFailed', {error: e.message}), 'error'));
  }
};

// Renders a checkbox list of all destinations into #destinationsPicker and
// keeps the hidden `destinations` input in sync as a comma-separated string.
// `currentValue` is the saved annotation string ("name1,name2"). Names that
// are present in the saved value but no longer match any known destination
// are kept as disabled "(missing)" entries so a simple Update doesn't silently
// drop the allow-list.
function loadDestinationsPicker(currentValue) {
  const picker = document.getElementById('destinationsPicker');
  const hidden = document.querySelector('#sourceForm input[name="destinations"]');
  if (!picker || !hidden) return;
  const selected = (currentValue || '').split(',').map(s => s.trim()).filter(Boolean);
  api('/api/destinations').then(dests => {
    dests = dests || [];
    const known = new Set(dests.map(d => d.name));
    const missing = selected.filter(n => !known.has(n));

    if (dests.length === 0 && missing.length === 0) {
      picker.innerHTML = '<div class="multi-select-empty">No destinations configured. Create one first, then assign it here.</div>';
      hidden.value = '';
      return;
    }

    const rows = [];
    for (const d of dests) {
      const checked = selected.includes(d.name) ? 'checked' : '';
      rows.push(`<label>
        <input type="checkbox" value="${escAttr(d.name)}" ${checked}>
        <span>${escHTML(d.name)}</span>
        <span class="ms-meta">${escHTML(d.storageType || '')}</span>
      </label>`);
    }
    for (const name of missing) {
      // Ghost entry: not in the cluster right now (renamed / deleted /
      // different namespace). Keep it checked so Save doesn't drop it; the
      // disabled checkbox tells the user something is off.
      rows.push(`<label class="ms-missing" title="This destination is in the saved allow-list but no destination Secret with that logical name currently exists.">
        <input type="checkbox" value="${escAttr(name)}" checked disabled data-ghost="1">
        <span>${escHTML(name)}</span>
        <span class="ms-meta">missing</span>
      </label>`);
    }
    picker.innerHTML = rows.join('');

    const sync = () => {
      const checked = picker.querySelectorAll('input[type="checkbox"]:checked');
      hidden.value = Array.from(checked).map(cb => cb.value).join(',');
    };
    picker.querySelectorAll('input[type="checkbox"]').forEach(cb => {
      cb.addEventListener('change', sync);
    });
    sync();
  }).catch(e => {
    picker.innerHTML = `<div class="multi-select-empty">Failed to load destinations: ${escHTML(e.message)}</div>`;
  });
}

window.submitSourceForm = async function(e, secretName) {
  e.preventDefault();
  const f = e.target;
  // Tri-state selects: empty string = "use default" → don't send the field at
  // all so the operator falls back to chart defaults. "true"/"false" pin the
  // annotation explicitly. Same pattern as anonymizeTables above.
  const triState = v => v === 'true' ? true : v === 'false' ? false : null;
  const body = {
    name: f.name.value,
    dbType: f.dbType.value,
    host: f.host.value,
    port: f.port.value,
    database: f.database.value,
    schedule: f.schedule.value,
    jitterMinutes: f.jitterMinutes.value,
    username: f.username.value,
    password: f.password.value,
    retentionDays: f.retentionDays.value,
    minKeep: f.minKeep.value,
    destinations: f.destinations.value,
    // Two-option select ("Yes" / "No") — always emit a concrete bool. The
    // tri-state pattern below (analyzerEnabled / emptyDumpCheck) sends null
    // for "use default" because those have a third option in the form;
    // anonymizeTables doesn't, so null here meant "user picked No but I
    // discarded the choice" — the backend's merge then left the previous
    // annotation in place, silently ignoring the change.
    anonymizeTables: f.anonymizeTables.value === 'true',
    analyzerEnabled: triState(f.analyzerEnabled.value),
    emptyDumpCheck: triState(f.emptyDumpCheck.value),
    rowDropThreshold: f.rowDropThreshold.value,
    sizeDropThreshold: f.sizeDropThreshold.value,
    restoreVerificationMode: f.restoreVerificationMode.value,
    restoreVerificationInterval: f.restoreVerificationInterval.value,
    verificationImage: f.verificationImage.value,
    verificationVolumeSize: f.verificationVolumeSize.value,
  };
  try {
    if (secretName) {
      await api('/api/sources/' + secretName, { method: 'PUT', body: JSON.stringify(body) });
      toast(tr('toast.sourceUpdated'), 'success');
    } else {
      await api('/api/sources', { method: 'POST', body: JSON.stringify(body) });
      toast(tr('toast.sourceCreated'), 'success');
    }
    closeModal();
    renderPage(currentPage());
  } catch(e) {
    toast(e.message, 'error');
  }
};

window.deleteSource = function(secretName, displayName) {
  openModal(tr('modal.delete.title', {name: displayName}), `
    <div class="confirm-text">${tr('modal.delete.warning')}</div>
    <div class="form-actions">
      <button class="btn btn-secondary" onclick="closeModal()" title="Keep this source — close without deleting">${tr('common.cancel')}</button>
      <button class="btn btn-danger" onclick="confirmDeleteSource('${secretName}')" title="Permanently delete the source Secret. The CronJob is cascaded via OwnerReference; existing dumps in storage remain.">${tr('common.delete')}</button>
    </div>`);
};

window.confirmDeleteSource = async function(secretName) {
  try {
    await api('/api/sources/' + secretName, { method: 'DELETE' });
    toast(tr('toast.sourceDeleted'), 'success');
    closeModal();
    location.hash = '#/sources';
  } catch(e) { toast(e.message, 'error'); }
};

// Cached stats so SSE refreshes don't refire the per-destination probes.
// Same shape as _slowProbes on the dashboard. 60 s TTL + in-flight guard
// for the same reason — see the SLOW_PROBE_TTL_MS comment above.
let _destStatsCache = { stats: [], lastFetch: 0 };
let _destStatsInFlight = false;
async function refreshDestStats() {
  if (_destStatsInFlight) return;
  _destStatsInFlight = true;
  // Same render-after-inFlight-reset rule — see refreshFleetSummary.
  let success = false;
  try {
    let s;
    try {
      s = await api('/api/destination-stats');
    } catch(e) {
      return;
    }
    _destStatsCache = { stats: s || [], lastFetch: Date.now() };
    success = true;
  } finally {
    _destStatsInFlight = false;
  }
  if (success && (currentPage() === 'destinations' || currentPage() === 'dashboard')) {
    renderPage(currentPage(), false);
  }
}

// --- Destinations ---
async function renderDestinations(loading = true) {
  if (loading) showLoading();
  let dests = [];
  try {
    dests = (await api('/api/destinations')) || [];
  } catch(e) { toast(e.message, 'error'); }
  const stats = _destStatsCache.stats;
  if (loading && Date.now() - _destStatsCache.lastFetch > SLOW_PROBE_TTL_MS) refreshDestStats();
  const statsByName = {};
  stats.forEach(s => { statsByName[s.name] = s; });

  const destGetters = {
    createdAt:   d => parseTsRFC(d.createdAt),
    name:        d => (d.name || '').toLowerCase(),
    storageType: d => d.storageType || '',
  };
  const dst = sortState.destinations;
  const sortedDests = sortBy(dests, destGetters[dst.col] || destGetters.createdAt, dst.dir);

  content.innerHTML = `
    <div class="page-header">
      <div><h1>${tr('page.destinations.title')}</h1><div class="subtitle">${tr('page.destinations.subtitle')}</div></div>
      <div style="display:flex;gap:8px;align-items:center">
        ${dests.length > 0 ? renderSortControl('destinations', [
          ['createdAt', tr('table.createdAt')],['name', tr('common.name')],['storageType', tr('common.type')],
        ]) : ''}
        <button class="btn btn-primary" onclick="openDestForm()" title="Create a new storage destination (SFTP or S3-compatible) for backup uploads">+ ${tr('buttons.addDestination')}</button>
      </div>
    </div>
    ${dests.length === 0 ? `
    <div class="empty-state">
      <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/></svg>
      <h3>${tr('page.destinations.empty')}</h3>
      <p>${tr('page.destinations.emptyHint')}</p>
      <button class="btn btn-primary" onclick="openDestForm()" title="Create your first storage destination">+ ${tr('buttons.addDestination')}</button>
    </div>` : `
    <div style="display:grid;grid-template-columns:repeat(auto-fill,minmax(340px,1fr));gap:16px">
      ${sortedDests.map(d => {
        const st = statsByName[d.name];
        return `
      <div class="detail-card">
        <div style="display:flex;justify-content:space-between;align-items:start;margin-bottom:12px">
          <div>
            <div style="font-weight:600;font-size:15px;color:var(--text-heading)">${escHTML(d.name)}</div>
            <span class="badge badge-${d.storageType}" style="margin-top:6px">${d.storageType}</span>
          </div>
          <span class="dest-status" id="dest-status-${escAttr(d.secretName)}"></span>
        </div>
        <div class="detail-row"><span class="key">Secret</span><code class="val">${escHTML(d.secretName)}</code></div>
        <div class="detail-row"><span class="key">${tr('form.destination.label.host')}</span><span class="val">${escHTML(d.host || '—')}</span></div>
        <div class="detail-row"><span class="key">${tr('form.destination.label.pathPrefix')}</span><span class="val">${escHTML(d.pathPrefix || '/')}</span></div>
        <div class="detail-row"><span class="key">${tr('table.createdAt')}</span><span class="val">${d.createdAt ? timeAgo(d.createdAt) : '—'}</span></div>
        ${st && !st.error ? `
        <div style="margin-top:8px;padding-top:8px;border-top:1px solid var(--border)">
          <div class="detail-row"><span class="key">${tr('stat.backups')}</span><span class="val">${st.backupCount}</span></div>
          <div class="detail-row"><span class="key">${tr('stat.totalSize')}</span><span class="val">${humanBytes(st.totalSizeBytes)}</span></div>
          <div class="detail-row"><span class="key">${tr('stat.oldest')}</span><span class="val">${st.oldestBackup ? timeAgo(st.oldestBackup) : '—'}</span></div>
          <div class="detail-row"><span class="key">${tr('stat.newest')}</span><span class="val">${st.newestBackup ? timeAgo(st.newestBackup) : '—'}</span></div>
        </div>` : st && st.error ? `
        <div style="margin-top:8px;padding-top:8px;border-top:1px solid var(--border);color:var(--danger);font-size:12px">
          ${tr('card.storageUnreachable')}: ${escHTML(st.error)}
        </div>` : ''}
        <div style="display:flex;gap:6px;margin-top:12px;justify-content:flex-end">
          <button class="btn btn-ghost btn-sm" onclick="testDestConnection('${escJS(d.secretName)}','${escJS(d.name)}')" title="Probe this destination — verifies SSH/SFTP login or S3 bucket access without uploading anything">&#128268; ${tr('buttons.test')}</button>
          <button class="btn btn-ghost btn-sm" onclick="openDestForm('${escJS(d.secretName)}')" title="Edit this destination's credentials and connection details">${tr('common.edit')}</button>
          <button class="btn btn-ghost btn-sm" style="color:var(--danger)" onclick="deleteDest('${escJS(d.secretName)}','${escJS(d.name)}')" title="Delete this destination Secret. Existing dumps stored at this destination remain intact.">${tr('common.delete')}</button>
        </div>
      </div>`;
      }).join('')}
    </div>`}`;
}

// --- Destination Form ---
window.openDestForm = function(secretName) {
  const isEdit = !!secretName;
  const title = isEdit ? tr('form.destination.editTitle') : tr('form.destination.createTitle');

  const sftpFields = `
    <div class="form-row"><div class="form-group"><label>${tr('form.destination.label.host')} *</label><input name="data_host" required></div>
      <div class="form-group"><label>${tr('form.destination.label.port')}</label><input name="data_port" placeholder="22"></div></div>
    <div class="form-group"><label>${tr('form.destination.label.username')} *</label><input name="data_username" required></div>
    <div class="form-group"><label>${tr('form.destination.label.authMethod')} *</label>
      <div style="display:flex;gap:16px;margin-top:4px">
        <label style="font-weight:normal;cursor:pointer"><input type="radio" name="sftpAuthMethod" value="key" checked onchange="toggleSFTPAuth(this.value)"> ${tr('form.destination.label.authMethodKey')}</label>
        <label style="font-weight:normal;cursor:pointer"><input type="radio" name="sftpAuthMethod" value="password" onchange="toggleSFTPAuth(this.value)"> ${tr('form.destination.label.authMethodPassword')}</label>
      </div>
    </div>
    <div class="form-group" data-sftp-auth="key"><label>${tr('form.destination.label.sshKey')} *</label><textarea name="data_ssh-private-key" rows="3" placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"></textarea></div>
    <div class="form-group" data-sftp-auth="password" style="display:none"><label>${tr('form.destination.label.password')} *</label><input name="data_password" type="password" autocomplete="new-password"></div>
    <div class="form-group"><label>${tr('form.destination.label.knownHosts')}</label>
      <textarea name="data_known-hosts" rows="2" placeholder="ssh-keyscan output"></textarea>
      <div class="hint">${tr('form.destination.hint.knownHosts')}</div>
    </div>
    <div class="form-group">
      <label style="font-weight:normal;cursor:pointer">
        <input type="checkbox" name="data_insecure-skip-host-verify" value="true">
        ${tr('form.destination.label.insecureSkipHostVerify')}
      </label>
      <div class="hint hint-warn">${tr('form.destination.hint.insecureSkipHostVerify')}</div>
    </div>`;

  const s3Fields = `
    <div class="form-group"><label>${tr('form.destination.label.endpoint')} *</label><input name="data_endpoint" required placeholder="s3.amazonaws.com"></div>
    <div class="form-row"><div class="form-group"><label>${tr('form.destination.label.bucket')} *</label><input name="data_bucket" required></div>
      <div class="form-group"><label>${tr('form.destination.label.region')}</label><input name="data_region" placeholder="${tr('form.destination.placeholder.region')}"></div></div>
    <div class="form-row"><div class="form-group"><label>${tr('form.destination.label.accessKey')}</label><input name="data_access-key"></div>
      <div class="form-group"><label>${tr('form.destination.label.secretKey')}</label><input name="data_secret-key" type="password"></div></div>`;

  const ftpsFields = `
    <div class="form-row"><div class="form-group"><label>${tr('form.destination.label.host')} *</label><input name="data_host" required></div>
      <div class="form-group"><label>${tr('form.destination.label.port')}</label><input name="data_port" placeholder="21"></div></div>
    <div class="form-group"><label>${tr('form.destination.label.username')} *</label><input name="data_username" required></div>
    <div class="form-group"><label>${tr('form.destination.label.password')} *</label><input name="data_password" type="password" autocomplete="new-password" required></div>
    <div class="form-group"><label>${tr('form.destination.label.tlsMode')}</label>
      <select name="data_tls-mode">
        <option value="explicit">${tr('form.destination.tlsMode.explicit')}</option>
        <option value="implicit">${tr('form.destination.tlsMode.implicit')}</option>
      </select>
      <div class="hint">${tr('form.destination.hint.tlsMode')}</div>
    </div>
    <div class="form-group">
      <label style="font-weight:normal;cursor:pointer">
        <input type="checkbox" name="data_insecure-skip-cert-verify" value="true">
        ${tr('form.destination.label.insecureSkipCertVerify')}
      </label>
      <div class="hint hint-warn">${tr('form.destination.hint.insecureSkipCertVerify')}</div>
    </div>`;

  openModal(title, `<form id="destForm" onsubmit="submitDestForm(event, '${secretName || ''}')">
    <div class="form-row">
      <div class="form-group"><label>${tr('common.name')} *</label><input name="name" required placeholder="${tr('form.destination.placeholder.name')}" ${isEdit ? 'disabled' : ''}></div>
      <div class="form-group"><label>${tr('form.destination.label.type')} *</label>
        <select name="storageType" required onchange="toggleDestFields(this.value)">
          <option value="">${tr('form.source.placeholder.selectType')}</option>
          <option value="sftp">${tr('form.destination.type.sftp')}</option>
          <option value="hetzner-sftp">${tr('form.destination.type.hetznerSftp')}</option>
          <option value="ftps">${tr('form.destination.type.ftps')}</option>
          <option value="s3">${tr('form.destination.type.s3')}</option>
        </select></div>
    </div>
    <div class="form-group"><label>${tr('form.destination.label.pathPrefix')}</label><input name="pathPrefix" placeholder="${tr('form.destination.placeholder.pathPrefix')}"></div>
    <div id="destTypeFields"></div>
    <div class="form-actions">
      <button type="button" class="btn btn-secondary" onclick="closeModal()" title="Discard changes and close this dialog">${tr('common.cancel')}</button>
      <button type="submit" class="btn btn-primary" title="${isEdit ? 'Save the modified destination Secret — the operator picks up changes on the next run' : 'Create the destination Secret — sources can target it via the destinations annotation or pick it up on next run if the source has no allow-list'}">${isEdit ? tr('common.update') : tr('common.create')}</button>
    </div>
  </form>
  <div id="destSFTPTemplate" style="display:none">${sftpFields}</div>
  <div id="destS3Template" style="display:none">${s3Fields}</div>
  <div id="destFTPSTemplate" style="display:none">${ftpsFields}</div>`);

  if (isEdit) {
    api('/api/destinations/' + secretName).then(d => {
      const f = $('#destForm');
      f.name.value = d.name || '';
      f.storageType.value = d.storageType || '';
      f.pathPrefix.value = d.pathPrefix || '';
      toggleDestFields(d.storageType);
      if (d.data) {
        // For SFTP destinations, preselect the auth method based on which
        // sensitive field the API masked. Both fields are masked as '***'
        // so we treat presence (not value) as the signal. password wins
        // when both are stored — matches the backend's preference order.
        if (d.storageType === 'sftp' || d.storageType === 'hetzner-sftp') {
          const hasPwd = Object.prototype.hasOwnProperty.call(d.data, 'password');
          const hasKey = Object.prototype.hasOwnProperty.call(d.data, 'ssh-private-key');
          if (hasPwd && !hasKey) {
            const r = f.querySelector('input[name="sftpAuthMethod"][value="password"]');
            if (r) { r.checked = true; toggleSFTPAuth('password'); }
          }
        }
        Object.entries(d.data).forEach(([k, v]) => {
          const inp = f.querySelector(`[name="data_${k}"]`);
          if (!inp) return;
          if (inp.type === 'checkbox') {
            inp.checked = (v === 'true');
          } else if (v !== '***') {
            inp.value = v;
          }
        });
      }
    }).catch(e => toast(tr('toast.loadFailed', {error: e.message}), 'error'));
  }
};

window.toggleDestFields = function(type) {
  const container = $('#destTypeFields');
  if (type === 'sftp' || type === 'hetzner-sftp') {
    container.innerHTML = $('#destSFTPTemplate').innerHTML;
  } else if (type === 's3') {
    container.innerHTML = $('#destS3Template').innerHTML;
  } else if (type === 'ftps') {
    container.innerHTML = $('#destFTPSTemplate').innerHTML;
  } else {
    container.innerHTML = '';
  }
};

// Show only the input matching the selected SFTP auth method. The hidden
// input is left in the DOM so the user can flip back without re-typing
// during a single form session; submitDestForm drops the unselected
// method's value before sending so we don't ship both up the wire.
window.toggleSFTPAuth = function(method) {
  $$('[data-sftp-auth]').forEach(el => {
    el.style.display = el.getAttribute('data-sftp-auth') === method ? '' : 'none';
  });
};

// Sensitive fields are masked as *** on edit, so an empty form value
// means "user did not retype it, keep the stored secret" rather than
// "user cleared it". Non-sensitive fields (host, port, known-hosts, …)
// follow the opposite rule: empty after edit means "user actually
// emptied it, remove the field from the Secret" — otherwise the form
// silently ignores clears and the user wonders why the old value is
// stuck. Mirror of the backend's sensitiveKeys set in handlers_api.go.
const DEST_SENSITIVE_KEYS = new Set([
  'password', 'ssh-private-key', 'secret-key', 'access-key', 'secret-access-key',
]);

window.submitDestForm = async function(e, secretName) {
  e.preventDefault();
  const f = e.target;
  const data = {};
  const removeKeys = [];
  $$('[name^="data_"]', f).forEach(inp => {
    const key = inp.name.replace('data_', '');
    // Checkboxes carry their value attribute regardless of checked state;
    // unchecked must mean "drop the key", not "store the literal string".
    if (inp.type === 'checkbox') {
      if (inp.checked && inp.value) data[key] = inp.value;
      else removeKeys.push(key);
      return;
    }
    if (inp.value) {
      data[key] = inp.value;
    } else if (secretName && !DEST_SENSITIVE_KEYS.has(key)) {
      // Edit-mode only: explicit clear of a non-sensitive field.
      // On create we just omit the empty field.
      removeKeys.push(key);
    }
  });
  // For SFTP, only ship the auth field the user actually chose. Switching
  // from key→password during an edit would otherwise leave the previous
  // credential in the Secret because PUT merges rather than replaces —
  // removeKeys is the explicit drop list. (The unchecked-checkbox case is
  // already handled by the main loop above.)
  const sType = f.storageType.value;
  if (sType === 'sftp' || sType === 'hetzner-sftp') {
    const sel = f.querySelector('input[name="sftpAuthMethod"]:checked');
    const method = sel ? sel.value : 'key';
    if (method === 'key') {
      delete data['password'];
      if (!removeKeys.includes('password')) removeKeys.push('password');
    } else {
      delete data['ssh-private-key'];
      if (!removeKeys.includes('ssh-private-key')) removeKeys.push('ssh-private-key');
    }
  }
  const body = {
    name: f.name.value,
    storageType: f.storageType.value,
    pathPrefix: f.pathPrefix.value,
    data: data,
    removeKeys: removeKeys,
  };
  try {
    if (secretName) {
      await api('/api/destinations/' + secretName, { method: 'PUT', body: JSON.stringify(body) });
      toast(tr('toast.destinationUpdated'), 'success');
    } else {
      await api('/api/destinations', { method: 'POST', body: JSON.stringify(body) });
      toast(tr('toast.destinationCreated'), 'success');
    }
    closeModal();
    renderPage(currentPage());
  } catch(e) { toast(e.message, 'error'); }
};

window.testDestConnection = async function(secretName, displayName) {
  const el = document.getElementById('dest-status-' + secretName);
  if (el) { el.innerHTML = `<span class="badge badge-pending">${tr('badge.testing')}</span>`; }
  try {
    const result = await api('/api/destinations/' + secretName + '/test', { method: 'POST' });
    if (result.ok) {
      if (el) el.innerHTML = `<span class="badge badge-ok">${tr('badge.connected')}</span>`;
      toast(displayName + ': ' + tr('badge.ok'), 'success');
    } else {
      if (el) el.innerHTML = `<span class="badge badge-failed" title="${escAttr(result.error || '')}">${tr('badge.failed')}</span>`;
      toast(displayName + ': ' + (result.error || tr('badge.failed')), 'error');
    }
  } catch(e) {
    if (el) el.innerHTML = `<span class="badge badge-failed">${tr('badge.error')}</span>`;
    toast(tr('toast.loadFailed', {error: e.message}), 'error');
  }
};

window.deleteDest = function(secretName, displayName) {
  openModal(tr('modal.delete.title', {name: displayName}), `
    <div class="confirm-text">${tr('modal.delete.warning')}</div>
    <div class="form-actions">
      <button class="btn btn-secondary" onclick="closeModal()" title="Keep this destination — close without deleting">${tr('common.cancel')}</button>
      <button class="btn btn-danger" onclick="confirmDeleteDest('${secretName}')" title="Permanently delete the destination Secret. Sources will skip it on next run; existing dumps in storage remain.">${tr('common.delete')}</button>
    </div>`);
};

window.confirmDeleteDest = async function(secretName) {
  try {
    await api('/api/destinations/' + secretName, { method: 'DELETE' });
    toast(tr('toast.destinationDeleted'), 'success');
    closeModal();
    renderDestinations();
  } catch(e) { toast(e.message, 'error'); }
};

// --- Alert descriptions for each alert type ---
// Icons stay static (universal emoji); titles, desc and action come from
// the active dictionary so a language switch updates the text without
// shipping per-language icons.
const ALERT_ICONS = {
  BackupOverdue: '⏰',
  BackupDestinationFailing: '💾',
  BackupDumpSizeCollapsed: '📉',
  BackupSchemaChanged: '🔧',
  BackupAnomaliesAppearing: '⚠️',
  BackupLastRunFailed: '❌',
  BackupSucceeded: '🟢',
  BackupOperatorTestAlert: '🧪',
};
const ALERT_DESCRIPTIONS = ALERT_ICONS; // legacy alias used by the rules-reference table

function getAlertDescription(alertname) {
  const icon = ALERT_ICONS[alertname] || '🔔';
  const titleKey = 'alertDesc.' + alertname + '.title';
  const descKey  = 'alertDesc.' + alertname + '.desc';
  const actKey   = 'alertDesc.' + alertname + '.action';
  // tr() falls back to the raw key when missing; that's how we detect
  // "unknown alertname" and substitute the _default text without an extra branch.
  const title = tr(titleKey) === titleKey ? alertname : tr(titleKey);
  const desc  = tr(descKey)  === descKey  ? tr('alertDesc._default.desc')   : tr(descKey);
  const action = tr(actKey)  === actKey   ? tr('alertDesc._default.action') : tr(actKey);
  return { title, icon, desc, action };
}

// --- Alerts ---
async function renderAlerts(loading = true) {
  if (loading) showLoading();

  // Fetch alerts + status in parallel
  let alertsResp = null, statusResp = null;
  let errMsg = '';
  try {
    const [a, s] = await Promise.all([
      api('/api/alerts').catch(e => { errMsg = e.message || 'unknown error'; return null; }),
      api('/api/alerts/status').catch(() => null),
    ]);
    alertsResp = a;
    statusResp = s;
  } catch(e) {
    errMsg = e.message || 'unknown error';
  }

  const items = alertsResp ? (alertsResp.items || []) : [];
  const counts = alertsResp ? (alertsResp.counts || {}) : {};
  const amURL = alertsResp ? alertsResp.alertmanagerUrl : '';
  if (alertsResp) updateAlertsPill(counts);

  // --- Connection Status Banner ---
  let statusBanner = '';
  if (statusResp) {
    const prom = statusResp.prometheus || {};
    const am = statusResp.alertmanager || {};
    const mode = statusResp.mode || 'none';

    const promStatus = !prom.configured
      ? `<span class="badge badge-pending">${tr('page.alerts.notConfigured')}</span>`
      : prom.reachable
        ? `<span class="badge badge-ok">${tr('page.alerts.connected')}</span>${prom.version ? ' <span style="font-size:11px;color:var(--text-muted)">v' + escHTML(prom.version) + '</span>' : ''}`
        : `<span class="badge badge-critical">${tr('page.alerts.unreachable')}</span> <span style="font-size:11px;color:var(--text-muted)">${escHTML(prom.error || '')}</span>`;

    const amStatus = !am.configured
      ? `<span class="badge badge-pending">${tr('page.alerts.notConfigured')}</span>`
      : am.reachable
        ? `<span class="badge badge-ok">${tr('page.alerts.connected')}</span>${am.version ? ' <span style="font-size:11px;color:var(--text-muted)">v' + escHTML(am.version) + '</span>' : ''}`
        : `<span class="badge badge-critical">${tr('page.alerts.unreachable')}</span> <span style="font-size:11px;color:var(--text-muted)">${escHTML(am.error || '')}</span>`;

    const modeLabel = mode === 'prometheus' ? tr('page.alerts.modePrometheus')
      : mode === 'local' ? tr('page.alerts.modeLocal')
      : tr('page.alerts.modeNone');
    const modeBadge = mode === 'prometheus' ? 'ok' : mode === 'local' ? 'warning' : 'critical';

    statusBanner = `
      <div class="table-card" style="margin-bottom:16px">
        <div style="padding:12px 16px;border-bottom:1px solid var(--border)">
          <strong>${tr('page.alerts.connectionStatus')}</strong>
          <span class="badge badge-${modeBadge}" style="margin-left:8px">${tr('page.alerts.mode')}: ${escHTML(modeLabel)}</span>
        </div>
        <table>
          <tbody>
            <tr><td style="width:180px;font-weight:500">Prometheus</td><td>${promStatus}</td><td style="font-size:12px;color:var(--text-muted)">${prom.configured ? escHTML(prom.url || '') : 'Set <code>alerts.prometheusURL</code> in Helm values'}</td></tr>
            <tr><td style="font-weight:500">Alertmanager</td><td>${amStatus}</td><td style="font-size:12px;color:var(--text-muted)">${am.configured ? escHTML(am.url || '') : 'Set <code>alerts.alertmanagerURL</code> in Helm values'}</td></tr>
          </tbody>
        </table>
      </div>`;
  }

  // --- Setup Guide (shown when nothing is configured) ---
  let setupGuide = '';
  if (statusResp && !statusResp.prometheus.configured && !statusResp.alertmanager.configured) {
    setupGuide = `
      <div class="table-card" style="margin-bottom:16px">
        <div style="padding:12px 16px;border-bottom:1px solid var(--border)">
          <strong>Setup Guide</strong> — How to connect Prometheus &amp; Alertmanager
        </div>
        <div style="padding:16px;font-size:13px;line-height:1.7">
          <p style="margin:0 0 12px"><strong>The backup operator works in 3 modes:</strong></p>
          <table style="width:100%;margin-bottom:16px">
            <thead><tr><th>Mode</th><th>What you need</th><th>What you get</th></tr></thead>
            <tbody>
              <tr><td><span class="badge badge-ok">Full</span></td><td>Prometheus + Alertmanager</td><td>Real-time alerts with debounce + notifications (Slack, Email, PagerDuty)</td></tr>
              <tr><td><span class="badge badge-warning">Prometheus only</span></td><td>Prometheus (e.g. kube-prometheus-stack)</td><td>Canonical alert evaluation with <code>for:</code> duration, visible in this UI</td></tr>
              <tr><td><span class="badge badge-pending">Local</span></td><td>Nothing (built-in)</td><td>Immediate alert evaluation against operator metrics — no external setup needed</td></tr>
            </tbody>
          </table>

          <p style="margin:0 0 8px"><strong>Step 1: Install kube-prometheus-stack</strong> (if not already installed)</p>
          <pre style="background:var(--bg-secondary);padding:12px;border-radius:6px;overflow-x:auto;font-size:12px;margin:0 0 16px">helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm install kube-prometheus-stack prometheus-community/kube-prometheus-stack \\
  -n monitoring --create-namespace</pre>

          <p style="margin:0 0 8px"><strong>Step 2: Configure the backup operator</strong></p>
          <pre style="background:var(--bg-secondary);padding:12px;border-radius:6px;overflow-x:auto;font-size:12px;margin:0 0 16px"># values.yaml
alerts:
  prometheusURL: "http://prometheus-operated.monitoring.svc.cluster.local:9090"
  alertmanagerURL: "http://alertmanager-operated.monitoring.svc.cluster.local:9093"

# Must match your kube-prometheus-stack release name
prometheusReleaseLabel: "kube-prometheus-stack"</pre>

          <p style="margin:0 0 8px"><strong>Step 3: Upgrade the release</strong></p>
          <pre style="background:var(--bg-secondary);padding:12px;border-radius:6px;overflow-x:auto;font-size:12px;margin:0 0 16px">helm upgrade backup-operator ./charts/backup-operator -n backup -f values.yaml</pre>

          <p style="margin:0 0 8px"><strong>Step 4: Configure notifications in Alertmanager</strong></p>
          <details style="margin-bottom:8px">
            <summary style="cursor:pointer;font-weight:500">Slack example</summary>
            <pre style="background:var(--bg-secondary);padding:12px;border-radius:6px;overflow-x:auto;font-size:12px;margin:8px 0 0"># alertmanager-config (Secret or inline in kube-prometheus-stack values)
route:
  receiver: default
  routes:
    - match:
        alertname: =~"^Backup.*"
      receiver: backup-slack
receivers:
  - name: default
    # ...
  - name: backup-slack
    slack_configs:
      - api_url: "https://hooks.slack.com/services/YOUR/WEBHOOK/URL"
        channel: "#backup-alerts"
        title: '{{ .CommonLabels.alertname }}'
        text: '{{ .CommonAnnotations.summary }}'
        send_resolved: true</pre>
          </details>
          <details>
            <summary style="cursor:pointer;font-weight:500">Email example</summary>
            <pre style="background:var(--bg-secondary);padding:12px;border-radius:6px;overflow-x:auto;font-size:12px;margin:8px 0 0"># alertmanager-config
route:
  receiver: default
  routes:
    - match:
        alertname: =~"^Backup.*"
      receiver: backup-email
receivers:
  - name: default
    # ...
  - name: backup-email
    email_configs:
      - to: "ops-team@yourcompany.com"
        from: "alertmanager@yourcompany.com"
        smarthost: "smtp.yourcompany.com:587"
        auth_username: "alertmanager@yourcompany.com"
        auth_password: "your-smtp-password"
        send_resolved: true</pre>
          </details>
        </div>
      </div>`;
  }

  // --- Source mode banner ---
  const localCount = items.filter(a => a.source === 'local').length;
  const sourceBanner = localCount === items.length && items.length > 0
    ? `<div class="banner banner-info" style="margin-bottom:12px">${tr('banner.localEvaluator')}</div>`
    : '';

  // --- Alert info/error banner ---
  let alertError = '';
  if (!alertsResp) {
    alertError = `<div class="banner banner-warning" style="margin-bottom:12px">${tr('banner.alertsLoadFailed', {error: errMsg})}</div>`;
  }

  // --- Test alert button ---
  const testBtn = statusResp && statusResp.alertmanager && statusResp.alertmanager.reachable
    ? '<button class="btn btn-secondary btn-sm" onclick="sendTestAlert()" id="btnTestAlert">🧪 ' + tr('buttons.sendTest') + '</button>'
    : '';

  content.innerHTML = `
    <div class="page-header">
      <div>
        <h1>${tr('page.alerts.title')}</h1>
        <div class="subtitle">${tr('page.alerts.subtitle')}${amURL ? ' — <a href="' + escAttr(amURL) + '" target="_blank">' + tr('page.alerts.openInAlertmanager') + '</a>' : ''}</div>
      </div>
      <div style="display:flex;gap:8px">
        ${testBtn}
        <button class="btn btn-secondary btn-sm" onclick="renderAlerts(false)">↻ ${tr('buttons.refresh')}</button>
      </div>
    </div>
    ${statusBanner}
    ${setupGuide}
    ${alertError}
    ${sourceBanner}
    <div class="stats-row">
      <div class="stat-card"><div class="label">${tr('stat.critical')}</div><div class="value${counts.critical > 0 ? ' bad' : ''}">${counts.critical || 0}</div></div>
      <div class="stat-card"><div class="label">${tr('stat.warning')}</div><div class="value${counts.warning > 0 ? ' bad' : ''}">${counts.warning || 0}</div></div>
      <div class="stat-card"><div class="label">${tr('stat.info')}</div><div class="value">${counts.info || 0}</div></div>
    </div>
    ${items.length === 0
      ? `<div class="empty-state"><h3>${tr('page.alerts.empty')}</h3><p>${tr('page.alerts.empty')}</p></div>`
      : `<div class="table-card">
        <table>
          <thead><tr>
            <th>${tr('table.severity')}</th><th>${tr('table.alert')}</th><th>${tr('table.target')}</th><th>${tr('table.destination')}</th>
            <th>${tr('table.sinceFiring')}</th><th>Source</th><th>${tr('table.summary')}</th>
          </tr></thead>
          <tbody>${items.map(a => {
            const info = getAlertDescription(a.alertname);
            return `<tr>
            <td><span class="badge badge-${escAttr(a.severity || 'info')}">${escHTML(a.severity || 'info')}</span></td>
            <td>
              <details style="cursor:pointer">
                <summary><code style="font-size:12px">${escHTML(info.icon)} ${escHTML(info.title)}</code></summary>
                <div style="padding:8px 0;font-size:12px;line-height:1.6">
                  <p style="margin:0 0 6px">${escHTML(info.desc)}</p>
                  <p style="margin:0;color:var(--text-muted)"><strong>${tr('table.action')}:</strong> ${escHTML(info.action)}</p>
                </div>
              </details>
            </td>
            <td>${a.target ? '<a href="#/target/' + escAttr(a.target) + '" style="color:var(--accent)">' + escHTML(a.target) + '</a>' : '—'}</td>
            <td>${a.destination ? escHTML(a.destination) : '—'}</td>
            <td style="font-size:12px;color:var(--text-muted)">${a.activeSince ? timeAgo(a.activeSince) : '—'}</td>
            <td><span class="badge badge-${a.source === 'prometheus' ? 'ok' : 'pending'}" title="${a.source === 'prometheus' ? 'From Prometheus — honors rule for: duration' : 'Local heuristic — fires immediately, no for: debounce'}">${escHTML(a.source || '?')}</span></td>
            <td style="font-size:13px">${escHTML(a.summary || '')}</td>
          </tr>`;
          }).join('')}</tbody>
        </table>
      </div>`}

    <div class="table-card" style="margin-top:24px">
      <div style="padding:12px 16px;border-bottom:1px solid var(--border)">
        <strong>${tr('page.alerts.rulesReference')}</strong>
        <span style="font-size:12px;color:var(--text-muted);margin-left:8px">${tr('page.alerts.rulesReferenceSub', {count: Object.keys(ALERT_DESCRIPTIONS).filter(k => k !== 'BackupOperatorTestAlert').length})}</span>
      </div>
      <table>
        <thead><tr><th>${tr('page.alerts.alertCol')}</th><th>${tr('page.alerts.severityCol')}</th><th>${tr('page.alerts.conditionCol')}</th><th>${tr('page.alerts.descriptionCol')}</th></tr></thead>
        <tbody>
          ${Object.keys(ALERT_DESCRIPTIONS).filter(k => k !== 'BackupOperatorTestAlert').map(name => {
            const info = getAlertDescription(name);
            return `<tr>
            <td><code style="font-size:12px">${escHTML(info.icon)} ${escHTML(info.title)}</code></td>
            <td><span class="badge badge-${name === 'BackupDumpSizeCollapsed' ? 'critical' : name === 'BackupSchemaChanged' || name === 'BackupSucceeded' ? 'info' : 'warning'}">${name === 'BackupDumpSizeCollapsed' ? 'critical' : name === 'BackupSchemaChanged' || name === 'BackupSucceeded' ? 'info' : 'warning'}</span></td>
            <td style="font-size:12px">${escHTML(info.desc)}</td>
            <td style="font-size:12px;color:var(--text-muted)">${escHTML(info.action)}</td>
          </tr>`;
          }).join('')}
        </tbody>
      </table>
    </div>`;
}

// Send test alert to Alertmanager
window.sendTestAlert = async function() {
  const btn = document.getElementById('btnTestAlert');
  if (btn) { btn.disabled = true; btn.textContent = tr('common.loading'); }
  try {
    const resp = await api('/api/alerts/test', { method: 'POST' });
    toast(resp.message || tr('toast.testAlertSent'), 'success');
  } catch(e) {
    toast(e.message || tr('toast.saveFailed', {error: e.message}), 'error');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = '🧪 ' + tr('buttons.sendTest'); }
  }
};

// updateAlertsPill keeps the sidebar counter in sync with the latest /api/alerts
// response. We call it after every alert refresh and once on startup; the SSE
// refresh tick triggers periodic re-fetches when the user is on other pages
// so the pill stays current without polling.
function updateAlertsPill(counts) {
  const el = $('#navAlertsPill');
  if (!el) return;
  const total = (counts.critical || 0) + (counts.warning || 0) + (counts.info || 0);
  if (total === 0) { el.hidden = true; return; }
  el.hidden = false;
  el.textContent = total;
  el.className = 'alerts-pill ' + (counts.critical > 0 ? 'critical' : counts.warning > 0 ? 'warning' : 'info');
}

async function refreshAlertsPill() {
  try {
    const resp = await api('/api/alerts');
    if (resp && resp.counts) updateAlertsPill(resp.counts);
  } catch(_) { /* pill stays as-is on error */ }
}

// --- Jobs ---
let jobProgressTimer = null;

function fmtDurationShort(sec) {
  if (sec == null || !isFinite(sec) || sec < 0) return '';
  if (sec < 60) return `${Math.round(sec)}s`;
  const m = Math.floor(sec / 60), s = Math.round(sec % 60);
  if (m < 60) return s ? `${m}m ${s}s` : `${m}m`;
  const h = Math.floor(m / 60), mm = m % 60;
  return mm ? `${h}h ${mm}m` : `${h}h`;
}

// fmtNextRun renders a CronJob's next fire time as "in 4h 32m (02:00)"
// or "in 12d (Mon 02:00)" depending on horizon. Pass an ISO timestamp
// string from the target's NextRun field. Returns '' if input is falsy
// or already in the past (next tick is imminent / SSE catch-up race).
function fmtNextRun(iso) {
  if (!iso) return '';
  const t = new Date(iso).getTime();
  if (!isFinite(t)) return '';
  const sec = (t - Date.now()) / 1000;
  if (sec <= 0) return tr('target.nextRunImminent') || 'imminent';
  const d = new Date(t);
  // Inside 24h: relative + "at HH:MM" today/tomorrow
  if (sec < 86400) {
    const hhmm = d.toLocaleTimeString([], {hour: '2-digit', minute: '2-digit'});
    return `${tr('target.in') || 'in'} ${fmtDurationShort(sec)} (${hhmm})`;
  }
  // >24h: show day-of-week + time + relative duration
  const dayShort = d.toLocaleDateString([], {weekday: 'short'});
  const hhmm = d.toLocaleTimeString([], {hour: '2-digit', minute: '2-digit'});
  return `${tr('target.in') || 'in'} ${fmtDurationShort(sec)} (${dayShort} ${hhmm})`;
}

// renderProgressCell builds the Duration cell content. For non-running jobs
// it returns the static formatted duration. For running jobs it renders a
// progress bar driven by elapsed time and an estimate from past runs (when
// available); without an estimate, it shows elapsed-only.
function renderProgressCell(j) {
  if (j.status !== 'running') return j.duration || '—';
  const startMs = parseTsRFC(j.startTime);
  if (!startMs) return '—';
  const elapsed = (Date.now() - startMs) / 1000;
  const est = j.estimatedDurationSeconds || 0;
  const sample = j.estimateSampleSize || 0;
  if (est > 0) {
    const ratio = Math.min(elapsed / est, 0.99);
    const overdue = elapsed > est;
    const remaining = Math.max(est - elapsed, 0);
    const fillCls = overdue ? 'job-progress-fill overdue' : 'job-progress-fill';
    const labelCls = overdue ? 'job-progress-label overdue' : 'job-progress-label';
    const label = overdue
      ? `läuft ${fmtDurationShort(elapsed)} — länger als üblich (Ø ${fmtDurationShort(est)}, n=${sample})`
      : `${fmtDurationShort(elapsed)} / ~${fmtDurationShort(est)} — ${fmtDurationShort(remaining)} verbleibend (n=${sample})`;
    return `<div class="job-progress">
      <div class="job-progress-bar"><div class="${fillCls}" style="width:${(ratio * 100).toFixed(1)}%"></div></div>
      <div class="${labelCls}">${escHTML(label)}</div>
    </div>`;
  }
  return `<span style="color:var(--text-muted)">läuft seit ${fmtDurationShort(elapsed)}</span>`;
}

async function renderJobs(loading = true) {
  if (loading) showLoading();
  let jobs = [];
  try { jobs = (await api('/api/jobs')) || []; } catch(e) { toast(e.message, 'error'); }

  const jobGetters = {
    name:      j => j.name || '',
    target:    j => (j.target || '').toLowerCase(),
    status:    j => j.status || '',
    startTime: j => parseTsRFC(j.startTime),
    duration:  j => parseDurationSec(j.duration),
  };
  const js = sortState.jobs;
  const sortedJobs = sortBy(jobs, jobGetters[js.col] || jobGetters.startTime, js.dir);

  content.innerHTML = `
    <div class="page-header">
      <div><h1>${tr('page.jobs.title')}</h1><div class="subtitle">${tr('page.jobs.subtitle')}</div></div>
    </div>
    <div class="table-card">
      ${jobs.length === 0 ? `<div class="empty-state"><h3>${tr('page.jobs.empty')}</h3><p>Jobs appear when backups run — either on schedule or triggered manually.</p></div>` : `
      <table>
        <thead><tr>
          <th class="num row-num">#</th>
          <th class="sortable" onclick="toggleSort('jobs','name')">${tr('nav.jobs')}${sortIndicator('jobs','name')}</th>
          <th class="sortable" onclick="toggleSort('jobs','target')">${tr('table.target')}${sortIndicator('jobs','target')}</th>
          <th class="sortable" onclick="toggleSort('jobs','status')">${tr('table.status')}${sortIndicator('jobs','status')}</th>
          <th class="sortable" onclick="toggleSort('jobs','startTime')">Started${sortIndicator('jobs','startTime')}</th>
          <th class="sortable" onclick="toggleSort('jobs','duration')">${tr('table.duration')}${sortIndicator('jobs','duration')}</th>
        </tr></thead>
        <tbody>${sortedJobs.map((j, i) => `<tr data-job-name="${escAttr(j.name)}">
          <td class="num row-num">${i + 1}</td>
          <td style="font-family:ui-monospace,monospace;font-size:12px">${escHTML(j.name)}</td>
          <td><strong>${escHTML(j.target || '—')}</strong></td>
          <td><span class="badge badge-${j.status}">${j.status}</span></td>
          <td style="color:var(--text-muted);font-size:12px">${j.startTime ? new Date(j.startTime).toLocaleString() : '—'}</td>
          <td class="job-duration-cell" style="font-size:12px">${renderProgressCell(j)}</td>
        </tr>`).join('')}</tbody>
      </table>`}
    </div>`;

  // Tick progress bars locally between SSE refreshes so they advance smoothly
  // without re-fetching /api/jobs every second.
  if (jobProgressTimer) { clearInterval(jobProgressTimer); jobProgressTimer = null; }
  const runningJobs = sortedJobs.filter(j => j.status === 'running');
  if (runningJobs.length > 0) {
    jobProgressTimer = setInterval(() => {
      // If user navigated away, the cells will be gone; guard against null.
      runningJobs.forEach(j => {
        const row = document.querySelector(`tr[data-job-name="${CSS.escape(j.name)}"] .job-duration-cell`);
        if (row) row.innerHTML = renderProgressCell(j);
      });
    }, 1000);
  }
}

// --- Audit log ---
let auditFilter = 'all';
async function renderAudit(loading = true) {
  if (loading) showLoading();
  let data = { entries: [], total: 0, limit: 200 };
  try {
    const url = '/api/audit-log' + (auditFilter !== 'all' ? '?category=' + encodeURIComponent(auditFilter) : '');
    data = (await api(url)) || data;
  } catch(e) { toast(e.message, 'error'); }

  const entries = data.entries || [];
  const truncated = data.total > entries.length;

  const categories = [
    ['all',       tr('page.audit.category.all'),       'Show every audit event'],
    ['backup',    tr('page.audit.category.backup'),    'BackupStarted / BackupCompleted / BackupFailed'],
    ['retention', tr('page.audit.category.retention'), 'Old dumps deleted by retention policy'],
    ['keys',      tr('page.audit.category.keys'),      'Age recipient additions, removals, refusals'],
    ['config',    tr('page.audit.category.config'),    'Source / destination / settings changes'],
    ['other',     tr('page.audit.category.other'),     'Anything emitted by our components that does not fit above'],
  ];

  content.innerHTML = `
    <div class="page-header">
      <div><h1>${tr('page.audit.title')}</h1><div class="subtitle">${tr('page.audit.subtitle')}</div></div>
      <div style="display:flex;gap:8px;align-items:center">
        <span style="color:var(--text-muted);font-size:12px">${entries.length}${truncated ? ' ' + tr('page.audit.of') + ' ' + data.total : ''} ${tr('page.audit.events')}</span>
        <button class="btn btn-secondary btn-sm" onclick="renderAudit(true)" title="Re-fetch the events list from Kubernetes">&#8635; ${tr('buttons.refresh')}</button>
      </div>
    </div>
    <div style="display:flex;gap:6px;flex-wrap:wrap;margin-bottom:16px">
      ${categories.map(([cat, label, tip]) => `
        <button class="btn btn-${auditFilter === cat ? 'primary' : 'ghost'} btn-sm"
          onclick="setAuditFilter('${cat}')" title="${escAttr(tip)}">${label}</button>
      `).join('')}
    </div>
    <div class="table-card">
      ${entries.length === 0 ? `
      <div class="empty-state">
        <h3>${tr('page.audit.empty')}</h3>
        <p>${auditFilter === 'all' ? tr('page.audit.emptyAll') : tr('page.audit.emptyFiltered')}</p>
      </div>` : `
      <table>
        <thead><tr>
          <th class="num row-num">#</th>
          <th>${tr('target.time')}</th>
          <th>${tr('table.severity')}</th>
          <th>${tr('table.reason')}</th>
          <th>${tr('table.object')}</th>
          <th>${tr('page.audit.component')}</th>
          <th>${tr('table.message')}</th>
        </tr></thead>
        <tbody>${entries.map((e, i) => `<tr>
          <td class="num row-num">${i + 1}</td>
          <td style="font-size:12px;white-space:nowrap" title="${escAttr(e.timestamp)}">${e.timestamp ? new Date(e.timestamp).toLocaleString() : '—'}</td>
          <td><span class="badge badge-${e.type === 'Warning' ? 'failed' : 'ok'}">${escHTML(e.type)}</span></td>
          <td><code style="font-size:11px">${escHTML(e.reason)}</code></td>
          <td style="font-size:12px"><code>${escHTML(e.object)}</code></td>
          <td style="font-size:11px;color:var(--text-muted)">${escHTML(e.component)}</td>
          <td style="font-size:12px;word-break:break-word">${escHTML(e.message)}</td>
        </tr>`).join('')}</tbody>
      </table>
      ${truncated ? `<p style="padding:8px 16px;color:var(--text-muted);font-size:12px">${tr('auditPage.showingMostRecent', {shown: entries.length, total: data.total})}</p>` : ''}`}
    </div>`;
}

window.setAuditFilter = function(cat) {
  auditFilter = cat;
  renderAudit(true);
};

function sortRuns(runs) {
  const g = {
    timestamp: r => parseTsCompact(r.timestamp),
    status:    r => r.status || '',
    size:      r => r.status !== 'failed' ? (r.encryptedSizeBytes || 0) : null,
    schema:    r => r.report ? (r.report.schemaChanged ? 1 : 0) : null,
    tables:    r => (r.stats && r.stats.tables) ? r.stats.tables.length : null,
    anomalies: r => (r.report && r.report.anomalies) ? r.report.anomalies.length : 0,
    verification: r => r.verification ? r.verification.verdict : '',
    restoreVerification: r => r.restoreVerification ? r.restoreVerification.verdict : '',
  };
  const s = sortState.runs;
  return sortBy(runs, g[s.col] || g.timestamp, s.dir);
}

// Go's time.Duration String form: e.g. "1h2m3s", "45s", "0s". Best-effort.
function parseDurationSec(s) {
  if (!s) return null;
  let total = 0, m;
  const re = /(\d+)([hms])/g;
  while ((m = re.exec(s)) !== null) {
    const n = +m[1];
    if (m[2] === 'h') total += n * 3600;
    else if (m[2] === 'm') total += n * 60;
    else total += n;
  }
  return total || null;
}

// --- Target detail ---
// Pure-SVG line chart for the encrypted-dump-size trend on the target detail
// page. Failed runs are excluded (no size to plot). When fewer than 2
// successful runs are available, returns an empty-state message instead of
// a misleading single-point chart.
function renderSizeChart(runs) {
  const points = (runs || [])
    .filter(r => r.status !== 'failed' && r.encryptedSizeBytes > 0)
    .map(r => ({ ts: parseTsCompact(r.timestamp), size: r.encryptedSizeBytes }))
    .filter(p => p.ts !== null)
    .sort((a, b) => a.ts - b.ts);

  if (points.length < 2) {
    return '<div class="chart-empty">Not enough successful runs yet — at least 2 are needed to draw a trend.</div>';
  }

  const W = 700, H = 220;
  const ML = 64, MR = 16, MT = 12, MB = 30;
  const PW = W - ML - MR, PH = H - MT - MB;

  const xs = points.map(p => p.ts);
  const ys = points.map(p => p.size);
  const xMin = xs[0], xMax = xs[xs.length - 1];
  const xRange = (xMax - xMin) || 1;
  // niceMax: round yMax up so axis labels are tidy bytes (KiB/MiB step).
  const yMaxRaw = Math.max(...ys);
  const yMax = niceCeil(yMaxRaw * 1.1);

  const x = t => ML + ((t - xMin) / xRange) * PW;
  const y = v => MT + PH - (v / yMax) * PH;

  const nX = Math.min(6, points.length);
  const xTicks = [];
  for (let i = 0; i < nX; i++) {
    const t = xMin + (xRange * i) / (nX - 1);
    xTicks.push({ x: x(t), label: shortDate(t) });
  }
  const yTicks = [];
  for (let i = 0; i <= 4; i++) {
    const v = (yMax * i) / 4;
    yTicks.push({ y: y(v), label: humanBytes(v) });
  }

  const linePath = points.map((p, i) =>
    (i === 0 ? 'M' : 'L') + x(p.ts).toFixed(1) + ',' + y(p.size).toFixed(1)
  ).join(' ');
  // Area path: line + drop to bottom + close, gives a soft fill below.
  const areaPath = linePath +
    ' L' + x(points[points.length - 1].ts).toFixed(1) + ',' + (MT + PH).toFixed(1) +
    ' L' + x(points[0].ts).toFixed(1) + ',' + (MT + PH).toFixed(1) + ' Z';

  const dots = points.map(p => `<circle cx="${x(p.ts).toFixed(1)}" cy="${y(p.size).toFixed(1)}" r="3" class="chart-point"><title>${escHTML(new Date(p.ts).toLocaleString() + ' — ' + humanBytes(p.size))}</title></circle>`).join('');

  return `<svg viewBox="0 0 ${W} ${H}" class="chart-svg" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Dump size trend">
    ${yTicks.map(t => `<line x1="${ML}" y1="${t.y.toFixed(1)}" x2="${W - MR}" y2="${t.y.toFixed(1)}" class="chart-grid"/><text x="${ML - 8}" y="${(t.y + 4).toFixed(1)}" class="chart-axis-text" text-anchor="end">${escHTML(t.label)}</text>`).join('')}
    ${xTicks.map(t => `<text x="${t.x.toFixed(1)}" y="${(H - MB + 18).toFixed(1)}" class="chart-axis-text" text-anchor="middle">${escHTML(t.label)}</text>`).join('')}
    <line x1="${ML}" y1="${MT}" x2="${ML}" y2="${H - MB}" class="chart-axis"/>
    <line x1="${ML}" y1="${H - MB}" x2="${W - MR}" y2="${H - MB}" class="chart-axis"/>
    <path d="${areaPath}" class="chart-area"/>
    <path d="${linePath}" class="chart-line" fill="none"/>
    ${dots}
  </svg>`;
}

// niceCeil rounds up to a "human-friendly" number for axis maxes:
// next 1/2/5 × 10^n. Avoids labels like "1.234 GiB" on the axis.
function niceCeil(n) {
  if (n <= 0) return 1;
  const exp = Math.floor(Math.log10(n));
  const base = Math.pow(10, exp);
  const m = n / base;
  let nice;
  if (m <= 1)      nice = 1;
  else if (m <= 2) nice = 2;
  else if (m <= 5) nice = 5;
  else             nice = 10;
  return nice * base;
}
function shortDate(ms) {
  const d = new Date(ms);
  return (d.getMonth() + 1) + '/' + d.getDate();
}

// GitHub-style activity heatmap: one cell per day for the last 91 days
// (13 full weeks). A day is green when every run on that day succeeded,
// red when every run failed, amber when at least one of each, and dim
// when no run was recorded. Out-of-window padding cells (the leading
// days of the first column needed to align Sunday-first) render empty.
function renderStatusHeatmap(runs) {
  const days = 91;
  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const windowStart = new Date(today);
  windowStart.setDate(windowStart.getDate() - days + 1);

  const byDate = new Map();
  (runs || []).forEach(r => {
    const ts = parseTsCompact(r.timestamp);
    if (ts === null) return;
    const d = new Date(ts);
    d.setHours(0, 0, 0, 0);
    const key = isoDate(d);
    let b = byDate.get(key);
    if (!b) { b = { ok: 0, failed: 0 }; byDate.set(key, b); }
    if (r.status === 'failed') b.failed++; else b.ok++;
  });

  // Align grid to Sunday before windowStart so weeks line up cleanly.
  const gridStart = new Date(windowStart);
  gridStart.setDate(gridStart.getDate() - gridStart.getDay());

  const cells = [];
  for (let cur = new Date(gridStart); cur <= today; cur.setDate(cur.getDate() + 1)) {
    const key = isoDate(cur);
    const b = byDate.get(key);
    const inWindow = cur >= windowStart;
    let cls = 'empty';
    if (b && inWindow) {
      if (b.failed === 0) cls = 'ok';
      else if (b.ok === 0) cls = 'failed';
      else cls = 'mixed';
    }
    cells.push({
      date: new Date(cur), key, cls,
      ok: b ? b.ok : 0,
      failed: b ? b.failed : 0,
      visible: inWindow,
    });
  }

  if (cells.filter(c => c.visible && (c.ok + c.failed) > 0).length === 0) {
    return '<div class="chart-empty">No runs in the last ' + days + ' days.</div>';
  }

  const size = 12, gap = 3, step = size + gap;
  const ML = 28, MT = 18;
  const cols = Math.ceil(cells.length / 7);
  const W = ML + cols * step;
  const H = MT + 7 * step;

  const weekdayLabels = { 1: 'Mon', 3: 'Wed', 5: 'Fri' };
  const monthLabels = [];
  let lastMonth = -1;
  for (let c = 0; c < cols; c++) {
    const cell = cells[c * 7];
    if (!cell) continue;
    const m = cell.date.getMonth();
    if (m !== lastMonth) {
      monthLabels.push({
        x: ML + c * step,
        text: cell.date.toLocaleString('default', { month: 'short' }),
      });
      lastMonth = m;
    }
  }

  const cellSvg = cells.map((c, i) => {
    const col = Math.floor(i / 7), row = i % 7;
    const x = ML + col * step, y = MT + row * step;
    const total = c.ok + c.failed;
    const tip = !c.visible ? '' :
      total === 0 ? c.key + ' — no runs'
                  : c.key + ' — ' + c.ok + ' ok, ' + c.failed + ' failed';
    return `<rect x="${x}" y="${y}" width="${size}" height="${size}" rx="2" class="hm-cell hm-${c.cls}${c.visible ? '' : ' hm-out'}"><title>${escHTML(tip)}</title></rect>`;
  }).join('');

  const wdaySvg = Object.entries(weekdayLabels).map(([row, label]) =>
    `<text x="0" y="${MT + (+row) * step + size - 2}" class="chart-axis-text">${label}</text>`
  ).join('');

  const monthSvg = monthLabels.map(m =>
    `<text x="${m.x}" y="${MT - 6}" class="chart-axis-text">${escHTML(m.text)}</text>`
  ).join('');

  const legend = `<div class="hm-legend">
    <span>Less</span>
    <span class="hm-dot hm-empty"></span>
    <span class="hm-dot hm-ok"></span>
    <span class="hm-dot hm-mixed"></span>
    <span class="hm-dot hm-failed"></span>
    <span>More</span>
  </div>`;

  return `<svg viewBox="0 0 ${W} ${H}" class="chart-svg heatmap-svg" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Run status heatmap">
    ${wdaySvg}${monthSvg}${cellSvg}
  </svg>${legend}`;
}

function isoDate(d) {
  // Local-date YYYY-MM-DD; we bucket by local day so the heatmap matches
  // the user's timezone, not UTC.
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, '0');
  const day = String(d.getDate()).padStart(2, '0');
  return y + '-' + m + '-' + day;
}

// renderStorageDonut shows actual filesystem usage per destination from
// /api/destination-stats. Unlike renderStorageByDestination (which
// extrapolates "current snapshot × destinations"), the donut reflects
// what's really on disk — counting every retained run, not just the
// latest. Hovering shows file count and byte total per slice.
const STORAGE_DONUT_COLORS = [
  '#5b6eef', '#10b981', '#f59e0b', '#ef4444',
  '#8b5cf6', '#06b6d4', '#ec4899', '#84cc16',
];
function renderStorageDonut(stats) {
  const entries = (stats || []).filter(s => s && s.totalSizeBytes > 0)
    .map(s => ({ name: s.name, bytes: s.totalSizeBytes || 0, files: s.totalFiles || 0 }))
    .sort((a, b) => b.bytes - a.bytes);
  if (entries.length === 0) {
    return '<div class="chart-empty">' + tr('chart.storageDonut.empty') + '</div>';
  }
  const total = entries.reduce((s, e) => s + e.bytes, 0);
  const cx = 90, cy = 90, r = 70, rInner = 42;
  // Start at 12-o'clock; sweep clockwise. SVG arcs are easier if we
  // accumulate angles as we go.
  let acc = -Math.PI / 2;
  const segs = entries.map((e, i) => {
    const frac = e.bytes / total;
    const angle = frac * Math.PI * 2;
    const start = acc;
    const end = acc + angle;
    acc = end;
    const x1 = cx + Math.cos(start) * r,     y1 = cy + Math.sin(start) * r;
    const x2 = cx + Math.cos(end)   * r,     y2 = cy + Math.sin(end)   * r;
    const x3 = cx + Math.cos(end)   * rInner, y3 = cy + Math.sin(end)   * rInner;
    const x4 = cx + Math.cos(start) * rInner, y4 = cy + Math.sin(start) * rInner;
    const large = angle > Math.PI ? 1 : 0;
    const color = STORAGE_DONUT_COLORS[i % STORAGE_DONUT_COLORS.length];
    const pct = (frac * 100).toFixed(1);
    const path = `M${x1.toFixed(2)},${y1.toFixed(2)} A${r},${r} 0 ${large} 1 ${x2.toFixed(2)},${y2.toFixed(2)} L${x3.toFixed(2)},${y3.toFixed(2)} A${rInner},${rInner} 0 ${large} 0 ${x4.toFixed(2)},${y4.toFixed(2)} Z`;
    return {
      path, color, pct, ...e,
    };
  });
  return `<div style="display:flex;gap:16px;align-items:center;flex-wrap:wrap">
    <svg viewBox="0 0 180 180" class="chart-svg" style="max-width:180px;flex:0 0 180px" role="img" aria-label="Storage by destination">
      ${segs.map(s => `<path d="${s.path}" fill="${s.color}" stroke="var(--bg)" stroke-width="1"><title>${escAttr(s.name)} — ${humanBytes(s.bytes)} (${s.pct}%), ${s.files} files</title></path>`).join('')}
      <text x="${cx}" y="${cy - 4}" text-anchor="middle" class="chart-axis-text" style="font-size:11px;fill:var(--text-muted)">${tr('chart.storageDonut.total')}</text>
      <text x="${cx}" y="${cy + 12}" text-anchor="middle" style="font-size:13px;font-weight:600;fill:var(--text)">${humanBytes(total)}</text>
    </svg>
    <div style="flex:1;min-width:140px;font-size:12px">
      ${segs.map(s => `<div style="display:flex;align-items:center;gap:6px;padding:2px 0">
        <span style="display:inline-block;width:10px;height:10px;background:${s.color};border-radius:2px;flex:0 0 10px"></span>
        <span style="flex:1;color:var(--text);overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="${escAttr(s.name)}">${escHTML(s.name)}</span>
        <span style="color:var(--text-muted);font-size:11px">${humanBytes(s.bytes)}</span>
      </div>`).join('')}
    </div>
  </div>`;
}

// renderDurationDistribution draws a horizontal range-bar per target:
// the bar spans min→max with a vertical tick at the median and a small
// marker at p95. Sorted by median descending so the slowest targets
// sit at the top. Spots outliers — "every backup takes <2 min except
// target X which takes 45 min" — at a glance. Failed runs are
// excluded server-side (they fail fast and would skew the median to
// zero); the Count column shows how many successful runs informed the
// distribution.
function renderDurationDistribution(stats) {
  const rows = (stats || []).filter(s => s && s.count > 0).slice(0, 10);
  if (rows.length === 0) {
    return '<div class="chart-empty">' + tr('chart.durationDist.empty') + '</div>';
  }
  const W = 700, rowH = 22, padTop = 8, padBottom = 22, padL = 140, padR = 60;
  const PW = W - padL - padR;
  const H = padTop + rows.length * rowH + padBottom;
  // Single shared x-axis: 0 → max(max across all targets). Per-target
  // axes would make outliers invisible (the slow target's bar would
  // span the same visual length as a fast target's bar).
  const xMax = rows.reduce((m, r) => Math.max(m, r.max), 1);
  const xScale = v => padL + (v / xMax) * PW;

  // Hour/min ticks: 5 evenly-spaced labels
  const xTicks = [];
  for (let i = 0; i <= 4; i++) {
    const v = (xMax * i) / 4;
    xTicks.push({ x: xScale(v), label: fmtDurationShort(v) });
  }

  const bars = rows.map((r, i) => {
    const y = padTop + i * rowH;
    const cy = y + rowH / 2;
    const x1 = xScale(r.min), x2 = xScale(r.max);
    const xMed = xScale(r.median), xP95 = xScale(r.p95);
    return `<g>
      <a href="#/target/${escAttr(r.target)}" style="cursor:pointer">
        <text x="${(padL - 8).toFixed(1)}" y="${(cy + 4).toFixed(1)}" text-anchor="end" class="chart-axis-text" style="font-size:11px;fill:var(--accent)">${escHTML(r.target)}</text>
      </a>
      <line x1="${x1.toFixed(1)}" y1="${cy.toFixed(1)}" x2="${x2.toFixed(1)}" y2="${cy.toFixed(1)}" stroke="var(--accent, #5b6eef)" stroke-width="3" opacity="0.4" stroke-linecap="round"><title>${escAttr(r.target + ' — min ' + fmtDurationShort(r.min) + ', max ' + fmtDurationShort(r.max) + ', n=' + r.count)}</title></line>
      <line x1="${xMed.toFixed(1)}" y1="${(cy - 7).toFixed(1)}" x2="${xMed.toFixed(1)}" y2="${(cy + 7).toFixed(1)}" stroke="var(--accent, #5b6eef)" stroke-width="2.5"><title>median ${escAttr(fmtDurationShort(r.median))}</title></line>
      <circle cx="${xP95.toFixed(1)}" cy="${cy.toFixed(1)}" r="3.5" fill="var(--warning, #f59e0b)" stroke="var(--bg)" stroke-width="1"><title>p95 ${escAttr(fmtDurationShort(r.p95))}</title></circle>
      <text x="${(W - padR + 6).toFixed(1)}" y="${(cy + 4).toFixed(1)}" class="chart-axis-text" style="font-size:11px">n=${r.count}</text>
    </g>`;
  }).join('');

  return `<svg viewBox="0 0 ${W} ${H}" class="chart-svg" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Run duration distribution per target">
    ${xTicks.map(t => `<line x1="${t.x.toFixed(1)}" y1="${padTop}" x2="${t.x.toFixed(1)}" y2="${(H - padBottom + 2).toFixed(1)}" class="chart-grid"/><text x="${t.x.toFixed(1)}" y="${(H - padBottom + 14).toFixed(1)}" text-anchor="middle" class="chart-axis-text" style="font-size:10px">${escHTML(t.label)}</text>`).join('')}
    ${bars}
  </svg>`;
}

// renderVerificationTrend plots the daily restore-verification pass
// rate as a line over the 30-day window. Only runs with a
// RestoreVerification block contribute to the rate — sources with
// mode=off (the default) don't count toward the denominator at all,
// so a day with zero verifier-armed runs renders as a gap. The
// underlying area shading shows total verifier-armed-run volume so an
// operator can tell whether a 100% rate is "1 of 1" or "200 of 200".
function renderVerificationTrend(daily) {
  const data = (daily || []).filter(d => d && d.day);
  const armed = data.filter(d => (d.passed + d.failed) > 0);
  if (armed.length === 0) {
    return '<div class="chart-empty">' + tr('chart.verifyTrend.empty') + '</div>';
  }
  const W = 700, H = 220, ML = 56, MR = 50, MT = 12, MB = 28;
  const PW = W - ML - MR, PH = H - MT - MB;

  const xs = data.map((_, i) => ML + (i / Math.max(data.length - 1, 1)) * PW);
  // Two Y axes: left = pass rate 0-100%, right = volume (bar height).
  const yPct = pct => MT + PH - (pct / 100) * PH;
  const volMax = Math.max(1, ...data.map(d => d.passed + d.failed));
  const volH = v => (v / volMax) * (PH * 0.4); // bars take bottom 40% only

  // Volume bars (grey, behind the line)
  const bars = data.map((d, i) => {
    const total = d.passed + d.failed;
    if (total === 0) return '';
    const h = volH(total);
    const w = (PW / data.length) * 0.7;
    const x = xs[i] - w / 2;
    return `<rect x="${x.toFixed(1)}" y="${(MT + PH - h).toFixed(1)}" width="${w.toFixed(1)}" height="${h.toFixed(1)}" fill="var(--text-muted)" opacity="0.15"><title>${escAttr(d.day + ': ' + total + ' verifier-armed run(s)')}</title></rect>`;
  }).join('');

  // Pass-rate line — gaps where no verifier ran that day. Build path
  // by walking the array, starting a new sub-path after a gap.
  let path = '';
  let inPath = false;
  data.forEach((d, i) => {
    const total = d.passed + d.failed;
    if (total === 0) { inPath = false; return; }
    const pct = (d.passed / total) * 100;
    path += (inPath ? ' L' : ' M') + xs[i].toFixed(1) + ',' + yPct(pct).toFixed(1);
    inPath = true;
  });
  const dots = data.map((d, i) => {
    const total = d.passed + d.failed;
    if (total === 0) return '';
    const pct = (d.passed / total) * 100;
    const color = pct === 100 ? 'var(--success, #10b981)' : (pct >= 80 ? 'var(--warning, #f59e0b)' : 'var(--danger, #ef4444)');
    return `<circle cx="${xs[i].toFixed(1)}" cy="${yPct(pct).toFixed(1)}" r="3.5" fill="${color}" stroke="var(--bg)" stroke-width="1"><title>${escAttr(d.day + ': ' + pct.toFixed(1) + '% (' + d.passed + '/' + total + ')')}</title></circle>`;
  }).join('');

  // Y ticks at 0/50/100%
  const yTicks = [0, 50, 100].map(p => ({ y: yPct(p), label: p + '%' }));
  // X ticks every 7 days
  const xTicks = [];
  for (let i = 0; i < data.length; i += 7) {
    xTicks.push({ x: xs[i], label: data[i].day.slice(5) });
  }

  return `<svg viewBox="0 0 ${W} ${H}" class="chart-svg" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Restore-verification pass rate trend">
    ${yTicks.map(t => `<line x1="${ML}" y1="${t.y.toFixed(1)}" x2="${(W - MR).toFixed(1)}" y2="${t.y.toFixed(1)}" class="chart-grid"/><text x="${(ML - 6).toFixed(1)}" y="${(t.y + 4).toFixed(1)}" text-anchor="end" class="chart-axis-text" style="font-size:10px">${escHTML(t.label)}</text>`).join('')}
    ${bars}
    <path d="${path}" fill="none" stroke="var(--accent, #5b6eef)" stroke-width="2"/>
    ${dots}
    ${xTicks.map(t => `<text x="${t.x.toFixed(1)}" y="${(H - MB + 14).toFixed(1)}" text-anchor="middle" class="chart-axis-text" style="font-size:10px">${escHTML(t.label)}</text>`).join('')}
    <text x="${(W - MR + 6).toFixed(1)}" y="${(MT + PH - volH(volMax) - 4).toFixed(1)}" class="chart-axis-text" style="font-size:9px;opacity:0.6">${escHTML(tr('chart.verifyTrend.volume', {n: volMax}))}</text>
  </svg>`;
}

// renderStorageGrowth draws a stacked area chart of daily upload
// bytes per source over the heatmap window (30 days). NOT cumulative
// storage — that would require knowing each destination's retention
// policy and reconciling deletes, which we don't track. "Daily upload
// volume per source" answers the operationally relevant question:
// "which source is driving my storage growth?" Per-source is more
// actionable than per-DB-type for capacity planning — operators know
// their tech stack, they want to know which workload to investigate.
//
// With many sources the stack can get visually busy; that's the
// trade-off. Hash-based colours stay stable across reloads so the
// same source always gets the same slice of the rainbow.

// hashHue derives a deterministic 0-359 HSL hue from a string. Same
// string → same hue across reloads. Avoids the "every reload my
// chart looks different" problem of random palettes.
function hashHue(s) {
  let h = 0;
  for (let i = 0; i < s.length; i++) {
    h = ((h << 5) - h + s.charCodeAt(i)) | 0;
  }
  // Bias away from yellow/green range that clashes with the
  // success/warning UI colours.
  return ((h % 360) + 360) % 360;
}
function colorForTarget(name) {
  // 65% saturation + 58% lightness sits comfortably on the dark
  // background without bleaching out, and the HSL spacing means
  // adjacent stacked layers always look distinct from each other.
  return `hsl(${hashHue(name)}, 65%, 58%)`;
}

function renderStorageGrowth(points) {
  const days = (points || []).filter(p => p && p.day);
  if (days.length < 2) {
    return '<div class="chart-empty">' + tr('chart.storageGrowth.empty') + '</div>';
  }
  // Discover all targets that contributed any bytes in the window.
  // Sort by total volume descending so the largest source is at the
  // bottom of the stack (closest to the x-axis) — that's the
  // conventional reading order for stacked area, big-stuff-at-bottom.
  const totalsByTarget = new Map();
  days.forEach(d => {
    if (!d.perTarget) return;
    Object.entries(d.perTarget).forEach(([name, bytes]) => {
      if (bytes > 0) totalsByTarget.set(name, (totalsByTarget.get(name) || 0) + bytes);
    });
  });
  const targets = [...totalsByTarget.entries()].sort((a, b) => b[1] - a[1]).map(e => e[0]);
  if (targets.length === 0) {
    return '<div class="chart-empty">' + tr('chart.storageGrowth.empty') + '</div>';
  }

  const W = 700, H = 220, ML = 60, MR = 16, MT = 12, MB = 28;
  const PW = W - ML - MR, PH = H - MT - MB;
  const xs = days.map((_, i) => ML + (i / Math.max(days.length - 1, 1)) * PW);

  // Build cumulative-stack series, biggest-at-bottom (reverse the
  // targets array for the stack order so the largest layer sits
  // closest to the axis; legend stays in size-desc order separately).
  const stackOrder = targets.slice().reverse();
  const series = stackOrder.map(t => days.map(d => (d.perTarget && d.perTarget[t]) || 0));
  const cumTop = new Array(days.length).fill(0);
  const stacked = series.map(layer => layer.map((v, i) => {
    const top = cumTop[i] + v;
    const out = { bottom: cumTop[i], top };
    cumTop[i] = top;
    return out;
  }));
  const yMax = niceCeil(Math.max(1, ...cumTop) * 1.1);
  const y = v => MT + PH - (v / yMax) * PH;

  const layers = stacked.map((layer, li) => {
    const targetName = stackOrder[li];
    const fill = colorForTarget(targetName);
    const total = totalsByTarget.get(targetName) || 0;
    const topPath = layer.map((p, i) => (i === 0 ? 'M' : 'L') + xs[i].toFixed(1) + ',' + y(p.top).toFixed(1)).join(' ');
    const botPath = layer.slice().reverse().map((p, idx) => {
      const i = layer.length - 1 - idx;
      return 'L' + xs[i].toFixed(1) + ',' + y(p.bottom).toFixed(1);
    }).join(' ');
    return `<path d="${topPath} ${botPath} Z" fill="${fill}" opacity="0.78"><title>${escAttr(targetName + ' — ' + humanBytes(total) + ' in window')}</title></path>`;
  }).join('');

  const yTicks = [];
  for (let i = 0; i <= 4; i++) {
    const v = (yMax * i) / 4;
    yTicks.push({ y: y(v), label: humanBytes(v) });
  }
  const nX = Math.min(6, days.length);
  const xTicks = [];
  for (let i = 0; i < nX; i++) {
    const idx = Math.round((days.length - 1) * (i / (nX - 1)));
    xTicks.push({ x: xs[idx], label: days[idx].day.slice(5) });
  }

  // Legend in size-desc order so the biggest source is listed first
  // (matches the chart's bottom-to-top reading order). Click-link to
  // jump to that target's detail page — the chart becomes a nav
  // surface too.
  const legend = targets.map(t => {
    const color = colorForTarget(t);
    return `<a href="#/target/${escAttr(t)}" style="display:inline-flex;align-items:center;gap:4px;font-size:11px;margin-right:10px;text-decoration:none;color:var(--text)">
      <span style="display:inline-block;width:10px;height:10px;background:${color};border-radius:2px"></span>${escHTML(t)}
    </a>`;
  }).join('');

  return `<div>
    <svg viewBox="0 0 ${W} ${H}" class="chart-svg" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Daily upload bytes by source">
      ${yTicks.map(t => `<line x1="${ML}" y1="${t.y.toFixed(1)}" x2="${W - MR}" y2="${t.y.toFixed(1)}" class="chart-grid"/><text x="${ML - 6}" y="${(t.y + 4).toFixed(1)}" text-anchor="end" class="chart-axis-text">${escHTML(t.label)}</text>`).join('')}
      ${layers}
      ${xTicks.map(t => `<text x="${t.x.toFixed(1)}" y="${(MT + PH + 18).toFixed(1)}" text-anchor="middle" class="chart-axis-text" style="font-size:10px">${escHTML(t.label)}</text>`).join('')}
    </svg>
    <div style="padding:4px 8px 0;color:var(--text-muted)">${legend}</div>
  </div>`;
}

// renderAnomalyStream shows analyzer anomalies as colored dots on a
// 30-day horizontal timeline. Each dot is one anomaly; severity drives
// the colour. Hover for kind/subject/detail; click on the timestamp
// jumps to the target detail page where the full Report is rendered.
const ANOMALY_SEVERITY_COLOR = {
  critical: 'var(--danger, #ef4444)',
  warning:  'var(--warning, #f59e0b)',
  info:     'var(--accent, #5b6eef)',
};
function renderAnomalyStream(items) {
  const list = items || [];
  if (list.length === 0) {
    return '<div class="chart-empty" style="padding:32px 16px">' + tr('chart.anomalyStream.empty') + '</div>';
  }
  // Compute a 30-day axis ending today UTC. Anomalies older than that
  // are ignored (the cap on the server is also 30 days, but we don't
  // assume).
  const dayMs = 24 * 3600 * 1000;
  const endUTC = Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth(), new Date().getUTCDate());
  const startUTC = endUTC - 29 * dayMs;
  const W = 700, H = 200, ML = 60, MR = 16, MT = 16, MB = 28;
  const PW = W - ML - MR, PH = H - MT - MB;

  // Group anomalies by target row. Limit rows to 8 to keep the chart
  // legible; "+ N more" footer if truncated.
  const byTarget = new Map();
  list.forEach(a => {
    if (!byTarget.has(a.target)) byTarget.set(a.target, []);
    byTarget.get(a.target).push(a);
  });
  const targets = [...byTarget.keys()].sort();
  const visible = targets.slice(0, 8);
  const truncatedCount = targets.length - visible.length;

  const rowH = visible.length > 0 ? PH / visible.length : PH;

  const xs = ts => {
    const t = new Date(ts).getTime();
    const clamped = Math.max(startUTC, Math.min(endUTC + dayMs, t));
    return ML + ((clamped - startUTC) / (30 * dayMs)) * PW;
  };

  // Week ticks
  const xTicks = [];
  for (let i = 0; i < 5; i++) {
    const t = startUTC + i * 7 * dayMs;
    xTicks.push({ x: ML + ((t - startUTC) / (30 * dayMs)) * PW, label: new Date(t).toISOString().slice(5, 10) });
  }
  // Today marker
  const todayX = xs(new Date().toISOString());

  const rowsSvg = visible.map((tgt, ri) => {
    const cy = MT + (ri + 0.5) * rowH;
    const dots = byTarget.get(tgt).map(a => {
      const cx = xs(a.time);
      const color = ANOMALY_SEVERITY_COLOR[a.severity] || ANOMALY_SEVERITY_COLOR.info;
      const tt = `${a.target} · ${a.kind}${a.subject ? ' · ' + a.subject : ''}\n${new Date(a.time).toLocaleString()}${a.detail ? '\n' + a.detail : ''}`;
      return `<circle cx="${cx.toFixed(1)}" cy="${cy.toFixed(1)}" r="4" fill="${color}" stroke="var(--bg)" stroke-width="1"><title>${escAttr(tt)}</title></circle>`;
    }).join('');
    return `<g>
      <a href="#/target/${escAttr(tgt)}" style="cursor:pointer">
        <text x="${(ML - 6).toFixed(1)}" y="${(cy + 4).toFixed(1)}" text-anchor="end" class="chart-axis-text" style="font-size:11px;fill:var(--accent)">${escHTML(tgt)}</text>
      </a>
      <line x1="${ML}" y1="${cy.toFixed(1)}" x2="${(W - MR).toFixed(1)}" y2="${cy.toFixed(1)}" class="chart-grid"/>
      ${dots}
    </g>`;
  }).join('');

  return `<div>
    <svg viewBox="0 0 ${W} ${H}" class="chart-svg" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Analyzer anomaly timeline">
      ${xTicks.map(t => `<text x="${t.x.toFixed(1)}" y="${(MT - 4).toFixed(1)}" text-anchor="middle" class="chart-axis-text" style="font-size:10px">${escHTML(t.label)}</text>`).join('')}
      <line x1="${todayX.toFixed(1)}" y1="${MT}" x2="${todayX.toFixed(1)}" y2="${(H - MB).toFixed(1)}" stroke="var(--accent, #5b6eef)" stroke-width="1" stroke-dasharray="2,3" opacity="0.5"/>
      ${rowsSvg}
    </svg>
    ${truncatedCount > 0 ? `<div style="padding:4px 8px;color:var(--text-muted);font-size:11px">${tr('chart.anomalyStream.moreTargets', {count: truncatedCount})}</div>` : ''}
  </div>`;
}

// renderFleetHeatmap shows per-target, per-day backup status as a grid.
// Reads /api/dashboard/heatmap; each row is one target, columns are
// days (oldest left, today right). Cell colours mirror the GitHub-
// contributions style the per-target heatmap already uses, so an
// operator scanning the dashboard finds the same visual language.
//
// Status palette:
//   ok     → green
//   failed → red
//   mixed  → amber
//   none   → muted grey
//
// Click on any row's label routes to that target's detail page.
function renderFleetHeatmap(rows) {
  if (!rows || rows.length === 0) {
    return '<div class="chart-empty">' + tr('chart.fleetHeatmap.empty') + '</div>';
  }
  const days = rows[0].days ? rows[0].days.length : 30;
  // Cell + row geometry. Row height (cellSize + cellGap) MUST stay
  // ≥ the label font-size in px or labels overlap when many targets
  // are listed. 14 px cell + 4 px gap = 18 px row, comfortable for
  // an 11 px label.
  const cellSize = 14, cellGap = 4;
  const padL = 150, padTop = 24, padR = 16, padBottom = 32;
  const gridW = days * (cellSize + cellGap) - cellGap;
  const W = padL + gridW + padR;
  const H = padTop + rows.length * (cellSize + cellGap) - cellGap + padBottom;

  const statusFill = {
    ok:     'var(--success, #10b981)',
    failed: 'var(--danger, #ef4444)',
    mixed:  'var(--warning, #f59e0b)',
    none:   'var(--bg-input, #2a2a2a)',
  };

  // Truncate over-long target names so they fit padL without
  // bleeding into the grid. The full name is still visible on hover
  // via <title> on the link.
  const truncate = (s, n) => s.length > n ? s.slice(0, n - 1) + '…' : s;

  // Weekly tick on the day axis — too many labels make the row
  // unreadable. Pick every 7th day starting from the right (today).
  const xTicks = [];
  for (let i = days - 1; i >= 0; i -= 7) {
    const day = rows[0].days[i].day;
    const x = padL + i * (cellSize + cellGap) + cellSize / 2;
    const label = day.slice(5); // MM-DD
    xTicks.push({ x, label });
  }

  const rowSvg = rows.map((r, ri) => {
    const y = padTop + ri * (cellSize + cellGap);
    const labelY = y + cellSize / 2 + 4;
    const labelText = truncate(r.target, 20);
    const cells = r.days.map((c, ci) => {
      const x = padL + ci * (cellSize + cellGap);
      const fill = statusFill[c.status] || statusFill.none;
      const tt = `${r.target} · ${c.day}: ${c.runs > 0 ? c.runs + ' run(s), ' + c.status : 'no run'}`;
      return `<rect x="${x}" y="${y}" width="${cellSize}" height="${cellSize}" rx="2" fill="${fill}"><title>${escAttr(tt)}</title></rect>`;
    }).join('');
    return `<g>
      <a href="#/target/${escAttr(r.target)}" style="cursor:pointer">
        <title>${escAttr(r.target)}</title>
        <text x="${(padL - 8).toFixed(1)}" y="${labelY.toFixed(1)}" text-anchor="end" class="chart-axis-text" style="font-size:11px;fill:var(--accent)">${escHTML(labelText)}</text>
      </a>
      ${cells}
    </g>`;
  }).join('');

  // Legend at the bottom — flexbox-style horizontal stack with
  // explicit min-spacing so labels never overlap regardless of width.
  const legend = [
    ['ok', tr('chart.fleetHeatmap.legend.ok')],
    ['mixed', tr('chart.fleetHeatmap.legend.mixed')],
    ['failed', tr('chart.fleetHeatmap.legend.failed')],
    ['none', tr('chart.fleetHeatmap.legend.none')],
  ];
  const legendSpacing = Math.max(110, (W - padL) / legend.length);
  const legendSvg = legend.map(([k, label], i) => {
    const x = padL + i * legendSpacing;
    const y = H - 14;
    return `<rect x="${x}" y="${(y - 9).toFixed(1)}" width="10" height="10" rx="2" fill="${statusFill[k]}"/><text x="${(x + 14).toFixed(1)}" y="${y.toFixed(1)}" class="chart-axis-text" style="font-size:10px">${escHTML(label)}</text>`;
  }).join('');

  // Explicit height on the SVG (matching the natural H) overrides
  // the chart-svg max-height clamp; the min-width keeps the cell
  // grid at native px size on narrow viewports — the parent
  // .chart-card scrolls horizontally rather than scaling the SVG
  // down to where cells become microscopic. chart-svg-tall opts
  // out of the 260 px cap so labels stay readable.
  return `<svg viewBox="0 0 ${W} ${H}" class="chart-svg chart-svg-tall" preserveAspectRatio="xMidYMin meet" role="img" aria-label="Fleet backup heatmap" style="height:${H}px;max-height:${H}px;min-width:${W}px">
    ${xTicks.map(t => `<text x="${t.x.toFixed(1)}" y="${(padTop - 8).toFixed(1)}" text-anchor="middle" class="chart-axis-text" style="font-size:10px">${escHTML(t.label)}</text>`).join('')}
    ${rowSvg}
    ${legendSvg}
  </svg>`;
}

// renderNextRunGantt plots each target's NextRun on a 24-hour timeline
// starting at "now". Surfaces thundering-herd peaks (10 targets all
// scheduled at 02:00) and immediate upcoming work. Bar width is the
// estimated duration when available (median over last successful runs),
// minimum 4 px so a 30s backup doesn't disappear. Suspended sources are
// skipped — they have no nextRun.
function renderNextRunGantt(targets) {
  const now = Date.now();
  const horizonMs = 24 * 3600 * 1000;
  const rows = (targets || [])
    .filter(t => !t.Suspended && t.nextRun)
    .map(t => {
      const ms = new Date(t.nextRun).getTime();
      const durSec = (t.Latest && t.Latest.durationSeconds) ||
                     (t.Latest && t.Latest.encryptedSizeBytes ? 60 : 30);
      return { name: t.Name, dbType: t.DBType, startMs: ms, durSec };
    })
    .filter(r => isFinite(r.startMs) && r.startMs >= now - 60000 && r.startMs < now + horizonMs)
    .sort((a, b) => a.startMs - b.startMs);

  if (rows.length === 0) {
    return '<div class="chart-empty">' + tr('chart.nextRunGantt.empty') + '</div>';
  }
  const W = 700, rowH = 24, padTop = 24, padBottom = 22, padL = 96, padR = 16;
  const PW = W - padL - padR;
  const H = padTop + rows.length * rowH + padBottom;
  const xPos = ms => padL + ((ms - now) / horizonMs) * PW;

  // Hour-tick lines + labels (every 3h)
  const ticks = [];
  for (let h = 0; h <= 24; h += 3) {
    const t = now + h * 3600 * 1000;
    ticks.push({ x: xPos(t), label: new Date(t).toLocaleTimeString([], {hour: '2-digit', minute: '2-digit'}) });
  }

  // Count overlapping bars at any given minute → flag thundering-herd
  // peaks. Two or more bars whose start time falls within the same
  // 60-second window get a subtle warning border.
  const bucketMin = new Map();
  rows.forEach(r => {
    const k = Math.floor(r.startMs / 60000);
    bucketMin.set(k, (bucketMin.get(k) || 0) + 1);
  });

  const bars = rows.map((r, i) => {
    const x = xPos(r.startMs);
    const widthPx = Math.max(4, (r.durSec / 86400) * PW);
    const y = padTop + i * rowH;
    const tier = bucketMin.get(Math.floor(r.startMs / 60000)) || 1;
    const cls = tier > 1 ? 'gantt-bar gantt-bar-peak' : 'gantt-bar';
    const when = new Date(r.startMs).toLocaleString();
    const tooltip = `${r.name} (${r.dbType}) — ${when}, est ${fmtDurationShort(r.durSec)}${tier > 1 ? ` — ⚠ ${tier} targets fire in the same minute` : ''}`;
    return `<g>
      <text x="${padL - 6}" y="${(y + rowH/2 + 4).toFixed(1)}" text-anchor="end" class="chart-axis-text" style="font-size:11px"><tspan>${escHTML(r.name)}</tspan></text>
      <rect x="${x.toFixed(1)}" y="${(y + 5).toFixed(1)}" width="${widthPx.toFixed(1)}" height="${(rowH - 10).toFixed(1)}" rx="2" class="${cls}"><title>${escAttr(tooltip)}</title></rect>
    </g>`;
  }).join('');

  return `<svg viewBox="0 0 ${W} ${H}" class="chart-svg" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Upcoming backup runs">
    ${ticks.map(t => `<line x1="${t.x.toFixed(1)}" y1="${padTop - 4}" x2="${t.x.toFixed(1)}" y2="${(H - padBottom + 4).toFixed(1)}" class="chart-grid"/>
       <text x="${t.x.toFixed(1)}" y="${(H - padBottom + 16).toFixed(1)}" text-anchor="middle" class="chart-axis-text" style="font-size:10px">${escHTML(t.label)}</text>`).join('')}
    <line x1="${padL}" y1="${padTop - 4}" x2="${padL}" y2="${(H - padBottom + 4).toFixed(1)}" class="chart-axis"/>
    ${bars}
  </svg>`;
}

// Approximation: each destination receives every target's most recent
// successful dump. The bar shows that single snapshot, not historical
// retention. Good enough to spot "destination A holds 90% of the bytes".
function renderStorageByDestination(targets, dests) {
  if (!targets || targets.length === 0 || !dests || dests.length === 0) return '';

  const byDest = new Map();
  dests.forEach(d => byDest.set(d.name, []));
  targets.forEach(t => {
    if (!t.Latest || t.Latest.status === 'failed') return;
    const size = t.Latest.encryptedSizeBytes || 0;
    if (size === 0) return;
    const targetDests = (t.Destinations && t.Destinations.length > 0)
      ? t.Destinations
      : dests.map(d => d.name); // unrestricted source fans out everywhere
    targetDests.forEach(name => {
      if (!byDest.has(name)) byDest.set(name, []);
      byDest.get(name).push({ target: t.Name, size });
    });
  });

  const rows = [];
  for (const [name, items] of byDest) {
    items.sort((a, b) => b.size - a.size);
    const total = items.reduce((s, i) => s + i.size, 0);
    rows.push({ name, items, total });
  }
  rows.sort((a, b) => b.total - a.total);

  const maxTotal = rows.reduce((m, r) => Math.max(m, r.total), 0);
  if (maxTotal === 0) return '';

  return `<div class="chart-card" style="margin-bottom:16px">
    <h3>${tr('card.storageByDestination')} <span class="chart-card-sub">${tr('card.storageSubtitle')}</span></h3>
    <div class="stack-bar-list">
      ${rows.map(r => `
        <div class="stack-bar-row">
          <div class="stack-bar-label" title="${escAttr(r.name)}">${escHTML(r.name)}</div>
          <div class="stack-bar" style="width:${maxTotal > 0 ? (r.total / maxTotal * 100).toFixed(1) : 0}%">
            ${r.items.map(it => `<div class="stack-segment" style="width:${(it.size / r.total * 100).toFixed(2)}%;background:${colorForTarget(it.target)}" title="${escAttr(it.target + ' — ' + humanBytes(it.size))}"></div>`).join('')}
          </div>
          <div class="stack-bar-total">${humanBytes(r.total)}</div>
        </div>
      `).join('')}
    </div>
  </div>`;
}

// Stable colour from target name — same target gets the same colour across
// all bars and across page reloads. djb2-style hash → HSL hue.
function colorForTarget(name) {
  let hash = 5381;
  for (let i = 0; i < name.length; i++) hash = ((hash << 5) + hash + name.charCodeAt(i)) | 0;
  const hue = Math.abs(hash) % 360;
  return 'hsl(' + hue + ', 55%, 55%)';
}

// Per-table row-count trend on the target detail page. Builds a name→history
// map across all successful runs, then renders the latest run's tables sorted
// by current row count, with a tiny sparkline showing direction. Tables that
// only ever appeared in one run still get a single point.
function renderTablesCard(runs) {
  const sorted = (runs || [])
    .filter(r => r.status !== 'failed' && r.stats && Array.isArray(r.stats.tables))
    .map(r => ({ ts: parseTsCompact(r.timestamp), tables: r.stats.tables }))
    .filter(r => r.ts !== null)
    .sort((a, b) => a.ts - b.ts);
  if (sorted.length === 0) return '';

  const history = new Map();
  sorted.forEach(r => {
    r.tables.forEach(t => {
      if (!history.has(t.name)) history.set(t.name, []);
      history.get(t.name).push(t.rowCount);
    });
  });

  const latest = sorted[sorted.length - 1].tables.slice()
    .sort((a, b) => (b.rowCount || 0) - (a.rowCount || 0));
  if (latest.length === 0) return '';

  return `<div class="chart-card">
    <h3>${tr('card.tablesLatest')} <span class="chart-card-sub">${tr(latest.length === 1 ? 'card.tablesSubtitle' : 'card.tablesSubtitlePlural', {count: latest.length, runs: sorted.length})}</span></h3>
    <div class="table-scroll">
    <table class="tbl-compact">
      <thead><tr><th>${tr('verification.table')}</th><th class="num">${tr('table.runCount')}</th><th>Trend</th><th class="num">${tr('table.size')}</th></tr></thead>
      <tbody>${latest.map(t => `<tr>
        <td class="cell-mono">${escHTML(t.name)}</td>
        <td class="num">${formatNum(t.rowCount)}</td>
        <td>${renderSparkline(history.get(t.name) || [])}</td>
        <td class="num">${humanBytes(t.sizeBytes)}</td>
      </tr>`).join('')}</tbody>
    </table>
    </div>
  </div>`;
}

// Tiny inline SVG line — width 80, height 20, no axes. Direction-coloured:
// last value vs first → green if up >5%, red if down >5%, muted otherwise.
function renderSparkline(values) {
  if (!values || values.length === 0) return '';
  if (values.length === 1) {
    return '<svg viewBox="0 0 80 20" class="sparkline"><circle cx="40" cy="10" r="2" class="sparkline-point"/></svg>';
  }
  const W = 80, H = 20, pad = 2;
  const min = Math.min(...values), max = Math.max(...values);
  const range = (max - min) || 1;
  const xStep = (W - 2 * pad) / (values.length - 1);
  const path = values.map((v, i) => {
    const x = pad + i * xStep;
    const y = pad + (H - 2 * pad) - ((v - min) / range) * (H - 2 * pad);
    return (i === 0 ? 'M' : 'L') + x.toFixed(1) + ',' + y.toFixed(1);
  }).join(' ');
  const first = values[0], last = values[values.length - 1];
  let cls = 'sparkline-flat';
  if (first > 0) {
    if (last < first * 0.95) cls = 'sparkline-down';
    else if (last > first * 1.05) cls = 'sparkline-up';
  }
  return `<svg viewBox="0 0 ${W} ${H}" class="sparkline ${cls}"><path d="${path}"/></svg>`;
}

function formatNum(n) {
  if (n == null) return '—';
  if (n >= 1e9) return (n / 1e9).toFixed(1) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
  return String(n);
}

async function renderTargetDetail(name, loading = true) {
  // Empty name shouldn't reach here from a normal click — hash is
  // `#/target/<name>` and currentParam returns the second segment. If
  // we got here with no name, the hash was malformed (e.g. someone
  // typed `#/target/` by hand). Show an explicit error rather than
  // silently bouncing to Dashboard, so the user can see what went
  // wrong instead of "I clicked and ended up somewhere else".
  if (!name) {
    if (loading) showLoading();
    content.innerHTML = `<div class="empty-state"><h3>Missing target name in URL</h3>
      <p>Hash was <code>${escAttr(location.hash)}</code> — expected <code>#/target/&lt;name&gt;</code>.</p>
      <a href="#/" class="btn btn-secondary">Back to Dashboard</a></div>`;
    return;
  }
  if (loading) showLoading();
  let targets = [], runs = [], dests = [], jobs = [];
  // fastDataAllSettled keeps a per-URL last-good cache, so a transient
  // /api/jobs failure doesn't blow away targets and leave us showing
  // "target not found" for a real target.
  const got = await fastDataAllSettled(['/api/targets', '/api/destinations', '/api/jobs']);
  targets = got[0] || [];
  dests   = got[1] || [];
  jobs    = got[2] || [];

  const target = targets.find(t => t.Name === name);
  if (!target) {
    content.innerHTML = `<div class="empty-state"><h3>Target not found</h3><p>"${escAttr(name)}" does not exist.</p>
      <a href="#/" class="btn btn-secondary" title="Return to the Dashboard">Back to Dashboard</a></div>`;
    return;
  }

  try { runs = (await api('/api/targets/' + name + '/runs')) || []; } catch(e) { /* ok */ }

  // Find any in-flight Job for this target so we can render the same
  // progress bar the Jobs page uses. Multiple running jobs is rare but
  // possible (manual trigger overlap); pick the newest.
  const runningJob = jobs
    .filter(j => j.target === name && j.status === 'running')
    .sort((a, b) => parseTsRFC(b.startTime) - parseTsRFC(a.startTime))[0] || null;

  content.innerHTML = `
    <div class="page-header">
      <div>
        <div style="margin-bottom:8px"><a href="#/" style="color:var(--text-muted);font-size:13px;text-decoration:none">&larr; Dashboard</a></div>
        <h1>${escHTML(name)} <span class="badge badge-${target.DBType}">${target.DBType}</span></h1>
      </div>
      <div style="display:flex;gap:8px">
        <button class="btn btn-secondary btn-sm" onclick="triggerBackup('${escJS(name)}')" title="Trigger a manual backup run for this target now — creates a one-off Job from the CronJob template">&#9654; ${tr('buttons.runNow')}</button>
        <button class="btn btn-secondary btn-sm" onclick="openSourceForm('${escJS(target.SecretName)}')" title="Edit this source's connection details and schedule">${tr('common.edit')}</button>
        <button class="btn btn-danger btn-sm" onclick="deleteSource('${escJS(target.SecretName)}','${escJS(name)}')" title="Permanently delete this source. The CronJob is cascaded; existing dumps remain in storage.">${tr('common.delete')}</button>
      </div>
    </div>
    ${runningJob ? `
    <div class="detail-card running-banner" data-job-name="${escAttr(runningJob.name)}" style="margin-bottom:16px;border-left:3px solid var(--accent,#3b82f6)">
      <h3 style="display:flex;align-items:center;gap:8px">
        <span class="badge badge-running">${tr('badge.running') || 'running'}</span>
        ${tr('target.runningTitle') || 'Backup läuft gerade'}
      </h3>
      <div class="job-progress-host">${renderProgressCell(runningJob)}</div>
      <div style="font-size:11px;color:var(--text-muted);margin-top:6px">
        ${tr('table.startTime') || 'Started'}: ${new Date(runningJob.startTime).toLocaleString()} · Job: <code>${escHTML(runningJob.name)}</code>
      </div>
    </div>` : ''}
    <div class="detail-grid">
      <div class="detail-card">
        <h3>${tr('target.configuration')}</h3>
        <div class="detail-row"><span class="key">${tr('table.schedule')}</span><code class="val">${escHTML(target.Schedule)}</code></div>
        <div class="detail-row"><span class="key">${tr('target.nextRun') || 'Nächster Lauf'}</span><span class="val">${target.Suspended ? `<span style="color:var(--text-muted)">${tr('badge.suspended') || 'pausiert'}</span>` : escHTML(fmtNextRun(target.nextRun)) || '—'}</span></div>
        <div class="detail-row"><span class="key">${tr('table.destinations')}</span><span class="val">${(target.Destinations||[]).join(', ') || tr('common.all').toLowerCase()}</span></div>
        <div class="detail-row"><span class="key">${tr('table.status')}</span>
          ${target.Latest ? (target.Latest.status === 'failed'
            ? failedBadge(target.Latest)
            : `<span class="badge badge-ok">${tr('badge.ok')}</span>`)
            : `<span class="badge badge-pending">${tr('badge.noRuns')}</span>`}</div>
      </div>
      <div class="detail-card">
        <h3>${tr('target.latestRun')}</h3>
        ${target.Latest ? (target.Latest.status === 'failed' ? `
        <div class="detail-row"><span class="key">${tr('target.time')}</span><span class="val">${timeAgo(target.Latest.timestamp)}</span></div>
        <div class="detail-row"><span class="key">${tr('table.phase')}</span><span class="val">${escHTML(target.Latest.phase || '—')}</span></div>
        <div class="detail-row" style="align-items:flex-start"><span class="key">${tr('table.error')}</span><pre class="val" style="color:var(--danger);font-size:12px;white-space:pre-wrap;word-break:break-word;margin:0;background:var(--bg-input);padding:8px;border-radius:4px;max-height:160px;overflow:auto">${escHTML(target.Latest.error || '(no message)')}</pre></div>
        ` : `
        <div class="detail-row"><span class="key">${tr('target.time')}</span><span class="val">${timeAgo(target.Latest.timestamp)}</span></div>
        <div class="detail-row"><span class="key">${tr('table.size')}</span><span class="val">${humanBytes(target.Latest.encryptedSizeBytes)}</span></div>
        <div class="detail-row"><span class="key">${tr('target.sha256')}</span><code class="val" style="font-size:11px">${escHTML((target.Latest.sha256 || '—').substring(0, 16))}${target.Latest.sha256 ? '...' : ''}</code></div>
        <div class="detail-row"><span class="key">${tr('target.verificationLabel')}</span><span class="val">${renderVerificationBadge(target.Latest.verification)}</span></div>
        ${target.Latest.restoreVerification ? `<div class="detail-row"><span class="key">${tr('target.restoreLabel')}</span><span class="val">${renderRestoreVerificationBadge(target.Latest.restoreVerification)}</span></div>` : ''}
        ${renderCharsetRow(target.Latest)}
        ${renderSchemaAgeRow(target.Latest)}
        `) : `<div style="color:var(--text-muted);padding:12px 0">${tr('target.noRuns')}</div>`}
      </div>
    </div>
    ${renderVerificationDetail(target.Latest)}
    ${renderRestoreVerificationDetail(target.Latest)}
    ${renderAnalysisCoverageCard(target)}
    ${runs.length > 0 ? `
    <div class="chart-grid-2">
      <div class="chart-card">
        <h3>${tr('target.dumpSizeTrend')}</h3>
        ${renderSizeChart(runs)}
      </div>
      <div class="chart-card">
        <h3>${tr('target.runStatusHeatmap')}</h3>
        ${renderStatusHeatmap(runs)}
      </div>
    </div>
    ${renderTablesCard(runs)}` : ''}
    <div class="table-card">
      <div class="table-card-header"><h2>${tr('target.runHistory')}</h2></div>
      ${runs.length === 0 ? `<div class="empty-state"><p>${tr('target.noRuns')}</p></div>` : `
      <table>
        <thead><tr>
          <th class="num row-num">#</th>
          <th class="sortable" onclick="toggleSort('runs','timestamp')">${tr('table.timestamp')}${sortIndicator('runs','timestamp')}</th>
          <th class="sortable" onclick="toggleSort('runs','status')">${tr('table.status')}${sortIndicator('runs','status')}</th>
          <th class="num sortable" onclick="toggleSort('runs','size')">${tr('table.size')}${sortIndicator('runs','size')}</th>
          <th>${tr('table.destinations')}</th>
          <th class="sortable" onclick="toggleSort('runs','verification')">${tr('table.verification')}${sortIndicator('runs','verification')}</th>
          <th class="sortable" onclick="toggleSort('runs','restoreVerification')">${tr('target.restoreLabel')}${sortIndicator('runs','restoreVerification')}</th>
          <th class="sortable" onclick="toggleSort('runs','schema')">Schema${sortIndicator('runs','schema')}</th>
          <th class="num sortable" onclick="toggleSort('runs','tables')">${tr('verification.table')}${sortIndicator('runs','tables')}</th>
          <th class="sortable" onclick="toggleSort('runs','anomalies')">${tr('table.anomalies')} / ${tr('table.error')}${sortIndicator('runs','anomalies')}</th>
          <th>${tr('row.download')}</th>
        </tr></thead>
        <tbody>${sortRuns(runs).map((r, i) => `<tr>
          <td class="num row-num">${i + 1}</td>
          <td style="font-size:12px">${r.timestamp ? new Date(r.timestamp.replace(/(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z/,'$1-$2-$3T$4:$5:$6Z')).toLocaleString() : '—'}</td>
          <td>${r.status === 'failed' ? failedBadge(r) : `<span class="badge badge-ok">${tr('badge.ok')}</span>`}</td>
          <td class="num" style="font-size:12px">${r.status !== 'failed' ? humanBytes(r.encryptedSizeBytes) : '—'}</td>
          <td style="font-size:11px">${r.destinations && r.destinations.length > 0
            ? r.destinations.map(d => {
                const cls = d.status === 'success' ? 'badge-ok' : 'badge-failed';
                const tip = d.error ? ' title="' + escAttr(d.error) + '"' : '';
                return '<span class="badge ' + cls + '" style="margin:1px;font-size:10px"' + tip + '>' + escHTML(d.name) + '</span>';
              }).join('')
            : '<span style="color:var(--text-muted)">—</span>'}</td>
          <td>${renderVerificationBadge(r.verification)}</td>
          <td>${renderRestoreVerificationBadge(r.restoreVerification)}</td>
          <td>${renderSchemaCharsetCell(r)}</td>
          <td class="num" style="font-size:12px">${r.stats && r.stats.tables ? r.stats.tables.length : '—'}</td>
          <td>${r.status === 'failed'
            ? `<span style="color:var(--danger);font-size:12px;word-break:break-word" title="${escAttr(r.error || '')}">${escHTML(truncate(r.error, 120) || '(no message)')}</span>`
            : (r.report && r.report.anomalies ? `<span class="num" style="color:var(--danger)">${r.report.anomalies.length}</span>` : '<span class="num">0</span>')}</td>
          <td>${r.status !== 'failed' ? renderDownloadLinks(name, r, target.Destinations) : '—'}</td>
        </tr>`).join('')}</tbody>
      </table>`}
    </div>`;

  // Per-second tick of the running-banner progress bar — same pattern as
  // the Jobs page so the user sees the time bar advance without waiting
  // for the next SSE refresh.
  if (jobProgressTimer) { clearInterval(jobProgressTimer); jobProgressTimer = null; }
  if (runningJob) {
    jobProgressTimer = setInterval(() => {
      const host = document.querySelector(`.running-banner[data-job-name="${CSS.escape(runningJob.name)}"] .job-progress-host`);
      if (host) host.innerHTML = renderProgressCell(runningJob);
    }, 1000);
  }
}

function renderDownloadLinks(targetName, run, destNames) {
  const ts = escHTML(run.timestamp);
  const successDests = run.destinations ? run.destinations.filter(d => d.status === 'success') : [];
  if (successDests.length <= 1) {
    const destName = successDests.length === 1 ? successDests[0].name : '';
    const destParam = destName ? '?destination=' + encodeURIComponent(destName) : '';
    const destArg  = destName ? `'${escHTML(destName)}'` : 'null';
    return `<button class="btn btn-ghost btn-sm" style="font-size:11px" onclick="viewMeta('${escJS(targetName)}','${ts}',${destArg})" title="View the unencrypted meta.json sidecar (table stats, schema fingerprint, SHA256) in the browser">&#128065;</button>
      <a href="/download/${escAttr(targetName)}/${ts}/meta${destParam}" download="${escAttr(targetName)}-${ts}.meta.json" class="btn btn-ghost btn-sm" style="font-size:11px" title="Download the unencrypted meta.json sidecar">.json</a>
      <a href="/download/${escAttr(targetName)}/${ts}/dump${destParam}" class="btn btn-ghost btn-sm" style="font-size:11px" title="Download the age-encrypted dump (.sql.gz.age). Decrypt locally with backup-restore + your offline private key.">.age</a>`;
  }
  return `<div class="dropdown" style="display:inline-block">
    <button class="btn btn-ghost btn-sm" style="font-size:11px" onclick="this.nextElementSibling.classList.toggle('open')" title="This run was uploaded to multiple destinations — pick one to download or view from">Download &#9662;</button>
    <div class="dropdown-menu">${successDests.map(d =>
      `<a href="/download/${escAttr(targetName)}/${ts}/dump?destination=${encodeURIComponent(d.name)}" class="dropdown-item" style="font-size:12px" title="Download the age-encrypted dump from ${escAttr(d.name)}">
        ${escHTML(d.name)} <span style="opacity:0.6;font-size:10px">(${d.storageType})</span>
      </a>`
    ).join('')}
    <hr style="margin:4px 0;border:none;border-top:1px solid var(--border)">
    ${successDests.map(d =>
      `<button class="dropdown-item" style="font-size:11px;text-align:left;background:none;border:none;width:100%;cursor:pointer" onclick="viewMeta('${escJS(targetName)}','${ts}','${escJS(d.name)}')" title="View the meta.json sidecar from ${escAttr(d.name)} in the browser">
        &#128065; view: ${escHTML(d.name)}
      </button>`
    ).join('')}
    ${successDests.map(d =>
      `<a href="/download/${escAttr(targetName)}/${ts}/meta?destination=${encodeURIComponent(d.name)}" download="${escAttr(targetName)}-${ts}.meta.json" class="dropdown-item" style="font-size:11px;opacity:0.7" title="Download the meta.json sidecar from ${escAttr(d.name)}">
        meta: ${escHTML(d.name)}
      </a>`
    ).join('')}
    </div>
  </div>`;
}

// --- JSON viewer ---
window.viewMeta = async function(targetName, timestamp, destination) {
  const url = '/download/' + encodeURIComponent(targetName) + '/' + encodeURIComponent(timestamp) + '/meta' +
    (destination ? '?destination=' + encodeURIComponent(destination) : '');
  openModal(targetName + ' — ' + timestamp, '<div class="empty-state"><div class="spinner"></div></div>');
  try {
    const resp = await fetch(url);
    if (!resp.ok) throw new Error('HTTP ' + resp.status);
    const data = await resp.json();
    const pretty = JSON.stringify(data, null, 2);
    const fname = `${targetName}-${timestamp}.meta.json`;
    $('#modal-body').innerHTML = `
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:12px;gap:12px;flex-wrap:wrap">
        <span style="color:var(--text-muted);font-size:12px">${destination ? 'destination: <code>' + escHTML(destination) + '</code>' : ''}</span>
        <div style="display:flex;gap:6px">
          <button class="btn btn-ghost btn-sm" onclick="copyToClipboard(this, ${JSON.stringify(JSON.stringify(data, null, 2))})" title="Copy the full pretty-printed JSON to clipboard">Copy</button>
          <a href="${url}" download="${escAttr(fname)}" class="btn btn-secondary btn-sm" title="Save this meta.json as a file">↓ Download</a>
        </div>
      </div>
      <pre class="json-viewer">${jsonHighlight(pretty)}</pre>`;
  } catch(e) {
    $('#modal-body').innerHTML = `<div class="empty-state"><h3>${tr('card.failedToLoad')}</h3><p style="color:var(--danger)">${escHTML(e.message)}</p></div>`;
  }
};

function jsonHighlight(json) {
  const escaped = escHTML(json);
  return escaped.replace(
    /("(?:\\.|[^"\\])*"(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d+)?(?:[eE][+\-]?\d+)?)/g,
    function(match, _g1, colon) {
      if (match[0] === '"') return colon ? '<span class="json-key">' + match + '</span>' : '<span class="json-str">' + match + '</span>';
      if (match === 'true' || match === 'false') return '<span class="json-bool">' + match + '</span>';
      if (match === 'null') return '<span class="json-null">' + match + '</span>';
      return '<span class="json-num">' + match + '</span>';
    }
  );
}

window.copyToClipboard = async function(btn, text) {
  try {
    await navigator.clipboard.writeText(text);
    const orig = btn.textContent;
    btn.textContent = 'Copied!';
    setTimeout(() => { btn.textContent = orig; }, 1500);
  } catch(e) {
    toast('Copy failed: ' + e.message, 'error');
  }
};

// Per-DB-type capability matrix for analysis features. "active" means the
// check is wired and meaningful; "n/a" means the engine doesn't model the
// concept (e.g. mongo has no DB-level charset). The matrix is the source of
// truth for what the Analysis Coverage card renders — keep in sync with the
// dumper / analyzer code paths.
const ANALYSIS_CAPS = {
  postgres: { schema: true, charset: true,  rowCounter: true,  emptyDump: true, sizeDrop: true, scrub: true },
  mysql:    { schema: true, charset: true,  rowCounter: true,  emptyDump: true, sizeDrop: true, scrub: true },
  mariadb:  { schema: true, charset: true,  rowCounter: true,  emptyDump: true, sizeDrop: true, scrub: true },
  mongo:    { schema: true, charset: false, rowCounter: false, emptyDump: true, sizeDrop: true, scrub: true },
  redis:    { schema: true, charset: false, rowCounter: false, emptyDump: true, sizeDrop: true, scrub: true },
};

// Three-state status per check, rendered as a colored badge.
//   active   → check is on for this source (toggle on, DB type supports it)
//   disabled → toggle is off (operator opted out)
//   n/a      → DB type doesn't support this check at all
function analysisStatus(toggleOn, supported) {
  if (!supported) return { cls: 'badge-pending', label: tr('analysis.status.na'), icon: '—' };
  if (!toggleOn)  return { cls: 'badge-warn',    label: tr('analysis.status.disabled'), icon: '⊘' };
  return                 { cls: 'badge-ok',      label: tr('analysis.status.active'), icon: '✓' };
}

function renderAnalysisCoverageCard(target) {
  if (!target) return '';
  const caps = ANALYSIS_CAPS[target.DBType] || {};
  const a = target.Analysis || {};
  const analyzerOn = a.analyzerEnabled !== false; // default true
  const emptyOn = a.emptyDumpCheck !== false;     // default true

  // Each row pairs a check with a single status. We tie the analyzer-driven
  // signals (schema/charset/row-drop/size-drop) to the analyzerEnabled
  // toggle because turning the analyzer off skips stats collection entirely
  // — the underlying gauges go absent and no alert can fire.
  const rows = [
    { name: tr('analysis.row.emptyDumpCheck'),
      desc: caps.rowCounter ? tr('analysis.row.emptyDumpDescOn') : tr('analysis.row.emptyDumpDescOff'),
      status: analysisStatus(emptyOn, caps.emptyDump) },
    { name: tr('analysis.row.schemaDrift'),
      desc: tr('analysis.row.schemaDriftDesc'),
      status: analysisStatus(analyzerOn, caps.schema) },
    { name: tr('analysis.row.charsetDrift'),
      desc: tr('analysis.row.charsetDriftDesc'),
      status: analysisStatus(analyzerOn, caps.charset) },
    { name: tr('analysis.row.rowCountDrop'),
      desc: tr('analysis.row.rowCountDropDesc'),
      status: analysisStatus(analyzerOn, caps.rowCounter) },
    { name: tr('analysis.row.dumpSizeCollapse'),
      desc: tr('analysis.row.dumpSizeCollapseDesc'),
      status: analysisStatus(analyzerOn, caps.sizeDrop) },
    { name: tr('analysis.row.storageScrub'),
      desc: tr('analysis.row.storageScrubDesc'),
      status: analysisStatus(true, caps.scrub) },
  ];

  return `
    <div class="table-card">
      <div class="table-card-header"><h2>${tr('target.analysisCoverage')}</h2></div>
      <div style="padding:8px 16px 4px;color:var(--text-muted);font-size:12px">${tr('analysis.subtitle', {dbType: target.DBType})}</div>
      <table>
        <thead><tr>
          <th class="num row-num">#</th>
          <th>${tr('analysis.check')}</th>
          <th>${tr('table.status')}</th>
          <th>${tr('analysis.notes')}</th>
        </tr></thead>
        <tbody>${rows.map((r, i) => `<tr>
          <td class="num row-num">${i + 1}</td>
          <td><strong>${escHTML(r.name)}</strong></td>
          <td><span class="badge ${r.status.cls}">${r.status.icon} ${r.status.label}</span></td>
          <td style="color:var(--text-muted);font-size:12px">${escHTML(r.desc)}</td>
        </tr>`).join('')}</tbody>
      </table>
    </div>`;
}

// Storage-scrub chip rendered under a destination-health badge. Hidden when
// the operator has no scrub data for this (target, destination) pair —
// either because STORAGE_SCRUB_ENABLED is false or because the scrubber
// hasn't run yet. ScrubLastCheck=0 means "never scrubbed".
function renderScrubChip(h) {
  if (!h || !h.scrubStatus) return '';
  const ok = h.scrubStatus === 'ok';
  const cls = ok ? 'badge-ok' : 'badge-failed';
  const icon = ok ? '&#10003;' : '&#10007;';
  const when = h.scrubLastCheck
    ? ' (' + timeAgo(new Date(h.scrubLastCheck * 1000).toISOString()) + ')'
    : '';
  const tip = ok
    ? 'SHA256 scrub matched meta.json' + when
    : 'SHA256 scrub failed' + (h.scrubFailedTotal ? ' — ' + h.scrubFailedTotal + ' total failures' : '') + when;
  return '<div style="font-size:10px;margin-top:2px"><span class="badge ' + cls + '" style="font-size:9px;padding:1px 4px" title="' + escHTML(tip) + '">' + icon + ' scrub</span></div>';
}

// Run-history "Schema" cell. Combines schema-fingerprint drift and charset
// drift into one column to keep table width sane. The two are independent
// signals (schema = column shape; charset = encoding) but in practice a
// reviewer scanning history wants both at a glance.
function renderSchemaCharsetCell(r) {
  if (!r.report) return '—';
  const schemaBadge = r.report.schemaChanged
    ? `<span class="badge badge-failed">${tr('schemaCell.changed')}</span>`
    : `<span class="badge badge-ok">${tr('schemaCell.stable')}</span>`;
  const charsetBadge = r.report.charsetChanged
    ? ' <span class="badge badge-warn" style="font-size:10px" title="charset/collation changed since previous run">CS</span>'
    : '';
  return schemaBadge + charsetBadge;
}

// Charset / collation row. Renders nothing for runs without recorded encoding
// (mongo, redis, legacy metas) — keeps the detail card uncluttered for DB
// types where the field doesn't apply. Drift indicator pulls from
// report.charsetChanged so it lines up with the BackupCharsetChanged alert.
function renderCharsetRow(run) {
  if (!run || !run.stats) return '';
  const cs = run.stats.charset || '';
  const co = run.stats.collation || '';
  if (!cs && !co) return '';
  const drift = run.report && run.report.charsetChanged;
  const value = escHTML(cs) + (co ? ' / ' + escHTML(co) : '');
  const badge = drift
    ? '<span class="badge badge-warn" style="margin-left:8px;font-size:10px" title="character_set or collation differs from previous run — multibyte chars may truncate on restore">drift</span>'
    : '';
  return `<div class="detail-row"><span class="key">${tr('row.charset')}</span><span class="val" style="font-family:var(--font-mono,monospace);font-size:12px">${value}${badge}</span></div>`;
}

// "Schema unchanged for N days" — leverages meta.schemaChangedAt which is
// carried forward across runs, so a single meta tells you the schema's age.
// Hidden when the field is absent (legacy metas, mongo without schema hash).
function renderSchemaAgeRow(run) {
  if (!run || !run.schemaChangedAt) return '';
  const t = new Date(run.schemaChangedAt);
  if (isNaN(t.getTime())) return '';
  const days = Math.floor((Date.now() - t.getTime()) / 86400000);
  const label = days <= 0 ? tr('time.today') : days === 1 ? tr('time.oneDay') : tr('time.nDays', {n: days});
  const tip = ' title="Schema fingerprint last changed at ' + escHTML(t.toISOString()) + '. Old schemas may not match the current application."';
  return `<div class="detail-row"><span class="key">${tr('schemaAge.label')}</span><span class="val"${tip}>${tr('time.unchangedFor', {label})}</span></div>`;
}

// --- Verification ---
function renderVerificationBadge(v) {
  if (!v) return '<span style="color:var(--text-muted)">—</span>';
  const clsMap = {
    'match': 'badge-ok',
    'mismatch': 'badge-failed',
    'partial': 'badge-warn',
    'skipped': 'badge-pending'
  };
  const cls = clsMap[v.verdict] || 'badge-pending';
  const labelKey = 'verification.verdictLabel.' + v.verdict;
  const label = tr(labelKey) === labelKey ? (v.verdict || '?') : tr(labelKey);
  const tip = v.summary ? ' title="' + escHTML(v.summary) + '"' : '';
  return `<span class="badge ${cls}"${tip}>${label}</span>`;
}

// renderRestoreVerificationBadge renders the verdict for a single run's
// restoreVerification block (or "—" when absent). Distinct from
// renderVerificationBadge — that one targets the in-stream DumpVerification
// produced during the dump. This one targets the post-upload round-trip
// proof (decrypt → parse / restore against an ephemeral DB pod).
function renderRestoreVerificationBadge(rv) {
  if (!rv) return '<span style="color:var(--text-muted)">—</span>';
  const clsMap = {
    'match':    'badge-ok',
    'mismatch': 'badge-failed',
    'partial':  'badge-warn',
    'skipped':  'badge-pending'
  };
  const cls = clsMap[rv.verdict] || 'badge-pending';
  const labelKey = 'verification.verdictLabel.' + rv.verdict;
  const label = tr(labelKey) === labelKey ? (rv.verdict || '?') : tr(labelKey);
  const mode = rv.mode ? ' · ' + escHTML(rv.mode) : '';
  const tip = rv.summary ? ' title="' + escHTML(rv.summary) + '"' : '';
  return `<span class="badge ${cls}"${tip}>${label}${mode}</span>`;
}

function renderRestoreVerificationDetail(run) {
  if (!run || !run.restoreVerification || run.status === 'failed') return '';
  const rv = run.restoreVerification;
  const dur = (rv.durationSeconds != null && !isNaN(rv.durationSeconds))
    ? rv.durationSeconds.toFixed(1) + 's' : '—';
  const completed = rv.completedAt ? new Date(rv.completedAt).toLocaleString() : '—';
  // statsError lives on the run, not on rv: it explains *why* preTables
  // was empty and therefore why a schema-only / sample / full verifier
  // ended up Skipped. Without surfacing it here, the operator sees
  // "Skipped" with no reason and reaches for unrelated toggles.
  const skippedHint = (rv.verdict === 'skipped' && run.statsError)
    ? `<div class="detail-row" style="align-items:flex-start"><span class="key">Stats error</span><pre class="val" style="color:var(--warn,#f0b400);font-size:12px;white-space:pre-wrap;word-break:break-word;margin:0;background:var(--bg-input);padding:8px;border-radius:4px;max-height:160px;overflow:auto" title="The pre-dump CollectStats call failed, so the verifier had no preTables to compare against. Fix the underlying access (DB permissions, network) — the analyzer toggle is unrelated.">${escHTML(run.statsError)}</pre></div>` : '';
  return `
    <div class="table-card verification-card">
      <div class="table-card-header">
        <h2>${tr('card.restoreVerificationCard')}</h2>
        ${renderRestoreVerificationBadge(rv)}
      </div>
      ${rv.summary ? `<div class="verification-summary">${escHTML(rv.summary)}</div>` : ''}
      <div class="detail-row"><span class="key">${tr('form.source.label.verificationMode')}</span><span class="val">${escHTML(rv.mode || '—')}</span></div>
      <div class="detail-row"><span class="key">${tr('table.completedAt')}</span><span class="val">${completed}</span></div>
      <div class="detail-row"><span class="key">${tr('table.duration')}</span><span class="val">${dur}</span></div>
      ${rv.ephemeralRecipientFingerprint ? `<div class="detail-row"><span class="key">Ephemeral recipient</span><code class="val" style="font-size:11px">${escHTML(rv.ephemeralRecipientFingerprint)}</code></div>` : ''}
      ${skippedHint}
      ${rv.error ? `<div class="detail-row" style="align-items:flex-start"><span class="key">Error</span><pre class="val" style="color:var(--danger);font-size:12px;white-space:pre-wrap;word-break:break-word;margin:0;background:var(--bg-input);padding:8px;border-radius:4px;max-height:160px;overflow:auto">${escHTML(rv.error)}</pre></div>` : ''}
    </div>`;
}

function renderVerificationDetail(run) {
  if (!run || !run.verification || run.status === 'failed') return '';
  const v = run.verification;
  if (!v.tables || v.tables.length === 0) return '';

  const verdictIcon = { 'match': '&#10003;', 'mismatch': '&#10007;', 'partial': '&#9888;', 'skipped': '—' };
  const verdictCls = { 'match': 'badge-ok', 'mismatch': 'badge-failed', 'partial': 'badge-warn', 'skipped': 'badge-pending' };
  const hasDumpCounts = v.dumpRowCounts && Object.keys(v.dumpRowCounts).length > 0;

  return `
    <div class="table-card verification-card">
      <div class="table-card-header">
        <h2>${tr('card.dumpVerification')}</h2>
        ${renderVerificationBadge(v)}
      </div>
      <div class="verification-summary">${escHTML(v.summary || '')}</div>
      <table>
        <thead><tr>
          <th class="num row-num">#</th>
          <th>${tr('verification.table')}</th>
          <th class="num">${tr('verification.preDumpRows')}</th>
          <th class="num">${tr('verification.postDumpRows')}</th>
          ${hasDumpCounts ? '<th class="num">' + tr('verification.dumpRows') + '</th>' : ''}
          <th>${tr('verification.verdict')}</th>
          <th>${tr('verification.detail')}</th>
        </tr></thead>
        <tbody>${v.tables.map((t, i) => `<tr>
          <td class="num row-num">${i + 1}</td>
          <td style="font-size:12px;font-family:var(--font-mono,monospace)">${escHTML(t.name)}</td>
          <td class="num" style="font-size:12px">${fmtCount(t.preDumpRows)}</td>
          <td class="num" style="font-size:12px">${fmtCount(t.postDumpRows)}</td>
          ${hasDumpCounts ? `<td class="num" style="font-size:12px">${fmtCount(t.dumpRows)}</td>` : ''}
          <td><span class="badge ${verdictCls[t.verdict] || 'badge-pending'}">${verdictIcon[t.verdict] || '?'} ${escHTML(t.verdict)}</span></td>
          <td style="font-size:11px;color:var(--text-muted)">${escHTML(t.detail || '')}</td>
        </tr>`).join('')}</tbody>
      </table>
    </div>`;
}

// --- Trigger ---
window.triggerBackup = async function(targetName) {
  try {
    await api('/api/trigger/' + targetName, { method: 'POST' });
    toast(tr('toast.triggered') + ': ' + targetName, 'success');
  } catch(e) { toast(tr('toast.triggerFailed', {error: e.message}), 'error'); }
};

// Pause/resume scheduled runs without deleting the source. Hits the
// dedicated suspend endpoint so we don't have to send the full source
// body — that endpoint only patches the suspended annotation.
window.toggleSourceSuspend = async function(secretName, displayName, suspend) {
  try {
    await api('/api/sources/' + secretName + '/suspend', {
      method: 'POST',
      body: JSON.stringify({ suspend }),
    });
    toast((suspend ? tr('toast.paused') : tr('toast.resumed')) + ': ' + displayName, 'success');
    renderPage(currentPage());
  } catch(e) {
    toast((suspend ? tr('buttons.pause') : tr('buttons.resume')) + ': ' + e.message, 'error');
  }
};

// --- Settings Wizard ---
let settingsStep = 0;
// Built fresh each call so the active language wins. The result is read
// dozens of times within a single render — keep callers light by using a
// local const.
function getSettingsSteps() {
  return [
    { id: 'schedule', title: tr('page.settings.section.schedule'), icon: '&#128339;' },
    { id: 'retention', title: tr('page.settings.section.retention'), icon: '&#128451;' },
    { id: 'resources', title: tr('page.settings.section.worker'), icon: '&#9881;' },
    { id: 'review', title: tr('page.settings.section.review'), icon: '&#10003;' }
  ];
}

window.renderSettings = renderSettings;
async function renderSettings(loading = true) {
  if (loading) showLoading();
  let settings = null;
  let errorMsg = '';
  try {
    const resp = await api('/api/settings');
    settings = resp.settings || null;
  } catch(e) {
    errorMsg = e.message || 'unknown error';
    console.error('[Settings] Failed to load:', errorMsg);
  }

  if (!settings) {
    window._currentSettings = null;
    content.innerHTML = `
      <div class="page-header">
        <div><h1>${tr('page.settings.title')}</h1><div class="subtitle">${tr('page.settings.subtitle')}</div></div>
      </div>
      <div class="empty-state">
        <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09"/></svg>
        <h3>${tr('page.settings.notAvailable')}</h3>
        <p>${tr('toast.loadFailed', {error: errorMsg || ''})}</p>
        <p style="margin-top:8px;font-size:0.9em;opacity:0.7">Ensure the operator is deployed with <code>ui.enabled=true</code> and the Docker image is rebuilt after code changes.</p>
        <button class="btn btn-primary" onclick="renderSettings(true)" style="margin-top:12px" title="Reload the settings ConfigMap from Kubernetes">${tr('buttons.refresh')}</button>
      </div>`;
    return;
  }

  window._currentSettings = settings;
  settingsStep = 0;
  renderSettingsPage(settings);
}

function renderSettingsPage(settings) {
  const settingsSteps = getSettingsSteps();
  content.innerHTML = `
    <div class="page-header">
      <div><h1>${tr('page.settings.title')}</h1><div class="subtitle">${tr('page.settings.wizardSubtitle')}</div></div>
      <div style="display:flex;gap:8px">
        <button class="btn btn-secondary" onclick="exportSettings()" title="Download a values.yaml snippet matching the current settings — drop into your Helm chart for GitOps-style management">&#8681; ${tr('buttons.exportValues')}</button>
      </div>
    </div>
    <div class="wizard">
      <div class="wizard-steps">
        ${settingsSteps.map((s, i) => `
          <div class="wizard-step ${i === settingsStep ? 'active' : ''} ${i < settingsStep ? 'done' : ''}" onclick="goToStep(${i})">
            <div class="wizard-step-num">${i < settingsStep ? '&#10003;' : i + 1}</div>
            <div class="wizard-step-label">${s.title}</div>
          </div>
          ${i < settingsSteps.length - 1 ? '<div class="wizard-step-line"></div>' : ''}
        `).join('')}
      </div>
      <form id="settingsForm" onsubmit="submitSettings(event)">
        <div class="wizard-body">
          ${renderSettingsStepContent(settingsStep, settings)}
        </div>
        <div class="wizard-footer">
          <div>
            ${settingsStep > 0 ? '<button type="button" class="btn btn-secondary" onclick="goToStep(' + (settingsStep - 1) + ')" title="Return to the previous wizard step (your changes are preserved)">&#8592; ' + tr('buttons.back') + '</button>' : ''}
          </div>
          <div style="display:flex;gap:8px">
            ${settingsStep < settingsSteps.length - 1
              ? '<button type="button" class="btn btn-primary" onclick="goToStep(' + (settingsStep + 1) + ')" title="Continue to the next wizard step">' + tr('buttons.next') + ' &#8594;</button>'
              : '<button type="submit" class="btn btn-primary" title="Persist all settings to the operator\'s ConfigMap. Takes effect immediately for new runs.">' + tr('buttons.applySettings') + '</button>'}
          </div>
        </div>
      </form>
    </div>
    `;
  // Age-keys management moved to its own page (#/age-keys, see
  // renderAgeKeys) so the sidebar's active-link highlight and direct
  // bookmarking work. Keep loadAgeKeysSection() pure so renderAgeKeys
  // can reuse the existing async-load logic.
}

// --- Age Keys page ---
async function renderAgeKeys(loading = true) {
  if (loading) showLoading();
  content.innerHTML = `
    <div class="page-header">
      <div>
        <h1>${tr('nav.ageKeys') || 'Age Keys'}</h1>
        <div class="subtitle">${tr('ageKeys.subtitle') || 'Public keys (age recipients) backups are encrypted to. Rotation: add new, wait one cycle, remove old.'}</div>
      </div>
    </div>
    <div id="age-keys-section" class="table-card"></div>
  `;
  loadAgeKeysSection();
}

// --- Age recipient (public key) management ---
async function loadAgeKeysSection() {
  const host = $('#age-keys-section');
  if (!host) return;
  host.innerHTML = `<div class="table-card-header"><h2>${tr('ageKeys.title')}</h2></div><div class="empty-state"><div class="spinner"></div></div>`;
  let resp;
  try {
    resp = await fetch('/api/age-keys').then(r => r.json());
  } catch(e) {
    host.innerHTML = `<div class="table-card-header"><h2>${tr('ageKeys.titleShort')}</h2></div>
      <div class="empty-state"><p style="color:var(--danger)">${tr('toast.loadFailed', {error: e.message})}</p></div>`;
    return;
  }
  if (!resp.ok && resp.message) {
    host.innerHTML = `<div class="table-card-header"><h2>${tr('ageKeys.titleShort')}</h2></div>
      <div class="empty-state"><p>${escHTML(resp.message)}</p></div>`;
    return;
  }
  const keys = resp.keys || [];
  const canMutate = !!resp.canMutate;
  host.innerHTML = `
    <div class="table-card-header">
      <h2>${tr('ageKeys.title')}</h2>
      <span style="color:var(--text-muted);font-size:12px">${keys.length} · Secret: <code>${escHTML(resp.secretName || '—')}</code></span>
    </div>
    <p style="padding:0 16px;color:var(--text-muted);font-size:13px;margin:0 0 12px">
      ${tr('ageKeys.intro')}
      ${canMutate ? '' : '<br><span style="color:var(--warning,#d97706)">' + tr('ageKeys.readOnly') + '</span>'}
    </p>
    ${keys.length === 0 ? `<div class="empty-state"><p>${tr('ageKeys.noKeys')} ${tr('ageKeys.noKeysHint')}</p></div>` : `
    <table>
      <thead><tr>
        <th class="num row-num">#</th>
        <th>${tr('row.fingerprint')}</th>
        <th>${tr('common.name')}</th>
        ${canMutate ? '<th style="width:1%"></th>' : ''}
      </tr></thead>
      <tbody>${keys.map((k, i) => `<tr>
        <td class="num row-num">${i + 1}</td>
        <td><code style="font-size:12px;background:var(--bg-input);padding:2px 6px;border-radius:4px">${escHTML(k.hash)}</code></td>
        <td><code style="font-size:11px;word-break:break-all">${escHTML(k.recipient)}</code></td>
        ${canMutate ? `<td style="white-space:nowrap">
          <button class="btn btn-ghost btn-sm" style="color:var(--danger)"
            onclick="removeAgeKey('${escJS(k.recipient)}','${escJS(k.hash)}')"
            title="Remove this public key. Future backups will no longer encrypt to it. The last recipient cannot be removed.">&#10005; ${tr('common.delete')}</button>
        </td>` : ''}
      </tr>`).join('')}</tbody>
    </table>`}
    ${canMutate ? `
    <div style="padding:16px;border-top:1px solid var(--border)">
      <h3 style="font-size:13px;margin:0 0 8px">${tr('ageKeys.addNew')}</h3>
      <form onsubmit="addAgeKey(event)">
        <div style="display:flex;gap:8px">
          <input type="text" name="recipient" required pattern="age1[0-9a-z]+"
            placeholder="age1qx0...your-recipient..."
            style="flex:1;font-family:ui-monospace,monospace;font-size:12px"
            title="An age X25519 recipient — starts with 'age1' followed by base32 characters. Generate with 'age-keygen' offline; paste only the public line here.">
          <button type="submit" class="btn btn-primary btn-sm"
            title="Add this recipient. Future backup runs will encrypt to it in addition to the existing recipients. Validation rejects malformed strings before saving.">+ ${tr('buttons.addKey')}</button>
        </div>
        <div class="hint">Paste the <code>age1...</code> public line from <code>age-keygen</code>. The private key must stay offline — never paste it here.</div>
      </form>
    </div>` : ''}`;
}

window.addAgeKey = async function(ev) {
  ev.preventDefault();
  const form = ev.target;
  const recipient = form.recipient.value.trim();
  if (!recipient) return;
  try {
    const resp = await api('/api/age-keys', { method: 'POST', body: JSON.stringify({ recipient }) });
    toast(resp.message || tr('ageKeys.added'), 'success');
    form.reset();
    loadAgeKeysSection();
  } catch(e) {
    toast(tr('ageKeys.addFailed', {error: e.message}), 'error');
  }
};

window.removeAgeKey = async function(recipient, hash) {
  const ok = confirm(
    'Remove this age recipient?\n\n' +
    'Fingerprint: ' + hash + '\n\n' +
    'Future backups will no longer encrypt to this recipient. ' +
    'Make sure you still hold a private key for one of the other listed recipients — ' +
    'otherwise future backups become un-decryptable.\n\n' +
    'Existing dumps in storage are not affected.'
  );
  if (!ok) return;
  try {
    const resp = await api('/api/age-keys/' + encodeURIComponent(recipient), { method: 'DELETE' });
    toast(resp.message || tr('ageKeys.removed'), 'success');
    loadAgeKeysSection();
  } catch(e) {
    toast(tr('ageKeys.removeFailed', {error: e.message}), 'error');
  }
};

function renderSettingsStepContent(step, s) {
  switch(step) {
    case 0: return `
      <h3>${tr('page.settings.section.schedule')}</h3>
      <p class="wizard-desc">${tr('page.settings.wizard.scheduleDesc')}</p>
      <div class="form-group">
        <label for="defaultSchedule">${tr('page.settings.wizard.label.defaultSchedule')}</label>
        <input type="text" id="defaultSchedule" name="defaultSchedule" value="${escAttr(s.defaultSchedule)}" placeholder="0 2 * * *">
        <div class="hint">${tr('page.settings.wizard.hint.schedule')}</div>
      </div>
      <div class="form-group">
        <label for="runTimeoutSeconds">${tr('page.settings.wizard.label.runTimeout')}</label>
        <input type="number" id="runTimeoutSeconds" name="runTimeoutSeconds" value="${escAttr(s.runTimeoutSeconds)}" placeholder="3600" min="0">
        <div class="hint">${tr('page.settings.wizard.hint.timeout')}</div>
      </div>`;

    case 1: return `
      <h3>${tr('page.settings.section.retention')}</h3>
      <p class="wizard-desc">${tr('page.settings.wizard.retentionDesc')}</p>
      <div class="form-row">
        <div class="form-group">
          <label for="defaultRetentionDays">${tr('page.settings.wizard.label.retentionDays')}</label>
          <input type="number" id="defaultRetentionDays" name="defaultRetentionDays" value="${escAttr(s.defaultRetentionDays)}" placeholder="30" min="0">
          <div class="hint">${tr('page.settings.wizard.hint.retentionDays')}</div>
        </div>
        <div class="form-group">
          <label for="defaultMinKeep">${tr('page.settings.wizard.label.minKeep')}</label>
          <input type="number" id="defaultMinKeep" name="defaultMinKeep" value="${escAttr(s.defaultMinKeep)}" placeholder="3" min="0">
          <div class="hint">${tr('page.settings.wizard.hint.minKeep')}</div>
        </div>
      </div>
      <div class="form-row">
        <div class="form-group">
          <label for="tempDir">${tr('page.settings.wizard.label.tempDir')}</label>
          <input type="text" id="tempDir" name="tempDir" value="${escAttr(s.tempDir)}" placeholder="/tmp/backup-operator">
          <div class="hint">${tr('page.settings.wizard.hint.tempDir')}</div>
        </div>
        <div class="form-group">
          <label for="tempDirSize">${tr('page.settings.wizard.label.tempDirSize')}</label>
          <input type="text" id="tempDirSize" name="tempDirSize" value="${escAttr(s.tempDirSize)}" placeholder="10Gi">
          <div class="hint">${tr('page.settings.wizard.hint.tempDirSize')}</div>
        </div>
      </div>`;

    case 2: return `
      <h3>${tr('page.settings.section.worker')}</h3>
      <p class="wizard-desc">${tr('page.settings.wizard.workerDesc')}</p>
      <div class="form-section">
        <h4>${tr('page.settings.wizard.limits')}</h4>
        <div class="form-row">
          <div class="form-group">
            <label for="workerCpuLimit">${tr('page.settings.wizard.label.cpuLimit')}</label>
            <input type="text" id="workerCpuLimit" name="workerCpuLimit" value="${escAttr(s.workerCpuLimit)}" placeholder="2000m">
            <div class="hint">${tr('page.settings.wizard.hint.cpuLimit')}</div>
          </div>
          <div class="form-group">
            <label for="workerMemoryLimit">${tr('page.settings.wizard.label.memoryLimit')}</label>
            <input type="text" id="workerMemoryLimit" name="workerMemoryLimit" value="${escAttr(s.workerMemoryLimit)}" placeholder="2Gi">
            <div class="hint">${tr('page.settings.wizard.hint.memoryLimit')}</div>
          </div>
        </div>
      </div>
      <div class="form-section">
        <h4>${tr('page.settings.wizard.requests')}</h4>
        <div class="form-row">
          <div class="form-group">
            <label for="workerCpuRequest">${tr('page.settings.wizard.label.cpuRequest')}</label>
            <input type="text" id="workerCpuRequest" name="workerCpuRequest" value="${escAttr(s.workerCpuRequest)}" placeholder="250m">
            <div class="hint">${tr('page.settings.wizard.hint.cpuRequest')}</div>
          </div>
          <div class="form-group">
            <label for="workerMemoryRequest">${tr('page.settings.wizard.label.memoryRequest')}</label>
            <input type="text" id="workerMemoryRequest" name="workerMemoryRequest" value="${escAttr(s.workerMemoryRequest)}" placeholder="256Mi">
            <div class="hint">${tr('page.settings.wizard.hint.memoryRequest')}</div>
          </div>
        </div>
      </div>`;

    case 3:
      return `
      <h3>${tr('page.settings.section.review')}</h3>
      <p class="wizard-desc">${tr('page.settings.wizard.reviewDesc')}</p>
      <div class="review-grid">
        <div class="detail-card">
          <h3>${tr('page.settings.section.schedule')}</h3>
          <div class="detail-row"><span class="key">${tr('table.schedule')}</span><code class="val">${escHTML(s.defaultSchedule)}</code></div>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.timeout')}</span><span class="val">${escHTML(s.runTimeoutSeconds)}s</span></div>
        </div>
        <div class="detail-card">
          <h3>${tr('page.settings.section.retention')}</h3>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.retentionDays')}</span><span class="val">${escHTML(s.defaultRetentionDays)}</span></div>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.minKeep')}</span><span class="val">${escHTML(s.defaultMinKeep)}</span></div>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.tempDir')}</span><code class="val">${escHTML(s.tempDir)}</code></div>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.tempDirSize')}</span><span class="val">${escHTML(s.tempDirSize)}</span></div>
        </div>
        <div class="detail-card">
          <h3>${tr('page.settings.section.worker')}</h3>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.cpuLimit')}</span><span class="val">${escHTML(s.workerCpuLimit) || '—'}</span></div>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.memoryLimit')}</span><span class="val">${escHTML(s.workerMemoryLimit) || '—'}</span></div>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.cpuRequest')}</span><span class="val">${escHTML(s.workerCpuRequest) || '—'}</span></div>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.memoryRequest')}</span><span class="val">${escHTML(s.workerMemoryRequest) || '—'}</span></div>
        </div>
      </div>
      <div class="wizard-note">
        ${tr('page.settings.wizard.note')}
      </div>`;
  }
}

window.goToStep = function(n) {
  // Collect current form values into _currentSettings before navigating.
  const form = $('#settingsForm');
  if (form && window._currentSettings) {
    const fd = new FormData(form);
    for (const [k, v] of fd.entries()) {
      window._currentSettings[k] = v;
    }
  }
  settingsStep = Math.max(0, Math.min(n, getSettingsSteps().length - 1));
  // Re-render from cached settings without refetching from the API.
  renderSettingsPage(window._currentSettings);
};

window.submitSettings = async function(e) {
  e.preventDefault();
  const form = $('#settingsForm');
  if (form && window._currentSettings) {
    const fd = new FormData(form);
    for (const [k, v] of fd.entries()) {
      window._currentSettings[k] = v;
    }
  }
  const s = window._currentSettings;
  if (!s) { toast('No settings loaded — please reload the page', 'error'); return; }

  const btn = form ? form.querySelector('[type="submit"]') : null;
  if (btn) { btn.disabled = true; btn.textContent = 'Saving...'; }

  try {
    await api('/api/settings', { method: 'PUT', body: JSON.stringify(s) });
    toast(tr('toast.settingsSaved'), 'success');
  } catch(e) {
    console.error('[Settings] Save failed:', e.message);
    toast(tr('toast.saveFailed', {error: e.message}), 'error');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = 'Save Settings'; }
  }
};

window.exportSettings = async function() {
  try {
    const resp = await fetch('/api/settings/export');
    if (!resp.ok) {
      const err = await resp.json();
      throw new Error(err.message || 'export failed');
    }
    const blob = await resp.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'values.yaml';
    a.click();
    URL.revokeObjectURL(url);
    toast(tr('toast.exportOk'), 'success');
  } catch(e) { toast(tr('toast.exportFail', {error: e.message}), 'error'); }
};

// --- Close dropdowns on outside click ---
document.addEventListener('click', function(e) {
  if (!e.target.closest('.dropdown')) {
    $$('.dropdown-menu.open').forEach(m => m.classList.remove('open'));
  }
});

// --- Init ---
(async function init() {
  // Always preload EN as the fallback dictionary first, so t() has a
  // safety net even if the user's language fails to load.
  await loadLang(fallbackLang);
  currentLang = detectLang();
  if (currentLang !== fallbackLang) await loadLang(currentLang);
  document.documentElement.lang = currentLang;
  applyStaticTranslations();
  // Wire the language picker (rendered by index.html, populated here).
  const picker = $('#langPicker');
  if (picker) {
    picker.innerHTML = availableLangs.map(code => {
      const name = (dictionaries[code] && dictionaries[code].lang && dictionaries[code].lang.name) || code;
      return `<option value="${code}">${name}</option>`;
    }).join('');
    picker.value = currentLang;
    picker.addEventListener('change', () => setLang(picker.value));
    // Lazy-load the rest of the dictionaries in the background so the
    // picker's labels render in their native names (e.g. "Deutsch").
    availableLangs.forEach(c => { if (c !== currentLang && c !== fallbackLang) loadLang(c).then(() => {
      const opt = picker.querySelector('option[value="' + c + '"]');
      if (opt && dictionaries[c] && dictionaries[c].lang) opt.textContent = dictionaries[c].lang.name;
    }); });
  }
  connectSSE();
  renderPage(currentPage());
  // Sidebar alerts pill — refresh on init and every 30s in the background so
  // the counter is current regardless of which page the user is on.
  refreshAlertsPill();
  setInterval(refreshAlertsPill, 30000);
})();

})();
