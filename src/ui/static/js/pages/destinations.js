// pages/destinations.js — Destinations list, destination form, destination stats cache.
// Shared helpers ($, tr, api, escHTML, …) come from the classic core.js /
// charts.js scripts via the global scope; cross-page state lives in state.js.
import { state, isStaleRender } from './state.js';
import { currentPage, registerPage, renderPage } from './router.js';
import { SLOW_PROBE_TTL_MS } from './dashboard.js';

// Cached stats so SSE refreshes don't refire the per-destination probes.
// Same shape as state.slowProbes on the dashboard. 60 s TTL + in-flight guard
// for the same reason — see the SLOW_PROBE_TTL_MS comment above.
export let _destStatsInFlight = false;
export async function refreshDestStats() {
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
    state.destStatsCache = { stats: s || [], lastFetch: Date.now() };
    success = true;
  } finally {
    _destStatsInFlight = false;
  }
  if (success && (currentPage() === 'destinations' || currentPage() === 'dashboard')) {
    renderPage(currentPage(), false);
  }
}


// --- Destinations ---
export let _destsLastGood = null;
export async function renderDestinations(loading = true) {
  const _gen = state.renderGen;
  if (loading) showLoading();
  let dests = [];
  try {
    dests = (await api('/api/destinations')) || [];
    _destsLastGood = dests;
  } catch(e) {
    if (_destsLastGood) { dests = _destsLastGood; }
    else { toast(e.message, 'error'); }
  }
  if (isStaleRender(_gen)) return;
  const stats = state.destStatsCache.stats;
  if (loading && Date.now() - state.destStatsCache.lastFetch > SLOW_PROBE_TTL_MS) refreshDestStats();
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
        <button class="btn btn-primary" data-action="open-dest-form" title="${escAttr(trText('tooltip.destinations.add'))}">+ ${tr('buttons.addDestination')}</button>
      </div>
    </div>
    ${dests.length === 0 ? `
    <div class="empty-state">
      <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/></svg>
      <h3>${tr('page.destinations.empty')}</h3>
      <p>${tr('page.destinations.emptyHint')}</p>
      <button class="btn btn-primary" data-action="open-dest-form" title="${escAttr(trText('tooltip.destinations.addFirst'))}">+ ${tr('buttons.addDestination')}</button>
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
          <span class="dest-status" id="dest-status-${escAttr(d.objectName)}"></span>
        </div>
        <div class="detail-row"><span class="key">${tr('row.credentials')}</span><code class="val">${escHTML(d.credentialsSecret || '—')}</code></div>
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
          <button class="btn btn-ghost btn-sm" ${actionAttrs('test-dest', d.objectName, d.name)} title="${escAttr(trText('tooltip.destinations.test'))}">&#128268; ${tr('buttons.test')}</button>
          <button class="btn btn-ghost btn-sm" ${actionAttrs('open-dest-form', d.objectName)} title="${escAttr(trText('tooltip.destinations.edit'))}">${tr('common.edit')}</button>
          <button class="btn btn-ghost btn-sm" style="color:var(--danger)" ${actionAttrs('delete-dest', d.objectName, d.name)} title="${escAttr(trText('tooltip.destinations.delete'))}">${tr('common.delete')}</button>
        </div>
      </div>`;
      }).join('')}
    </div>`}`;
}

// --- Destination Form ---
export function openDestForm(objectName) {
  const isEdit = !!objectName;
  const title = isEdit ? tr('form.destination.editTitle') : tr('form.destination.createTitle');

  const sftpFields = `
    <div class="form-row"><div class="form-group"><label>${tr('form.destination.label.host')} *</label><input name="data_host" required></div>
      <div class="form-group"><label>${tr('form.destination.label.port')}</label><input name="data_port" placeholder="22">
        <div class="hint">${tr('form.destination.hint.hetzner')}</div></div></div>
    <div class="form-group"><label>${tr('form.destination.label.username')} *</label><input name="data_username" required></div>
    <div class="form-group"><label>${tr('form.destination.label.authMethod')} *</label>
      <div style="display:flex;gap:16px;margin-top:4px">
        <label style="font-weight:normal;cursor:pointer"><input type="radio" name="sftpAuthMethod" value="key" checked data-change="sftp-auth"> ${tr('form.destination.label.authMethodKey')}</label>
        <label style="font-weight:normal;cursor:pointer"><input type="radio" name="sftpAuthMethod" value="password" data-change="sftp-auth"> ${tr('form.destination.label.authMethodPassword')}</label>
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

  // Data-key names MUST match the storage backend schema (storage/s3/s3.go):
  // access-key-id / secret-access-key, NOT access-key / secret-key. The old
  // names produced a Secret the backend rejected at first use, so every
  // UI-created S3 destination failed. Endpoint is optional — AWS is inferred
  // from the region when it's blank; only non-AWS providers (MinIO, R2, B2,
  // Hetzner) need it. Sensitive fields carry a "*" label but no `required`
  // attribute so edit-mode (where they come back masked and blank) can save
  // and preserve the stored value — same convention as the SFTP creds above.
  const s3Fields = `
    <div class="form-group"><label>${tr('form.destination.label.endpoint')}</label>
      <input name="data_endpoint" placeholder="${tr('form.destination.placeholder.endpoint')}">
      <div class="hint">${tr('form.destination.hint.endpoint')}</div>
    </div>
    <div class="form-row"><div class="form-group"><label>${tr('form.destination.label.bucket')} *</label><input name="data_bucket" required></div>
      <div class="form-group"><label>${tr('form.destination.label.region')}</label><input name="data_region" placeholder="${tr('form.destination.placeholder.region')}"></div></div>
    <div class="form-row"><div class="form-group"><label>${tr('form.destination.label.accessKey')} *</label><input name="data_access-key-id"></div>
      <div class="form-group"><label>${tr('form.destination.label.secretKey')} *</label><input name="data_secret-access-key" type="password"></div></div>
    <div class="form-group">
      <label style="font-weight:normal;cursor:pointer">
        <input type="checkbox" name="data_path-style" value="true">
        ${tr('form.destination.label.pathStyle')}
      </label>
      <div class="hint">${tr('form.destination.hint.pathStyle')}</div>
    </div>`;

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

  const azureFields = `
    <div class="form-group"><label>${tr('form.destination.label.accountName')} *</label><input name="data_account-name" required></div>
    <div class="form-group"><label>${tr('form.destination.label.accountKey')} *</label><input name="data_account-key" type="password"></div>
    <div class="form-group"><label>${tr('form.destination.label.container')} *</label><input name="data_container" required></div>`;

  const gcsFields = `
    <div class="form-group"><label>${tr('form.destination.label.bucket')} *</label><input name="data_bucket" required></div>
    <div class="form-group"><label>${tr('form.destination.label.serviceAccountJson')} *</label>
      <textarea name="data_service-account-json" rows="4" placeholder='{"type":"service_account",...}'></textarea>
      <div class="hint">${tr('form.destination.hint.serviceAccountJson')}</div>
    </div>`;

  openModal(title, `<form id="destForm" data-submit="submit-dest" data-arg="${escAttr(objectName || '')}">
    <div class="form-row">
      <div class="form-group"><label>${tr('common.name')} *</label><input name="name" required placeholder="${tr('form.destination.placeholder.name')}" ${isEdit ? 'disabled' : ''}></div>
      <div class="form-group"><label>${tr('form.destination.label.type')} *</label>
        <select name="storageType" required data-change="dest-type">
          <option value="">${tr('form.source.placeholder.selectType')}</option>
          <option value="sftp">${tr('form.destination.type.sftp')}</option>
          <option value="ftps">${tr('form.destination.type.ftps')}</option>
          <option value="s3">${tr('form.destination.type.s3')}</option>
          <option value="azure">${tr('form.destination.type.azure')}</option>
          <option value="gcs">${tr('form.destination.type.gcs')}</option>
        </select></div>
    </div>
    <div class="form-group"><label>${tr('form.destination.label.pathPrefix')}</label><input name="pathPrefix" placeholder="${tr('form.destination.placeholder.pathPrefix')}"></div>
    <div id="destTypeFields"></div>
    <div class="form-actions">
      <button type="button" class="btn btn-secondary" data-action="close-modal" title="${escAttr(trText('tooltip.form.discard'))}">${tr('common.cancel')}</button>
      <button type="submit" class="btn btn-primary" title="${escAttr(trText(isEdit ? 'tooltip.form.saveDestination' : 'tooltip.form.createDestination'))}">${isEdit ? tr('common.update') : tr('common.create')}</button>
    </div>
  </form>
  <div id="destSFTPTemplate" style="display:none">${sftpFields}</div>
  <div id="destS3Template" style="display:none">${s3Fields}</div>
  <div id="destFTPSTemplate" style="display:none">${ftpsFields}</div>
  <div id="destAzureTemplate" style="display:none">${azureFields}</div>
  <div id="destGCSTemplate" style="display:none">${gcsFields}</div>`);

  if (isEdit) {
    api('/api/destinations/' + encodeURIComponent(objectName)).then(d => {
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
        if (d.storageType === 'sftp') {
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
    }).catch(e => toast(trText('toast.loadFailed', {error: e.message}), 'error'));
  }
}

export function toggleDestFields(type) {
  const container = $('#destTypeFields');
  const templates = {
    'sftp': '#destSFTPTemplate',
    's3': '#destS3Template', 'ftps': '#destFTPSTemplate',
    'azure': '#destAzureTemplate', 'gcs': '#destGCSTemplate',
  };
  const tpl = templates[type];
  container.innerHTML = tpl ? $(tpl).innerHTML : '';
}

// Show only the input matching the selected SFTP auth method. The hidden
// input is left in the DOM so the user can flip back without re-typing
// during a single form session; submitDestForm drops the unselected
// method's value before sending so we don't ship both up the wire.
export function toggleSFTPAuth(method) {
  $$('[data-sftp-auth]').forEach(el => {
    el.style.display = el.getAttribute('data-sftp-auth') === method ? '' : 'none';
  });
}

// Sensitive fields are masked as *** on edit, so an empty form value
// means "user did not retype it, keep the stored secret" rather than
// "user cleared it". Non-sensitive fields (host, port, known-hosts, …)
// follow the opposite rule: empty after edit means "user actually
// emptied it, remove the field from the Secret" — otherwise the form
// silently ignores clears and the user wonders why the old value is
// stuck. Mirror of the backend's sensitiveKeys set in handlers_api.go.
// Mirror of the backend sensitiveKeys mask (ui/handlers_api.go): every key the
// API returns as "***" must be here, so that on edit an intentionally-blank
// (masked) field is PRESERVED via PUT-merge rather than pushed to removeKeys
// and deleted. 'access-key' / 'secret-key' are legacy aliases kept for old
// Secrets; new S3 destinations use access-key-id / secret-access-key.
export const DEST_SENSITIVE_KEYS = new Set([
  'password', 'ssh-private-key', 'secret-key', 'access-key', 'secret-access-key',
  'access-key-id', 'account-key', 'service-account-json',
]);

export async function submitDestForm(e, objectName) {
  e.preventDefault();
  const f = e.target;
  const submitBtn = f.querySelector('button[type="submit"]');
  if (submitBtn && submitBtn.disabled) return; // double-submit guard
  if (submitBtn) submitBtn.disabled = true;
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
    } else if (objectName && !DEST_SENSITIVE_KEYS.has(key)) {
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
  if (sType === 'sftp') {
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
    if (objectName) {
      await api('/api/destinations/' + encodeURIComponent(objectName), { method: 'PUT', body: JSON.stringify(body) });
      toast(trText('toast.destinationUpdated'), 'success');
    } else {
      await api('/api/destinations', { method: 'POST', body: JSON.stringify(body) });
      toast(trText('toast.destinationCreated'), 'success');
    }
    closeModal();
    renderPage(currentPage());
  } catch(e) { toast(e.message, 'error'); }
  finally { if (submitBtn) submitBtn.disabled = false; }
}

export async function testDestConnection(objectName, displayName) {
  const el = document.getElementById('dest-status-' + objectName);
  if (el) { el.innerHTML = `<span class="badge badge-pending">${tr('badge.testing')}</span>`; }
  try {
    // User-initiated probe: the server bounds it at 15 s (upload+readback),
    // so allow more than the default client timeout before giving up.
    const result = await api('/api/destinations/' + encodeURIComponent(objectName) + '/test', { method: 'POST', timeoutMs: 30000 });
    if (result.ok) {
      if (el) el.innerHTML = `<span class="badge badge-ok">${tr('badge.connected')}</span>`;
      toast(displayName + ': ' + tr('badge.ok'), 'success');
    } else {
      if (el) el.innerHTML = `<span class="badge badge-failed" title="${escAttr(result.error || '')}">${tr('badge.failed')}</span>`;
      toast(displayName + ': ' + (result.error || tr('badge.failed')), 'error');
    }
  } catch(e) {
    if (el) el.innerHTML = `<span class="badge badge-failed">${tr('badge.error')}</span>`;
    toast(trText('toast.loadFailed', {error: e.message}), 'error');
  }
}

export function deleteDest(objectName, displayName) {
  openModal(trText('modal.delete.title', {name: displayName}), `
    <div class="confirm-text">${tr('modal.delete.warning')}</div>
    <div class="form-actions">
      <button class="btn btn-secondary" data-action="close-modal" title="${escAttr(trText('tooltip.destinations.keep'))}">${tr('common.cancel')}</button>
      <button class="btn btn-danger" ${actionAttrs('confirm-delete-dest', objectName)} title="${escAttr(trText('tooltip.destinations.confirmDelete'))}">${tr('common.delete')}</button>
    </div>`);
}

export async function confirmDeleteDest(objectName) {
  try {
    await api('/api/destinations/' + encodeURIComponent(objectName), { method: 'DELETE' });
    toast(trText('toast.destinationDeleted'), 'success');
    closeModal();
    renderDestinations();
  } catch(e) { toast(e.message, 'error'); }
}
registerAction('open-dest-form', (el, e, name) => openDestForm(name || undefined));
registerAction('test-dest', (el, e, obj, display) => testDestConnection(obj, display));
registerAction('delete-dest', (el, e, obj, display) => deleteDest(obj, display));
registerAction('confirm-delete-dest', (el, e, obj) => confirmDeleteDest(obj));
registerAction('submit-dest', (form, e, obj) => submitDestForm(e, obj));
registerAction('sftp-auth', el => toggleSFTPAuth(el.value));
registerAction('dest-type', el => toggleDestFields(el.value));
registerPage('destinations', loading => renderDestinations(loading));
