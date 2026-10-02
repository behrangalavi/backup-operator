// pages/sources.js — Sources list, source form, trigger / suspend actions.
// Shared helpers ($, tr, api, escHTML, …) come from the classic core.js /
// charts.js scripts via the global scope; cross-page state lives in state.js.
import { state, isStaleRender } from './state.js';
import { currentPage, registerPage, renderPage } from './router.js';
import { fmtNextRun } from './jobs.js';

// --- Sources ---
export let _sourcesLastGood = null;
export async function renderSources(loading = true) {
  const _gen = state.renderGen;
  if (loading) showLoading();
  let targets = [];
  try {
    targets = (await api('/api/targets')) || [];
    _sourcesLastGood = targets;
  } catch(e) {
    if (_sourcesLastGood) { targets = _sourcesLastGood; }
    else { toast(e.message, 'error'); }
  }
  if (isStaleRender(_gen)) return;

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
        <button class="btn btn-primary" data-action="open-source-form" title="${escAttr(trText('tooltip.sources.add'))}">+ ${tr('buttons.addSource')}</button>
      </div>
    </div>
    ${targets.length === 0 ? `
    <div class="empty-state">
      <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M21 12c0 1.66-4 3-9 3s-9-1.34-9-3"/><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"/></svg>
      <h3>${tr('page.sources.empty')}</h3>
      <p>${tr('page.sources.emptyHint')}</p>
      <button class="btn btn-primary" data-action="open-source-form" title="${escAttr(trText('tooltip.sources.addFirst'))}">+ ${tr('buttons.addSource')}</button>
    </div>` : `
    <div style="display:grid;grid-template-columns:repeat(auto-fill,minmax(340px,1fr));gap:16px">
      ${sortedTargets.map(t => `
      <div class="detail-card" style="cursor:pointer" ${actionAttrs('goto-target', t.Name)} role="button" tabindex="0" aria-label="${escAttr(t.Name)}">
        <div style="display:flex;justify-content:space-between;align-items:start;margin-bottom:12px">
          <div>
            <div style="font-weight:600;font-size:15px;color:var(--text-heading)">${escHTML(t.Name)}</div>
            <span class="badge badge-${t.DBType}" style="margin-top:6px">${t.DBType}</span>
            ${t.Suspended ? `<span class="badge badge-warn" style="margin-top:6px;margin-left:4px" title="${escAttr(trText('tooltip.sources.paused'))}">${tr('badge.paused')}</span>` : ''}
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
          <button class="btn btn-ghost btn-sm" ${actionAttrs('trigger-backup', t.Name)} title="${escAttr(trText('tooltip.sources.run'))}">&#9654; ${tr('buttons.run')}</button>
          ${t.Suspended
            ? `<button class="btn btn-ghost btn-sm" style="color:var(--success)" ${actionAttrs('toggle-suspend', t.ObjectName, t.Name, 'false')} title="${escAttr(trText('tooltip.sources.resume'))}">${tr('buttons.resume')}</button>`
            : `<button class="btn btn-ghost btn-sm" style="color:var(--warning)" ${actionAttrs('toggle-suspend', t.ObjectName, t.Name, 'true')} title="${escAttr(trText('tooltip.sources.pause'))}">${tr('buttons.pause')}</button>`}
          <button class="btn btn-ghost btn-sm" ${actionAttrs('open-source-form', t.ObjectName)} title="${escAttr(trText('tooltip.sources.edit'))}">${tr('common.edit')}</button>
          <button class="btn btn-ghost btn-sm" style="color:var(--danger)" ${actionAttrs('delete-source', t.ObjectName, t.Name)} title="${escAttr(trText('tooltip.sources.delete'))}">${tr('common.delete')}</button>
        </div>
      </div>`).join('')}
    </div>`}`;
}

// --- Source Form ---

// Lazy-loaded cluster capabilities. Cached for the page session so
// every source-form open does not pay a SubjectAccessReview round-trip.
// Cleared by the SSE 'refresh' tick path is unnecessary — RBAC changes
// require a Helm upgrade and a page reload anyway.
export let _clusterCapabilitiesCache = null;
export async function getClusterCapabilities() {
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
export function isPhase2VerificationMode(mode) {
  return mode === 'schema-only' || mode === 'sample' || mode === 'full';
}

// Render the warning banner inside the open source form when the user
// picks Phase-2 but the cluster currently won't allow it. We do not
// block save — the source secret is still useful (RBAC may flip on
// later, or the user may know the schedule won't run until a Helm
// upgrade lands). The banner just stops the silent "pods is forbidden"
// dead end.
export function refreshPhase2RBACWarning(formEl, caps) {
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

export function openSourceForm(objectName) {
  const isEdit = !!objectName;
  const title = isEdit ? tr('form.source.editTitle') : tr('form.source.createTitle');

  let formHTML = `<form id="sourceForm" data-submit="submit-source" data-arg="${escAttr(objectName || '')}">
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
      <div class="form-group"><label>${tr('form.source.label.database')}</label><input name="database" placeholder="${tr('form.source.placeholder.database')}">
        <div class="hint" id="databaseHint" style="display:none"></div></div>
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
      <div class="form-row">
        <div class="form-group"><label>${tr('form.source.label.compression')}</label>
          <select name="compression">
            <option value="">gzip (${tr('form.source.select.default')})</option>
            <option value="gzip">gzip</option>
            <option value="zstd">zstd</option>
          </select>
          <div class="hint">${tr('form.source.hint.compression')}</div></div>
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
      <button type="button" class="btn btn-secondary" data-action="close-modal" title="${escAttr(trText('tooltip.form.discard'))}">${tr('common.cancel')}</button>
      <button type="submit" class="btn btn-primary" title="${escAttr(trText(isEdit ? 'tooltip.form.saveSource' : 'tooltip.form.createSource'))}">${isEdit ? tr('common.update') : tr('common.create')}</button>
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

  // Show Redis-specific database hint when db-type is redis.
  const dbTypeSelect = formEl && formEl.elements['dbType'];
  const dbHint = document.getElementById('databaseHint');
  function syncDbHint() {
    if (!dbHint) return;
    if (dbTypeSelect && dbTypeSelect.value === 'redis') {
      dbHint.textContent = tr('form.source.hint.databaseRedis');
      dbHint.style.display = '';
    } else {
      dbHint.style.display = 'none';
    }
  }
  if (dbTypeSelect) dbTypeSelect.addEventListener('change', syncDbHint);
  syncDbHint();

  // In create mode we have no preselection to wait for — populate immediately.
  // In edit mode the source fetch below kicks off the picker so we don't fire
  // two fetches and risk the empty-preselection response overwriting the real
  // one if it arrives second.
  if (!isEdit) loadDestinationsPicker('');

  if (isEdit) {
    api('/api/sources/' + encodeURIComponent(objectName)).then(src => {
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
      f.compression.value = src.compression || '';
      // Programmatic value-set does not fire a change event, so the
      // Phase-2 banner and db-type hint need an explicit nudge.
      getClusterCapabilities().then(caps => refreshPhase2RBACWarning(f, caps));
      syncDbHint();
    }).catch(e => toast(trText('toast.loadFailed', {error: e.message}), 'error'));
  }
}

// Renders a checkbox list of all destinations into #destinationsPicker and
// keeps the hidden `destinations` input in sync as a comma-separated string.
// `currentValue` is the saved annotation string ("name1,name2"). Names that
// are present in the saved value but no longer match any known destination
// are kept as disabled "(missing)" entries so a simple Update doesn't silently
// drop the allow-list.
export function loadDestinationsPicker(currentValue) {
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
      rows.push(`<label class="ms-missing" title="${escAttr(trText('tooltip.form.missingDestination'))}">
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
    picker.innerHTML = `<div class="multi-select-empty">${tr('toast.destinationsLoadFailed', {error: e.message})}</div>`;
  });
}

export async function submitSourceForm(e, objectName) {
  e.preventDefault();
  const f = e.target;
  const submitBtn = f.querySelector('button[type="submit"]');
  if (submitBtn && submitBtn.disabled) return; // double-submit guard
  if (submitBtn) submitBtn.disabled = true;
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
    compression: f.compression.value,
  };
  try {
    if (objectName) {
      await api('/api/sources/' + encodeURIComponent(objectName), { method: 'PUT', body: JSON.stringify(body) });
      toast(trText('toast.sourceUpdated'), 'success');
    } else {
      await api('/api/sources', { method: 'POST', body: JSON.stringify(body) });
      toast(trText('toast.sourceCreated'), 'success');
    }
    closeModal();
    renderPage(currentPage());
  } catch(e) {
    toast(e.message, 'error');
  } finally {
    if (submitBtn) submitBtn.disabled = false;
  }
}

export function deleteSource(objectName, displayName) {
  openModal(trText('modal.delete.title', {name: displayName}), `
    <div class="confirm-text">${tr('modal.delete.warning')}</div>
    <div class="form-actions">
      <button class="btn btn-secondary" data-action="close-modal" title="${escAttr(trText('tooltip.sources.keep'))}">${tr('common.cancel')}</button>
      <button class="btn btn-danger" ${actionAttrs('confirm-delete-source', objectName)} title="${escAttr(trText('tooltip.sources.confirmDelete'))}">${tr('common.delete')}</button>
    </div>`);
}

export async function confirmDeleteSource(objectName) {
  try {
    await api('/api/sources/' + encodeURIComponent(objectName), { method: 'DELETE' });
    toast(trText('toast.sourceDeleted'), 'success');
    closeModal();
    location.hash = '#/sources';
  } catch(e) { toast(e.message, 'error'); }
}

// --- Trigger ---
// _inFlight de-duplicates user actions that must not run twice while the
// first request is still outstanding (a double-click on Run would create
// two Jobs). Keyed by action + object so unrelated targets stay independent.
export const _inFlight = new Set();
export async function triggerBackup(targetName) {
  const key = 'trigger:' + targetName;
  if (_inFlight.has(key)) return;
  _inFlight.add(key);
  try {
    await api('/api/trigger/' + encodeURIComponent(targetName), { method: 'POST' });
    toast(trText('toast.triggered') + ': ' + targetName, 'success');
  } catch(e) { toast(trText('toast.triggerFailed', {error: e.message}), 'error'); }
  finally { _inFlight.delete(key); }
}

// Pause/resume scheduled runs without deleting the source. Hits the
// dedicated suspend endpoint so we don't have to send the full source
// body — that endpoint only patches the suspended annotation.
export async function toggleSourceSuspend(objectName, displayName, suspend) {
  const key = 'suspend:' + objectName;
  if (_inFlight.has(key)) return;
  _inFlight.add(key);
  try {
    await api('/api/sources/' + encodeURIComponent(objectName) + '/suspend', {
      method: 'POST',
      body: JSON.stringify({ suspend }),
    });
    toast((suspend ? trText('toast.paused') : trText('toast.resumed')) + ': ' + displayName, 'success');
    renderPage(currentPage());
  } catch(e) {
    toast((suspend ? trText('buttons.pause') : trText('buttons.resume')) + ': ' + e.message, 'error');
  } finally { _inFlight.delete(key); }
}
registerAction('add-source-from-dashboard', () => { location.hash = '#/sources'; openSourceForm(); });
registerAction('open-source-form', (el, e, name) => openSourceForm(name || undefined));
registerAction('trigger-backup', (el, e, target) => triggerBackup(target));
registerAction('toggle-suspend', (el, e, obj, display, suspend) => toggleSourceSuspend(obj, display, suspend === 'true'));
registerAction('delete-source', (el, e, obj, display) => deleteSource(obj, display));
registerAction('confirm-delete-source', (el, e, obj) => confirmDeleteSource(obj));
registerAction('submit-source', (form, e, obj) => submitSourceForm(e, obj));
registerPage('sources', loading => renderSources(loading));
