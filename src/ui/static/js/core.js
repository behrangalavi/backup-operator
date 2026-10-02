// core.js — DOM helpers, i18n, API, toast, modal, helpers, sort
'use strict';


const $ = (sel, ctx) => (ctx || document).querySelector(sel);
const $$ = (sel, ctx) => [...(ctx || document).querySelectorAll(sel)];
const content = $('#content');

// --- Theme ---
// Three states: 'system' (follow prefers-color-scheme — the CSS does this on
// its own, no data-theme attribute), 'light' and 'dark' (explicit override via
// data-theme on <html>, which the stylesheet's token blocks key on). The choice
// is a per-browser convenience, so localStorage is the right home; every access
// is wrapped because storage may be unavailable (private window, blocked site
// data) and the page must still render with the system theme.
const THEMES = ['system', 'light', 'dark'];
function storedTheme() {
  try { const v = localStorage.getItem('theme'); return THEMES.includes(v) ? v : 'system'; } catch (_) { return 'system'; }
}
function applyTheme(mode) {
  if (!THEMES.includes(mode)) mode = 'system';
  const root = document.documentElement;
  if (mode === 'system') root.removeAttribute('data-theme'); else root.setAttribute('data-theme', mode);
  try { if (mode === 'system') localStorage.removeItem('theme'); else localStorage.setItem('theme', mode); } catch (_) { /* storage unavailable */ }
  const picker = $('#themePicker');
  if (picker && picker.value !== mode) picker.value = mode;
}
applyTheme(storedTheme());

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
    // Interpolated values are HTML-escaped: tr() output almost always lands
    // in innerHTML templates, and several vars carry server/K8s error text
    // that can echo user-controlled names. Use trText() for textContent
    // sinks (toast, modal title) where escaping would show literal entities.
    s = s.replace(/\{(\w+)\}/g, (_, k) => (vars[k] != null ? escHTML(String(vars[k])) : '{' + k + '}'));
  }
  return s;
}

// trText is tr() without HTML-escaping of the interpolated vars — for sinks
// that set textContent (toast, modal title), where the browser never parses
// the string as markup and escaping would render "&amp;" literally.
function trText(key, vars) {
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
    s = s.replace(/\{(\w+)\}/g, (_, k) => (vars[k] != null ? String(vars[k]) : '{' + k + '}'));
  }
  return s;
}

// _i18nInstall preloads a dictionary without a network fetch. Used by the
// node test harness; harmless in the browser.
function _i18nInstall(code, dict) { dictionaries[code] = dict; }

window.tr = tr; // expose for easy console debugging

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
    el.title = trText(el.dataset.i18nTitle);
  });
  $$('[data-i18n-aria-label]').forEach(el => {
    el.setAttribute('aria-label', trText(el.dataset.i18nAriaLabel));
  });
  // Refresh the language picker label and the theme picker's selection.
  const picker = $('#langPicker');
  if (picker) picker.value = currentLang;
  const themePicker = $('#themePicker');
  if (themePicker) themePicker.value = storedTheme();
}

// --- API helpers ---
// Default client-side request timeout. Browsers cap concurrent HTTP/1.1
// connections per host at ~6, and the SSE /api/events stream permanently
// occupies one of them. Without a timeout a single request the server
// holds open — e.g. a storage probe stalled on an unreachable QNAP FTPS
// destination — stays "pending" forever and pins a connection slot. A
// few of those saturate the pool, so every other request (even instant
// K8s-API reads like /api/destinations) queues behind them and the page
// renders only the nav with no content. The timeout aborts the stalled
// fetch, frees the slot, and lets the caller's last-good fallback render
// stale data instead of hanging. It is deliberately longer than the
// server-side probe bounds (8 s destination-health/stats/consistency,
// 15 s test-connection) so legitimate slow calls still complete; callers
// that need more can pass opts.timeoutMs.
const DEFAULT_API_TIMEOUT_MS = 20000;
async function api(path, opts = {}) {
  // Only set Content-Type on requests that carry a body. Sending it on
  // bodyless GETs/DELETEs is harmless for same-origin but can trigger a
  // CORS preflight (OPTIONS) when an auth proxy sits in front — which
  // some proxies don't handle, breaking the request with a 405.
  const headers = { ...opts.headers };
  if (opts.body) {
    headers['Content-Type'] = headers['Content-Type'] || 'application/json';
  }

  // Honour a caller-supplied AbortSignal while still enforcing our own
  // timeout. timeoutMs<=0 disables the timeout (e.g. long-poll callers).
  const timeoutMs = opts.timeoutMs != null ? opts.timeoutMs : DEFAULT_API_TIMEOUT_MS;
  const ctrl = new AbortController();
  let timer = null;
  if (timeoutMs > 0) {
    timer = setTimeout(() => ctrl.abort(), timeoutMs);
  }
  if (opts.signal) {
    if (opts.signal.aborted) ctrl.abort();
    else opts.signal.addEventListener('abort', () => ctrl.abort(), { once: true });
  }

  let resp;
  try {
    resp = await fetch(path, { ...opts, headers, signal: ctrl.signal });
  } catch (e) {
    // AbortError (our timeout or a caller abort) and genuine network
    // failures both land here. Surface a typed error so callers'
    // last-good fallbacks treat it like any other transient failure.
    const aborted = e && e.name === 'AbortError';
    const err = new Error(aborted ? 'request timed out' : (e && e.message) || 'network error');
    err.code = aborted ? 'timeout' : 'network_error';
    err.status = 0;
    throw err;
  } finally {
    if (timer) clearTimeout(timer);
  }

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

// fetchWithTimeout is the raw-Response sibling of api(): same AbortController
// timeout, no JSON parsing. For downloads (blob responses) only.
async function fetchWithTimeout(path, opts = {}) {
  const timeoutMs = opts.timeoutMs != null ? opts.timeoutMs : DEFAULT_API_TIMEOUT_MS;
  const ctrl = new AbortController();
  const timer = timeoutMs > 0 ? setTimeout(() => ctrl.abort(), timeoutMs) : null;
  try {
    return await fetch(path, { ...opts, signal: ctrl.signal });
  } catch (e) {
    const aborted = e && e.name === 'AbortError';
    const err = new Error(aborted ? trText('apiError.timeout') : (e && e.message) || trText('apiError.network'));
    err.code = aborted ? 'timeout' : 'network_error';
    throw err;
  } finally {
    if (timer) clearTimeout(timer);
  }
}

// Map an API error to a (message, toast-type) pair. Used by call sites
// that want code-driven presentation; sites that don't care can keep
// using toast(err.message, 'error') and behave exactly as before.
const apiErrorMessages = {
  validation:           e => e.message,
  bad_request:          e => e.message,
  conflict:             e => e.message,
  forbidden:            e => e.message || trText('apiError.forbidden'),
  not_found:            e => e.message || trText('apiError.notFound'),
  method_not_allowed:   e => e.message || trText('apiError.notAllowed'),
  server_error:         e => trText('apiError.server', {error: e.message || trText('apiError.tryAgain')}),
  timeout:              () => trText('apiError.timeout'),
  network_error:        () => trText('apiError.network'),
};
function apiErrorToast(err, fallback) {
  const msg = (apiErrorMessages[err.code] || (() => err.message || fallback || trText('apiError.generic')))(err);
  // 5xx, timeouts and network failures are "something is broken" — red.
  // 4xx is "that request didn't work" — a warning, so operators can tell
  // an invalid input from a dying backend at a glance.
  const broken = err.status >= 500 || err.code === 'server_error' || err.code === 'timeout' || err.code === 'network_error';
  toast(msg, broken ? 'error' : 'warning');
}

// --- Toast ---
function toast(msg, type = 'info') {
  const el = document.createElement('div');
  el.className = 'toast toast-' + type;
  // Errors interrupt assistive tech (role=alert); everything else is
  // announced politely via the container's aria-live region.
  if (type === 'error') el.setAttribute('role', 'alert');
  const text = document.createElement('span');
  text.className = 'toast-text';
  text.textContent = msg;
  const close = document.createElement('button');
  close.className = 'toast-close';
  close.type = 'button';
  close.setAttribute('aria-label', trText('common.dismiss'));
  close.textContent = '\u00d7';
  close.addEventListener('click', () => el.remove());
  el.appendChild(text);
  el.appendChild(close);
  $('#toasts').appendChild(el);
  // Errors stay until dismissed or 8 s — reading time matters more than
  // tidiness for something that went wrong.
  setTimeout(() => el.remove(), type === 'error' ? 8000 : 4000);
}

// --- Modal ---
// Tracks the element that opened the modal so we can return focus to it on
// close. Without this, screen-reader users (and keyboard users in general)
// land at the top of the document after every dialog interaction.
let modalReturnFocus = null;
const MODAL_FOCUSABLE = 'a[href], button:not([disabled]), input:not([type=hidden]):not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
function modalFocusables() {
  return $$(MODAL_FOCUSABLE, $('#modal')).filter(el => el.offsetParent !== null || el === document.activeElement);
}
window.openModal = function(title, bodyHTML) {
  modalReturnFocus = document.activeElement;
  $('#modal-title').textContent = title;
  $('#modal-body').innerHTML = bodyHTML;
  $('#modal-overlay').classList.remove('hidden');
  // The page behind the dialog is inert: no focus, no clicks, hidden from
  // assistive tech. `inert` is the modern primitive; aria-hidden backs it
  // up for browsers that predate it.
  const app = $('#app');
  if (app) { app.inert = true; app.setAttribute('aria-hidden', 'true'); }
  // Focus the first interactive element inside the body. Falls back to the
  // close button so the dialog is always reachable without a mouse.
  const target = $('#modal-body').querySelector(MODAL_FOCUSABLE) || $('#modal-overlay .modal-close');
  if (target) setTimeout(() => target.focus(), 0);
};
window.closeModal = function() {
  $('#modal-overlay').classList.add('hidden');
  const app = $('#app');
  if (app) { app.inert = false; app.removeAttribute('aria-hidden'); }
  if (modalReturnFocus && typeof modalReturnFocus.focus === 'function') {
    modalReturnFocus.focus();
  }
  modalReturnFocus = null;
};
$('#modal-overlay').addEventListener('click', e => {
  if (e.target === $('#modal-overlay')) closeModal();
});
// Esc closes any visible modal; Tab/Shift+Tab cycle within it (focus trap).
document.addEventListener('keydown', e => {
  const overlay = $('#modal-overlay');
  if (overlay.classList.contains('hidden')) return;
  if (e.key === 'Escape') { closeModal(); return; }
  if (e.key !== 'Tab') return;
  const items = modalFocusables();
  if (items.length === 0) { e.preventDefault(); return; }
  const first = items[0], last = items[items.length - 1];
  if (e.shiftKey && (document.activeElement === first || !$('#modal').contains(document.activeElement))) {
    e.preventDefault(); last.focus();
  } else if (!e.shiftKey && document.activeElement === last) {
    e.preventDefault(); first.focus();
  }
});

// --- Delegated actions ---
// The SPA ships a strict Content-Security-Policy (script-src 'self'), which
// forbids inline event handlers. Instead, templates mark elements with
// data-action / data-submit / data-change plus data-arg… attributes, and a
// single set of document-level listeners dispatches to handlers registered
// here. Arguments are plain attribute values (escAttr'd by the template), so
// the "wrong escaper for a JS-in-HTML context" class of XSS cannot recur.
const _actions = Object.create(null);
function registerAction(name, fn) { _actions[name] = fn; }
function registeredActions() { return Object.keys(_actions); }
// actionAttrs renders the data-* attributes for an action with up to three
// arguments. Args are stringified; null/undefined become "".
function actionAttrs(name, ...args) {
  let out = `data-action="${escAttr(name)}"`;
  args.forEach((a, i) => {
    out += ` data-arg${i === 0 ? '' : i + 1}="${escAttr(a == null ? '' : a)}"`;
  });
  return out;
}
function actionArgs(el) {
  const d = el.dataset;
  return [d.arg, d.arg2, d.arg3];
}
function dispatchAction(name, el, e) {
  const fn = _actions[name];
  if (!fn) { console.warn('[actions] no handler registered for', name); return false; }
  fn(el, e, ...actionArgs(el));
  return true;
}
document.addEventListener('click', e => {
  const el = e.target.closest('[data-action]');
  if (!el || el.disabled) return;
  // Native links/buttons keep their default unless the action says otherwise.
  if (el.tagName === 'A' && el.getAttribute('href') && el.getAttribute('href') !== '#') { dispatchAction(el.dataset.action, el, e); return; }
  e.preventDefault();
  dispatchAction(el.dataset.action, el, e);
});
document.addEventListener('submit', e => {
  const f = e.target.closest('form[data-submit]');
  if (!f) return;
  e.preventDefault();
  dispatchAction(f.dataset.submit, f, e);
});
document.addEventListener('change', e => {
  const el = e.target.closest('[data-change]');
  if (!el) return;
  dispatchAction(el.dataset.change, el, e);
});
// Keyboard parity for non-native clickable elements (role=button divs, sortable
// <th>): Enter and Space activate them like a real button.
document.addEventListener('keydown', e => {
  if (e.key !== 'Enter' && e.key !== ' ') return;
  const el = e.target;
  if (!el || !el.matches || !el.matches('[data-action][role="button"]')) return;
  if (el.tagName === 'BUTTON' || el.tagName === 'A' || el.tagName === 'INPUT') return;
  e.preventDefault();
  el.click();
});
registerAction('close-modal', () => closeModal());
registerAction('set-theme', el => applyTheme(el.value));
registerAction('set-sort', (el, e, list) => setSort(list, el.value));
registerAction('flip-sort', (el, e, list) => flipSortDir(list));
registerAction('sort', (el, e, list, col) => toggleSort(list, col));
registerAction('toggle-dropdown', el => {
  const menu = el.nextElementSibling;
  if (!menu) return;
  const open = menu.classList.toggle('open');
  el.setAttribute('aria-expanded', open ? 'true' : 'false');
});

// --- Helpers ---
function fmtCount(n) { return (n != null) ? n.toLocaleString() : '—'; }
// fmtCompact abbreviates large counts for chart labels (1.2K, 3.4M); fmtCount
// is the full locale-formatted number for tables. Formerly a charts.js-local
// `formatNum`.
function fmtCompact(n) {
  if (n == null) return '—';
  if (n >= 1e9) return (n / 1e9).toFixed(1) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
  return String(n);
}
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
// escHTML is for text nodes inside markup: `<`, `>`, `&` (the characters that
// matter in element content). Pure string replacement — no DOM round-trip —
// so it is identical in the browser and in the node test harness.
function escHTML(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;');
}
// targetHref builds the hash-router link for a target. Names are free-form
// (dots, spaces, %, # are all legal), so the path segment is URL-encoded and
// currentParam() decodes it on the way back in.
function targetHref(name) {
  return '#/target/' + encodeURIComponent(name);
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
// escJS is for JavaScript string literals embedded inside HTML attributes.
// No template uses inline handlers any more (CSP), it is kept for the rare
// script-text contexts and for the tests. escHTML/escAttr produce HTML
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
  return `<span class="badge badge-failed"${tip ? ' title="' + tip + '"' : ''}>${tr('badge.failed')}${phase}</span>`;
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
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  return String(a).localeCompare(String(b));
}
function sortBy(arr, getter, dir) {
  const sorted = arr.slice();
  sorted.sort((x, y) => {
    const a = getter(x), b = getter(y);
    // Nulls always sort LAST, independent of direction — so we must decide
    // them BEFORE the asc/desc negation below. (Previously null-handling
    // lived in cmp and got negated for desc, floating null/failed rows to
    // the top of every desc-sorted table.)
    if (a == null && b == null) return 0;
    if (a == null) return 1;
    if (b == null) return -1;
    const r = cmp(a, b);
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
  if (!s || s.col !== col) return '<span class="sort-ind" aria-hidden="true">↕</span>';
  return '<span class="sort-ind active" aria-hidden="true">' + (s.dir === 'asc' ? '▲' : '▼') + '</span>';
}
// sortableTh renders the attributes for a sortable column header: the
// delegated sort action, keyboard reachability and aria-sort for AT.
function sortableTh(list, col) {
  const s = sortState[list];
  const sort = (!s || s.col !== col) ? 'none' : (s.dir === 'asc' ? 'ascending' : 'descending');
  return `${actionAttrs('sort', list, col)} role="button" tabindex="0" aria-sort="${sort}"`;
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
    <span class="sort-label">${tr('common.sort')}</span>
    <select data-change="set-sort" data-arg="${escAttr(list)}" aria-label="${escAttr(tr('common.sortBy'))}">${opts}</select>
    <button class="btn btn-ghost btn-sm sort-dir" ${actionAttrs('flip-sort', list)} title="${escAttr(tr('common.toggleDirection'))}" aria-label="${escAttr(tr('common.toggleDirection'))}">${arrow}</button>
  </div>`;
}
