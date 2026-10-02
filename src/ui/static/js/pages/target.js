// pages/target.js — Target detail: run history, JSON viewer, analysis + verification cards.
// Shared helpers ($, tr, api, escHTML, …) come from the classic core.js /
// charts.js scripts via the global scope; cross-page state lives in state.js.
import { state, isStaleRender } from './state.js';
import { currentPage, currentParam, registerPage } from './router.js';
import { _fastDataCache, fastDataAllSettled } from './dashboard.js';
import { fmtNextRun, renderProgressCell } from './jobs.js';

export function sortRuns(runs) {
  const g = {
    timestamp: r => parseTsCompact(r.timestamp),
    status:    r => r.status || '',
    size:      r => r.status !== 'failed' ? (r.encryptedSizeBytes || 0) : null,
    schema:    r => r.report ? (r.report.schemaChanged ? 1 : 0) : null,
    tables:    r => (r.stats && r.stats.tables) ? r.stats.tables.length : null,
    anomalies: r => (r.report && r.report.anomalies) ? r.report.anomalies.length : 0,
    verification: r => r.verification ? r.verification.verdict : '',
    restoreVerification: r => r.restoreVerification ? r.restoreVerification.verdict : '',
  };
  const s = sortState.runs;
  return sortBy(runs, g[s.col] || g.timestamp, s.dir);
}

// Go's time.Duration String form: e.g. "1h2m3s", "45s", "0s", "743ms",
// "3.25s", "1.5µs". The old /(\d+)([hms])/ regex mis-read sub-second units —
// "743ms" parsed as 743 MINUTES (the trailing "s" was dropped and "743m" won)
// and "3.25s" as 25s — so duration-sorted rows were badly ordered. Parse the
// real grammar: a run of <float><unit>, ms/us/µs/ns matched before s/m/h.
export function parseDurationSec(s) {
  if (!s) return null;
  let total = 0, matched = false, m;
  const re = /([0-9]*\.?[0-9]+)(ns|us|µs|ms|s|m|h)/g;
  while ((m = re.exec(s)) !== null) {
    matched = true;
    const n = parseFloat(m[1]);
    switch (m[2]) {
      case 'ns': total += n / 1e9; break;
      case 'us': case 'µs': total += n / 1e6; break;
      case 'ms': total += n / 1e3; break;
      case 's':  total += n; break;
      case 'm':  total += n * 60; break;
      case 'h':  total += n * 3600; break;
    }
  }
  return matched ? total : null;
}


// --- Target detail run-history cache ---
// Run history is the only storage-probing fetch on the detail page (the
// backend probes each destination, up to 12 s apiece). Blocking the page
// render on it made two things bad: (1) clicking a source showed nothing
// for many seconds even though the summary cards come from the fast
// /api/targets read, and (2) a language switch — a purely presentational
// re-render — re-fired the slow probe. This mirrors the dashboard's
// stale-while-revalidate caches (state.fleetSummary / state.slowProbes): render the
// summary immediately, fill the runs region from cache, and only re-probe
// storage when the cache is for a different target, expired, or explicitly
// invalidated by an SSE event. `runs == null` means "never loaded yet";
// `failed` records a probe error so the region can show a retry affordance
// instead of a misleading "No runs".
export const RUN_HISTORY_TTL_MS = 30000;
export const _targetRunsInFlight = new Set(); // keyed by target name; dedups SSE-tick storms

// Run-history pagination + chart bound. A high-frequency schedule with long
// retention can accumulate tens of thousands of runs; rendering one table row
// AND one size-chart point per run froze the browser (the table is the worst
// offender — ~11 cells per row). We page the table client-side over the
// already-fetched array (no extra requests) and cap the size-chart to its most
// recent points. The status heatmap is day-bucketed (≤91 cells) so it stays
// full. _runHistoryPage resets when the viewed target changes.
export const RUN_HISTORY_PAGE_SIZE = 50;
export const MAX_TREND_POINTS = 365; // newest-N points fed to the size-trend chart
export let _runHistoryTarget = '';
export let _runHistoryPage = 0;

// gotoRunHistoryPage repaints ONLY the runs region with a new page, from the
// already-cached runs — never refetches. Clamped to valid range by the
// renderer.
export function gotoRunHistoryPage(name, page) {
  _runHistoryPage = page < 0 ? 0 : page;
  const region = document.getElementById('target-runs-region');
  if (region) {
    const target = (_fastDataCache['/api/targets'] || []).find(t => t.Name === name);
    region.innerHTML = targetRunsRegionHTML(name, target);
  }
}

// fetchTargetRuns refreshes the run-history cache for `name` in the
// background and repaints ONLY the runs region when done — never the whole
// page, so an in-flight probe can't yank the summary away. Per-name
// in-flight guard so rapid target switches each get their own slot while
// repeated events for the same target are deduped.
export async function fetchTargetRuns(name) {
  if (_targetRunsInFlight.has(name)) return;
  _targetRunsInFlight.add(name);
  let runs = null, failed = false;
  try {
    runs = (await api('/api/targets/' + encodeURIComponent(name) + '/runs')) || [];
  } catch (e) {
    failed = true;
  } finally {
    _targetRunsInFlight.delete(name);
  }
  if (failed) {
    // Keep any previously cached runs for this target; flag the failure so the
    // region offers a retry rather than claiming emptiness. Record lastFetch
    // even on failure so a failed probe honours the TTL cooldown — otherwise
    // the 10 s SSE refresh tick re-probes the unreachable destination every
    // tick (the connection storm that got the operator IP-blocked). The Retry
    // button and SSE invalidation both reset lastFetch to 0 to force an
    // immediate re-probe.
    if (state.targetRuns.name === name) { state.targetRuns.failed = true; state.targetRuns.lastFetch = Date.now(); }
    else state.targetRuns = { name, runs: null, lastFetch: Date.now(), failed: true };
  } else {
    state.targetRuns = { name, runs, lastFetch: Date.now(), failed: false };
  }
  // Repaint only if the user is still on this target's page.
  if (currentPage() === 'target' && currentParam() === name) {
    const region = document.getElementById('target-runs-region');
    if (region) {
      const target = (_fastDataCache['/api/targets'] || []).find(t => t.Name === name);
      region.innerHTML = targetRunsRegionHTML(name, target);
    }
  }
}

// reloadTargetRuns is the Retry button's handler: invalidate the cache for
// this target, kick a fresh probe, and repaint the region to its loading
// state immediately.
export function reloadTargetRuns(name) {
  if (state.targetRuns.name === name) { state.targetRuns.lastFetch = 0; state.targetRuns.failed = false; }
  fetchTargetRuns(name); // sets in-flight synchronously before we repaint
  const region = document.getElementById('target-runs-region');
  if (region) {
    const target = (_fastDataCache['/api/targets'] || []).find(t => t.Name === name);
    region.innerHTML = targetRunsRegionHTML(name, target);
  }
}

export function runHistoryShell(inner) {
  return `<div class="table-card"><div class="table-card-header"><h2>${tr('target.runHistory')}</h2></div>${inner}</div>`;
}

// targetRunsRegionHTML renders the runs-dependent part of the detail page
// (size/heatmap charts, tables card, run-history table) from the cached
// state.targetRuns state. Crucially it distinguishes three states a flat runs
// array can't: in-flight load, probe failure, and a genuinely empty
// history — so a slow/unreachable destination shows a spinner or a retry
// prompt, never a misleading "No runs".
export function targetRunsRegionHTML(name, target) {
  const cached = state.targetRuns.name === name ? state.targetRuns : null;
  const loading = _targetRunsInFlight.has(name) && !(cached && cached.runs != null);
  const runs = (cached && cached.runs) || [];

  if (loading) {
    return runHistoryShell(`<div class="empty-state"><div class="spinner"></div>
      <p style="color:var(--text-muted);margin-top:8px">${tr('target.runsLoading')}</p></div>`);
  }
  if (runs.length === 0) {
    // A target whose latest meta exists MUST have at least one run, so an
    // empty list there is a probe failure (timeout / unreachable
    // destination), not a genuinely empty history. Offer a retry.
    const probeFailed = (cached && cached.failed) || (target && target.Latest);
    if (probeFailed) {
      return runHistoryShell(`<div class="empty-state"><p>${tr('target.runsUnavailable')}</p>
        <button class="btn btn-secondary btn-sm" ${actionAttrs('reload-runs', name)}>${tr('common.retry')}</button></div>`);
    }
    return runHistoryShell(`<div class="empty-state"><p>${tr('target.noRuns')}</p></div>`);
  }

  // Reset to the first page whenever the viewed target changes, so a fresh
  // detail view never lands on a stale page index from a previous target.
  if (_runHistoryTarget !== name) { _runHistoryTarget = name; _runHistoryPage = 0; }

  // Table: page the (sorted) runs client-side. Charts: cap the size-trend to
  // its most recent points (the status heatmap aggregates by day and is safe).
  const sorted = sortRuns(runs);
  const total = sorted.length;
  const pageCount = Math.max(1, Math.ceil(total / RUN_HISTORY_PAGE_SIZE));
  if (_runHistoryPage > pageCount - 1) _runHistoryPage = pageCount - 1;
  const startIdx = _runHistoryPage * RUN_HISTORY_PAGE_SIZE;
  const pageRuns = sorted.slice(startIdx, startIdx + RUN_HISTORY_PAGE_SIZE);
  const trendRuns = runs.length > MAX_TREND_POINTS ? runs.slice(0, MAX_TREND_POINTS) : runs;

  const pager = total > RUN_HISTORY_PAGE_SIZE ? `
      <div class="table-pager" style="display:flex;align-items:center;gap:8px;padding:8px 12px;justify-content:flex-end">
        <span style="font-size:12px;color:var(--text-muted)">${tr('pager.range', {from: startIdx + 1, to: startIdx + pageRuns.length, total})}</span>
        <button class="btn btn-ghost btn-sm" ${_runHistoryPage <= 0 ? 'disabled' : ''} ${actionAttrs('runs-page', name, _runHistoryPage - 1)} title="${tr('pager.prev')}">‹</button>
        <span style="font-size:12px">${tr('pager.page', {page: _runHistoryPage + 1, pages: pageCount})}</span>
        <button class="btn btn-ghost btn-sm" ${_runHistoryPage >= pageCount - 1 ? 'disabled' : ''} ${actionAttrs('runs-page', name, _runHistoryPage + 1)} title="${tr('pager.next')}">›</button>
      </div>` : '';

  return `
    <div class="chart-grid-2">
      <div class="chart-card">
        <h3>${tr('target.dumpSizeTrend')}${runs.length > MAX_TREND_POINTS ? ` <span class="chart-card-sub">${tr('chart.recentN', {n: MAX_TREND_POINTS})}</span>` : ''}</h3>
        ${renderSizeChart(trendRuns)}
      </div>
      <div class="chart-card">
        <h3>${tr('target.runStatusHeatmap')}</h3>
        ${renderStatusHeatmap(runs)}
      </div>
    </div>
    ${renderTablesCard(runs)}
    <div class="table-card">
      <div class="table-card-header"><h2>${tr('target.runHistory')} <span class="chart-card-sub">${tr('pager.totalRuns', {total})}</span></h2></div>
      <table>
        <thead><tr>
          <th class="num row-num">#</th>
          <th class="sortable" ${sortableTh('runs','timestamp')}>${tr('table.timestamp')}${sortIndicator('runs','timestamp')}</th>
          <th class="sortable" ${sortableTh('runs','status')}>${tr('table.status')}${sortIndicator('runs','status')}</th>
          <th class="num sortable" ${sortableTh('runs','size')}>${tr('table.size')}${sortIndicator('runs','size')}</th>
          <th>${tr('table.destinations')}</th>
          <th class="sortable" ${sortableTh('runs','verification')}>${tr('table.verification')}${sortIndicator('runs','verification')}</th>
          <th class="sortable" ${sortableTh('runs','restoreVerification')}>${tr('target.restoreLabel')}${sortIndicator('runs','restoreVerification')}</th>
          <th class="sortable" ${sortableTh('runs','schema')}>${tr('table.schema')}${sortIndicator('runs','schema')}</th>
          <th class="num sortable" ${sortableTh('runs','tables')}>${tr('verification.table')}${sortIndicator('runs','tables')}</th>
          <th class="sortable" ${sortableTh('runs','anomalies')}>${tr('table.anomalies')} / ${tr('table.error')}${sortIndicator('runs','anomalies')}</th>
          <th>${tr('row.download')}</th>
        </tr></thead>
        <tbody>${pageRuns.map((r, i) => `<tr>
          <td class="num row-num">${startIdx + i + 1}</td>
          <td style="font-size:12px">${r.timestamp ? new Date(r.timestamp.replace(/(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z/,'$1-$2-$3T$4:$5:$6Z')).toLocaleString() : '—'}</td>
          <td>${r.status === 'failed' ? failedBadge(r) : `<span class="badge badge-ok">${tr('badge.ok')}</span>`}</td>
          <td class="num" style="font-size:12px">${r.status !== 'failed' ? humanBytes(r.encryptedSizeBytes) : '—'}</td>
          <td style="font-size:11px">${r.destinations && r.destinations.length > 0
            ? r.destinations.map(d => {
                const cls = d.status === 'success' ? 'badge-ok' : 'badge-failed';
                const tip = d.error ? ' title="' + escAttr(d.error) + '"' : '';
                return '<span class="badge ' + cls + '" style="margin:1px;font-size:10px"' + tip + '>' + escHTML(d.name) + '</span>';
              }).join('')
            : '<span style="color:var(--text-muted)">—</span>'}</td>
          <td>${renderVerificationBadge(r.verification)}</td>
          <td>${renderRestoreVerificationBadge(r.restoreVerification)}</td>
          <td>${renderSchemaCharsetCell(r)}</td>
          <td class="num" style="font-size:12px">${r.stats && r.stats.tables ? r.stats.tables.length : '—'}</td>
          <td>${r.status === 'failed'
            ? `<span style="color:var(--danger);font-size:12px;word-break:break-word" title="${escAttr(r.error || '')}">${escHTML(truncate(r.error, 120) || '(no message)')}</span>`
            : (r.report && r.report.anomalies ? `<span class="num" style="color:var(--danger)">${r.report.anomalies.length}</span>` : '<span class="num">0</span>')}</td>
          <td>${r.status !== 'failed' ? renderDownloadLinks(name, r, target && target.Destinations) : '—'}</td>
        </tr>`).join('')}</tbody>
      </table>${pager}
    </div>`;
}

export async function renderTargetDetail(name, loading = true) {
  const _gen = state.renderGen;
  // Empty name shouldn't reach here from a normal click — hash is
  // `#/target/<name>` and currentParam returns the second segment. If
  // we got here with no name, the hash was malformed (e.g. someone
  // typed `#/target/` by hand). Show an explicit error rather than
  // silently bouncing to Dashboard, so the user can see what went
  // wrong instead of "I clicked and ended up somewhere else".
  if (!name) {
    if (loading) showLoading();
    if (isStaleRender(_gen)) return;
    content.innerHTML = `<div class="empty-state"><h3>${tr('target.missingNameTitle')}</h3>
      <p>${tr('target.missingNameBody', {hash: location.hash})}</p>
      <a href="#/" class="btn btn-secondary">${tr('buttons.backToDashboard')}</a></div>`;
    return;
  }
  if (loading) showLoading();
  let targets = [], jobs = [];
  // fastDataAllSettled keeps a per-URL last-good cache, so a transient
  // /api/jobs failure doesn't blow away targets and leave us showing
  // "target not found" for a real target. All three are fast, non-blocking
  // server-side reads — unlike run history, which is fetched separately
  // below so the slow storage probe never holds up the summary render.
  const got = await fastDataAllSettled(['/api/targets', '/api/destinations', '/api/jobs']);
  if (isStaleRender(_gen)) return;
  targets = got[0] || [];
  jobs    = got[2] || [];

  const target = targets.find(t => t.Name === name);
  if (!target) {
    content.innerHTML = `<div class="empty-state"><h3>Target not found</h3><p>"${escAttr(name)}" does not exist.</p>
      <a href="#/" class="btn btn-secondary" title="${escAttr(trText('tooltip.target.backToDashboard'))}">${tr('buttons.backToDashboard')}</a></div>`;
    return;
  }

  // Run history: stale-while-revalidate (see state.targetRuns above). Kick a
  // background refresh only when the cache is for a different target,
  // expired, or invalidated by SSE — NOT on a language switch or sort,
  // which reuse the cache and so re-render instantly. fetchTargetRuns sets
  // its in-flight flag synchronously, so the region below shows a spinner.
  // Note: a failed probe is "fresh" for the TTL window too (its lastFetch is
  // stamped on failure), so we back off instead of re-probing every SSE tick.
  const runsFresh = state.targetRuns.name === name &&
    (Date.now() - state.targetRuns.lastFetch < RUN_HISTORY_TTL_MS);
  if (!runsFresh) fetchTargetRuns(name);

  // Find any in-flight Job for this target so we can render the same
  // progress bar the Jobs page uses. Multiple running jobs is rare but
  // possible (manual trigger overlap); pick the newest.
  const runningJob = jobs
    .filter(j => j.target === name && j.status === 'running')
    .sort((a, b) => parseTsRFC(b.startTime) - parseTsRFC(a.startTime))[0] || null;

  content.innerHTML = `
    <div class="page-header">
      <div>
        <div style="margin-bottom:8px"><a href="#/" style="color:var(--text-muted);font-size:13px;text-decoration:none">&larr; Dashboard</a></div>
        <h1>${escHTML(name)} <span class="badge badge-${target.DBType}">${target.DBType}</span></h1>
      </div>
      <div style="display:flex;gap:8px">
        <button class="btn btn-secondary btn-sm" ${actionAttrs('trigger-backup', name)} title="${escAttr(trText('tooltip.sources.run'))}">&#9654; ${tr('buttons.runNow')}</button>
        <button class="btn btn-secondary btn-sm" ${actionAttrs('open-source-form', target.ObjectName)} title="${escAttr(trText('tooltip.sources.edit'))}">${tr('common.edit')}</button>
        <button class="btn btn-danger btn-sm" ${actionAttrs('delete-source', target.ObjectName, name)} title="${escAttr(trText('tooltip.sources.delete'))}">${tr('common.delete')}</button>
      </div>
    </div>
    ${runningJob ? `
    <div class="detail-card running-banner" data-job-name="${escAttr(runningJob.name)}" style="margin-bottom:16px;border-left:3px solid var(--accent)">
      <h3 style="display:flex;align-items:center;gap:8px">
        <span class="badge badge-running">${tr('badge.running')}</span>
        ${tr('target.runningTitle') || 'Backup läuft gerade'}
      </h3>
      <div class="job-progress-host">${renderProgressCell(runningJob)}</div>
      <div style="font-size:11px;color:var(--text-muted);margin-top:6px">
        ${tr('table.startTime')}: ${new Date(runningJob.startTime).toLocaleString()} · Job: <code>${escHTML(runningJob.name)}</code>
      </div>
    </div>` : ''}
    <div class="detail-grid">
      <div class="detail-card">
        <h3>${tr('target.configuration')}</h3>
        <div class="detail-row"><span class="key">${tr('table.schedule')}</span><code class="val">${escHTML(target.Schedule)}</code></div>
        <div class="detail-row"><span class="key">${tr('target.nextRun')}</span><span class="val">${target.Suspended ? `<span style="color:var(--text-muted)">${tr('badge.paused')}</span>` : escHTML(fmtNextRun(target.nextRun)) || '—'}</span></div>
        <div class="detail-row"><span class="key">${tr('table.destinations')}</span><span class="val">${(target.Destinations||[]).join(', ') || tr('common.all').toLowerCase()}</span></div>
        <div class="detail-row"><span class="key">${tr('table.status')}</span>
          ${target.Latest ? (target.Latest.status === 'failed'
            ? failedBadge(target.Latest)
            : `<span class="badge badge-ok">${tr('badge.ok')}</span>`)
            : `<span class="badge badge-pending">${tr('badge.noRuns')}</span>`}</div>
      </div>
      <div class="detail-card">
        <h3>${tr('target.latestRun')}</h3>
        ${target.Latest ? (target.Latest.status === 'failed' ? `
        <div class="detail-row"><span class="key">${tr('target.time')}</span><span class="val">${timeAgo(target.Latest.timestamp)}</span></div>
        <div class="detail-row"><span class="key">${tr('table.phase')}</span><span class="val">${escHTML(target.Latest.phase || '—')}</span></div>
        <div class="detail-row" style="align-items:flex-start"><span class="key">${tr('table.error')}</span><pre class="val" style="color:var(--danger);font-size:12px;white-space:pre-wrap;word-break:break-word;margin:0;background:var(--bg-input);padding:8px;border-radius:4px;max-height:160px;overflow:auto">${escHTML(target.Latest.error || '(no message)')}</pre></div>
        ` : `
        <div class="detail-row"><span class="key">${tr('target.time')}</span><span class="val">${timeAgo(target.Latest.timestamp)}</span></div>
        <div class="detail-row"><span class="key">${tr('table.size')}</span><span class="val">${humanBytes(target.Latest.encryptedSizeBytes)}</span></div>
        <div class="detail-row"><span class="key">${tr('target.sha256')}</span><code class="val" style="font-size:11px">${escHTML((target.Latest.sha256 || '—').substring(0, 16))}${target.Latest.sha256 ? '...' : ''}</code></div>
        <div class="detail-row"><span class="key">${tr('target.verificationLabel')}</span><span class="val">${renderVerificationBadge(target.Latest.verification)}</span></div>
        ${target.Latest.restoreVerification ? `<div class="detail-row"><span class="key">${tr('target.restoreLabel')}</span><span class="val">${renderRestoreVerificationBadge(target.Latest.restoreVerification)}</span></div>` : ''}
        ${renderCharsetRow(target.Latest)}
        ${renderSchemaAgeRow(target.Latest)}
        `) : `<div style="color:var(--text-muted);padding:12px 0">${tr('target.noRuns')}</div>`}
      </div>
    </div>
    ${renderVerificationDetail(target.Latest)}
    ${renderRestoreVerificationDetail(target.Latest)}
    ${renderAnalysisCoverageCard(target)}
    <div id="target-runs-region">${targetRunsRegionHTML(name, target)}</div>`;

  // Per-second tick of the running-banner progress bar — same pattern as
  // the Jobs page so the user sees the time bar advance without waiting
  // for the next SSE refresh.
  if (state.jobProgressTimer) { clearInterval(state.jobProgressTimer); state.jobProgressTimer = null; }
  if (runningJob) {
    state.jobProgressTimer = setInterval(() => {
      const host = document.querySelector(`.running-banner[data-job-name="${CSS.escape(runningJob.name)}"] .job-progress-host`);
      if (host) host.innerHTML = renderProgressCell(runningJob);
    }, 1000);
  }
}

export function renderDownloadLinks(targetName, run, destNames) {
  // Two contexts for the same values, two escapers: encodeURIComponent for
  // URL path segments, escAttr for attribute values (actionAttrs does that
  // for the view-meta arguments). targetName and dest names are free-form,
  // so a single wrong escaper here is XSS (see the viewMeta Copy-button note).
  const tsUrl = encodeURIComponent(run.timestamp);
  const tUrl  = encodeURIComponent(targetName);
  const tAttr = escAttr(targetName);
  const successDests = run.destinations ? run.destinations.filter(d => d.status === 'success') : [];
  if (successDests.length <= 1) {
    const destName = successDests.length === 1 ? successDests[0].name : '';
    const destParam = destName ? '?destination=' + encodeURIComponent(destName) : '';
    return `<button class="btn btn-ghost btn-sm" style="font-size:11px" ${actionAttrs('view-meta', targetName, run.timestamp, destName)} title="${escAttr(trText('tooltip.runs.viewMeta'))}">&#128065;</button>
      <a href="/download/${tUrl}/${tsUrl}/meta${destParam}" download="${tAttr}-${escAttr(run.timestamp)}.meta.json" class="btn btn-ghost btn-sm" style="font-size:11px" title="${escAttr(trText('tooltip.runs.downloadMeta'))}">.json</a>
      <a href="/download/${tUrl}/${tsUrl}/dump${destParam}" class="btn btn-ghost btn-sm" style="font-size:11px" title="${escAttr(trText('tooltip.runs.downloadDump'))}">.age</a>`;
  }
  return `<div class="dropdown" style="display:inline-block">
    <button class="btn btn-ghost btn-sm" style="font-size:11px" data-action="toggle-dropdown" aria-haspopup="true" aria-expanded="false" title="${escAttr(trText('tooltip.runs.pickDestination'))}">${tr('buttons.download')} &#9662;</button>
    <div class="dropdown-menu">${successDests.map(d =>
      `<a href="/download/${tUrl}/${tsUrl}/dump?destination=${encodeURIComponent(d.name)}" class="dropdown-item" style="font-size:12px" title="${escAttr(trText('tooltip.runs.downloadDumpFrom', {dest: d.name}))}">
        ${escHTML(d.name)} <span style="opacity:0.6;font-size:10px">(${escHTML(d.storageType)})</span>
      </a>`
    ).join('')}
    <hr style="margin:4px 0;border:none;border-top:1px solid var(--border)">
    ${successDests.map(d =>
      `<button class="dropdown-item" style="font-size:11px;text-align:left;background:none;border:none;width:100%;cursor:pointer" ${actionAttrs('view-meta', targetName, run.timestamp, d.name)} title="${escAttr(trText('tooltip.runs.viewMetaFrom', {dest: d.name}))}">
        &#128065; view: ${escHTML(d.name)}
      </button>`
    ).join('')}
    ${successDests.map(d =>
      `<a href="/download/${tUrl}/${tsUrl}/meta?destination=${encodeURIComponent(d.name)}" download="${tAttr}-${escAttr(run.timestamp)}.meta.json" class="dropdown-item" style="font-size:11px;opacity:0.7" title="${escAttr(trText('tooltip.runs.downloadMetaFrom', {dest: d.name}))}">
        meta: ${escHTML(d.name)}
      </a>`
    ).join('')}
    </div>
  </div>`;
}

// --- JSON viewer ---
export async function viewMeta(targetName, timestamp, destination) {
  const url = '/download/' + encodeURIComponent(targetName) + '/' + encodeURIComponent(timestamp) + '/meta' +
    (destination ? '?destination=' + encodeURIComponent(destination) : '');
  openModal(targetName + ' — ' + timestamp, '<div class="empty-state"><div class="spinner"></div></div>');
  try {
    const data = await api(url);
    const pretty = JSON.stringify(data, null, 2);
    const fname = `${targetName}-${timestamp}.meta.json`;
    $('#modal-body').innerHTML = `
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:12px;gap:12px;flex-wrap:wrap">
        <span style="color:var(--text-muted);font-size:12px">${destination ? 'destination: <code>' + escHTML(destination) + '</code>' : ''}</span>
        <div style="display:flex;gap:6px">
          <button class="btn btn-ghost btn-sm" data-action="copy-meta" title="${escAttr(trText('tooltip.runs.copyJson'))}">${tr('buttons.copy')}</button>
          <a href="${escAttr(url)}" download="${escAttr(fname)}" class="btn btn-secondary btn-sm" title="${escAttr(trText('tooltip.runs.saveMeta'))}">↓ ${tr('buttons.download')}</a>
        </div>
      </div>
      <pre class="json-viewer">${jsonHighlight(pretty)}</pre>`;
    // The JSON payload travels as a JS property, never as markup: embedding
    // JSON.stringify output (unescaped `"` and `<`) in an attribute both
    // breaks the markup and lets attacker-controlled meta content (e.g. a
    // table named `"><img onerror=...>`) execute as stored XSS. The
    // delegated copy-meta action reads it back.
    const copyBtn = $('#modal-body').querySelector('[data-action="copy-meta"]');
    if (copyBtn) copyBtn._payload = pretty;
  } catch(e) {
    $('#modal-body').innerHTML = `<div class="empty-state"><h3>${tr('card.failedToLoad')}</h3><p style="color:var(--danger)">${escHTML(e.message)}</p></div>`;
  }
}

export function jsonHighlight(json) {
  const escaped = escHTML(json);
  return escaped.replace(
    /("(?:\\.|[^"\\])*"(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d+)?(?:[eE][+\-]?\d+)?)/g,
    function(match, _g1, colon) {
      if (match[0] === '"') return colon ? '<span class="json-key">' + match + '</span>' : '<span class="json-str">' + match + '</span>';
      if (match === 'true' || match === 'false') return '<span class="json-bool">' + match + '</span>';
      if (match === 'null') return '<span class="json-null">' + match + '</span>';
      return '<span class="json-num">' + match + '</span>';
    }
  );
}

export async function copyToClipboard(btn, text) {
  try {
    await navigator.clipboard.writeText(text);
    const orig = btn.textContent;
    btn.textContent = trText('toast.copied');
    setTimeout(() => { btn.textContent = orig; }, 1500);
  } catch(e) {
    toast(trText('toast.copyFailed', {error: e.message}), 'error');
  }
}

// Per-DB-type capability matrix for analysis features. "active" means the
// check is wired and meaningful; "n/a" means the engine doesn't model the
// concept (e.g. mongo has no DB-level charset). The matrix is the source of
// truth for what the Analysis Coverage card renders — keep in sync with the
// dumper / analyzer code paths.
export const ANALYSIS_CAPS = {
  postgres: { schema: true, charset: true,  rowCounter: true,  emptyDump: true, sizeDrop: true, scrub: true },
  mysql:    { schema: true, charset: true,  rowCounter: true,  emptyDump: true, sizeDrop: true, scrub: true },
  mariadb:  { schema: true, charset: true,  rowCounter: true,  emptyDump: true, sizeDrop: true, scrub: true },
  mongo:    { schema: true, charset: false, rowCounter: false, emptyDump: true, sizeDrop: true, scrub: true },
  redis:    { schema: true, charset: false, rowCounter: false, emptyDump: true, sizeDrop: true, scrub: true },
};

// Three-state status per check, rendered as a colored badge.
//   active   → check is on for this source (toggle on, DB type supports it)
//   disabled → toggle is off (operator opted out)
//   n/a      → DB type doesn't support this check at all
export function analysisStatus(toggleOn, supported) {
  if (!supported) return { cls: 'badge-pending', label: tr('analysis.status.na'), icon: '—' };
  if (!toggleOn)  return { cls: 'badge-warn',    label: tr('analysis.status.disabled'), icon: '⊘' };
  return                 { cls: 'badge-ok',      label: tr('analysis.status.active'), icon: '✓' };
}

export function renderAnalysisCoverageCard(target) {
  if (!target) return '';
  const caps = ANALYSIS_CAPS[target.DBType] || {};
  const a = target.Analysis || {};
  const analyzerOn = a.analyzerEnabled !== false; // default true
  const emptyOn = a.emptyDumpCheck !== false;     // default true

  // Each row pairs a check with a single status. We tie the analyzer-driven
  // signals (schema/charset/row-drop/size-drop) to the analyzerEnabled
  // toggle because turning the analyzer off skips stats collection entirely
  // — the underlying gauges go absent and no alert can fire.
  const rows = [
    { name: tr('analysis.row.emptyDumpCheck'),
      desc: caps.rowCounter ? tr('analysis.row.emptyDumpDescOn') : tr('analysis.row.emptyDumpDescOff'),
      status: analysisStatus(emptyOn, caps.emptyDump) },
    { name: tr('analysis.row.schemaDrift'),
      desc: tr('analysis.row.schemaDriftDesc'),
      status: analysisStatus(analyzerOn, caps.schema) },
    { name: tr('analysis.row.charsetDrift'),
      desc: tr('analysis.row.charsetDriftDesc'),
      status: analysisStatus(analyzerOn, caps.charset) },
    { name: tr('analysis.row.rowCountDrop'),
      desc: tr('analysis.row.rowCountDropDesc'),
      status: analysisStatus(analyzerOn, caps.rowCounter) },
    { name: tr('analysis.row.dumpSizeCollapse'),
      desc: tr('analysis.row.dumpSizeCollapseDesc'),
      status: analysisStatus(analyzerOn, caps.sizeDrop) },
    { name: tr('analysis.row.storageScrub'),
      desc: tr('analysis.row.storageScrubDesc'),
      status: analysisStatus(true, caps.scrub) },
  ];

  return `
    <div class="table-card">
      <div class="table-card-header"><h2>${tr('target.analysisCoverage')}</h2></div>
      <div style="padding:8px 16px 4px;color:var(--text-muted);font-size:12px">${tr('analysis.subtitle', {dbType: target.DBType})}</div>
      <table>
        <thead><tr>
          <th class="num row-num">#</th>
          <th>${tr('analysis.check')}</th>
          <th>${tr('table.status')}</th>
          <th>${tr('analysis.notes')}</th>
        </tr></thead>
        <tbody>${rows.map((r, i) => `<tr>
          <td class="num row-num">${i + 1}</td>
          <td><strong>${escHTML(r.name)}</strong></td>
          <td><span class="badge ${r.status.cls}">${r.status.icon} ${r.status.label}</span></td>
          <td style="color:var(--text-muted);font-size:12px">${escHTML(r.desc)}</td>
        </tr>`).join('')}</tbody>
      </table>
    </div>`;
}

// Storage-scrub chip rendered under a destination-health badge. Hidden when
// the operator has no scrub data for this (target, destination) pair —
// either because STORAGE_SCRUB_ENABLED is false or because the scrubber
// hasn't run yet. ScrubLastCheck=0 means "never scrubbed".
export function renderScrubChip(h) {
  if (!h || !h.scrubStatus) return '';
  const ok = h.scrubStatus === 'ok';
  const cls = ok ? 'badge-ok' : 'badge-failed';
  const icon = ok ? '&#10003;' : '&#10007;';
  const when = h.scrubLastCheck
    ? ' (' + timeAgo(new Date(h.scrubLastCheck * 1000).toISOString()) + ')'
    : '';
  const tip = ok
    ? 'SHA256 scrub matched meta.json' + when
    : 'SHA256 scrub failed' + (h.scrubFailedTotal ? ' — ' + h.scrubFailedTotal + ' total failures' : '') + when;
  return '<div style="font-size:10px;margin-top:2px"><span class="badge ' + cls + '" style="font-size:9px;padding:1px 4px" title="' + escAttr(tip) + '">' + icon + ' scrub</span></div>';
}

// Run-history "Schema" cell. Combines schema-fingerprint drift and charset
// drift into one column to keep table width sane. The two are independent
// signals (schema = column shape; charset = encoding) but in practice a
// reviewer scanning history wants both at a glance.
export function renderSchemaCharsetCell(r) {
  if (!r.report) return '—';
  const schemaBadge = r.report.schemaChanged
    ? `<span class="badge badge-failed">${tr('schemaCell.changed')}</span>`
    : `<span class="badge badge-ok">${tr('schemaCell.stable')}</span>`;
  const charsetBadge = r.report.charsetChanged
    ? ' <span class="badge badge-warn" style="font-size:10px" title="' + escAttr(trText('tooltip.runs.charsetChanged')) + '">CS</span>'
    : '';
  return schemaBadge + charsetBadge;
}

// Charset / collation row. Renders nothing for runs without recorded encoding
// (mongo, redis, legacy metas) — keeps the detail card uncluttered for DB
// types where the field doesn't apply. Drift indicator pulls from
// report.charsetChanged so it lines up with the BackupCharsetChanged alert.
export function renderCharsetRow(run) {
  if (!run || !run.stats) return '';
  const cs = run.stats.charset || '';
  const co = run.stats.collation || '';
  if (!cs && !co) return '';
  const drift = run.report && run.report.charsetChanged;
  const value = escHTML(cs) + (co ? ' / ' + escHTML(co) : '');
  const badge = drift
    ? '<span class="badge badge-warn" style="margin-left:8px;font-size:10px" title="' + escAttr(trText('tooltip.runs.charsetDrift')) + '">drift</span>'
    : '';
  return `<div class="detail-row"><span class="key">${tr('row.charset')}</span><span class="val" style="font-family:var(--font-mono,monospace);font-size:12px">${value}${badge}</span></div>`;
}

// "Schema unchanged for N days" — leverages meta.schemaChangedAt which is
// carried forward across runs, so a single meta tells you the schema's age.
// Hidden when the field is absent (legacy metas, mongo without schema hash).
export function renderSchemaAgeRow(run) {
  if (!run || !run.schemaChangedAt) return '';
  const t = new Date(run.schemaChangedAt);
  if (isNaN(t.getTime())) return '';
  const days = Math.floor((Date.now() - t.getTime()) / 86400000);
  const label = days <= 0 ? tr('time.today') : days === 1 ? tr('time.oneDay') : tr('time.nDays', {n: days});
  const tip = ' title="' + escAttr(trText('tooltip.runs.schemaChangedAt', {at: t.toISOString()})) + '"';
  return `<div class="detail-row"><span class="key">${tr('schemaAge.label')}</span><span class="val"${tip}>${tr('time.unchangedFor', {label})}</span></div>`;
}

// --- Verification ---
export function renderVerificationBadge(v) {
  if (!v) return '<span style="color:var(--text-muted)">—</span>';
  const clsMap = {
    'match': 'badge-ok',
    'mismatch': 'badge-failed',
    'partial': 'badge-warn',
    'skipped': 'badge-pending'
  };
  const cls = clsMap[v.verdict] || 'badge-pending';
  const labelKey = 'verification.verdictLabel.' + v.verdict;
  const label = tr(labelKey) === labelKey ? (v.verdict || '?') : tr(labelKey);
  const tip = v.summary ? ' title="' + escAttr(v.summary) + '"' : '';
  return `<span class="badge ${cls}"${tip}>${label}</span>`;
}

// renderRestoreVerificationBadge renders the verdict for a single run's
// restoreVerification block (or "—" when absent). Distinct from
// renderVerificationBadge — that one targets the in-stream DumpVerification
// produced during the dump. This one targets the post-upload round-trip
// proof (decrypt → parse / restore against an ephemeral DB pod).
export function renderRestoreVerificationBadge(rv) {
  if (!rv) return '<span style="color:var(--text-muted)">—</span>';
  const clsMap = {
    'match':    'badge-ok',
    'mismatch': 'badge-failed',
    'partial':  'badge-warn',
    'skipped':  'badge-pending'
  };
  const cls = clsMap[rv.verdict] || 'badge-pending';
  const labelKey = 'verification.verdictLabel.' + rv.verdict;
  const label = tr(labelKey) === labelKey ? (rv.verdict || '?') : tr(labelKey);
  const mode = rv.mode ? ' · ' + escHTML(rv.mode) : '';
  const tip = rv.summary ? ' title="' + escAttr(rv.summary) + '"' : '';
  return `<span class="badge ${cls}"${tip}>${label}${mode}</span>`;
}

export function renderRestoreVerificationDetail(run) {
  if (!run || !run.restoreVerification || run.status === 'failed') return '';
  const rv = run.restoreVerification;
  const dur = (rv.durationSeconds != null && !isNaN(rv.durationSeconds))
    ? rv.durationSeconds.toFixed(1) + 's' : '—';
  const completed = rv.completedAt ? new Date(rv.completedAt).toLocaleString() : '—';
  // statsError lives on the run, not on rv: it explains *why* preTables
  // was empty and therefore why a schema-only / sample / full verifier
  // ended up Skipped. Without surfacing it here, the operator sees
  // "Skipped" with no reason and reaches for unrelated toggles.
  const skippedHint = (rv.verdict === 'skipped' && run.statsError)
    ? `<div class="detail-row" style="align-items:flex-start"><span class="key">${tr('target.statsError')}</span><pre class="val" style="color:var(--warn);font-size:12px;white-space:pre-wrap;word-break:break-word;margin:0;background:var(--bg-input);padding:8px;border-radius:4px;max-height:160px;overflow:auto" title="${escAttr(trText('tooltip.runs.statsError'))}">${escHTML(run.statsError)}</pre></div>` : '';
  return `
    <div class="table-card verification-card">
      <div class="table-card-header">
        <h2>${tr('card.restoreVerificationCard')}</h2>
        ${renderRestoreVerificationBadge(rv)}
      </div>
      ${rv.summary ? `<div class="verification-summary">${escHTML(rv.summary)}</div>` : ''}
      <div class="detail-row"><span class="key">${tr('form.source.label.verificationMode')}</span><span class="val">${escHTML(rv.mode || '—')}</span></div>
      <div class="detail-row"><span class="key">${tr('table.completedAt')}</span><span class="val">${completed}</span></div>
      <div class="detail-row"><span class="key">${tr('table.duration')}</span><span class="val">${dur}</span></div>
      ${rv.ephemeralRecipientFingerprint ? `<div class="detail-row"><span class="key">Ephemeral recipient</span><code class="val" style="font-size:11px">${escHTML(rv.ephemeralRecipientFingerprint)}</code></div>` : ''}
      ${skippedHint}
      ${rv.error ? `<div class="detail-row" style="align-items:flex-start"><span class="key">Error</span><pre class="val" style="color:var(--danger);font-size:12px;white-space:pre-wrap;word-break:break-word;margin:0;background:var(--bg-input);padding:8px;border-radius:4px;max-height:160px;overflow:auto">${escHTML(rv.error)}</pre></div>` : ''}
    </div>`;
}

export function renderVerificationDetail(run) {
  if (!run || !run.verification || run.status === 'failed') return '';
  const v = run.verification;
  if (!v.tables || v.tables.length === 0) return '';

  const verdictIcon = { 'match': '&#10003;', 'mismatch': '&#10007;', 'partial': '&#9888;', 'skipped': '—' };
  const verdictCls = { 'match': 'badge-ok', 'mismatch': 'badge-failed', 'partial': 'badge-warn', 'skipped': 'badge-pending' };
  const hasDumpCounts = v.dumpRowCounts && Object.keys(v.dumpRowCounts).length > 0;

  return `
    <div class="table-card verification-card">
      <div class="table-card-header">
        <h2>${tr('card.dumpVerification')}</h2>
        ${renderVerificationBadge(v)}
      </div>
      <div class="verification-summary">${escHTML(v.summary || '')}</div>
      <table>
        <thead><tr>
          <th class="num row-num">#</th>
          <th>${tr('verification.table')}</th>
          <th class="num">${tr('verification.preDumpRows')}</th>
          <th class="num">${tr('verification.postDumpRows')}</th>
          ${hasDumpCounts ? '<th class="num">' + tr('verification.dumpRows') + '</th>' : ''}
          <th>${tr('verification.verdict')}</th>
          <th>${tr('verification.detail')}</th>
        </tr></thead>
        <tbody>${v.tables.map((t, i) => `<tr>
          <td class="num row-num">${i + 1}</td>
          <td style="font-size:12px;font-family:var(--font-mono,monospace)">${escHTML(t.name)}</td>
          <td class="num" style="font-size:12px">${fmtCount(t.preDumpRows)}</td>
          <td class="num" style="font-size:12px">${fmtCount(t.postDumpRows)}</td>
          ${hasDumpCounts ? `<td class="num" style="font-size:12px">${fmtCount(t.dumpRows)}</td>` : ''}
          <td><span class="badge ${verdictCls[t.verdict] || 'badge-pending'}">${verdictIcon[t.verdict] || '?'} ${escHTML(t.verdict)}</span></td>
          <td style="font-size:11px;color:var(--text-muted)">${escHTML(t.detail || '')}</td>
        </tr>`).join('')}</tbody>
      </table>
    </div>`;
}
registerAction('runs-page', (el, e, name, page) => gotoRunHistoryPage(name, parseInt(page, 10) || 0));
registerAction('reload-runs', (el, e, name) => reloadTargetRuns(name));
registerAction('view-meta', (el, e, target, ts, dest) => viewMeta(target, ts, dest || null));
registerAction('copy-meta', el => copyToClipboard(el, el._payload || ''));
registerPage('target', (loading, param) => renderTargetDetail(param, loading));
