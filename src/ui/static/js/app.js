// app.js — Entry point: SSE, router, init
// Modules loaded before this file: js/core.js, js/charts.js, js/pages.js
'use strict';

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
  refresh:             ['dashboard', 'sources', 'destinations', 'target'],
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
let _sseBackoff = 0;
const _sseBaseDelay = 1000;
const _sseMaxDelay = 30000;
function connectSSE() {
  if (eventSource) eventSource.close();
  eventSource = new EventSource('/api/events');
  const dot = $('.status-dot');
  const txt = $('.status-text');

  eventSource.addEventListener('connected', () => {
    dot.className = 'status-dot connected';
    txt.textContent = tr('status.live');
    _sseBackoff = 0;
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
    const delay = Math.min(_sseBaseDelay * Math.pow(2, _sseBackoff), _sseMaxDelay);
    const jitter = delay * (0.5 + Math.random() * 0.5);
    _sseBackoff++;
    setTimeout(connectSSE, jitter);
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

// _renderGen is a monotonic counter bumped ONLY on user-initiated
// navigation (renderPage with loading=true). Each async render function
// captures the current gen at entry; before writing content.innerHTML
// it checks isStaleRender(gen). When the user clicks View 1 → starts
// loading → clicks View 2, View 1's stale fetch resolves later and
// would overwrite View 2's content, producing the "page flickers back
// and forth" symptom. Now View 1's late write is dropped because its
// gen no longer matches. Background SSE re-renders (loading=false) do
// NOT bump the gen — so rapid SSE events can never starve a user-
// initiated render.
let _renderGen = 0;
function newRenderGen() { return ++_renderGen; }
function isStaleRender(gen) { return gen !== _renderGen; }

window.addEventListener('hashchange', () => renderPage(currentPage()));

function renderPage(page, loading = true) {
  // Bump gen ONLY on user-initiated navigation (loading=true) — this
  // ensures a user click always completes its render. Background SSE
  // re-renders (loading=false) reuse the current gen so they cannot
  // invalidate a user-initiated render that is still awaiting data.
  // Without this guard, rapid SSE events (e.g. job_state_change during
  // an active backup) each bumped the gen and dropped the previous
  // render — including the user's — leaving the page stuck on the
  // loading spinner until events calmed down.
  if (loading) newRenderGen();
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


// Cross-file bindings used by core.js (setLang, sort handlers)
window._renderPage = renderPage;
window._currentPage = currentPage;

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
