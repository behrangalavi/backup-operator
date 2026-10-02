// pages/router.js — hash router + page registry.
// Pages register themselves (registerPage) so the router has no import of
// any page module and the dependency graph stays acyclic.
import { state, newRenderGen } from './state.js';

const _pages = Object.create(null);
export function registerPage(name, fn) { _pages[name] = fn; }
export function registeredPages() { return Object.keys(_pages); }

export function currentPage() {
  const hash = location.hash.slice(2) || 'dashboard';
  return hash.split('/')[0] || 'dashboard';
}
export function currentParam() {
  const parts = (location.hash.slice(2) || '').split('/');
  if (parts.length <= 1) return null;
  const raw = parts.slice(1).join('/');
  // Links are built with targetHref() (encodeURIComponent); decode here so a
  // target named "my db%%" or "a.b" round-trips. Malformed escapes fall back
  // to the raw value rather than throwing inside the router.
  try { return decodeURIComponent(raw); } catch (_) { return raw; }
}

export function renderPage(page, loading = true) {
  // Bump gen ONLY on user-initiated navigation (loading=true) — a user click
  // always completes its render; background SSE re-renders (loading=false)
  // reuse the current gen so they cannot starve a user-initiated render.
  if (loading) newRenderGen();
  $$('.nav-link').forEach(a => {
    a.classList.toggle('active', a.dataset.page === page);
  });
  // Stop the per-second progress-bar tick whenever we leave a page that
  // uses it (Jobs + Target detail). Both renderers re-arm as needed.
  if (page !== 'jobs' && page !== 'target' && state.jobProgressTimer) {
    clearInterval(state.jobProgressTimer);
    state.jobProgressTimer = null;
  }
  const fn = _pages[page] || _pages.dashboard;
  if (fn) fn(loading, currentParam());
}

// Cross-file bindings used by core.js (setLang, sort handlers), which is a
// classic script and cannot import.
window._renderPage = renderPage;
window._currentPage = currentPage;
