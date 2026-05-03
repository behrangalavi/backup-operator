// backup-operator SPA
(function() {
'use strict';

const $ = (sel, ctx) => (ctx || document).querySelector(sel);
const $$ = (sel, ctx) => [...(ctx || document).querySelectorAll(sel)];
const content = $('#content');

// --- API helpers ---
async function api(path, opts = {}) {
  const resp = await fetch(path, {
    headers: { 'Content-Type': 'application/json', ...opts.headers },
    ...opts
  });
  const data = await resp.json();
  if (!resp.ok) throw new Error(data.message || resp.statusText);
  return data;
}

// --- SSE ---
let eventSource = null;
function connectSSE() {
  if (eventSource) eventSource.close();
  eventSource = new EventSource('/api/events');
  const dot = $('.status-dot');
  const txt = $('.status-text');

  eventSource.addEventListener('connected', () => {
    dot.className = 'status-dot connected';
    txt.textContent = 'Live';
  });
  eventSource.addEventListener('refresh', () => {
    const page = currentPage();
    if (page === 'dashboard') renderDashboard(false);
    if (page === 'jobs') renderJobs(false);
  });
  ['source_created','source_updated','source_deleted',
   'destination_created','destination_updated','destination_deleted',
   'backup_triggered','settings_updated'].forEach(ev => {
    eventSource.addEventListener(ev, () => renderPage(currentPage(), false));
  });
  eventSource.onerror = () => {
    dot.className = 'status-dot error';
    txt.textContent = 'Disconnected';
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
  switch(page) {
    case 'dashboard': renderDashboard(loading); break;
    case 'sources': renderSources(loading); break;
    case 'destinations': renderDestinations(loading); break;
    case 'jobs': renderJobs(loading); break;
    case 'target': renderTargetDetail(currentParam(), loading); break;
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
window.openModal = function(title, bodyHTML) {
  $('#modal-title').textContent = title;
  $('#modal-body').innerHTML = bodyHTML;
  $('#modal-overlay').classList.remove('hidden');
};
window.closeModal = function() {
  $('#modal-overlay').classList.add('hidden');
};
$('#modal-overlay').addEventListener('click', e => {
  if (e.target === $('#modal-overlay')) closeModal();
});

// --- Helpers ---
function humanBytes(n) {
  if (!n || n === 0) return '0 B';
  const units = ['B','KiB','MiB','GiB','TiB'];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(1)) + ' ' + units[i];
}
function timeAgo(ts) {
  if (!ts) return 'never';
  const d = new Date(ts.replace(/(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z/,
    '$1-$2-$3T$4:$5:$6Z'));
  if (isNaN(d)) return ts;
  const diff = (Date.now() - d.getTime()) / 1000;
  if (diff < 60) return 'just now';
  if (diff < 3600) return Math.floor(diff/60) + 'm ago';
  if (diff < 86400) return Math.floor(diff/3600) + 'h ago';
  return Math.floor(diff/86400) + 'd ago';
}
function escHTML(s) {
  const d = document.createElement('div');
  d.textContent = s || '';
  return d.innerHTML;
}
// Render a Failed badge with phase suffix and full error in tooltip.
// Matches the legacy templates' "✗ failed (phase)" + title=error pattern.
function failedBadge(m) {
  const phase = m && m.phase ? ' (' + escHTML(m.phase) + ')' : '';
  const tip = m && (m.error || m.phase) ? escHTML((m.phase ? m.phase + ': ' : '') + (m.error || '')) : '';
  return `<span class="badge badge-failed"${tip ? ' title="' + tip + '"' : ''}>Failed${phase}</span>`;
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
  if (!s || s.col !== col) return '<span class="sort-ind"></span>';
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
async function renderDashboard(loading = true) {
  if (loading) showLoading();
  let targets = [], dests = [], jobs = [], healthEntries = [], consistencyIssues = [];
  try {
    [targets, dests, jobs, healthEntries, consistencyIssues] = await Promise.all([
      api('/api/targets'), api('/api/destinations'), api('/api/jobs'),
      api('/api/destination-health').catch(() => []),
      api('/api/consistency-check').catch(() => []),
    ]);
  } catch(e) { /* partial data is ok */ }

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
      <div><h1>Dashboard</h1><div class="subtitle">Backup overview</div></div>
    </div>
    <div class="stats-row">
      <div class="stat-card"><div class="label">Sources</div><div class="value">${targets.length}</div></div>
      <div class="stat-card"><div class="label">Healthy</div><div class="value ok">${ok}</div></div>
      <div class="stat-card"><div class="label">Failed</div><div class="value${failed > 0 ? ' bad' : ''}">${failed}</div></div>
      <div class="stat-card"><div class="label">Destinations</div><div class="value">${dests.length}</div></div>
      <div class="stat-card"><div class="label">Running Jobs</div><div class="value">${running}</div></div>
    </div>
    <div class="table-card">
      <div class="table-card-header">
        <h2>Backup Targets</h2>
        <button class="btn btn-primary btn-sm" onclick="location.hash='#/sources';openSourceForm()">+ Add Source</button>
      </div>
      ${targets.length === 0 ? '<div class="empty-state"><h3>No backup sources</h3><p>Create a source to start backing up your databases.</p></div>' : `
      <table>
        <thead><tr>
          <th class="sortable" onclick="toggleSort('dashboard','name')">Target${sortIndicator('dashboard','name')}</th>
          <th class="sortable" onclick="toggleSort('dashboard','dbType')">Type${sortIndicator('dashboard','dbType')}</th>
          <th class="sortable" onclick="toggleSort('dashboard','schedule')">Schedule${sortIndicator('dashboard','schedule')}</th>
          <th class="sortable" onclick="toggleSort('dashboard','status')">Status${sortIndicator('dashboard','status')}</th>
          <th class="sortable" onclick="toggleSort('dashboard','lastRun')">Last Run${sortIndicator('dashboard','lastRun')}</th>
          <th class="num sortable" onclick="toggleSort('dashboard','size')">Size${sortIndicator('dashboard','size')}</th>
          <th class="sortable" onclick="toggleSort('dashboard','createdAt')">Created${sortIndicator('dashboard','createdAt')}</th>
          <th>Destinations</th><th></th>
        </tr></thead>
        <tbody>${sortedTargets.map(t => `<tr>
          <td><a href="#/target/${escHTML(t.Name)}" style="color:var(--accent);font-weight:600">${escHTML(t.Name)}</a></td>
          <td><span class="badge badge-${t.DBType}">${t.DBType}</span></td>
          <td><code style="font-size:12px;background:var(--bg-input);padding:2px 6px;border-radius:4px">${escHTML(t.Schedule)}</code></td>
          <td>${t.Latest ? (t.Latest.status === 'failed'
            ? failedBadge(t.Latest)
            : '<span class="badge badge-ok">OK</span>')
            : '<span class="badge badge-pending">No runs</span>'}</td>
          <td style="color:var(--text-muted);font-size:12px">${t.Latest ? timeAgo(t.Latest.timestamp) : 'never'}</td>
          <td class="num" style="font-size:12px">${t.Latest && !t.Latest.status?.includes('fail') ? humanBytes(t.Latest.encryptedSizeBytes) : '—'}</td>
          <td style="color:var(--text-muted);font-size:12px">${t.CreatedAt ? timeAgo(t.CreatedAt) : '—'}</td>
          <td>${(t.Destinations || []).map(d => `<span class="badge badge-sftp" style="margin:1px">${escHTML(d)}</span>`).join('')}</td>
          <td style="white-space:nowrap">
            <button class="btn btn-ghost btn-sm" onclick="triggerBackup('${escHTML(t.Name)}')" title="Run now">&#9654;</button>
            <button class="btn btn-ghost btn-sm" onclick="openSourceForm('${escHTML(t.SecretName)}')" title="Edit">&#9998;</button>
            <button class="btn btn-ghost btn-sm" style="color:var(--danger)" onclick="deleteSource('${escHTML(t.SecretName)}','${escHTML(t.Name)}')" title="Delete">&#10005;</button>
          </td>
        </tr>`).join('')}</tbody>
      </table>`}
    </div>
    ${consistencyIssues.length > 0 ? `
    <div class="table-card" style="border-left:3px solid var(--danger)">
      <div class="table-card-header"><h2 style="color:var(--danger)">Consistency Warnings</h2></div>
      <p style="padding:0 16px;color:var(--text-muted);font-size:13px;margin:0 0 8px">Backups found in some destinations but missing from others.</p>
      <table>
        <thead><tr><th>Target</th><th>Timestamp</th><th>Present In</th><th>Missing From</th></tr></thead>
        <tbody>${consistencyIssues.slice(0, 20).map(ci => `<tr>
          <td><strong>${escHTML(ci.target)}</strong></td>
          <td style="font-size:12px">${escHTML(ci.timestamp)}</td>
          <td>${(ci.presentIn||[]).map(d => `<span class="badge badge-ok" style="margin:1px">${escHTML(d)}</span>`).join('')}</td>
          <td>${(ci.missingFrom||[]).map(d => `<span class="badge badge-failed" style="margin:1px">${escHTML(d)}</span>`).join('')}</td>
        </tr>`).join('')}</tbody>
      </table>
      ${consistencyIssues.length > 20 ? `<p style="padding:8px 16px;color:var(--text-muted);font-size:12px">...and ${consistencyIssues.length - 20} more</p>` : ''}
    </div>` : ''}
    ${healthEntries.length > 0 && dests.length > 1 ? `
    <div class="table-card">
      <div class="table-card-header"><h2>Destination Health Matrix</h2></div>
      <table>
        <thead><tr>
          <th>Target</th>
          ${[...new Set(healthEntries.map(h => h.destination))].map(d => `<th style="text-align:center">${escHTML(d)}</th>`).join('')}
        </tr></thead>
        <tbody>${(() => {
          const destNames = [...new Set(healthEntries.map(h => h.destination))];
          const targetNames = [...new Set(healthEntries.map(h => h.target))];
          const lookup = {};
          healthEntries.forEach(h => { lookup[h.target + '@' + h.destination] = h; });
          return targetNames.map(t => `<tr>
            <td><a href="#/target/${escHTML(t)}" style="color:var(--accent);font-weight:600">${escHTML(t)}</a></td>
            ${destNames.map(d => {
              const h = lookup[t + '@' + d];
              if (!h) return '<td style="text-align:center"><span class="badge" style="background:var(--bg-input);color:var(--text-muted)">N/A</span></td>';
              const badge = h.status === 'ok' ? 'badge-ok' : h.status === 'failed' ? 'badge-failed' : h.status === 'missing' ? 'badge-pending' : 'badge-failed';
              const label = h.status === 'ok' ? 'OK' : h.status === 'failed' ? 'Failed' : h.status === 'missing' ? 'No data' : 'Unreachable';
              const tip = h.error ? ' title="' + escHTML(h.error) + '"' : '';
              return '<td style="text-align:center"><span class="badge ' + badge + '"' + tip + '>' + label + '</span>' +
                (h.latestRun ? '<div style="font-size:10px;color:var(--text-muted)">' + timeAgo(h.latestRun) + '</div>' : '') + '</td>';
            }).join('')}
          </tr>`).join('');
        })()}</tbody>
      </table>
    </div>` : ''}`;
}

// --- Sources ---
async function renderSources(loading = true) {
  if (loading) showLoading();
  let targets = [];
  try { targets = await api('/api/targets'); } catch(e) { toast(e.message, 'error'); }

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
      <div><h1>Sources</h1><div class="subtitle">Database backup sources</div></div>
      <div style="display:flex;gap:8px;align-items:center">
        ${targets.length > 0 ? renderSortControl('sources', [
          ['createdAt','Created'],['name','Name'],['lastRun','Last Run'],['dbType','Type'],
        ]) : ''}
        <button class="btn btn-primary" onclick="openSourceForm()">+ Add Source</button>
      </div>
    </div>
    ${targets.length === 0 ? `
    <div class="empty-state">
      <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M21 12c0 1.66-4 3-9 3s-9-1.34-9-3"/><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"/></svg>
      <h3>No sources configured</h3>
      <p>Add a database source to start backing up. Sources are Kubernetes Secrets with backup labels.</p>
      <button class="btn btn-primary" onclick="openSourceForm()">+ Add Source</button>
    </div>` : `
    <div style="display:grid;grid-template-columns:repeat(auto-fill,minmax(340px,1fr));gap:16px">
      ${sortedTargets.map(t => `
      <div class="detail-card" style="cursor:pointer" onclick="location.hash='#/target/${escHTML(t.Name)}'">
        <div style="display:flex;justify-content:space-between;align-items:start;margin-bottom:12px">
          <div>
            <div style="font-weight:600;font-size:15px;color:var(--text-heading)">${escHTML(t.Name)}</div>
            <span class="badge badge-${t.DBType}" style="margin-top:6px">${t.DBType}</span>
          </div>
          ${t.Latest ? (t.Latest.status === 'failed'
            ? failedBadge(t.Latest)
            : '<span class="badge badge-ok">OK</span>')
            : '<span class="badge badge-pending">No runs</span>'}
        </div>
        <div class="detail-row"><span class="key">Schedule</span><code class="val">${escHTML(t.Schedule)}</code></div>
        <div class="detail-row"><span class="key">Last run</span><span class="val">${t.Latest ? timeAgo(t.Latest.timestamp) : 'never'}</span></div>
        ${t.Latest && t.Latest.status === 'failed' && t.Latest.error ? `
        <div class="detail-row" style="align-items:flex-start"><span class="key">Error</span><span class="val" style="color:var(--danger);font-size:12px;word-break:break-word" title="${escHTML(t.Latest.error)}">${escHTML(truncate(t.Latest.error, 140))}</span></div>` : ''}
        <div class="detail-row"><span class="key">Created</span><span class="val">${t.CreatedAt ? timeAgo(t.CreatedAt) : '—'}</span></div>
        <div class="detail-row"><span class="key">Destinations</span><span class="val">${(t.Destinations||[]).join(', ') || 'all'}</span></div>
        <div style="display:flex;gap:6px;margin-top:12px;justify-content:flex-end">
          <button class="btn btn-ghost btn-sm" onclick="event.stopPropagation();triggerBackup('${escHTML(t.Name)}')" title="Run now">&#9654; Run</button>
          <button class="btn btn-ghost btn-sm" onclick="event.stopPropagation();openSourceForm('${escHTML(t.SecretName)}')">Edit</button>
          <button class="btn btn-ghost btn-sm" style="color:var(--danger)" onclick="event.stopPropagation();deleteSource('${escHTML(t.SecretName)}','${escHTML(t.Name)}')">Delete</button>
        </div>
      </div>`).join('')}
    </div>`}`;
}

// --- Source Form ---
window.openSourceForm = function(secretName) {
  const isEdit = !!secretName;
  const title = isEdit ? 'Edit Source' : 'New Backup Source';

  let formHTML = `<form id="sourceForm" onsubmit="submitSourceForm(event, '${secretName || ''}')">
    <div class="form-row">
      <div class="form-group"><label>Target Name *</label>
        <input name="name" required placeholder="prod-users" ${isEdit ? 'disabled' : ''}></div>
      <div class="form-group"><label>DB Type *</label>
        <select name="dbType" required>
          <option value="">Select...</option>
          <option value="postgres">PostgreSQL</option>
          <option value="mysql">MySQL</option>
          <option value="mongo">MongoDB</option>
        </select></div>
    </div>
    <div class="form-row">
      <div class="form-group"><label>Host *</label><input name="host" required placeholder="db.example.com"></div>
      <div class="form-group"><label>Port</label><input name="port" placeholder="auto-detect"></div>
    </div>
    <div class="form-row">
      <div class="form-group"><label>Database</label><input name="database" placeholder="mydb"></div>
      <div class="form-group"><label>Schedule</label>
        <input name="schedule" placeholder="0 2 * * *">
        <div class="hint">Cron expression (default: 0 2 * * *)</div></div>
    </div>
    <div class="form-row">
      <div class="form-group"><label>Username *</label><input name="username" required></div>
      <div class="form-group"><label>Password</label><input name="password" type="password" placeholder="${isEdit ? '(unchanged if empty)' : ''}"></div>
    </div>
    <div class="form-section"><h4>Retention & Analysis</h4>
      <div class="form-row">
        <div class="form-group"><label>Retention Days</label><input name="retentionDays" placeholder="30"></div>
        <div class="form-group"><label>Min Keep</label><input name="minKeep" placeholder="3"></div>
      </div>
      <div class="form-row">
        <div class="form-group"><label>Destinations</label>
          <input name="destinations" placeholder="comma-separated or empty for all">
          <div class="hint">Leave empty to fan out to all destinations</div></div>
        <div class="form-group"><label>Anonymize Tables</label>
          <select name="anonymizeTables"><option value="">No</option><option value="true">Yes</option></select></div>
      </div>
    </div>
    <div class="form-actions">
      <button type="button" class="btn btn-secondary" onclick="closeModal()">Cancel</button>
      <button type="submit" class="btn btn-primary">${isEdit ? 'Update' : 'Create'} Source</button>
    </div>
  </form>`;

  openModal(title, formHTML);

  if (isEdit) {
    api('/api/sources/' + secretName).then(src => {
      const f = $('#sourceForm');
      f.name.value = src.name || '';
      f.dbType.value = src.dbType || '';
      f.host.value = src.host || '';
      f.port.value = src.port || '';
      f.database.value = src.database || '';
      f.schedule.value = src.schedule || '';
      f.username.value = src.username || '';
      f.retentionDays.value = src.retentionDays || '';
      f.minKeep.value = src.minKeep || '';
      f.destinations.value = src.destinations || '';
      if (src.anonymizeTables === 'true') f.anonymizeTables.value = 'true';
    }).catch(e => toast('Failed to load source: ' + e.message, 'error'));
  }
};

window.submitSourceForm = async function(e, secretName) {
  e.preventDefault();
  const f = e.target;
  const body = {
    name: f.name.value,
    dbType: f.dbType.value,
    host: f.host.value,
    port: f.port.value,
    database: f.database.value,
    schedule: f.schedule.value,
    username: f.username.value,
    password: f.password.value,
    retentionDays: f.retentionDays.value,
    minKeep: f.minKeep.value,
    destinations: f.destinations.value,
    anonymizeTables: f.anonymizeTables.value === 'true' ? true : null,
  };
  try {
    if (secretName) {
      await api('/api/sources/' + secretName, { method: 'PUT', body: JSON.stringify(body) });
      toast('Source updated', 'success');
    } else {
      await api('/api/sources', { method: 'POST', body: JSON.stringify(body) });
      toast('Source created', 'success');
    }
    closeModal();
    renderPage(currentPage());
  } catch(e) {
    toast(e.message, 'error');
  }
};

window.deleteSource = function(secretName, displayName) {
  openModal('Delete Source', `
    <div class="confirm-text">Are you sure you want to delete <span class="confirm-name">${escHTML(displayName)}</span>?
    This will remove the source Secret and its managed CronJob. Existing backups in storage will not be deleted.</div>
    <div class="form-actions">
      <button class="btn btn-secondary" onclick="closeModal()">Cancel</button>
      <button class="btn btn-danger" onclick="confirmDeleteSource('${secretName}')">Delete Source</button>
    </div>`);
};

window.confirmDeleteSource = async function(secretName) {
  try {
    await api('/api/sources/' + secretName, { method: 'DELETE' });
    toast('Source deleted', 'success');
    closeModal();
    location.hash = '#/sources';
  } catch(e) { toast(e.message, 'error'); }
};

// --- Destinations ---
async function renderDestinations(loading = true) {
  if (loading) showLoading();
  let dests = [], stats = [];
  try {
    [dests, stats] = await Promise.all([
      api('/api/destinations'),
      api('/api/destination-stats').catch(() => []),
    ]);
  } catch(e) { toast(e.message, 'error'); }
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
      <div><h1>Destinations</h1><div class="subtitle">Storage backends for backup uploads</div></div>
      <div style="display:flex;gap:8px;align-items:center">
        ${dests.length > 0 ? renderSortControl('destinations', [
          ['createdAt','Created'],['name','Name'],['storageType','Type'],
        ]) : ''}
        <button class="btn btn-primary" onclick="openDestForm()">+ Add Destination</button>
      </div>
    </div>
    ${dests.length === 0 ? `
    <div class="empty-state">
      <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/></svg>
      <h3>No destinations configured</h3>
      <p>Add a destination to define where backups are stored. Destinations are Kubernetes Secrets with storage labels.</p>
      <button class="btn btn-primary" onclick="openDestForm()">+ Add Destination</button>
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
          <span class="dest-status" id="dest-status-${escHTML(d.secretName)}"></span>
        </div>
        <div class="detail-row"><span class="key">Secret</span><code class="val">${escHTML(d.secretName)}</code></div>
        <div class="detail-row"><span class="key">Host</span><span class="val">${escHTML(d.host || '—')}</span></div>
        <div class="detail-row"><span class="key">Path Prefix</span><span class="val">${escHTML(d.pathPrefix || '/')}</span></div>
        <div class="detail-row"><span class="key">Created</span><span class="val">${d.createdAt ? timeAgo(d.createdAt) : '—'}</span></div>
        ${st && !st.error ? `
        <div style="margin-top:8px;padding-top:8px;border-top:1px solid var(--border)">
          <div class="detail-row"><span class="key">Backups</span><span class="val">${st.backupCount}</span></div>
          <div class="detail-row"><span class="key">Total Size</span><span class="val">${humanBytes(st.totalSizeBytes)}</span></div>
          <div class="detail-row"><span class="key">Oldest</span><span class="val">${st.oldestBackup ? timeAgo(st.oldestBackup) : '—'}</span></div>
          <div class="detail-row"><span class="key">Newest</span><span class="val">${st.newestBackup ? timeAgo(st.newestBackup) : '—'}</span></div>
        </div>` : st && st.error ? `
        <div style="margin-top:8px;padding-top:8px;border-top:1px solid var(--border);color:var(--danger);font-size:12px">
          Storage unreachable: ${escHTML(st.error)}
        </div>` : ''}
        <div style="display:flex;gap:6px;margin-top:12px;justify-content:flex-end">
          <button class="btn btn-ghost btn-sm" onclick="testDestConnection('${escHTML(d.secretName)}','${escHTML(d.name)}')" title="Test connectivity">&#128268; Test</button>
          <button class="btn btn-ghost btn-sm" onclick="openDestForm('${escHTML(d.secretName)}')">Edit</button>
          <button class="btn btn-ghost btn-sm" style="color:var(--danger)" onclick="deleteDest('${escHTML(d.secretName)}','${escHTML(d.name)}')">Delete</button>
        </div>
      </div>`;
      }).join('')}
    </div>`}`;
}

// --- Destination Form ---
window.openDestForm = function(secretName) {
  const isEdit = !!secretName;
  const title = isEdit ? 'Edit Destination' : 'New Destination';

  const sftpFields = `
    <div class="form-row"><div class="form-group"><label>Host *</label><input name="data_host" required></div>
      <div class="form-group"><label>Port</label><input name="data_port" placeholder="22"></div></div>
    <div class="form-group"><label>Username *</label><input name="data_username" required></div>
    <div class="form-group"><label>SSH Private Key</label><textarea name="data_ssh-private-key" rows="3" placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"></textarea></div>
    <div class="form-group"><label>Known Hosts</label><textarea name="data_known-hosts" rows="2" placeholder="ssh-keyscan output"></textarea></div>`;

  const s3Fields = `
    <div class="form-group"><label>Endpoint *</label><input name="data_endpoint" required placeholder="s3.amazonaws.com"></div>
    <div class="form-row"><div class="form-group"><label>Bucket *</label><input name="data_bucket" required></div>
      <div class="form-group"><label>Region</label><input name="data_region" placeholder="us-east-1"></div></div>
    <div class="form-row"><div class="form-group"><label>Access Key</label><input name="data_access-key"></div>
      <div class="form-group"><label>Secret Key</label><input name="data_secret-key" type="password"></div></div>`;

  openModal(title, `<form id="destForm" onsubmit="submitDestForm(event, '${secretName || ''}')">
    <div class="form-row">
      <div class="form-group"><label>Name *</label><input name="name" required placeholder="hetzner-sb" ${isEdit ? 'disabled' : ''}></div>
      <div class="form-group"><label>Storage Type *</label>
        <select name="storageType" required onchange="toggleDestFields(this.value)">
          <option value="">Select...</option>
          <option value="sftp">SFTP</option>
          <option value="hetzner-sftp">Hetzner SFTP</option>
          <option value="s3">S3</option>
        </select></div>
    </div>
    <div class="form-group"><label>Path Prefix</label><input name="pathPrefix" placeholder="/cluster-prod"></div>
    <div id="destTypeFields"></div>
    <div id="destSFTPTemplate" style="display:none">${sftpFields}</div>
    <div id="destS3Template" style="display:none">${s3Fields}</div>
    <div class="form-actions">
      <button type="button" class="btn btn-secondary" onclick="closeModal()">Cancel</button>
      <button type="submit" class="btn btn-primary">${isEdit ? 'Update' : 'Create'} Destination</button>
    </div>
  </form>`);

  if (isEdit) {
    api('/api/destinations/' + secretName).then(d => {
      const f = $('#destForm');
      f.name.value = d.name || '';
      f.storageType.value = d.storageType || '';
      f.pathPrefix.value = d.pathPrefix || '';
      toggleDestFields(d.storageType);
      if (d.data) {
        Object.entries(d.data).forEach(([k, v]) => {
          const inp = f.querySelector(`[name="data_${k}"]`);
          if (inp && v !== '***') inp.value = v;
        });
      }
    }).catch(e => toast('Failed to load destination: ' + e.message, 'error'));
  }
};

window.toggleDestFields = function(type) {
  const container = $('#destTypeFields');
  if (type === 'sftp' || type === 'hetzner-sftp') {
    container.innerHTML = $('#destSFTPTemplate').innerHTML;
  } else if (type === 's3') {
    container.innerHTML = $('#destS3Template').innerHTML;
  } else {
    container.innerHTML = '';
  }
};

window.submitDestForm = async function(e, secretName) {
  e.preventDefault();
  const f = e.target;
  const data = {};
  $$('[name^="data_"]', f).forEach(inp => {
    const key = inp.name.replace('data_', '');
    if (inp.value) data[key] = inp.value;
  });
  const body = {
    name: f.name.value,
    storageType: f.storageType.value,
    pathPrefix: f.pathPrefix.value,
    data: data,
  };
  try {
    if (secretName) {
      await api('/api/destinations/' + secretName, { method: 'PUT', body: JSON.stringify(body) });
      toast('Destination updated', 'success');
    } else {
      await api('/api/destinations', { method: 'POST', body: JSON.stringify(body) });
      toast('Destination created', 'success');
    }
    closeModal();
    renderPage(currentPage());
  } catch(e) { toast(e.message, 'error'); }
};

window.testDestConnection = async function(secretName, displayName) {
  const el = document.getElementById('dest-status-' + secretName);
  if (el) { el.innerHTML = '<span class="badge badge-pending">Testing...</span>'; }
  try {
    const result = await api('/api/destinations/' + secretName + '/test', { method: 'POST' });
    if (result.ok) {
      if (el) el.innerHTML = '<span class="badge badge-ok">Connected</span>';
      toast(displayName + ': connection OK', 'success');
    } else {
      if (el) el.innerHTML = '<span class="badge badge-failed" title="' + escHTML(result.error || '') + '">Failed</span>';
      toast(displayName + ': ' + (result.error || 'connection failed'), 'error');
    }
  } catch(e) {
    if (el) el.innerHTML = '<span class="badge badge-failed">Error</span>';
    toast('Test failed: ' + e.message, 'error');
  }
};

window.deleteDest = function(secretName, displayName) {
  openModal('Delete Destination', `
    <div class="confirm-text">Are you sure you want to delete destination <span class="confirm-name">${escHTML(displayName)}</span>?
    Existing backups in this storage will not be affected.</div>
    <div class="form-actions">
      <button class="btn btn-secondary" onclick="closeModal()">Cancel</button>
      <button class="btn btn-danger" onclick="confirmDeleteDest('${secretName}')">Delete Destination</button>
    </div>`);
};

window.confirmDeleteDest = async function(secretName) {
  try {
    await api('/api/destinations/' + secretName, { method: 'DELETE' });
    toast('Destination deleted', 'success');
    closeModal();
    renderDestinations();
  } catch(e) { toast(e.message, 'error'); }
};

// --- Jobs ---
async function renderJobs(loading = true) {
  if (loading) showLoading();
  let jobs = [];
  try { jobs = await api('/api/jobs'); } catch(e) { toast(e.message, 'error'); }

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
      <div><h1>Jobs</h1><div class="subtitle">Backup job execution history</div></div>
    </div>
    <div class="table-card">
      ${jobs.length === 0 ? '<div class="empty-state"><h3>No jobs yet</h3><p>Jobs appear when backups run — either on schedule or triggered manually.</p></div>' : `
      <table>
        <thead><tr>
          <th class="sortable" onclick="toggleSort('jobs','name')">Job${sortIndicator('jobs','name')}</th>
          <th class="sortable" onclick="toggleSort('jobs','target')">Target${sortIndicator('jobs','target')}</th>
          <th class="sortable" onclick="toggleSort('jobs','status')">Status${sortIndicator('jobs','status')}</th>
          <th class="sortable" onclick="toggleSort('jobs','startTime')">Started${sortIndicator('jobs','startTime')}</th>
          <th class="sortable" onclick="toggleSort('jobs','duration')">Duration${sortIndicator('jobs','duration')}</th>
        </tr></thead>
        <tbody>${sortedJobs.map(j => `<tr>
          <td style="font-family:ui-monospace,monospace;font-size:12px">${escHTML(j.name)}</td>
          <td><strong>${escHTML(j.target || '—')}</strong></td>
          <td><span class="badge badge-${j.status}">${j.status}</span></td>
          <td style="color:var(--text-muted);font-size:12px">${j.startTime ? new Date(j.startTime).toLocaleString() : '—'}</td>
          <td style="font-size:12px">${j.duration || '—'}</td>
        </tr>`).join('')}</tbody>
      </table>`}
    </div>`;
}

function sortRuns(runs) {
  const g = {
    timestamp: r => parseTsCompact(r.timestamp),
    status:    r => r.status || '',
    size:      r => r.status !== 'failed' ? (r.encryptedSizeBytes || 0) : null,
    schema:    r => r.report ? (r.report.schemaChanged ? 1 : 0) : null,
    tables:    r => (r.stats && r.stats.tables) ? r.stats.tables.length : null,
    anomalies: r => (r.report && r.report.anomalies) ? r.report.anomalies.length : 0,
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
async function renderTargetDetail(name, loading = true) {
  if (!name) { renderDashboard(); return; }
  if (loading) showLoading();
  let targets = [], runs = [], dests = [];
  try {
    [targets, dests] = await Promise.all([api('/api/targets'), api('/api/destinations')]);
  } catch(e) { toast(e.message, 'error'); }

  const target = targets.find(t => t.Name === name);
  if (!target) {
    content.innerHTML = `<div class="empty-state"><h3>Target not found</h3><p>"${escHTML(name)}" does not exist.</p>
      <a href="#/" class="btn btn-secondary">Back to Dashboard</a></div>`;
    return;
  }

  try { runs = (await api('/api/targets/' + name + '/runs')) || []; } catch(e) { /* ok */ }

  content.innerHTML = `
    <div class="page-header">
      <div>
        <div style="margin-bottom:8px"><a href="#/" style="color:var(--text-muted);font-size:13px;text-decoration:none">&larr; Dashboard</a></div>
        <h1>${escHTML(name)} <span class="badge badge-${target.DBType}">${target.DBType}</span></h1>
      </div>
      <div style="display:flex;gap:8px">
        <button class="btn btn-secondary btn-sm" onclick="triggerBackup('${escHTML(name)}')">&#9654; Run Now</button>
        <button class="btn btn-secondary btn-sm" onclick="openSourceForm('${escHTML(target.SecretName)}')">Edit</button>
        <button class="btn btn-danger btn-sm" onclick="deleteSource('${escHTML(target.SecretName)}','${escHTML(name)}')">Delete</button>
      </div>
    </div>
    <div class="detail-grid">
      <div class="detail-card">
        <h3>Configuration</h3>
        <div class="detail-row"><span class="key">Schedule</span><code class="val">${escHTML(target.Schedule)}</code></div>
        <div class="detail-row"><span class="key">Destinations</span><span class="val">${(target.Destinations||[]).join(', ') || 'all'}</span></div>
        <div class="detail-row"><span class="key">Status</span>
          ${target.Latest ? (target.Latest.status === 'failed'
            ? failedBadge(target.Latest)
            : '<span class="badge badge-ok">OK</span>')
            : '<span class="badge badge-pending">No runs</span>'}</div>
      </div>
      <div class="detail-card">
        <h3>Latest Run</h3>
        ${target.Latest ? (target.Latest.status === 'failed' ? `
        <div class="detail-row"><span class="key">Time</span><span class="val">${timeAgo(target.Latest.timestamp)}</span></div>
        <div class="detail-row"><span class="key">Phase</span><span class="val">${escHTML(target.Latest.phase || '—')}</span></div>
        <div class="detail-row" style="align-items:flex-start"><span class="key">Error</span><pre class="val" style="color:var(--danger);font-size:12px;white-space:pre-wrap;word-break:break-word;margin:0;background:var(--bg-input);padding:8px;border-radius:4px;max-height:160px;overflow:auto">${escHTML(target.Latest.error || '(no message)')}</pre></div>
        ` : `
        <div class="detail-row"><span class="key">Time</span><span class="val">${timeAgo(target.Latest.timestamp)}</span></div>
        <div class="detail-row"><span class="key">Size</span><span class="val">${humanBytes(target.Latest.encryptedSizeBytes)}</span></div>
        <div class="detail-row"><span class="key">SHA256</span><code class="val" style="font-size:11px">${escHTML((target.Latest.sha256 || '—').substring(0, 16))}${target.Latest.sha256 ? '...' : ''}</code></div>
        <div class="detail-row"><span class="key">Verification</span><span class="val">${renderVerificationBadge(target.Latest.verification)}</span></div>
        `) : '<div style="color:var(--text-muted);padding:12px 0">No runs recorded</div>'}
      </div>
    </div>
    ${renderVerificationDetail(target.Latest)}
    <div class="table-card">
      <div class="table-card-header"><h2>Run History</h2></div>
      ${runs.length === 0 ? '<div class="empty-state"><p>No runs recorded for this target.</p></div>' : `
      <table>
        <thead><tr>
          <th class="sortable" onclick="toggleSort('runs','timestamp')">Timestamp${sortIndicator('runs','timestamp')}</th>
          <th class="sortable" onclick="toggleSort('runs','status')">Status${sortIndicator('runs','status')}</th>
          <th class="num sortable" onclick="toggleSort('runs','size')">Size${sortIndicator('runs','size')}</th>
          <th>Destinations</th>
          <th class="sortable" onclick="toggleSort('runs','verification')">Verification${sortIndicator('runs','verification')}</th>
          <th class="sortable" onclick="toggleSort('runs','schema')">Schema${sortIndicator('runs','schema')}</th>
          <th class="num sortable" onclick="toggleSort('runs','tables')">Tables${sortIndicator('runs','tables')}</th>
          <th class="sortable" onclick="toggleSort('runs','anomalies')">Anomalies / Error${sortIndicator('runs','anomalies')}</th>
          <th>Download</th>
        </tr></thead>
        <tbody>${sortRuns(runs).map(r => `<tr>
          <td style="font-size:12px">${r.timestamp ? new Date(r.timestamp.replace(/(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z/,'$1-$2-$3T$4:$5:$6Z')).toLocaleString() : '—'}</td>
          <td>${r.status === 'failed' ? failedBadge(r) : '<span class="badge badge-ok">OK</span>'}</td>
          <td class="num" style="font-size:12px">${r.status !== 'failed' ? humanBytes(r.encryptedSizeBytes) : '—'}</td>
          <td style="font-size:11px">${r.destinations && r.destinations.length > 0
            ? r.destinations.map(d => {
                const cls = d.status === 'success' ? 'badge-ok' : 'badge-failed';
                const tip = d.error ? ' title="' + escHTML(d.error) + '"' : '';
                return '<span class="badge ' + cls + '" style="margin:1px;font-size:10px"' + tip + '>' + escHTML(d.name) + '</span>';
              }).join('')
            : '<span style="color:var(--text-muted)">—</span>'}</td>
          <td>${renderVerificationBadge(r.verification)}</td>
          <td>${r.report ? (r.report.schemaChanged ? '<span class="badge badge-failed">Changed</span>' : '<span class="badge badge-ok">Stable</span>') : '—'}</td>
          <td class="num" style="font-size:12px">${r.stats && r.stats.tables ? r.stats.tables.length : '—'}</td>
          <td>${r.status === 'failed'
            ? `<span style="color:var(--danger);font-size:12px;word-break:break-word" title="${escHTML(r.error || '')}">${escHTML(truncate(r.error, 120) || '(no message)')}</span>`
            : (r.report && r.report.anomalies ? `<span class="num" style="color:var(--danger)">${r.report.anomalies.length}</span>` : '<span class="num">0</span>')}</td>
          <td>${r.status !== 'failed' ? renderDownloadLinks(name, r, target.Destinations) : '—'}</td>
        </tr>`).join('')}</tbody>
      </table>`}
    </div>`;
}

function renderDownloadLinks(targetName, run, destNames) {
  const ts = escHTML(run.timestamp);
  const successDests = run.destinations ? run.destinations.filter(d => d.status === 'success') : [];
  if (successDests.length <= 1) {
    const destParam = successDests.length === 1 ? '?destination=' + encodeURIComponent(successDests[0].name) : '';
    return `<a href="/download/${escHTML(targetName)}/${ts}/meta${destParam}" class="btn btn-ghost btn-sm" style="font-size:11px">.json</a>
      <a href="/download/${escHTML(targetName)}/${ts}/dump${destParam}" class="btn btn-ghost btn-sm" style="font-size:11px">.age</a>`;
  }
  return `<div class="dropdown" style="display:inline-block">
    <button class="btn btn-ghost btn-sm" style="font-size:11px" onclick="this.nextElementSibling.classList.toggle('open')">Download &#9662;</button>
    <div class="dropdown-menu">${successDests.map(d =>
      `<a href="/download/${escHTML(targetName)}/${ts}/dump?destination=${encodeURIComponent(d.name)}" class="dropdown-item" style="font-size:12px">
        ${escHTML(d.name)} <span style="opacity:0.6;font-size:10px">(${d.storageType})</span>
      </a>`
    ).join('')}
    <hr style="margin:4px 0;border:none;border-top:1px solid var(--border)">
    ${successDests.map(d =>
      `<a href="/download/${escHTML(targetName)}/${ts}/meta?destination=${encodeURIComponent(d.name)}" class="dropdown-item" style="font-size:11px;opacity:0.7">
        meta: ${escHTML(d.name)}
      </a>`
    ).join('')}
    </div>
  </div>`;
}

// --- Verification ---
function renderVerificationBadge(v) {
  if (!v) return '<span style="color:var(--text-muted)">—</span>';
  const verdictMap = {
    'match': { cls: 'badge-ok', label: 'Verified' },
    'mismatch': { cls: 'badge-failed', label: 'Mismatch' },
    'partial': { cls: 'badge-warn', label: 'Partial' },
    'skipped': { cls: 'badge-pending', label: 'Skipped' }
  };
  const info = verdictMap[v.verdict] || { cls: 'badge-pending', label: v.verdict };
  const tip = v.summary ? ' title="' + escHTML(v.summary) + '"' : '';
  return `<span class="badge ${info.cls}"${tip}>${info.label}</span>`;
}

function renderVerificationDetail(run) {
  if (!run || !run.verification || run.status === 'failed') return '';
  const v = run.verification;
  if (!v.tables || v.tables.length === 0) return '';

  const verdictIcon = { 'match': '&#10003;', 'mismatch': '&#10007;', 'partial': '&#9888;', 'skipped': '—' };
  const verdictCls = { 'match': 'badge-ok', 'mismatch': 'badge-failed', 'partial': 'badge-warn', 'skipped': 'badge-pending' };

  return `
    <div class="table-card verification-card">
      <div class="table-card-header">
        <h2>Dump Integrity Verification</h2>
        ${renderVerificationBadge(v)}
      </div>
      <div class="verification-summary">${escHTML(v.summary || '')}</div>
      <table>
        <thead><tr>
          <th>Table</th>
          <th class="num">Pre-Dump Rows</th>
          <th class="num">Post-Dump Rows</th>
          <th class="num">Dump Rows</th>
          <th>Verdict</th>
          <th>Detail</th>
        </tr></thead>
        <tbody>${v.tables.map(t => `<tr>
          <td style="font-size:12px;font-family:var(--font-mono,monospace)">${escHTML(t.name)}</td>
          <td class="num" style="font-size:12px">${t.preDumpRows != null ? t.preDumpRows.toLocaleString() : '—'}</td>
          <td class="num" style="font-size:12px">${t.postDumpRows != null ? t.postDumpRows.toLocaleString() : '—'}</td>
          <td class="num" style="font-size:12px">${t.dumpRows != null ? t.dumpRows.toLocaleString() : '—'}</td>
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
    toast('Backup triggered for ' + targetName, 'success');
  } catch(e) { toast('Trigger failed: ' + e.message, 'error'); }
};

// --- Settings Wizard ---
let settingsStep = 0;
const settingsSteps = [
  { id: 'schedule', title: 'Schedule & Timeout', icon: '&#128339;' },
  { id: 'retention', title: 'Retention Policy', icon: '&#128451;' },
  { id: 'resources', title: 'Worker Resources', icon: '&#9881;' },
  { id: 'review', title: 'Review & Apply', icon: '&#10003;' }
];

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
        <div><h1>Settings</h1><div class="subtitle">Operator configuration</div></div>
      </div>
      <div class="empty-state">
        <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09"/></svg>
        <h3>Settings not available</h3>
        <p>Could not load operator settings.${errorMsg ? ' <strong>Error:</strong> ' + escHTML(errorMsg) : ''}</p>
        <p style="margin-top:8px;font-size:0.9em;opacity:0.7">Ensure the operator is deployed with <code>ui.enabled=true</code> and the Docker image is rebuilt after code changes.</p>
        <button class="btn btn-primary" onclick="renderSettings(true)" style="margin-top:12px">Retry</button>
      </div>`;
    return;
  }

  window._currentSettings = settings;
  settingsStep = 0;
  renderSettingsPage(settings);
}

function renderSettingsPage(settings) {
  content.innerHTML = `
    <div class="page-header">
      <div><h1>Settings</h1><div class="subtitle">Operator configuration wizard</div></div>
      <div style="display:flex;gap:8px">
        <button class="btn btn-secondary" onclick="exportSettings()">&#8681; Export values.yaml</button>
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
            ${settingsStep > 0 ? '<button type="button" class="btn btn-secondary" onclick="goToStep(' + (settingsStep - 1) + ')">&#8592; Back</button>' : ''}
          </div>
          <div style="display:flex;gap:8px">
            ${settingsStep < settingsSteps.length - 1
              ? '<button type="button" class="btn btn-primary" onclick="goToStep(' + (settingsStep + 1) + ')">Next &#8594;</button>'
              : '<button type="submit" class="btn btn-primary">Save Settings</button>'}
          </div>
        </div>
      </form>
    </div>`;
}

function renderSettingsStepContent(step, s) {
  switch(step) {
    case 0: return `
      <h3>Schedule & Timeout</h3>
      <p class="wizard-desc">Configure the default backup schedule and execution timeout.</p>
      <div class="form-group">
        <label for="defaultSchedule">Default Cron Schedule</label>
        <input type="text" id="defaultSchedule" name="defaultSchedule" value="${escHTML(s.defaultSchedule)}" placeholder="0 2 * * *">
        <div class="hint">Cron expression for new sources without a custom schedule. Example: "0 2 * * *" = daily at 2 AM</div>
      </div>
      <div class="form-group">
        <label for="runTimeoutSeconds">Run Timeout (seconds)</label>
        <input type="number" id="runTimeoutSeconds" name="runTimeoutSeconds" value="${escHTML(s.runTimeoutSeconds)}" placeholder="3600" min="0">
        <div class="hint">Maximum duration for a single backup run before it's killed. 3600 = 1 hour.</div>
      </div>`;

    case 1: return `
      <h3>Retention Policy</h3>
      <p class="wizard-desc">Control how long backups are kept and the minimum safety floor.</p>
      <div class="form-row">
        <div class="form-group">
          <label for="defaultRetentionDays">Retention Days</label>
          <input type="number" id="defaultRetentionDays" name="defaultRetentionDays" value="${escHTML(s.defaultRetentionDays)}" placeholder="30" min="0">
          <div class="hint">Backups older than this are pruned. 0 = keep forever.</div>
        </div>
        <div class="form-group">
          <label for="defaultMinKeep">Minimum Keep</label>
          <input type="number" id="defaultMinKeep" name="defaultMinKeep" value="${escHTML(s.defaultMinKeep)}" placeholder="3" min="0">
          <div class="hint">Always keep at least this many backups, regardless of retention age.</div>
        </div>
      </div>
      <div class="form-row">
        <div class="form-group">
          <label for="tempDir">Temp Directory</label>
          <input type="text" id="tempDir" name="tempDir" value="${escHTML(s.tempDir)}" placeholder="/tmp/backup-operator">
          <div class="hint">Scratch space for encrypted dumps before upload.</div>
        </div>
        <div class="form-group">
          <label for="tempDirSize">Temp Dir Size</label>
          <input type="text" id="tempDirSize" name="tempDirSize" value="${escHTML(s.tempDirSize)}" placeholder="10Gi">
          <div class="hint">emptyDir size limit. Increase for large dumps.</div>
        </div>
      </div>`;

    case 2: return `
      <h3>Worker Resources</h3>
      <p class="wizard-desc">CPU and memory limits for backup worker pods spawned by CronJobs.</p>
      <div class="form-section">
        <h4>Limits</h4>
        <div class="form-row">
          <div class="form-group">
            <label for="workerCpuLimit">CPU Limit</label>
            <input type="text" id="workerCpuLimit" name="workerCpuLimit" value="${escHTML(s.workerCpuLimit)}" placeholder="2000m">
            <div class="hint">e.g. 2000m = 2 cores</div>
          </div>
          <div class="form-group">
            <label for="workerMemoryLimit">Memory Limit</label>
            <input type="text" id="workerMemoryLimit" name="workerMemoryLimit" value="${escHTML(s.workerMemoryLimit)}" placeholder="2Gi">
            <div class="hint">e.g. 2Gi, 512Mi</div>
          </div>
        </div>
      </div>
      <div class="form-section">
        <h4>Requests</h4>
        <div class="form-row">
          <div class="form-group">
            <label for="workerCpuRequest">CPU Request</label>
            <input type="text" id="workerCpuRequest" name="workerCpuRequest" value="${escHTML(s.workerCpuRequest)}" placeholder="250m">
            <div class="hint">Minimum guaranteed CPU</div>
          </div>
          <div class="form-group">
            <label for="workerMemoryRequest">Memory Request</label>
            <input type="text" id="workerMemoryRequest" name="workerMemoryRequest" value="${escHTML(s.workerMemoryRequest)}" placeholder="256Mi">
            <div class="hint">Minimum guaranteed memory</div>
          </div>
        </div>
      </div>`;

    case 3:
      return `
      <h3>Review & Apply</h3>
      <p class="wizard-desc">Review your settings before saving. Changes take effect immediately for new backup runs.</p>
      <div class="review-grid">
        <div class="detail-card">
          <h3>Schedule & Timeout</h3>
          <div class="detail-row"><span class="key">Schedule</span><code class="val">${escHTML(s.defaultSchedule)}</code></div>
          <div class="detail-row"><span class="key">Timeout</span><span class="val">${escHTML(s.runTimeoutSeconds)}s</span></div>
        </div>
        <div class="detail-card">
          <h3>Retention</h3>
          <div class="detail-row"><span class="key">Retention Days</span><span class="val">${escHTML(s.defaultRetentionDays)}</span></div>
          <div class="detail-row"><span class="key">Min Keep</span><span class="val">${escHTML(s.defaultMinKeep)}</span></div>
          <div class="detail-row"><span class="key">Temp Dir</span><code class="val">${escHTML(s.tempDir)}</code></div>
          <div class="detail-row"><span class="key">Temp Dir Size</span><span class="val">${escHTML(s.tempDirSize)}</span></div>
        </div>
        <div class="detail-card">
          <h3>Worker Resources</h3>
          <div class="detail-row"><span class="key">CPU Limit</span><span class="val">${escHTML(s.workerCpuLimit) || '—'}</span></div>
          <div class="detail-row"><span class="key">Memory Limit</span><span class="val">${escHTML(s.workerMemoryLimit) || '—'}</span></div>
          <div class="detail-row"><span class="key">CPU Request</span><span class="val">${escHTML(s.workerCpuRequest) || '—'}</span></div>
          <div class="detail-row"><span class="key">Memory Request</span><span class="val">${escHTML(s.workerMemoryRequest) || '—'}</span></div>
        </div>
      </div>
      <div class="wizard-note">
        <strong>Note:</strong> Click "Export values.yaml" to download a Helm-compatible values file for GitOps workflows.
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
  settingsStep = Math.max(0, Math.min(n, settingsSteps.length - 1));
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
    toast('Settings saved successfully', 'success');
  } catch(e) {
    console.error('[Settings] Save failed:', e.message);
    toast('Failed to save: ' + e.message, 'error');
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
    toast('values.yaml exported', 'success');
  } catch(e) { toast('Export failed: ' + e.message, 'error'); }
};

// --- Close dropdowns on outside click ---
document.addEventListener('click', function(e) {
  if (!e.target.closest('.dropdown')) {
    $$('.dropdown-menu.open').forEach(m => m.classList.remove('open'));
  }
});

// --- Init ---
connectSSE();
renderPage(currentPage());

})();
