// pages/audit.js — Audit log page.
// Shared helpers ($, tr, api, escHTML, …) come from the classic core.js /
// charts.js scripts via the global scope; cross-page state lives in state.js.
import { state, isStaleRender } from './state.js';
import { registerPage } from './router.js';

// --- Audit log ---
export let auditFilter = 'all';
export let _auditLastGood = null;
export async function renderAudit(loading = true) {
  const _gen = state.renderGen;
  if (loading) showLoading();
  let data = { entries: [], total: 0, limit: 200 };
  try {
    const url = '/api/audit-log' + (auditFilter !== 'all' ? '?category=' + encodeURIComponent(auditFilter) : '');
    data = (await api(url)) || data;
    _auditLastGood = data;
  } catch(e) {
    if (_auditLastGood) { data = _auditLastGood; }
    else { toast(e.message, 'error'); }
  }
  if (isStaleRender(_gen)) return;

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
        <button class="btn btn-secondary btn-sm" ${actionAttrs('refresh-audit', 'true')} title="${escAttr(trText('tooltip.audit.refresh'))}">&#8635; ${tr('buttons.refresh')}</button>
      </div>
    </div>
    <div style="display:flex;gap:6px;flex-wrap:wrap;margin-bottom:16px">
      ${categories.map(([cat, label, tip]) => `
        <button class="btn btn-${auditFilter === cat ? 'primary' : 'ghost'} btn-sm"
          ${actionAttrs('audit-filter', cat)} title="${escAttr(tip)}">${label}</button>
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

export function setAuditFilter(cat) {
  auditFilter = cat;
  renderAudit(true);
}
registerAction('refresh-audit', () => renderAudit(true));
registerAction('audit-filter', (el, e, cat) => setAuditFilter(cat));
registerPage('audit', loading => renderAudit(loading));
