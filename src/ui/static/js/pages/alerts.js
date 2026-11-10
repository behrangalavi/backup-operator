// pages/alerts.js — Alerts page, alert catalogue, sidebar alerts pill.
// Shared helpers ($, tr, api, escHTML, …) come from the classic core.js /
// charts.js scripts via the global scope; cross-page state lives in state.js.
import { state, isStaleRender } from './state.js';
import { registerPage } from './router.js';

// --- Alert descriptions for each alert type ---
// Icons stay static (universal emoji); titles, desc and action come from
// the active dictionary so a language switch updates the text without
// shipping per-language icons.
export const ALERT_ICONS = {
  BackupOverdue: '⏰',
  BackupDestinationFailing: '💾',
  BackupDumpSizeCollapsed: '📉',
  BackupSchemaChanged: '🔧',
  BackupCharsetChanged: '🔤',
  BackupAnomaliesAppearing: '⚠️',
  BackupLastRunFailed: '❌',
  BackupStorageCorrupted: '🧬',
  BackupRetentionFailing: '🗑️',
  BackupRestoreVerificationFailed: '🚨',
  BackupRestoreVerificationStale: '🕰️',
  BackupSucceeded: '🟢',
  BackupOperatorTestAlert: '🧪',
};
export const ALERT_DESCRIPTIONS = ALERT_ICONS; // legacy alias used by the rules-reference table

// Severity per shipped rule, mirroring docs/observability.md (§13) / values.yaml. Used by the
// rules-reference table; keep in sync when adding a rule. Default 'warning'.
export const ALERT_SEVERITY = {
  BackupOverdue: 'warning',
  BackupDestinationFailing: 'warning',
  BackupDumpSizeCollapsed: 'critical',
  BackupSchemaChanged: 'info',
  BackupCharsetChanged: 'warning',
  BackupStorageCorrupted: 'critical',
  BackupRetentionFailing: 'warning',
  BackupAnomaliesAppearing: 'warning',
  BackupLastRunFailed: 'warning',
  BackupSucceeded: 'info',
  BackupRestoreVerificationFailed: 'critical',
  BackupRestoreVerificationStale: 'warning',
};
export function alertSeverity(name) { return ALERT_SEVERITY[name] || 'warning'; }

export function getAlertDescription(alertname) {
  const icon = ALERT_ICONS[alertname] || '🔔';
  const titleKey = 'alertDesc.' + alertname + '.title';
  const descKey  = 'alertDesc.' + alertname + '.desc';
  const actKey   = 'alertDesc.' + alertname + '.action';
  // tr() falls back to the raw key when missing; that's how we detect
  // "unknown alertname" and substitute the _default text without an extra branch.
  const title = tr(titleKey) === titleKey ? alertname : tr(titleKey);
  const desc  = tr(descKey)  === descKey  ? tr('alertDesc._default.desc')   : tr(descKey);
  const action = tr(actKey)  === actKey   ? tr('alertDesc._default.action') : tr(actKey);
  return { title, icon, desc, action };
}


// --- Alerts ---
export async function renderAlerts(loading = true) {
  const _gen = state.renderGen;
  if (loading) showLoading();

  // Fetch alerts + status in parallel
  let alertsResp = null, statusResp = null;
  let errMsg = '';
  try {
    const [a, s] = await Promise.all([
      api('/api/alerts').catch(e => { errMsg = e.message || 'unknown error'; return null; }),
      api('/api/alerts/status').catch(() => null),
    ]);
    alertsResp = a;
    statusResp = s;
  } catch(e) {
    errMsg = e.message || 'unknown error';
  }

  const items = alertsResp ? (alertsResp.items || []) : [];
  const counts = alertsResp ? (alertsResp.counts || {}) : {};
  const amURL = alertsResp ? alertsResp.alertmanagerUrl : '';
  if (alertsResp) updateAlertsPill(counts);

  // --- Connection Status Banner ---
  let statusBanner = '';
  if (statusResp) {
    const prom = statusResp.prometheus || {};
    const am = statusResp.alertmanager || {};
    const mode = statusResp.mode || 'none';

    const promStatus = !prom.configured
      ? `<span class="badge badge-pending">${tr('page.alerts.notConfigured')}</span>`
      : prom.reachable
        ? `<span class="badge badge-ok">${tr('page.alerts.connected')}</span>${prom.version ? ' <span style="font-size:11px;color:var(--text-muted)">v' + escHTML(prom.version) + '</span>' : ''}`
        : `<span class="badge badge-critical">${tr('page.alerts.unreachable')}</span> <span style="font-size:11px;color:var(--text-muted)">${escHTML(prom.error || '')}</span>`;

    const amStatus = !am.configured
      ? `<span class="badge badge-pending">${tr('page.alerts.notConfigured')}</span>`
      : am.reachable
        ? `<span class="badge badge-ok">${tr('page.alerts.connected')}</span>${am.version ? ' <span style="font-size:11px;color:var(--text-muted)">v' + escHTML(am.version) + '</span>' : ''}`
        : `<span class="badge badge-critical">${tr('page.alerts.unreachable')}</span> <span style="font-size:11px;color:var(--text-muted)">${escHTML(am.error || '')}</span>`;

    const modeLabel = mode === 'prometheus' ? tr('page.alerts.modePrometheus')
      : mode === 'local' ? tr('page.alerts.modeLocal')
      : tr('page.alerts.modeNone');
    const modeBadge = mode === 'prometheus' ? 'ok' : mode === 'local' ? 'warning' : 'critical';

    statusBanner = `
      <div class="table-card" style="margin-bottom:16px">
        <div style="padding:12px 16px;border-bottom:1px solid var(--border)">
          <strong>${tr('page.alerts.connectionStatus')}</strong>
          <span class="badge badge-${modeBadge}" style="margin-left:8px">${tr('page.alerts.mode')}: ${escHTML(modeLabel)}</span>
        </div>
        <table>
          <tbody>
            <tr><td style="width:180px;font-weight:500">Prometheus</td><td>${promStatus}</td><td style="font-size:12px;color:var(--text-muted)">${prom.configured ? escHTML(prom.url || '') : 'Set <code>alerts.prometheusURL</code> in Helm values'}</td></tr>
            <tr><td style="font-weight:500">Alertmanager</td><td>${amStatus}</td><td style="font-size:12px;color:var(--text-muted)">${am.configured ? escHTML(am.url || '') : 'Set <code>alerts.alertmanagerURL</code> in Helm values'}</td></tr>
          </tbody>
        </table>
      </div>`;
  }

  // --- Setup Guide (shown when nothing is configured) ---
  // Guard the field access (mirrors the `|| {}` at the top of the block): a
  // status response missing `prometheus`/`alertmanager` would otherwise throw
  // a TypeError in this async renderer and hang the Alerts page on its spinner.
  let setupGuide = '';
  if (statusResp && !(statusResp.prometheus || {}).configured && !(statusResp.alertmanager || {}).configured) {
    setupGuide = `
      <div class="table-card" style="margin-bottom:16px">
        <div style="padding:12px 16px;border-bottom:1px solid var(--border)">
          <strong>Setup Guide</strong> — How to connect Prometheus &amp; Alertmanager
        </div>
        <div style="padding:16px;font-size:13px;line-height:1.7">
          <p style="margin:0 0 12px"><strong>The backup operator works in 3 modes:</strong></p>
          <table style="width:100%;margin-bottom:16px">
            <thead><tr><th>Mode</th><th>What you need</th><th>What you get</th></tr></thead>
            <tbody>
              <tr><td><span class="badge badge-ok">Full</span></td><td>Prometheus + Alertmanager</td><td>Real-time alerts with debounce + notifications (Slack, Email, PagerDuty)</td></tr>
              <tr><td><span class="badge badge-warning">Prometheus only</span></td><td>Prometheus (e.g. kube-prometheus-stack)</td><td>Canonical alert evaluation with <code>for:</code> duration, visible in this UI</td></tr>
              <tr><td><span class="badge badge-pending">Local</span></td><td>Nothing (built-in)</td><td>Immediate alert evaluation against operator metrics — no external setup needed</td></tr>
            </tbody>
          </table>

          <p style="margin:0 0 8px"><strong>Step 1: Install kube-prometheus-stack</strong> (if not already installed)</p>
          <pre style="background:var(--bg-secondary);padding:12px;border-radius:6px;overflow-x:auto;font-size:12px;margin:0 0 16px">helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm install kube-prometheus-stack prometheus-community/kube-prometheus-stack \\
  -n monitoring --create-namespace</pre>

          <p style="margin:0 0 8px"><strong>Step 2: Configure the backup operator</strong></p>
          <pre style="background:var(--bg-secondary);padding:12px;border-radius:6px;overflow-x:auto;font-size:12px;margin:0 0 16px"># values.yaml
alerts:
  prometheusURL: "http://prometheus-operated.monitoring.svc.cluster.local:9090"
  alertmanagerURL: "http://alertmanager-operated.monitoring.svc.cluster.local:9093"

# Must match your kube-prometheus-stack release name
prometheusReleaseLabel: "kube-prometheus-stack"</pre>

          <p style="margin:0 0 8px"><strong>Step 3: Upgrade the release</strong></p>
          <pre style="background:var(--bg-secondary);padding:12px;border-radius:6px;overflow-x:auto;font-size:12px;margin:0 0 16px">helm upgrade backup-operator ./charts/backup-operator -n backup -f values.yaml</pre>

          <p style="margin:0 0 8px"><strong>Step 4: Configure notifications in Alertmanager</strong></p>
          <details style="margin-bottom:8px">
            <summary style="cursor:pointer;font-weight:500">Slack example</summary>
            <pre style="background:var(--bg-secondary);padding:12px;border-radius:6px;overflow-x:auto;font-size:12px;margin:8px 0 0"># alertmanager-config (Secret or inline in kube-prometheus-stack values)
route:
  receiver: default
  routes:
    - match:
        alertname: =~"^Backup.*"
      receiver: backup-slack
receivers:
  - name: default
    # ...
  - name: backup-slack
    slack_configs:
      - api_url: "https://hooks.slack.com/services/YOUR/WEBHOOK/URL"
        channel: "#backup-alerts"
        title: '{{ .CommonLabels.alertname }}'
        text: '{{ .CommonAnnotations.summary }}'
        send_resolved: true</pre>
          </details>
          <details>
            <summary style="cursor:pointer;font-weight:500">Email example</summary>
            <pre style="background:var(--bg-secondary);padding:12px;border-radius:6px;overflow-x:auto;font-size:12px;margin:8px 0 0"># alertmanager-config
route:
  receiver: default
  routes:
    - match:
        alertname: =~"^Backup.*"
      receiver: backup-email
receivers:
  - name: default
    # ...
  - name: backup-email
    email_configs:
      - to: "ops-team@yourcompany.com"
        from: "alertmanager@yourcompany.com"
        smarthost: "smtp.yourcompany.com:587"
        auth_username: "alertmanager@yourcompany.com"
        auth_password: "your-smtp-password"
        send_resolved: true</pre>
          </details>
        </div>
      </div>`;
  }

  // --- Source mode banner ---
  const localCount = items.filter(a => a.source === 'local').length;
  const sourceBanner = localCount === items.length && items.length > 0
    ? `<div class="banner banner-info" style="margin-bottom:12px">${tr('banner.localEvaluator')}</div>`
    : '';

  // --- Alert info/error banner ---
  let alertError = '';
  if (!alertsResp) {
    alertError = `<div class="banner banner-warning" style="margin-bottom:12px">${tr('banner.alertsLoadFailed', {error: errMsg})}</div>`;
  }

  // --- Test alert button ---
  const testBtn = statusResp && statusResp.alertmanager && statusResp.alertmanager.reachable
    ? '<button class="btn btn-secondary btn-sm" data-action="send-test-alert" id="btnTestAlert">🧪 ' + tr('buttons.sendTest') + '</button>'
    : '';

  if (isStaleRender(_gen)) return;

  content.innerHTML = `
    <div class="page-header">
      <div>
        <h1>${tr('page.alerts.title')}</h1>
        <div class="subtitle">${tr('page.alerts.subtitle')}${amURL ? ' — <a href="' + escAttr(amURL) + '" target="_blank">' + tr('page.alerts.openInAlertmanager') + '</a>' : ''}</div>
      </div>
      <div style="display:flex;gap:8px">
        ${testBtn}
        <button class="btn btn-secondary btn-sm" ${actionAttrs('refresh-alerts', 'false')}>↻ ${tr('buttons.refresh')}</button>
      </div>
    </div>
    ${statusBanner}
    ${setupGuide}
    ${alertError}
    ${sourceBanner}
    <div class="stats-row">
      <div class="stat-card"><div class="label">${tr('stat.critical')}</div><div class="value${counts.critical > 0 ? ' bad' : ''}">${counts.critical || 0}</div></div>
      <div class="stat-card"><div class="label">${tr('stat.warning')}</div><div class="value${counts.warning > 0 ? ' bad' : ''}">${counts.warning || 0}</div></div>
      <div class="stat-card"><div class="label">${tr('stat.info')}</div><div class="value">${counts.info || 0}</div></div>
    </div>
    ${items.length === 0
      ? `<div class="empty-state"><h3>${tr('page.alerts.empty')}</h3><p>${tr('page.alerts.empty')}</p></div>`
      : `<div class="table-card">
        <table>
          <thead><tr>
            <th>${tr('table.severity')}</th><th>${tr('table.alert')}</th><th>${tr('table.target')}</th><th>${tr('table.destination')}</th>
            <th>${tr('table.sinceFiring')}</th><th>Source</th><th>${tr('table.summary')}</th>
          </tr></thead>
          <tbody>${items.map(a => {
            const info = getAlertDescription(a.alertname);
            return `<tr>
            <td><span class="badge badge-${escAttr(a.severity || 'info')}">${escHTML(a.severity || 'info')}</span></td>
            <td>
              <details style="cursor:pointer">
                <summary><code style="font-size:12px">${escHTML(info.icon)} ${escHTML(info.title)}</code></summary>
                <div style="padding:8px 0;font-size:12px;line-height:1.6">
                  <p style="margin:0 0 6px">${escHTML(info.desc)}</p>
                  <p style="margin:0;color:var(--text-muted)"><strong>${tr('table.action')}:</strong> ${escHTML(info.action)}</p>
                </div>
              </details>
            </td>
            <td>${a.target ? '<a href="#/target/' + escAttr(a.target) + '" style="color:var(--accent)">' + escHTML(a.target) + '</a>' : '—'}</td>
            <td>${a.destination ? escHTML(a.destination) : '—'}</td>
            <td style="font-size:12px;color:var(--text-muted)">${a.activeSince ? timeAgo(a.activeSince) : '—'}</td>
            <td><span class="badge badge-${a.source === 'prometheus' ? 'ok' : 'pending'}" title="${escAttr(trText(a.source === 'prometheus' ? 'tooltip.alerts.sourcePrometheus' : 'tooltip.alerts.sourceLocal'))}">${escHTML(a.source || '?')}</span></td>
            <td style="font-size:13px">${escHTML(a.summary || '')}</td>
          </tr>`;
          }).join('')}</tbody>
        </table>
      </div>`}

    <div class="table-card" style="margin-top:24px">
      <div style="padding:12px 16px;border-bottom:1px solid var(--border)">
        <strong>${tr('page.alerts.rulesReference')}</strong>
        <span style="font-size:12px;color:var(--text-muted);margin-left:8px">${tr('page.alerts.rulesReferenceSub', {count: Object.keys(ALERT_DESCRIPTIONS).filter(k => k !== 'BackupOperatorTestAlert').length})}</span>
      </div>
      <table>
        <thead><tr><th>${tr('page.alerts.alertCol')}</th><th>${tr('page.alerts.severityCol')}</th><th>${tr('page.alerts.conditionCol')}</th><th>${tr('page.alerts.descriptionCol')}</th></tr></thead>
        <tbody>
          ${Object.keys(ALERT_DESCRIPTIONS).filter(k => k !== 'BackupOperatorTestAlert').map(name => {
            const info = getAlertDescription(name);
            return `<tr>
            <td><code style="font-size:12px">${escHTML(info.icon)} ${escHTML(info.title)}</code></td>
            <td><span class="badge badge-${alertSeverity(name)}">${alertSeverity(name)}</span></td>
            <td style="font-size:12px">${escHTML(info.desc)}</td>
            <td style="font-size:12px;color:var(--text-muted)">${escHTML(info.action)}</td>
          </tr>`;
          }).join('')}
        </tbody>
      </table>
    </div>`;
}

// Send test alert to Alertmanager
export async function sendTestAlert() {
  const btn = document.getElementById('btnTestAlert');
  if (btn) { btn.disabled = true; btn.textContent = tr('common.loading'); }
  try {
    const resp = await api('/api/alerts/test', { method: 'POST' });
    toast(resp.message || tr('toast.testAlertSent'), 'success');
  } catch(e) {
    toast(e.message || trText('toast.saveFailed', {error: e.message}), 'error');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = '🧪 ' + tr('buttons.sendTest'); }
  }
}

// updateAlertsPill keeps the sidebar counter in sync with the latest /api/alerts
// response. We call it after every alert refresh and once on startup; the SSE
// refresh tick triggers periodic re-fetches when the user is on other pages
// so the pill stays current without polling.
export function updateAlertsPill(counts) {
  const el = $('#navAlertsPill');
  if (!el) return;
  const total = (counts.critical || 0) + (counts.warning || 0) + (counts.info || 0);
  if (total === 0) { el.hidden = true; return; }
  el.hidden = false;
  el.textContent = total;
  el.className = 'alerts-pill ' + (counts.critical > 0 ? 'critical' : counts.warning > 0 ? 'warning' : 'info');
}

export async function refreshAlertsPill() {
  try {
    const resp = await api('/api/alerts');
    if (resp && resp.counts) updateAlertsPill(resp.counts);
  } catch(_) {
    // Hide rather than keep a stale number: a count we cannot refresh is
    // worse than no count — it reads as "N alerts right now" when it may
    // be minutes old and the backend is unreachable.
    updateAlertsPill({});
  }
}
registerAction('send-test-alert', () => sendTestAlert());
registerAction('refresh-alerts', () => renderAlerts(false));
registerPage('alerts', loading => renderAlerts(loading));
