// pages/agekeys.js — Age-recipient (public key) management page.
// Shared helpers ($, tr, api, escHTML, …) come from the classic core.js /
// charts.js scripts via the global scope; cross-page state lives in state.js.
import { state, isStaleRender } from './state.js';
import { registerPage } from './router.js';

// --- Age Keys page ---
export async function renderAgeKeys(loading = true) {
  const _gen = state.renderGen;
  if (loading) showLoading();
  if (isStaleRender(_gen)) return;
  content.innerHTML = `
    <div class="page-header">
      <div>
        <h1>${tr('nav.ageKeys') || 'Age Keys'}</h1>
        <div class="subtitle">${tr('ageKeys.subtitle')}</div>
      </div>
    </div>
    <div id="age-keys-section" class="table-card"></div>
  `;
  loadAgeKeysSection();
}

// --- Age recipient (public key) management ---
export async function loadAgeKeysSection() {
  const host = $('#age-keys-section');
  if (!host) return;
  host.innerHTML = `<div class="table-card-header"><h2>${tr('ageKeys.title')}</h2></div><div class="empty-state"><div class="spinner"></div></div>`;
  let resp;
  try {
    resp = await api('/api/age-keys');
  } catch(e) {
    host.innerHTML = `<div class="table-card-header"><h2>${tr('ageKeys.titleShort')}</h2></div>
      <div class="empty-state"><p style="color:var(--danger)">${tr('toast.loadFailed', {error: e.message})}</p></div>`;
    return;
  }
  const keys = resp.keys || [];
  const canMutate = !!resp.canMutate;
  host.innerHTML = `
    <div class="table-card-header">
      <h2>${tr('ageKeys.title')}</h2>
      <span style="color:var(--text-muted);font-size:12px">${keys.length} · ${tr('ageKeys.merged')}: <code>${escHTML(resp.mergedSecret || '—')}</code></span>
    </div>
    <p style="padding:0 16px;color:var(--text-muted);font-size:13px;margin:0 0 12px">
      ${tr('ageKeys.intro')}
      ${canMutate ? '' : '<br><span style="color:var(--warning)">' + tr('ageKeys.readOnly') + '</span>'}
    </p>
    ${keys.length === 0 ? `<div class="empty-state"><p>${tr('ageKeys.noKeys')} ${tr('ageKeys.noKeysHint')}</p></div>` : `
    <table>
      <thead><tr>
        <th class="num row-num">#</th>
        <th>${tr('row.fingerprint')}</th>
        <th>${tr('common.name')}</th>
        <th>${tr('ageKeys.publicKey') || 'Public Key'}</th>
        ${canMutate ? '<th style="width:1%"></th>' : ''}
      </tr></thead>
      <tbody>${keys.map((k, i) => `<tr>
        <td class="num row-num">${i + 1}</td>
        <td><code style="font-size:12px;background:var(--bg-input);padding:2px 6px;border-radius:4px">${escHTML(k.hash)}</code></td>
        <td><code style="font-size:12px">${escHTML(k.name || '')}</code></td>
        <td><code style="font-size:11px;word-break:break-all">${escHTML(k.recipient)}</code></td>
        ${canMutate ? `<td style="white-space:nowrap">
          <button class="btn btn-ghost btn-sm" style="color:var(--danger)"
            ${actionAttrs('remove-age-key', k.recipient, k.hash)}
            title="${escAttr(trText('tooltip.ageKeys.remove'))}">&#10005; ${tr('common.delete')}</button>
        </td>` : ''}
      </tr>`).join('')}</tbody>
    </table>`}
    ${canMutate ? `
    <div style="padding:16px;border-top:1px solid var(--border)">
      <h3 style="font-size:13px;margin:0 0 8px">${tr('ageKeys.addNew')}</h3>
      <form data-submit="add-age-key">
        <div style="display:flex;gap:8px">
          <input type="text" name="recipient" required pattern="age1[0-9a-z]+"
            placeholder="age1qx0...your-recipient..."
            style="flex:1;font-family:ui-monospace,monospace;font-size:12px"
            title="${escAttr(trText('tooltip.ageKeys.recipientInput'))}">
          <button type="submit" class="btn btn-primary btn-sm"
            title="${escAttr(trText('tooltip.ageKeys.add'))}">+ ${tr('buttons.addKey')}</button>
        </div>
        <div class="hint">Paste the <code>age1...</code> public line from <code>age-keygen</code>. The private key must stay offline — never paste it here.</div>
      </form>
    </div>` : ''}`;
}

export async function addAgeKey(ev) {
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
    toast(trText('ageKeys.addFailed', {error: e.message}), 'error');
  }
}

export async function removeAgeKey(recipient, hash) {
  const ok = confirm(trText('ageKeys.confirmRemove', {hash}));
  if (!ok) return;
  try {
    const resp = await api('/api/age-keys/' + encodeURIComponent(recipient), { method: 'DELETE' });
    toast(resp.message || tr('ageKeys.removed'), 'success');
    loadAgeKeysSection();
  } catch(e) {
    toast(trText('ageKeys.removeFailed', {error: e.message}), 'error');
  }
}

export async function exportSettings() {
  try {
    const resp = await fetchWithTimeout('/api/settings/export');
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
    toast(trText('toast.exportOk'), 'success');
  } catch(e) { toast(trText('toast.exportFail', {error: e.message}), 'error'); }
}
registerAction('remove-age-key', (el, e, recipient, hash) => removeAgeKey(recipient, hash));
registerAction('export-settings', () => exportSettings());
registerAction('add-age-key', (form, e) => addAgeKey(e));
registerPage('age-keys', loading => renderAgeKeys(loading));
