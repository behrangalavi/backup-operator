// pages/dashboard.js — Dashboard: hero, charts, targets table, consistency + health matrices.
// Shared helpers ($, tr, api, escHTML, …) come from the classic core.js /
// charts.js scripts via the global scope; cross-page state lives in state.js.
import { state, isStaleRender } from './state.js';
import { currentPage, registerPage } from './router.js';
import { _destStatsInFlight, refreshDestStats } from './destinations.js';
import { fmtNextRun } from './jobs.js';
import { renderScrubChip } from './target.js';

// --- Dashboard ---
// Cached slow-probe results so SSE-triggered re-renders don't re-fire the
// probes on every refresh tick. TTL of 60 s comfortably outlives the SSE
// broadcast cadence (10 s) and Bridge's typical page-navigation pattern;
// stale-by-up-to-a-minute destination-health is fine for a dashboard.
// A separate _slowFetchInFlight guard prevents the "kick off probe → not
// done yet → next refresh kicks off another probe in parallel" storm
// that pinned an unreachable destination's 8 s timeout on every render.
export const SLOW_PROBE_TTL_MS = 60000;
export let _slowFetchInFlight = false;

// Last-good cache for the dashboard's fast Promise.all so a transient
// API failure during an SSE-triggered refresh doesn't reset the stats
// cards to 0/0/0/0/0 and yank the targets table away. Each entry is
// keyed by URL because all three fast endpoints share the same
// rejection behaviour. Updated only on a successful response.
export const _fastDataCache = { '/api/targets': null, '/api/destinations': null, '/api/jobs': null };
export async function fastDataAllSettled(urls) {
  const results = await Promise.allSettled(urls.map(u => api(u)));
  return urls.map((u, i) => {
    if (results[i].status === 'fulfilled' && results[i].value != null) {
      _fastDataCache[u] = results[i].value;
      return results[i].value;
    }
    // Failed or null — fall back to last good (may itself be null on
    // a cold start, which renderers handle via `|| []`).
    return _fastDataCache[u];
  });
}

// chartOrLoading is the standard pattern for chart cards on the
// dashboard: while the underlying data is being fetched AND nothing
// is cached yet, render a spinner in place of the empty-state. Once
// any data exists (even stale), render that data — a background
// refresh can update it later via SSE without yanking the chart away.
// A separate chart-refreshing dot in the header signals "fresher data
// on the way" when refresh is in-flight but stale data is shown.
export let _fleetSummaryInFlight = false;

export function renderTargetSparkline(targetName) {
  const row = (state.fleetSummary.heatmap || []).find(r => r.target === targetName);
  if (!row || !row.days || row.days.length === 0) return '';
  const days = row.days.slice(-7);
  const cellW = 8, cellH = 12, gap = 2;
  const W = days.length * (cellW + gap) - gap;
  const colorByStatus = {
    ok:     'var(--success)',
    failed: 'var(--danger)',
    mixed:  'var(--warning)',
    none:   'var(--bg-input)',
  };
  const cells = days.map((c, i) => {
    const x = i * (cellW + gap);
    const fill = colorByStatus[c.status] || colorByStatus.none;
    const tt = `${c.day}: ${c.runs > 0 ? c.runs + ' run(s), ' + c.status : 'no run'}`;
    return `<rect x="${x}" y="0" width="${cellW}" height="${cellH}" rx="2" fill="${fill}"><title>${escAttr(tt)}</title></rect>`;
  }).join('');
  return `<svg viewBox="0 0 ${W} ${cellH}" width="${W}" height="${cellH}" class="target-sparkline" aria-label="7-day status">${cells}</svg>`;
}

export async function refreshFleetSummary() {
  if (_fleetSummaryInFlight) return;
  _fleetSummaryInFlight = true;
  // Don't poison the cache on transient API failure. The previous
  // `.catch(() => ({}))` pattern wrote empty arrays AND updated
  // lastFetch, so the next 60 s of renders showed empty charts even
  // though the actual data was unchanged. Preserve the last good
  // values; leaving lastFetch alone means the next user-initiated
  // render retries immediately instead of waiting out the full TTL.
  //
  // CRITICAL: the post-fetch renderDashboard call MUST happen after
  // _fleetSummaryInFlight is reset, or chartOrLoading sees
  // (empty data + inFlight=true) and renders a permanent spinner.
  let success = false;
  try {
    let r;
    try {
      r = await api('/api/dashboard/heatmap?days=30');
    } catch(e) {
      return;
    }
    if (!r || typeof r !== 'object') return;
    state.fleetSummary = {
      heatmap:           r.heatmap           || [],
      storage:           r.storage           || [],
      anomalies:         r.anomalies         || [],
      durations:         r.durations         || [],
      verificationDaily: r.verificationDaily || [],
      lastFetch:         Date.now(),
    };
    success = true;
  } finally {
    _fleetSummaryInFlight = false;
  }
  if (success && currentPage() === 'dashboard') renderDashboard(false);
}
export async function refreshSlowProbes() {
  if (_slowFetchInFlight) return;
  _slowFetchInFlight = true;
  // Same render-after-inFlight-reset rule as refreshFleetSummary —
  // otherwise the post-fetch repaint sees inFlight=true and shows
  // a stuck loading state.
  let success = false;
  try {
    // Promise.allSettled so a transient failure on ONE endpoint
    // doesn't blow away the other's good response. If both failed,
    // preserve the previous cache entirely.
    const [hRes, cRes] = await Promise.allSettled([
      api('/api/destination-health'),
      api('/api/consistency-check'),
    ]);
    const hOk = hRes.status === 'fulfilled';
    const cOk = cRes.status === 'fulfilled';
    if (!hOk && !cOk) return;
    state.slowProbes = {
      health:      hOk ? (hRes.value || []) : state.slowProbes.health,
      consistency: cOk ? (cRes.value || []) : state.slowProbes.consistency,
      lastFetch:   Date.now(),
    };
    success = true;
  } finally {
    _slowFetchInFlight = false;
  }
  if (success && currentPage() === 'dashboard') renderDashboard(false);
}

// renderTargetSparkline shows the target's last 7 daily statuses as
// a compact strip of 7 coloured cells. Sourced from the fleet
// heatmap (last 7 entries of the 30-day row). Returns '' if the
// target has no row in the heatmap yet — that's the "brand-new
// source, no runs" case. Inline SVG so it sits next to the target
// name without disturbing the table layout.
export async function renderDashboard(loading = true) {
  const _gen = state.renderGen;
  if (loading) showLoading();
  let targets = [], dests = [], jobs = [];
  // Render as soon as the fast K8s-API calls return. The slow endpoints
  // (destination-health, consistency-check) dial every storage backend;
  // an unreachable destination would otherwise stall the dashboard
  // behind its 8 s probe timeout × N destinations on every refresh.
  // fastDataAllSettled falls back to the last good value per URL so a
  // transient failure on /api/jobs doesn't reset stats to 0/0/0/0/0.
  const got = await fastDataAllSettled(['/api/targets', '/api/destinations', '/api/jobs']);
  targets = got[0] || [];
  dests   = got[1] || [];
  jobs    = got[2] || [];

  const healthEntries = state.slowProbes.health;
  const consistencyIssues = state.slowProbes.consistency;

  // Kick off a slow-probe refresh only on user-initiated renders (page
  // navigation, manual reload) when the cache is stale. SSE-driven
  // re-renders (loading=false) reuse whatever's cached — otherwise an
  // unreachable destination's 8 s timeout × N dests would fire every
  // 10 s SSE tick.
  if (loading && Date.now() - state.slowProbes.lastFetch > SLOW_PROBE_TTL_MS) refreshSlowProbes();
  if (loading && Date.now() - state.fleetSummary.lastFetch > SLOW_PROBE_TTL_MS) refreshFleetSummary();
  // Storage Donut on the dashboard reads state.destStatsCache, which was
  // only refreshed by the destinations page — a user landing directly
  // on /dashboard saw a permanently empty donut. Trigger the refresh
  // from here too; the in-flight guard prevents duplicate fetches if
  // both pages were ever rendered in quick succession.
  if (loading && Date.now() - state.destStatsCache.lastFetch > SLOW_PROBE_TTL_MS) refreshDestStats();

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

  // User navigated away while we were awaiting — drop the write so
  // we don't clobber the page they're actually looking at now.
  if (isStaleRender(_gen)) return;

  content.innerHTML = `
    <div class="page-header">
      <div><h1>${tr('page.dashboard.title')}</h1><div class="subtitle">${tr('page.dashboard.subtitle')}</div></div>
    </div>
    ${renderHeroPanel(targets, dests, jobs)}
    <div class="chart-grid-2">
      <div class="chart-card">
        <h3>${tr('chart.storageDonut.title')}${refreshingDot(_destStatsInFlight, state.destStatsCache.stats.length > 0)} <span class="chart-card-sub">${tr('chart.storageDonut.sub')}</span></h3>
        ${chartOrLoading(state.destStatsCache.stats, _destStatsInFlight, () => renderStorageDonut(state.destStatsCache.stats))}
      </div>
      <div class="chart-card">
        <h3>${tr('chart.nextRunGantt.title')} <span class="chart-card-sub">${tr('chart.nextRunGantt.sub')}</span></h3>
        ${renderNextRunGantt(targets)}
      </div>
    </div>
    <div class="chart-card" style="margin-bottom:16px">
      <h3>${tr('chart.fleetHeatmap.title')}${refreshingDot(_fleetSummaryInFlight, state.fleetSummary.heatmap.length > 0)} <span class="chart-card-sub">${tr('chart.fleetHeatmap.sub')}</span></h3>
      ${chartOrLoading(state.fleetSummary.heatmap, _fleetSummaryInFlight, () => renderFleetHeatmap(state.fleetSummary.heatmap))}
    </div>
    <div class="chart-grid-2">
      <div class="chart-card">
        <h3>${tr('chart.storageGrowth.title')}${refreshingDot(_fleetSummaryInFlight, state.fleetSummary.storage.length > 0)} <span class="chart-card-sub">${tr('chart.storageGrowth.sub')}</span></h3>
        ${chartOrLoading(state.fleetSummary.storage, _fleetSummaryInFlight, () => renderStorageGrowth(state.fleetSummary.storage))}
      </div>
      <div class="chart-card">
        <h3>${tr('chart.anomalyStream.title')}${refreshingDot(_fleetSummaryInFlight, state.fleetSummary.anomalies.length > 0)} <span class="chart-card-sub">${tr('chart.anomalyStream.sub')}</span></h3>
        ${chartOrLoading(state.fleetSummary.anomalies, _fleetSummaryInFlight, () => renderAnomalyStream(state.fleetSummary.anomalies))}
      </div>
    </div>
    <div class="chart-grid-2">
      <div class="chart-card">
        <h3>${tr('chart.durationDist.title')}${refreshingDot(_fleetSummaryInFlight, state.fleetSummary.durations.length > 0)} <span class="chart-card-sub">${tr('chart.durationDist.sub')}</span></h3>
        ${chartOrLoading(state.fleetSummary.durations, _fleetSummaryInFlight, () => renderDurationDistribution(state.fleetSummary.durations))}
      </div>
      <div class="chart-card">
        <h3>${tr('chart.verifyTrend.title')}${refreshingDot(_fleetSummaryInFlight, state.fleetSummary.verificationDaily.length > 0)} <span class="chart-card-sub">${tr('chart.verifyTrend.sub')}</span></h3>
        ${chartOrLoading(state.fleetSummary.verificationDaily, _fleetSummaryInFlight, () => renderVerificationTrend(state.fleetSummary.verificationDaily))}
      </div>
    </div>
    ${renderStorageByDestination(targets, dests)}
    <div class="table-card">
      <div class="table-card-header">
        <h2>${tr('page.dashboard.targets')}</h2>
        <button class="btn btn-primary btn-sm" data-action="add-source-from-dashboard" title="${escAttr(trText('tooltip.sources.add'))}">+ ${tr('buttons.addSource')}</button>
      </div>
      ${targets.length === 0 ? `<div class="empty-state"><h3>${tr('page.dashboard.noTargets')}</h3><p>${tr('page.dashboard.noTargetsHint')}</p></div>` : `
      <table>
        <thead><tr>
          <th class="num row-num">#</th>
          <th class="sortable" ${sortableTh('dashboard','name')}>${tr('table.target')}${sortIndicator('dashboard','name')}</th>
          <th class="sortable" ${sortableTh('dashboard','dbType')}>${tr('table.type')}${sortIndicator('dashboard','dbType')}</th>
          <th class="sortable" ${sortableTh('dashboard','schedule')}>${tr('table.schedule')}${sortIndicator('dashboard','schedule')}</th>
          <th class="sortable" ${sortableTh('dashboard','status')}>${tr('table.status')}${sortIndicator('dashboard','status')}</th>
          <th class="sortable" ${sortableTh('dashboard','lastRun')}>${tr('table.lastRun')}${sortIndicator('dashboard','lastRun')}</th>
          <th class="num sortable" ${sortableTh('dashboard','size')}>${tr('table.size')}${sortIndicator('dashboard','size')}</th>
          <th class="sortable" ${sortableTh('dashboard','createdAt')}>${tr('table.createdAt')}${sortIndicator('dashboard','createdAt')}</th>
          <th>${tr('table.destinations')}</th><th></th>
        </tr></thead>
        <tbody>${sortedTargets.map((t, i) => `<tr>
          <td class="num row-num">${i + 1}</td>
          <td>
            <a href="${escAttr(targetHref(t.Name))}" style="color:var(--accent);font-weight:600">${escHTML(t.Name)}</a>
            <div class="target-sparkline-wrap" title="${escAttr(tr('hero.sparkline.title') || 'last 7 days')}">${renderTargetSparkline(t.Name)}</div>
          </td>
          <td><span class="badge badge-${t.DBType}">${t.DBType}</span></td>
          <td>
            <code style="font-size:12px;background:var(--bg-input);padding:2px 6px;border-radius:4px">${escHTML(t.Schedule)}</code>${t.Suspended ? ` <span class="badge badge-warn" style="margin-left:4px" title="${escAttr(trText('tooltip.sources.paused'))}">${tr('badge.paused')}</span>` : ''}
            ${!t.Suspended && t.nextRun ? `<div style="font-size:11px;color:var(--text-muted);margin-top:2px">${escHTML(fmtNextRun(t.nextRun))}</div>` : ''}
          </td>
          <td>${t.Latest ? (t.Latest.status === 'failed'
            ? failedBadge(t.Latest)
            : `<span class="badge badge-ok">${tr('badge.ok')}</span>`)
            : `<span class="badge badge-pending">${tr('badge.noRuns')}</span>`}</td>
          <td style="color:var(--text-muted);font-size:12px">${t.Latest ? timeAgo(t.Latest.timestamp) : tr('time.never')}</td>
          <td class="num" style="font-size:12px">${t.Latest && !t.Latest.status?.includes('fail') ? humanBytes(t.Latest.encryptedSizeBytes) : '—'}</td>
          <td style="color:var(--text-muted);font-size:12px">${t.CreatedAt ? timeAgo(t.CreatedAt) : '—'}</td>
          <td>${(t.Destinations || []).map(d => `<span class="badge badge-sftp" style="margin:1px">${escHTML(d)}</span>`).join('')}</td>
          <td style="white-space:nowrap">
            <button class="btn btn-ghost btn-sm" ${actionAttrs('trigger-backup', t.Name)} title="${escAttr(trText('tooltip.sources.run'))}">&#9654;</button>
            ${t.Suspended
              ? `<button class="btn btn-ghost btn-sm" style="color:var(--success)" ${actionAttrs('toggle-suspend', t.ObjectName, t.Name, 'false')} title="${escAttr(trText('tooltip.sources.resume'))}">&#9655;&#9655;</button>`
              : `<button class="btn btn-ghost btn-sm" style="color:var(--warning)" ${actionAttrs('toggle-suspend', t.ObjectName, t.Name, 'true')} title="${escAttr(trText('tooltip.sources.pause'))}">&#10074;&#10074;</button>`}
            <button class="btn btn-ghost btn-sm" ${actionAttrs('open-source-form', t.ObjectName)} title="${escAttr(trText('tooltip.sources.edit'))}">&#9998;</button>
            <button class="btn btn-ghost btn-sm" style="color:var(--danger)" ${actionAttrs('delete-source', t.ObjectName, t.Name)} title="${escAttr(trText('tooltip.sources.delete'))}">&#10005;</button>
          </td>
        </tr>`).join('')}</tbody>
      </table>`}
    </div>
    ${consistencyIssues.length > 0 ? `
    <div class="table-card" style="border-left:3px solid var(--danger)">
      <div class="table-card-header"><h2 style="color:var(--danger)">${tr('page.dashboard.consistency')}</h2></div>
      <p style="padding:0 16px;color:var(--text-muted);font-size:13px;margin:0 0 8px">${tr('consistency.hint')}</p>
      <table>
        <thead><tr><th class="num row-num">#</th><th>${tr('table.target')}</th><th>${tr('table.timestamp')}</th><th>${tr('table.presentIn')}</th><th>${tr('table.missingFrom')}</th></tr></thead>
        <tbody>${consistencyIssues.slice(0, 20).map((ci, i) => `<tr>
          <td class="num row-num">${i + 1}</td>
          <td><strong>${escHTML(ci.target)}</strong></td>
          <td style="font-size:12px">${escHTML(ci.timestamp)}</td>
          <td>${(ci.presentIn||[]).map(d => `<span class="badge badge-ok" style="margin:1px">${escHTML(d)}</span>`).join('')}</td>
          <td>${(ci.missingFrom||[]).map(d => `<span class="badge badge-failed" style="margin:1px">${escHTML(d)}</span>`).join('')}</td>
        </tr>`).join('')}</tbody>
      </table>
      ${consistencyIssues.length > 20 ? `<p style="padding:8px 16px;color:var(--text-muted);font-size:12px">${tr('consistency.andMore', {count: consistencyIssues.length - 20})}</p>` : ''}
    </div>` : ''}
    ${healthEntries.length > 0 && dests.length > 1 ? `
    <div class="table-card">
      <div class="table-card-header"><h2>${tr('page.dashboard.health')}</h2></div>
      <table>
        <thead><tr>
          <th class="num row-num">#</th>
          <th>${tr('table.target')}</th>
          ${[...new Set(healthEntries.map(h => h.destination))].map(d => `<th style="text-align:center">${escHTML(d)}</th>`).join('')}
        </tr></thead>
        <tbody>${(() => {
          const destNames = [...new Set(healthEntries.map(h => h.destination))];
          const targetNames = [...new Set(healthEntries.map(h => h.target))];
          const lookup = {};
          healthEntries.forEach(h => { lookup[h.target + '@' + h.destination] = h; });
          return targetNames.map((t, i) => `<tr>
            <td class="num row-num">${i + 1}</td>
            <td><a href="${escAttr(targetHref(t))}" style="color:var(--accent);font-weight:600">${escHTML(t)}</a></td>
            ${destNames.map(d => {
              const h = lookup[t + '@' + d];
              if (!h) return '<td style="text-align:center"><span class="badge" style="background:var(--bg-input);color:var(--text-muted)">N/A</span></td>';
              const badge = h.status === 'ok' ? 'badge-ok' : h.status === 'failed' ? 'badge-failed' : h.status === 'missing' ? 'badge-pending' : 'badge-failed';
              const label = h.status === 'ok' ? tr('badge.ok') : h.status === 'failed' ? tr('badge.failed') : h.status === 'missing' ? tr('badge.noData') : tr('badge.unreachable');
              const tip = h.error ? ' title="' + escAttr(h.error) + '"' : '';
              return '<td style="text-align:center"><span class="badge ' + badge + '"' + tip + '>' + label + '</span>' +
                (h.latestRun ? '<div style="font-size:10px;color:var(--text-muted)">' + timeAgo(h.latestRun) + '</div>' : '') +
                renderScrubChip(h) + '</td>';
            }).join('')}
          </tr>`).join('');
        })()}</tbody>
      </table>
    </div>` : ''}`;

  // Animate hero counters from the previously-displayed value to the
  // new one. _heroLastShown holds what was actually on screen before
  // the re-render — without that cache, every SSE tick wiped the
  // data-value attribute and tweened 0→N over and over. Computed
  // values mirror the renderHeroPanel logic above.
  const buckets24h = buildHourlyActivity(jobs);
  const targets24h = {
    runs24h:      buckets24h.reduce((s, b) => s + b.ok + b.failed + b.running, 0),
    failed24h:    buckets24h.reduce((s, b) => s + b.failed, 0),
    runningNow:   jobs.filter(j => j.status === 'running').length,
    destinations: dests.length,
  };
  for (const key of Object.keys(targets24h)) {
    animateCounter($(`[data-counter="${key}"]`), targets24h[key]);
    _heroLastShown[key] = targets24h[key];
  }
}
registerPage('dashboard', loading => renderDashboard(loading));
