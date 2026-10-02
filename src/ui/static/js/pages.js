// pages.js — entry module for the page renderers. Each page module registers
// its renderer (registerPage) and its delegated actions (registerAction) as a
// side effect of being imported; this file only pulls them in and re-exports
// what app.js needs. Order is irrelevant — modules reference each other via
// imports, and the classic core.js / charts.js globals are already loaded.
import './pages/state.js';
import './pages/router.js';
import './pages/dashboard.js';
import './pages/sources.js';
import './pages/destinations.js';
import './pages/alerts.js';
import './pages/jobs.js';
import './pages/audit.js';
import './pages/target.js';
import './pages/settings.js';
import './pages/agekeys.js';

export { renderDashboard, refreshFleetSummary } from './pages/dashboard.js';
export { renderSources } from './pages/sources.js';
export { renderDestinations } from './pages/destinations.js';
export { renderAlerts, refreshAlertsPill } from './pages/alerts.js';
export { renderJobs } from './pages/jobs.js';
export { renderAudit } from './pages/audit.js';
export { renderTargetDetail } from './pages/target.js';
export { renderSettings } from './pages/settings.js';
export { renderAgeKeys } from './pages/agekeys.js';
export { renderPage, currentPage, currentParam, registeredPages } from './pages/router.js';
export { state } from './pages/state.js';

// --- Close dropdowns on outside click ---

document.addEventListener('click', function(e) {
  if (!e.target.closest('.dropdown')) {
    $$('.dropdown-menu.open').forEach(m => m.classList.remove('open'));
  }
});
registerAction('goto-target', (el, e, name) => { location.hash = targetHref(name); });
