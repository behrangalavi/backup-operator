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
    if (page === 'dashboard') renderDashboard();
    if (page === 'jobs') renderJobs();
  });
  ['source_created','source_updated','source_deleted',
   'destination_created','destination_updated','destination_deleted',
   'backup_triggered','settings_updated'].forEach(ev => {
    eventSource.addEventListener(ev, () => {
      const page = currentPage();
      if (['dashboard','sources'].includes(page)) renderPage(page);
      if (['dashboard','destinations'].includes(page)) renderPage(page);
      if (page === 'jobs') renderJobs();
      if (page === 'settings') renderSettings();
    });
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

function renderPage(page) {
  $$('.nav-link').forEach(a => {
    a.classList.toggle('active', a.dataset.page === page);
  });
  switch(page) {
    case 'dashboard': renderDashboard(); break;
    case 'sources': renderSources(); break;
    case 'destinations': renderDestinations(); break;
    case 'jobs': renderJobs(); break;
    case 'target': renderTargetDetail(currentParam()); break;
    case 'settings': renderSettings(); break;
    default: renderDashboard();
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

// --- Dashboard ---
async function renderDashboard() {
  let targets = [], dests = [], jobs = [];
  try {
    [targets, dests, jobs] = await Promise.all([
      api('/api/targets'), api('/api/destinations'), api('/api/jobs')
    ]);
  } catch(e) { /* partial data is ok */ }

  const ok = targets.filter(t => t.Latest && !t.Latest.status?.includes('fail')).length;
  const failed = targets.filter(t => t.Latest?.status === 'failed').length;
  const running = jobs.filter(j => j.status === 'running').length;

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
          <th>Target</th><th>Type</th><th>Schedule</th><th>Status</th>
          <th>Last Run</th><th class="num">Size</th><th>Destinations</th><th></th>
        </tr></thead>
        <tbody>${targets.map(t => `<tr>
          <td><a href="#/target/${escHTML(t.Name)}" style="color:var(--accent);font-weight:600">${escHTML(t.Name)}</a></td>
          <td><span class="badge badge-${t.DBType}">${t.DBType}</span></td>
          <td><code style="font-size:12px;background:var(--bg-input);padding:2px 6px;border-radius:4px">${escHTML(t.Schedule)}</code></td>
          <td>${t.Latest ? (t.Latest.status === 'failed'
            ? '<span class="badge badge-failed">Failed</span>'
            : '<span class="badge badge-ok">OK</span>')
            : '<span class="badge badge-pending">No runs</span>'}</td>
          <td style="color:var(--text-muted);font-size:12px">${t.Latest ? timeAgo(t.Latest.timestamp) : 'never'}</td>
          <td class="num" style="font-size:12px">${t.Latest && !t.Latest.status?.includes('fail') ? humanBytes(t.Latest.encryptedSizeBytes) : '—'}</td>
          <td>${(t.Destinations || []).map(d => `<span class="badge badge-sftp" style="margin:1px">${escHTML(d)}</span>`).join('')}</td>
          <td><button class="btn btn-ghost btn-sm" onclick="triggerBackup('${escHTML(t.Name)}')" title="Trigger manual backup">&#9654;</button></td>
        </tr>`).join('')}</tbody>
      </table>`}
    </div>`;
}

// --- Sources ---
async function renderSources() {
  let targets = [];
  try { targets = await api('/api/targets'); } catch(e) { toast(e.message, 'error'); }

  content.innerHTML = `
    <div class="page-header">
      <div><h1>Sources</h1><div class="subtitle">Database backup sources</div></div>
      <button class="btn btn-primary" onclick="openSourceForm()">+ Add Source</button>
    </div>
    ${targets.length === 0 ? `
    <div class="empty-state">
      <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M21 12c0 1.66-4 3-9 3s-9-1.34-9-3"/><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"/></svg>
      <h3>No sources configured</h3>
      <p>Add a database source to start backing up. Sources are Kubernetes Secrets with backup labels.</p>
      <button class="btn btn-primary" onclick="openSourceForm()">+ Add Source</button>
    </div>` : `
    <div style="display:grid;grid-template-columns:repeat(auto-fill,minmax(340px,1fr));gap:16px">
      ${targets.map(t => `
      <div class="detail-card" style="cursor:pointer" onclick="location.hash='#/target/${escHTML(t.Name)}'">
        <div style="display:flex;justify-content:space-between;align-items:start;margin-bottom:12px">
          <div>
            <div style="font-weight:600;font-size:15px;color:var(--text-heading)">${escHTML(t.Name)}</div>
            <span class="badge badge-${t.DBType}" style="margin-top:6px">${t.DBType}</span>
          </div>
          ${t.Latest ? (t.Latest.status === 'failed'
            ? '<span class="badge badge-failed">Failed</span>'
            : '<span class="badge badge-ok">OK</span>')
            : '<span class="badge badge-pending">No runs</span>'}
        </div>
        <div class="detail-row"><span class="key">Schedule</span><code class="val">${escHTML(t.Schedule)}</code></div>
        <div class="detail-row"><span class="key">Last run</span><span class="val">${t.Latest ? timeAgo(t.Latest.timestamp) : 'never'}</span></div>
        <div class="detail-row"><span class="key">Destinations</span><span class="val">${(t.Destinations||[]).join(', ') || 'all'}</span></div>
        <div style="display:flex;gap:6px;margin-top:12px;justify-content:flex-end">
          <button class="btn btn-ghost btn-sm" onclick="event.stopPropagation();triggerBackup('${escHTML(t.Name)}')" title="Run now">&#9654; Run</button>
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
async function renderDestinations() {
  let dests = [];
  try { dests = await api('/api/destinations'); } catch(e) { toast(e.message, 'error'); }

  content.innerHTML = `
    <div class="page-header">
      <div><h1>Destinations</h1><div class="subtitle">Storage backends for backup uploads</div></div>
      <button class="btn btn-primary" onclick="openDestForm()">+ Add Destination</button>
    </div>
    ${dests.length === 0 ? `
    <div class="empty-state">
      <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/></svg>
      <h3>No destinations configured</h3>
      <p>Add a destination to define where backups are stored. Destinations are Kubernetes Secrets with storage labels.</p>
      <button class="btn btn-primary" onclick="openDestForm()">+ Add Destination</button>
    </div>` : `
    <div style="display:grid;grid-template-columns:repeat(auto-fill,minmax(320px,1fr));gap:16px">
      ${dests.map(d => `
      <div class="detail-card">
        <div style="display:flex;justify-content:space-between;align-items:start;margin-bottom:12px">
          <div>
            <div style="font-weight:600;font-size:15px;color:var(--text-heading)">${escHTML(d.name)}</div>
            <span class="badge badge-${d.storageType}" style="margin-top:6px">${d.storageType}</span>
          </div>
        </div>
        <div class="detail-row"><span class="key">Secret</span><code class="val">${escHTML(d.secretName)}</code></div>
        <div class="detail-row"><span class="key">Host</span><span class="val">${escHTML(d.host || '—')}</span></div>
        <div class="detail-row"><span class="key">Path Prefix</span><span class="val">${escHTML(d.pathPrefix || '/')}</span></div>
        <div style="display:flex;gap:6px;margin-top:12px;justify-content:flex-end">
          <button class="btn btn-ghost btn-sm" onclick="openDestForm('${escHTML(d.secretName)}')">Edit</button>
          <button class="btn btn-ghost btn-sm" style="color:var(--danger)" onclick="deleteDest('${escHTML(d.secretName)}','${escHTML(d.name)}')">Delete</button>
        </div>
      </div>`).join('')}
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
async function renderJobs() {
  let jobs = [];
  try { jobs = await api('/api/jobs'); } catch(e) { toast(e.message, 'error'); }

  content.innerHTML = `
    <div class="page-header">
      <div><h1>Jobs</h1><div class="subtitle">Backup job execution history</div></div>
    </div>
    <div class="table-card">
      ${jobs.length === 0 ? '<div class="empty-state"><h3>No jobs yet</h3><p>Jobs appear when backups run — either on schedule or triggered manually.</p></div>' : `
      <table>
        <thead><tr><th>Job</th><th>Target</th><th>Status</th><th>Started</th><th>Duration</th></tr></thead>
        <tbody>${jobs.map(j => `<tr>
          <td style="font-family:ui-monospace,monospace;font-size:12px">${escHTML(j.name)}</td>
          <td><strong>${escHTML(j.target || '—')}</strong></td>
          <td><span class="badge badge-${j.status}">${j.status}</span></td>
          <td style="color:var(--text-muted);font-size:12px">${j.startTime ? new Date(j.startTime).toLocaleString() : '—'}</td>
          <td style="font-size:12px">${j.duration || '—'}</td>
        </tr>`).join('')}</tbody>
      </table>`}
    </div>`;
}

// --- Target detail ---
async function renderTargetDetail(name) {
  if (!name) { renderDashboard(); return; }
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

  try { runs = await api('/api/targets/' + name + '/runs'); } catch(e) { /* ok */ }

  // Find secretName from targets - we need to look it up
  let secretName = '';
  try {
    const srcList = await api('/api/targets');
    // We don't have secretName from targets API, get from source secrets
  } catch(e) {}

  content.innerHTML = `
    <div class="page-header">
      <div>
        <div style="margin-bottom:8px"><a href="#/" style="color:var(--text-muted);font-size:13px;text-decoration:none">&larr; Dashboard</a></div>
        <h1>${escHTML(name)} <span class="badge badge-${target.DBType}">${target.DBType}</span></h1>
      </div>
      <div style="display:flex;gap:8px">
        <button class="btn btn-secondary btn-sm" onclick="triggerBackup('${escHTML(name)}')">&#9654; Run Now</button>
      </div>
    </div>
    <div class="detail-grid">
      <div class="detail-card">
        <h3>Configuration</h3>
        <div class="detail-row"><span class="key">Schedule</span><code class="val">${escHTML(target.Schedule)}</code></div>
        <div class="detail-row"><span class="key">Destinations</span><span class="val">${(target.Destinations||[]).join(', ') || 'all'}</span></div>
        <div class="detail-row"><span class="key">Status</span>
          ${target.Latest ? (target.Latest.status === 'failed'
            ? '<span class="badge badge-failed">Failed</span>'
            : '<span class="badge badge-ok">OK</span>')
            : '<span class="badge badge-pending">No runs</span>'}</div>
      </div>
      <div class="detail-card">
        <h3>Latest Run</h3>
        ${target.Latest ? `
        <div class="detail-row"><span class="key">Time</span><span class="val">${timeAgo(target.Latest.timestamp)}</span></div>
        <div class="detail-row"><span class="key">Size</span><span class="val">${humanBytes(target.Latest.encryptedSizeBytes)}</span></div>
        <div class="detail-row"><span class="key">SHA256</span><code class="val" style="font-size:11px">${escHTML((target.Latest.sha256 || '—').substring(0, 16))}${target.Latest.sha256 ? '...' : ''}</code></div>
        ` : '<div style="color:var(--text-muted);padding:12px 0">No runs recorded</div>'}
      </div>
    </div>
    <div class="table-card">
      <div class="table-card-header"><h2>Run History</h2></div>
      ${runs.length === 0 ? '<div class="empty-state"><p>No runs recorded for this target.</p></div>' : `
      <table>
        <thead><tr><th>Timestamp</th><th>Status</th><th class="num">Size</th><th>Schema</th><th class="num">Tables</th><th class="num">Anomalies</th><th>Download</th></tr></thead>
        <tbody>${runs.map(r => `<tr>
          <td style="font-size:12px">${r.timestamp ? new Date(r.timestamp.replace(/(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z/,'$1-$2-$3T$4:$5:$6Z')).toLocaleString() : '—'}</td>
          <td>${r.status === 'failed' ? '<span class="badge badge-failed">Failed</span>' : '<span class="badge badge-ok">OK</span>'}</td>
          <td class="num" style="font-size:12px">${r.status !== 'failed' ? humanBytes(r.encryptedSizeBytes) : '—'}</td>
          <td>${r.report ? (r.report.schemaChanged ? '<span class="badge badge-failed">Changed</span>' : '<span class="badge badge-ok">Stable</span>') : '—'}</td>
          <td class="num" style="font-size:12px">${r.stats ? r.stats.tables.length : '—'}</td>
          <td class="num">${r.report && r.report.anomalies ? '<span style="color:var(--danger)">' + r.report.anomalies.length + '</span>' : '0'}</td>
          <td>${r.status !== 'failed' ? `
            <a href="/download/${escHTML(name)}/${escHTML(r.timestamp)}/meta" class="btn btn-ghost btn-sm" style="font-size:11px">.json</a>
            <a href="/download/${escHTML(name)}/${escHTML(r.timestamp)}/dump" class="btn btn-ghost btn-sm" style="font-size:11px">.age</a>` : '—'}</td>
        </tr>`).join('')}</tbody>
      </table>`}
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

async function renderSettings() {
  let settings = null;
  let unavailable = false;
  try {
    const resp = await api('/api/settings');
    settings = resp.settings;
  } catch(e) {
    unavailable = true;
  }

  if (unavailable || !settings) {
    content.innerHTML = `
      <div class="page-header">
        <div><h1>Settings</h1><div class="subtitle">Operator configuration</div></div>
      </div>
      <div class="empty-state">
        <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09"/></svg>
        <h3>Settings not available</h3>
        <p>The settings wizard requires the operator to be deployed with <code>ui.enabled=true</code> in the Helm chart. The settings ConfigMap is created automatically when enabled.</p>
      </div>`;
    return;
  }

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

  // Store current settings for form navigation
  window._currentSettings = settings;
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
  // Collect form values before navigating
  const form = $('#settingsForm');
  if (form && window._currentSettings) {
    const fd = new FormData(form);
    for (const [k, v] of fd.entries()) {
      window._currentSettings[k] = v;
    }
  }
  settingsStep = Math.max(0, Math.min(n, settingsSteps.length - 1));
  renderSettings();
};

window.submitSettings = async function(e) {
  e.preventDefault();
  const s = window._currentSettings;
  if (!s) { toast('No settings loaded', 'error'); return; }

  try {
    await fetch('/api/settings', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(s)
    }).then(r => r.json()).then(d => {
      if (!d.ok) throw new Error(d.message);
    });
    toast('Settings saved successfully', 'success');
  } catch(e) {
    toast('Failed to save: ' + e.message, 'error');
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

// --- Init ---
connectSSE();
renderPage(currentPage());

})();
