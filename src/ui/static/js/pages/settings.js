// pages/settings.js — Settings page (read-only operator defaults + values.yaml export).
// Shared helpers ($, tr, api, escHTML, …) come from the classic core.js /
// charts.js scripts via the global scope; cross-page state lives in state.js.
import { state, isStaleRender } from './state.js';
import { registerPage } from './router.js';

// --- Settings (read-only operator defaults) ---
// Fleet-wide defaults are Helm values → env vars; the operator exposes the
// effective values and can export them as values.yaml. Per-source settings
// are typed fields on each BackupSource and are edited on the Sources page.
export async function renderSettings(loading = true) {
  const _gen = state.renderGen;
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

  if (isStaleRender(_gen)) return;

  if (!settings) {
    content.innerHTML = `
      <div class="page-header">
        <div><h1>${tr('page.settings.title')}</h1><div class="subtitle">${tr('page.settings.subtitle')}</div></div>
      </div>
      <div class="empty-state">
        <h3>${tr('page.settings.notAvailable')}</h3>
        <p>${tr('toast.loadFailed', {error: errorMsg || ''})}</p>
        <button class="btn btn-primary" ${actionAttrs('refresh-settings', 'true')} style="margin-top:12px" title="${escAttr(trText('tooltip.settings.reload'))}">${tr('buttons.refresh')}</button>
      </div>`;
    return;
  }
  renderSettingsPage(settings);
}

export function renderSettingsPage(s) {
  const v = x => escHTML(x || '') || '—';
  content.innerHTML = `
    <div class="page-header">
      <div><h1>${tr('page.settings.title')}</h1><div class="subtitle">${tr('page.settings.subtitleRO')}</div></div>
      <div style="display:flex;gap:8px">
        <button class="btn btn-secondary" data-action="export-settings" title="${escAttr(trText('tooltip.settings.export'))}">&#8681; ${tr('buttons.exportValues')}</button>
      </div>
    </div>
    <div class="banner banner-info" style="margin-bottom:16px">${tr('page.settings.readOnlyNote')}</div>
    <div class="review-grid">
      <div class="detail-card">
        <h3>${tr('page.settings.section.schedule')}</h3>
        <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.defaultSchedule')}</span><code class="val">${v(s.defaultSchedule)}</code></div>
        <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.runTimeout')}</span><span class="val">${v(s.runTimeoutSeconds)}</span></div>
      </div>
      <div class="detail-card">
        <h3>${tr('page.settings.section.retention')}</h3>
        <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.retentionDays')}</span><span class="val">${v(s.defaultRetentionDays)}</span></div>
        <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.minKeep')}</span><span class="val">${v(s.defaultMinKeep)}</span></div>
        <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.tempDir')}</span><code class="val">${v(s.tempDir)}</code></div>
        <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.tempDirSize')}</span><span class="val">${v(s.tempDirSize)}</span></div>
      </div>
      <div class="detail-card">
        <h3>${tr('page.settings.section.worker')}</h3>
        <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.cpuLimit')}</span><span class="val">${v(s.workerCpuLimit)}</span></div>
        <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.memoryLimit')}</span><span class="val">${v(s.workerMemoryLimit)}</span></div>
        <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.cpuRequest')}</span><span class="val">${v(s.workerCpuRequest)}</span></div>
        <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.memoryRequest')}</span><span class="val">${v(s.workerMemoryRequest)}</span></div>
      </div>
    </div>
    <div class="wizard-note" style="margin-top:16px">${tr('page.settings.exportHint')}</div>`;
}
registerAction('refresh-settings', () => renderSettings(true));
registerPage('settings', loading => renderSettings(loading));
