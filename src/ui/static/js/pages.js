// pages.js — Page renderers (dashboard, sources, destinations, alerts, jobs, audit, target detail, settings, age keys)
'use strict';

// --- Dashboard ---
// Cached slow-probe results so SSE-triggered re-renders don't re-fire the
// probes on every refresh tick. TTL of 60 s comfortably outlives the SSE
// broadcast cadence (10 s) and Bridge's typical page-navigation pattern;
// stale-by-up-to-a-minute destination-health is fine for a dashboard.
// A separate _slowFetchInFlight guard prevents the "kick off probe → not
// done yet → next refresh kicks off another probe in parallel" storm
// that pinned an unreachable destination's 8 s timeout on every render.
const SLOW_PROBE_TTL_MS = 60000;
let _slowProbes = { health: [], consistency: [], lastFetch: 0 };
let _slowFetchInFlight = false;

// Last-good cache for the dashboard's fast Promise.all so a transient
// API failure during an SSE-triggered refresh doesn't reset the stats
// cards to 0/0/0/0/0 and yank the targets table away. Each entry is
// keyed by URL because all three fast endpoints share the same
// rejection behaviour. Updated only on a successful response.
const _fastDataCache = { '/api/targets': null, '/api/destinations': null, '/api/jobs': null };
async function fastDataAllSettled(urls) {
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
let _fleetSummary = {
  heatmap: [], storage: [], anomalies: [],
  durations: [], verificationDaily: [],
  lastFetch: 0,
};
let _fleetSummaryInFlight = false;

function renderTargetSparkline(targetName) {
  const row = (_fleetSummary.heatmap || []).find(r => r.target === targetName);
  if (!row || !row.days || row.days.length === 0) return '';
  const days = row.days.slice(-7);
  const cellW = 8, cellH = 12, gap = 2;
  const W = days.length * (cellW + gap) - gap;
  const colorByStatus = {
    ok:     'var(--success, #10b981)',
    failed: 'var(--danger, #ef4444)',
    mixed:  'var(--warning, #f59e0b)',
    none:   'var(--bg-input, #2a2a2a)',
  };
  const cells = days.map((c, i) => {
    const x = i * (cellW + gap);
    const fill = colorByStatus[c.status] || colorByStatus.none;
    const tt = `${c.day}: ${c.runs > 0 ? c.runs + ' run(s), ' + c.status : 'no run'}`;
    return `<rect x="${x}" y="0" width="${cellW}" height="${cellH}" rx="2" fill="${fill}"><title>${escAttr(tt)}</title></rect>`;
  }).join('');
  return `<svg viewBox="0 0 ${W} ${cellH}" width="${W}" height="${cellH}" class="target-sparkline" aria-label="7-day status">${cells}</svg>`;
}

async function refreshFleetSummary() {
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
    _fleetSummary = {
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
async function refreshSlowProbes() {
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
    _slowProbes = {
      health:      hOk ? (hRes.value || []) : _slowProbes.health,
      consistency: cOk ? (cRes.value || []) : _slowProbes.consistency,
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
async function renderDashboard(loading = true) {
  const _gen = _renderGen;
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

  const healthEntries = _slowProbes.health;
  const consistencyIssues = _slowProbes.consistency;

  // Kick off a slow-probe refresh only on user-initiated renders (page
  // navigation, manual reload) when the cache is stale. SSE-driven
  // re-renders (loading=false) reuse whatever's cached — otherwise an
  // unreachable destination's 8 s timeout × N dests would fire every
  // 10 s SSE tick.
  if (loading && Date.now() - _slowProbes.lastFetch > SLOW_PROBE_TTL_MS) refreshSlowProbes();
  if (loading && Date.now() - _fleetSummary.lastFetch > SLOW_PROBE_TTL_MS) refreshFleetSummary();
  // Storage Donut on the dashboard reads _destStatsCache, which was
  // only refreshed by the destinations page — a user landing directly
  // on /dashboard saw a permanently empty donut. Trigger the refresh
  // from here too; the in-flight guard prevents duplicate fetches if
  // both pages were ever rendered in quick succession.
  if (loading && Date.now() - _destStatsCache.lastFetch > SLOW_PROBE_TTL_MS) refreshDestStats();

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
        <h3>${tr('chart.storageDonut.title')}${refreshingDot(_destStatsInFlight, _destStatsCache.stats.length > 0)} <span class="chart-card-sub">${tr('chart.storageDonut.sub')}</span></h3>
        ${chartOrLoading(_destStatsCache.stats, _destStatsInFlight, () => renderStorageDonut(_destStatsCache.stats))}
      </div>
      <div class="chart-card">
        <h3>${tr('chart.nextRunGantt.title')} <span class="chart-card-sub">${tr('chart.nextRunGantt.sub')}</span></h3>
        ${renderNextRunGantt(targets)}
      </div>
    </div>
    <div class="chart-card" style="margin-bottom:16px">
      <h3>${tr('chart.fleetHeatmap.title')}${refreshingDot(_fleetSummaryInFlight, _fleetSummary.heatmap.length > 0)} <span class="chart-card-sub">${tr('chart.fleetHeatmap.sub')}</span></h3>
      ${chartOrLoading(_fleetSummary.heatmap, _fleetSummaryInFlight, () => renderFleetHeatmap(_fleetSummary.heatmap))}
    </div>
    <div class="chart-grid-2">
      <div class="chart-card">
        <h3>${tr('chart.storageGrowth.title')}${refreshingDot(_fleetSummaryInFlight, _fleetSummary.storage.length > 0)} <span class="chart-card-sub">${tr('chart.storageGrowth.sub')}</span></h3>
        ${chartOrLoading(_fleetSummary.storage, _fleetSummaryInFlight, () => renderStorageGrowth(_fleetSummary.storage))}
      </div>
      <div class="chart-card">
        <h3>${tr('chart.anomalyStream.title')}${refreshingDot(_fleetSummaryInFlight, _fleetSummary.anomalies.length > 0)} <span class="chart-card-sub">${tr('chart.anomalyStream.sub')}</span></h3>
        ${chartOrLoading(_fleetSummary.anomalies, _fleetSummaryInFlight, () => renderAnomalyStream(_fleetSummary.anomalies))}
      </div>
    </div>
    <div class="chart-grid-2">
      <div class="chart-card">
        <h3>${tr('chart.durationDist.title')}${refreshingDot(_fleetSummaryInFlight, _fleetSummary.durations.length > 0)} <span class="chart-card-sub">${tr('chart.durationDist.sub')}</span></h3>
        ${chartOrLoading(_fleetSummary.durations, _fleetSummaryInFlight, () => renderDurationDistribution(_fleetSummary.durations))}
      </div>
      <div class="chart-card">
        <h3>${tr('chart.verifyTrend.title')}${refreshingDot(_fleetSummaryInFlight, _fleetSummary.verificationDaily.length > 0)} <span class="chart-card-sub">${tr('chart.verifyTrend.sub')}</span></h3>
        ${chartOrLoading(_fleetSummary.verificationDaily, _fleetSummaryInFlight, () => renderVerificationTrend(_fleetSummary.verificationDaily))}
      </div>
    </div>
    ${renderStorageByDestination(targets, dests)}
    <div class="table-card">
      <div class="table-card-header">
        <h2>${tr('page.dashboard.targets')}</h2>
        <button class="btn btn-primary btn-sm" onclick="location.hash='#/sources';openSourceForm()" title="Create a new backup source — opens a form that generates a labelled Kubernetes Secret">+ ${tr('buttons.addSource')}</button>
      </div>
      ${targets.length === 0 ? `<div class="empty-state"><h3>${tr('page.dashboard.noTargets')}</h3><p>${tr('page.dashboard.noTargetsHint')}</p></div>` : `
      <table>
        <thead><tr>
          <th class="num row-num">#</th>
          <th class="sortable" onclick="toggleSort('dashboard','name')">${tr('table.target')}${sortIndicator('dashboard','name')}</th>
          <th class="sortable" onclick="toggleSort('dashboard','dbType')">${tr('table.type')}${sortIndicator('dashboard','dbType')}</th>
          <th class="sortable" onclick="toggleSort('dashboard','schedule')">${tr('table.schedule')}${sortIndicator('dashboard','schedule')}</th>
          <th class="sortable" onclick="toggleSort('dashboard','status')">${tr('table.status')}${sortIndicator('dashboard','status')}</th>
          <th class="sortable" onclick="toggleSort('dashboard','lastRun')">${tr('table.lastRun')}${sortIndicator('dashboard','lastRun')}</th>
          <th class="num sortable" onclick="toggleSort('dashboard','size')">${tr('table.size')}${sortIndicator('dashboard','size')}</th>
          <th class="sortable" onclick="toggleSort('dashboard','createdAt')">${tr('table.createdAt')}${sortIndicator('dashboard','createdAt')}</th>
          <th>${tr('table.destinations')}</th><th></th>
        </tr></thead>
        <tbody>${sortedTargets.map((t, i) => `<tr>
          <td class="num row-num">${i + 1}</td>
          <td>
            <a href="#/target/${escAttr(t.Name)}" style="color:var(--accent);font-weight:600">${escHTML(t.Name)}</a>
            <div class="target-sparkline-wrap" title="${escAttr(tr('hero.sparkline.title') || 'last 7 days')}">${renderTargetSparkline(t.Name)}</div>
          </td>
          <td><span class="badge badge-${t.DBType}">${t.DBType}</span></td>
          <td>
            <code style="font-size:12px;background:var(--bg-input);padding:2px 6px;border-radius:4px">${escHTML(t.Schedule)}</code>${t.Suspended ? ` <span class="badge badge-warn" style="margin-left:4px" title="Scheduled runs are paused; manual triggers still work">${tr('badge.paused')}</span>` : ''}
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
            <button class="btn btn-ghost btn-sm" onclick="triggerBackup('${escJS(t.Name)}')" title="Trigger a manual backup run now (creates a one-off Job from the CronJob template)">&#9654;</button>
            ${t.Suspended
              ? `<button class="btn btn-ghost btn-sm" style="color:var(--success)" onclick="toggleSourceSuspend('${escJS(t.SecretName)}','${escJS(t.Name)}',false)" title="Resume scheduled runs">&#9655;&#9655;</button>`
              : `<button class="btn btn-ghost btn-sm" style="color:var(--warning)" onclick="toggleSourceSuspend('${escJS(t.SecretName)}','${escJS(t.Name)}',true)" title="Pause scheduled runs — sets backup.mogenius.io/suspended=true. Existing dumps kept; manual triggers still work.">&#10074;&#10074;</button>`}
            <button class="btn btn-ghost btn-sm" onclick="openSourceForm('${escJS(t.SecretName)}')" title="Edit this source's connection details and schedule">&#9998;</button>
            <button class="btn btn-ghost btn-sm" style="color:var(--danger)" onclick="deleteSource('${escJS(t.SecretName)}','${escJS(t.Name)}')" title="Delete this source — the CronJob is cascaded; existing dumps in storage are kept">&#10005;</button>
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
            <td><a href="#/target/${escAttr(t)}" style="color:var(--accent);font-weight:600">${escHTML(t)}</a></td>
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


// --- Sources ---
let _sourcesLastGood = null;
async function renderSources(loading = true) {
  const _gen = _renderGen;
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
        <button class="btn btn-primary" onclick="openSourceForm()" title="Create a new backup source — opens a form that generates a labelled Kubernetes Secret">+ ${tr('buttons.addSource')}</button>
      </div>
    </div>
    ${targets.length === 0 ? `
    <div class="empty-state">
      <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M21 12c0 1.66-4 3-9 3s-9-1.34-9-3"/><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"/></svg>
      <h3>${tr('page.sources.empty')}</h3>
      <p>${tr('page.sources.emptyHint')}</p>
      <button class="btn btn-primary" onclick="openSourceForm()" title="Create your first backup source">+ ${tr('buttons.addSource')}</button>
    </div>` : `
    <div style="display:grid;grid-template-columns:repeat(auto-fill,minmax(340px,1fr));gap:16px">
      ${sortedTargets.map(t => `
      <div class="detail-card" style="cursor:pointer" onclick="location.hash='#/target/${escJS(t.Name)}'">
        <div style="display:flex;justify-content:space-between;align-items:start;margin-bottom:12px">
          <div>
            <div style="font-weight:600;font-size:15px;color:var(--text-heading)">${escHTML(t.Name)}</div>
            <span class="badge badge-${t.DBType}" style="margin-top:6px">${t.DBType}</span>
            ${t.Suspended ? `<span class="badge badge-warn" style="margin-top:6px;margin-left:4px" title="Scheduled runs are paused; manual triggers still work">${tr('badge.paused')}</span>` : ''}
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
          <button class="btn btn-ghost btn-sm" onclick="event.stopPropagation();triggerBackup('${escJS(t.Name)}')" title="Trigger a manual backup run now (creates a one-off Job from the CronJob template; pause-state ignored for manual triggers)">&#9654; ${tr('buttons.run')}</button>
          ${t.Suspended
            ? `<button class="btn btn-ghost btn-sm" style="color:var(--success)" onclick="event.stopPropagation();toggleSourceSuspend('${escJS(t.SecretName)}','${escJS(t.Name)}',false)" title="Resume scheduled runs — the reconciler clears Spec.Suspend on the managed CronJob within seconds">${tr('buttons.resume')}</button>`
            : `<button class="btn btn-ghost btn-sm" style="color:var(--warning)" onclick="event.stopPropagation();toggleSourceSuspend('${escJS(t.SecretName)}','${escJS(t.Name)}',true)" title="Pause scheduled runs — sets backup.mogenius.io/suspended=true on the source Secret. Existing dumps and config are kept; manual Run still works.">${tr('buttons.pause')}</button>`}
          <button class="btn btn-ghost btn-sm" onclick="event.stopPropagation();openSourceForm('${escJS(t.SecretName)}')" title="Edit this source's connection details and schedule">${tr('common.edit')}</button>
          <button class="btn btn-ghost btn-sm" style="color:var(--danger)" onclick="event.stopPropagation();deleteSource('${escJS(t.SecretName)}','${escJS(t.Name)}')" title="Delete this source — the managed CronJob is removed via OwnerReference; existing dumps in storage are kept">${tr('common.delete')}</button>
        </div>
      </div>`).join('')}
    </div>`}`;
}

// --- Source Form ---

// Lazy-loaded cluster capabilities. Cached for the page session so
// every source-form open does not pay a SubjectAccessReview round-trip.
// Cleared by the SSE 'refresh' tick path is unnecessary — RBAC changes
// require a Helm upgrade and a page reload anyway.
let _clusterCapabilitiesCache = null;
async function getClusterCapabilities() {
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
function isPhase2VerificationMode(mode) {
  return mode === 'schema-only' || mode === 'sample' || mode === 'full';
}

// Render the warning banner inside the open source form when the user
// picks Phase-2 but the cluster currently won't allow it. We do not
// block save — the source secret is still useful (RBAC may flip on
// later, or the user may know the schedule won't run until a Helm
// upgrade lands). The banner just stops the silent "pods is forbidden"
// dead end.
function refreshPhase2RBACWarning(formEl, caps) {
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

window.openSourceForm = function(secretName) {
  const isEdit = !!secretName;
  const title = isEdit ? tr('form.source.editTitle') : tr('form.source.createTitle');

  let formHTML = `<form id="sourceForm" onsubmit="submitSourceForm(event, '${secretName || ''}')">
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
      <button type="button" class="btn btn-secondary" onclick="closeModal()" title="Discard changes and close this dialog">${tr('common.cancel')}</button>
      <button type="submit" class="btn btn-primary" title="${isEdit ? 'Save the modified source Secret — the operator reconciles changes within seconds' : 'Create the source Secret — the operator generates a CronJob within seconds'}">${isEdit ? tr('common.update') : tr('common.create')}</button>
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
    api('/api/sources/' + secretName).then(src => {
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
    }).catch(e => toast(tr('toast.loadFailed', {error: e.message}), 'error'));
  }
};

// Renders a checkbox list of all destinations into #destinationsPicker and
// keeps the hidden `destinations` input in sync as a comma-separated string.
// `currentValue` is the saved annotation string ("name1,name2"). Names that
// are present in the saved value but no longer match any known destination
// are kept as disabled "(missing)" entries so a simple Update doesn't silently
// drop the allow-list.
function loadDestinationsPicker(currentValue) {
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
      rows.push(`<label class="ms-missing" title="This destination is in the saved allow-list but no destination Secret with that logical name currently exists.">
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
    picker.innerHTML = `<div class="multi-select-empty">Failed to load destinations: ${escHTML(e.message)}</div>`;
  });
}

window.submitSourceForm = async function(e, secretName) {
  e.preventDefault();
  const f = e.target;
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
    if (secretName) {
      await api('/api/sources/' + secretName, { method: 'PUT', body: JSON.stringify(body) });
      toast(tr('toast.sourceUpdated'), 'success');
    } else {
      await api('/api/sources', { method: 'POST', body: JSON.stringify(body) });
      toast(tr('toast.sourceCreated'), 'success');
    }
    closeModal();
    renderPage(currentPage());
  } catch(e) {
    toast(e.message, 'error');
  }
};

window.deleteSource = function(secretName, displayName) {
  openModal(tr('modal.delete.title', {name: displayName}), `
    <div class="confirm-text">${tr('modal.delete.warning')}</div>
    <div class="form-actions">
      <button class="btn btn-secondary" onclick="closeModal()" title="Keep this source — close without deleting">${tr('common.cancel')}</button>
      <button class="btn btn-danger" onclick="confirmDeleteSource('${secretName}')" title="Permanently delete the source Secret. The CronJob is cascaded via OwnerReference; existing dumps in storage remain.">${tr('common.delete')}</button>
    </div>`);
};

window.confirmDeleteSource = async function(secretName) {
  try {
    await api('/api/sources/' + secretName, { method: 'DELETE' });
    toast(tr('toast.sourceDeleted'), 'success');
    closeModal();
    location.hash = '#/sources';
  } catch(e) { toast(e.message, 'error'); }
};

// Cached stats so SSE refreshes don't refire the per-destination probes.
// Same shape as _slowProbes on the dashboard. 60 s TTL + in-flight guard
// for the same reason — see the SLOW_PROBE_TTL_MS comment above.
let _destStatsCache = { stats: [], lastFetch: 0 };
let _destStatsInFlight = false;
async function refreshDestStats() {
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
    _destStatsCache = { stats: s || [], lastFetch: Date.now() };
    success = true;
  } finally {
    _destStatsInFlight = false;
  }
  if (success && (currentPage() === 'destinations' || currentPage() === 'dashboard')) {
    renderPage(currentPage(), false);
  }
}


// --- Destinations ---
let _destsLastGood = null;
async function renderDestinations(loading = true) {
  const _gen = _renderGen;
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
  const stats = _destStatsCache.stats;
  if (loading && Date.now() - _destStatsCache.lastFetch > SLOW_PROBE_TTL_MS) refreshDestStats();
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
        <button class="btn btn-primary" onclick="openDestForm()" title="Create a new storage destination (SFTP or S3-compatible) for backup uploads">+ ${tr('buttons.addDestination')}</button>
      </div>
    </div>
    ${dests.length === 0 ? `
    <div class="empty-state">
      <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/></svg>
      <h3>${tr('page.destinations.empty')}</h3>
      <p>${tr('page.destinations.emptyHint')}</p>
      <button class="btn btn-primary" onclick="openDestForm()" title="Create your first storage destination">+ ${tr('buttons.addDestination')}</button>
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
          <span class="dest-status" id="dest-status-${escAttr(d.secretName)}"></span>
        </div>
        <div class="detail-row"><span class="key">Secret</span><code class="val">${escHTML(d.secretName)}</code></div>
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
          <button class="btn btn-ghost btn-sm" onclick="testDestConnection('${escJS(d.secretName)}','${escJS(d.name)}')" title="Probe this destination — verifies SSH/SFTP login or S3 bucket access without uploading anything">&#128268; ${tr('buttons.test')}</button>
          <button class="btn btn-ghost btn-sm" onclick="openDestForm('${escJS(d.secretName)}')" title="Edit this destination's credentials and connection details">${tr('common.edit')}</button>
          <button class="btn btn-ghost btn-sm" style="color:var(--danger)" onclick="deleteDest('${escJS(d.secretName)}','${escJS(d.name)}')" title="Delete this destination Secret. Existing dumps stored at this destination remain intact.">${tr('common.delete')}</button>
        </div>
      </div>`;
      }).join('')}
    </div>`}`;
}

// --- Destination Form ---
window.openDestForm = function(secretName) {
  const isEdit = !!secretName;
  const title = isEdit ? tr('form.destination.editTitle') : tr('form.destination.createTitle');

  const sftpFields = `
    <div class="form-row"><div class="form-group"><label>${tr('form.destination.label.host')} *</label><input name="data_host" required></div>
      <div class="form-group"><label>${tr('form.destination.label.port')}</label><input name="data_port" placeholder="22"></div></div>
    <div class="form-group"><label>${tr('form.destination.label.username')} *</label><input name="data_username" required></div>
    <div class="form-group"><label>${tr('form.destination.label.authMethod')} *</label>
      <div style="display:flex;gap:16px;margin-top:4px">
        <label style="font-weight:normal;cursor:pointer"><input type="radio" name="sftpAuthMethod" value="key" checked onchange="toggleSFTPAuth(this.value)"> ${tr('form.destination.label.authMethodKey')}</label>
        <label style="font-weight:normal;cursor:pointer"><input type="radio" name="sftpAuthMethod" value="password" onchange="toggleSFTPAuth(this.value)"> ${tr('form.destination.label.authMethodPassword')}</label>
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

  const s3Fields = `
    <div class="form-group"><label>${tr('form.destination.label.endpoint')} *</label><input name="data_endpoint" required placeholder="s3.amazonaws.com"></div>
    <div class="form-row"><div class="form-group"><label>${tr('form.destination.label.bucket')} *</label><input name="data_bucket" required></div>
      <div class="form-group"><label>${tr('form.destination.label.region')}</label><input name="data_region" placeholder="${tr('form.destination.placeholder.region')}"></div></div>
    <div class="form-row"><div class="form-group"><label>${tr('form.destination.label.accessKey')}</label><input name="data_access-key"></div>
      <div class="form-group"><label>${tr('form.destination.label.secretKey')}</label><input name="data_secret-key" type="password"></div></div>`;

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
    <div class="form-group"><label>${tr('form.destination.label.accountKey')} *</label><input name="data_account-key" type="password" required></div>
    <div class="form-group"><label>${tr('form.destination.label.container')} *</label><input name="data_container" required></div>`;

  const gcsFields = `
    <div class="form-group"><label>${tr('form.destination.label.bucket')} *</label><input name="data_bucket" required></div>
    <div class="form-group"><label>${tr('form.destination.label.serviceAccountJson')} *</label>
      <textarea name="data_service-account-json" rows="4" required placeholder='{"type":"service_account",...}'></textarea>
      <div class="hint">${tr('form.destination.hint.serviceAccountJson')}</div>
    </div>`;

  openModal(title, `<form id="destForm" onsubmit="submitDestForm(event, '${secretName || ''}')">
    <div class="form-row">
      <div class="form-group"><label>${tr('common.name')} *</label><input name="name" required placeholder="${tr('form.destination.placeholder.name')}" ${isEdit ? 'disabled' : ''}></div>
      <div class="form-group"><label>${tr('form.destination.label.type')} *</label>
        <select name="storageType" required onchange="toggleDestFields(this.value)">
          <option value="">${tr('form.source.placeholder.selectType')}</option>
          <option value="sftp">${tr('form.destination.type.sftp')}</option>
          <option value="hetzner-sftp">${tr('form.destination.type.hetznerSftp')}</option>
          <option value="ftps">${tr('form.destination.type.ftps')}</option>
          <option value="s3">${tr('form.destination.type.s3')}</option>
          <option value="azure">${tr('form.destination.type.azure')}</option>
          <option value="gcs">${tr('form.destination.type.gcs')}</option>
        </select></div>
    </div>
    <div class="form-group"><label>${tr('form.destination.label.pathPrefix')}</label><input name="pathPrefix" placeholder="${tr('form.destination.placeholder.pathPrefix')}"></div>
    <div id="destTypeFields"></div>
    <div class="form-actions">
      <button type="button" class="btn btn-secondary" onclick="closeModal()" title="Discard changes and close this dialog">${tr('common.cancel')}</button>
      <button type="submit" class="btn btn-primary" title="${isEdit ? 'Save the modified destination Secret — the operator picks up changes on the next run' : 'Create the destination Secret — sources can target it via the destinations annotation or pick it up on next run if the source has no allow-list'}">${isEdit ? tr('common.update') : tr('common.create')}</button>
    </div>
  </form>
  <div id="destSFTPTemplate" style="display:none">${sftpFields}</div>
  <div id="destS3Template" style="display:none">${s3Fields}</div>
  <div id="destFTPSTemplate" style="display:none">${ftpsFields}</div>
  <div id="destAzureTemplate" style="display:none">${azureFields}</div>
  <div id="destGCSTemplate" style="display:none">${gcsFields}</div>`);

  if (isEdit) {
    api('/api/destinations/' + secretName).then(d => {
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
        if (d.storageType === 'sftp' || d.storageType === 'hetzner-sftp') {
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
    }).catch(e => toast(tr('toast.loadFailed', {error: e.message}), 'error'));
  }
};

window.toggleDestFields = function(type) {
  const container = $('#destTypeFields');
  const templates = {
    'sftp': '#destSFTPTemplate', 'hetzner-sftp': '#destSFTPTemplate',
    's3': '#destS3Template', 'ftps': '#destFTPSTemplate',
    'azure': '#destAzureTemplate', 'gcs': '#destGCSTemplate',
  };
  const tpl = templates[type];
  container.innerHTML = tpl ? $(tpl).innerHTML : '';
};

// Show only the input matching the selected SFTP auth method. The hidden
// input is left in the DOM so the user can flip back without re-typing
// during a single form session; submitDestForm drops the unselected
// method's value before sending so we don't ship both up the wire.
window.toggleSFTPAuth = function(method) {
  $$('[data-sftp-auth]').forEach(el => {
    el.style.display = el.getAttribute('data-sftp-auth') === method ? '' : 'none';
  });
};

// Sensitive fields are masked as *** on edit, so an empty form value
// means "user did not retype it, keep the stored secret" rather than
// "user cleared it". Non-sensitive fields (host, port, known-hosts, …)
// follow the opposite rule: empty after edit means "user actually
// emptied it, remove the field from the Secret" — otherwise the form
// silently ignores clears and the user wonders why the old value is
// stuck. Mirror of the backend's sensitiveKeys set in handlers_api.go.
const DEST_SENSITIVE_KEYS = new Set([
  'password', 'ssh-private-key', 'secret-key', 'access-key', 'secret-access-key',
]);

window.submitDestForm = async function(e, secretName) {
  e.preventDefault();
  const f = e.target;
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
    } else if (secretName && !DEST_SENSITIVE_KEYS.has(key)) {
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
  if (sType === 'sftp' || sType === 'hetzner-sftp') {
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
    if (secretName) {
      await api('/api/destinations/' + secretName, { method: 'PUT', body: JSON.stringify(body) });
      toast(tr('toast.destinationUpdated'), 'success');
    } else {
      await api('/api/destinations', { method: 'POST', body: JSON.stringify(body) });
      toast(tr('toast.destinationCreated'), 'success');
    }
    closeModal();
    renderPage(currentPage());
  } catch(e) { toast(e.message, 'error'); }
};

window.testDestConnection = async function(secretName, displayName) {
  const el = document.getElementById('dest-status-' + secretName);
  if (el) { el.innerHTML = `<span class="badge badge-pending">${tr('badge.testing')}</span>`; }
  try {
    // User-initiated probe: the server bounds it at 15 s (upload+readback),
    // so allow more than the default client timeout before giving up.
    const result = await api('/api/destinations/' + secretName + '/test', { method: 'POST', timeoutMs: 30000 });
    if (result.ok) {
      if (el) el.innerHTML = `<span class="badge badge-ok">${tr('badge.connected')}</span>`;
      toast(displayName + ': ' + tr('badge.ok'), 'success');
    } else {
      if (el) el.innerHTML = `<span class="badge badge-failed" title="${escAttr(result.error || '')}">${tr('badge.failed')}</span>`;
      toast(displayName + ': ' + (result.error || tr('badge.failed')), 'error');
    }
  } catch(e) {
    if (el) el.innerHTML = `<span class="badge badge-failed">${tr('badge.error')}</span>`;
    toast(tr('toast.loadFailed', {error: e.message}), 'error');
  }
};

window.deleteDest = function(secretName, displayName) {
  openModal(tr('modal.delete.title', {name: displayName}), `
    <div class="confirm-text">${tr('modal.delete.warning')}</div>
    <div class="form-actions">
      <button class="btn btn-secondary" onclick="closeModal()" title="Keep this destination — close without deleting">${tr('common.cancel')}</button>
      <button class="btn btn-danger" onclick="confirmDeleteDest('${secretName}')" title="Permanently delete the destination Secret. Sources will skip it on next run; existing dumps in storage remain.">${tr('common.delete')}</button>
    </div>`);
};

window.confirmDeleteDest = async function(secretName) {
  try {
    await api('/api/destinations/' + secretName, { method: 'DELETE' });
    toast(tr('toast.destinationDeleted'), 'success');
    closeModal();
    renderDestinations();
  } catch(e) { toast(e.message, 'error'); }
};

// --- Alert descriptions for each alert type ---
// Icons stay static (universal emoji); titles, desc and action come from
// the active dictionary so a language switch updates the text without
// shipping per-language icons.
const ALERT_ICONS = {
  BackupOverdue: '⏰',
  BackupDestinationFailing: '💾',
  BackupDumpSizeCollapsed: '📉',
  BackupSchemaChanged: '🔧',
  BackupAnomaliesAppearing: '⚠️',
  BackupLastRunFailed: '❌',
  BackupSucceeded: '🟢',
  BackupOperatorTestAlert: '🧪',
};
const ALERT_DESCRIPTIONS = ALERT_ICONS; // legacy alias used by the rules-reference table

function getAlertDescription(alertname) {
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
async function renderAlerts(loading = true) {
  const _gen = _renderGen;
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
  let setupGuide = '';
  if (statusResp && !statusResp.prometheus.configured && !statusResp.alertmanager.configured) {
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
    ? '<button class="btn btn-secondary btn-sm" onclick="sendTestAlert()" id="btnTestAlert">🧪 ' + tr('buttons.sendTest') + '</button>'
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
        <button class="btn btn-secondary btn-sm" onclick="renderAlerts(false)">↻ ${tr('buttons.refresh')}</button>
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
            <td><span class="badge badge-${a.source === 'prometheus' ? 'ok' : 'pending'}" title="${a.source === 'prometheus' ? 'From Prometheus — honors rule for: duration' : 'Local heuristic — fires immediately, no for: debounce'}">${escHTML(a.source || '?')}</span></td>
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
            <td><span class="badge badge-${name === 'BackupDumpSizeCollapsed' ? 'critical' : name === 'BackupSchemaChanged' || name === 'BackupSucceeded' ? 'info' : 'warning'}">${name === 'BackupDumpSizeCollapsed' ? 'critical' : name === 'BackupSchemaChanged' || name === 'BackupSucceeded' ? 'info' : 'warning'}</span></td>
            <td style="font-size:12px">${escHTML(info.desc)}</td>
            <td style="font-size:12px;color:var(--text-muted)">${escHTML(info.action)}</td>
          </tr>`;
          }).join('')}
        </tbody>
      </table>
    </div>`;
}

// Send test alert to Alertmanager
window.sendTestAlert = async function() {
  const btn = document.getElementById('btnTestAlert');
  if (btn) { btn.disabled = true; btn.textContent = tr('common.loading'); }
  try {
    const resp = await api('/api/alerts/test', { method: 'POST' });
    toast(resp.message || tr('toast.testAlertSent'), 'success');
  } catch(e) {
    toast(e.message || tr('toast.saveFailed', {error: e.message}), 'error');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = '🧪 ' + tr('buttons.sendTest'); }
  }
};

// updateAlertsPill keeps the sidebar counter in sync with the latest /api/alerts
// response. We call it after every alert refresh and once on startup; the SSE
// refresh tick triggers periodic re-fetches when the user is on other pages
// so the pill stays current without polling.
function updateAlertsPill(counts) {
  const el = $('#navAlertsPill');
  if (!el) return;
  const total = (counts.critical || 0) + (counts.warning || 0) + (counts.info || 0);
  if (total === 0) { el.hidden = true; return; }
  el.hidden = false;
  el.textContent = total;
  el.className = 'alerts-pill ' + (counts.critical > 0 ? 'critical' : counts.warning > 0 ? 'warning' : 'info');
}

async function refreshAlertsPill() {
  try {
    const resp = await api('/api/alerts');
    if (resp && resp.counts) updateAlertsPill(resp.counts);
  } catch(_) { /* pill stays as-is on error */ }
}


// --- Jobs ---
let jobProgressTimer = null;


// fmtNextRun renders a CronJob's next fire time as "in 4h 32m (02:00)"
// or "in 12d (Mon 02:00)" depending on horizon. Pass an ISO timestamp
// string from the target's NextRun field. Returns '' if input is falsy
// or already in the past (next tick is imminent / SSE catch-up race).
function fmtNextRun(iso) {
  if (!iso) return '';
  const t = new Date(iso).getTime();
  if (!isFinite(t)) return '';
  const sec = (t - Date.now()) / 1000;
  if (sec <= 0) return tr('target.nextRunImminent') || 'imminent';
  const d = new Date(t);
  // Inside 24h: relative + "at HH:MM" today/tomorrow
  if (sec < 86400) {
    const hhmm = d.toLocaleTimeString([], {hour: '2-digit', minute: '2-digit'});
    return `${tr('target.in') || 'in'} ${fmtDurationShort(sec)} (${hhmm})`;
  }
  // >24h: show day-of-week + time + relative duration
  const dayShort = d.toLocaleDateString([], {weekday: 'short'});
  const hhmm = d.toLocaleTimeString([], {hour: '2-digit', minute: '2-digit'});
  return `${tr('target.in') || 'in'} ${fmtDurationShort(sec)} (${dayShort} ${hhmm})`;
}

// renderProgressCell builds the Duration cell content. For non-running jobs
// it returns the static formatted duration. For running jobs it renders a
// progress bar driven by elapsed time and an estimate from past runs (when
// available); without an estimate, it shows elapsed-only.
function renderProgressCell(j) {
  if (j.status !== 'running') return j.duration || '—';
  const startMs = parseTsRFC(j.startTime);
  if (!startMs) return '—';
  const elapsed = (Date.now() - startMs) / 1000;
  const est = j.estimatedDurationSeconds || 0;
  const sample = j.estimateSampleSize || 0;
  if (est > 0) {
    const ratio = Math.min(elapsed / est, 0.99);
    const overdue = elapsed > est;
    const remaining = Math.max(est - elapsed, 0);
    const fillCls = overdue ? 'job-progress-fill overdue' : 'job-progress-fill';
    const labelCls = overdue ? 'job-progress-label overdue' : 'job-progress-label';
    const label = overdue
      ? tr('target.progressOverdue', {elapsed: fmtDurationShort(elapsed), est: fmtDurationShort(est), n: sample})
      : tr('target.progressNormal', {elapsed: fmtDurationShort(elapsed), est: fmtDurationShort(est), remaining: fmtDurationShort(remaining), n: sample});
    return `<div class="job-progress">
      <div class="job-progress-bar"><div class="${fillCls}" style="width:${(ratio * 100).toFixed(1)}%"></div></div>
      <div class="${labelCls}">${escHTML(label)}</div>
    </div>`;
  }
  return `<span style="color:var(--text-muted)">${escHTML(tr('target.progressNoEstimate', {elapsed: fmtDurationShort(elapsed)}))}</span>`;
}

let _jobsLastGood = null;
async function renderJobs(loading = true) {
  const _gen = _renderGen;
  if (loading) showLoading();
  let jobs = [];
  try {
    jobs = (await api('/api/jobs')) || [];
    _jobsLastGood = jobs;
  } catch(e) {
    if (_jobsLastGood) { jobs = _jobsLastGood; }
    else { toast(e.message, 'error'); }
  }

  const jobGetters = {
    name:      j => j.name || '',
    target:    j => (j.target || '').toLowerCase(),
    status:    j => j.status || '',
    startTime: j => parseTsRFC(j.startTime),
    duration:  j => parseDurationSec(j.duration),
  };
  const js = sortState.jobs;
  const sortedJobs = sortBy(jobs, jobGetters[js.col] || jobGetters.startTime, js.dir);

  if (isStaleRender(_gen)) return;

  content.innerHTML = `
    <div class="page-header">
      <div><h1>${tr('page.jobs.title')}</h1><div class="subtitle">${tr('page.jobs.subtitle')}</div></div>
    </div>
    <div class="table-card">
      ${jobs.length === 0 ? `<div class="empty-state"><h3>${tr('page.jobs.empty')}</h3><p>Jobs appear when backups run — either on schedule or triggered manually.</p></div>` : `
      <table>
        <thead><tr>
          <th class="num row-num">#</th>
          <th class="sortable" onclick="toggleSort('jobs','name')">${tr('nav.jobs')}${sortIndicator('jobs','name')}</th>
          <th class="sortable" onclick="toggleSort('jobs','target')">${tr('table.target')}${sortIndicator('jobs','target')}</th>
          <th class="sortable" onclick="toggleSort('jobs','status')">${tr('table.status')}${sortIndicator('jobs','status')}</th>
          <th class="sortable" onclick="toggleSort('jobs','startTime')">Started${sortIndicator('jobs','startTime')}</th>
          <th class="sortable" onclick="toggleSort('jobs','duration')">${tr('table.duration')}${sortIndicator('jobs','duration')}</th>
        </tr></thead>
        <tbody>${sortedJobs.map((j, i) => `<tr data-job-name="${escAttr(j.name)}">
          <td class="num row-num">${i + 1}</td>
          <td style="font-family:ui-monospace,monospace;font-size:12px">${escHTML(j.name)}</td>
          <td><strong>${escHTML(j.target || '—')}</strong></td>
          <td><span class="badge badge-${j.status}">${j.status}</span></td>
          <td style="color:var(--text-muted);font-size:12px">${j.startTime ? new Date(j.startTime).toLocaleString() : '—'}</td>
          <td class="job-duration-cell" style="font-size:12px">${renderProgressCell(j)}</td>
        </tr>`).join('')}</tbody>
      </table>`}
    </div>`;

  // Tick progress bars locally between SSE refreshes so they advance smoothly
  // without re-fetching /api/jobs every second.
  if (jobProgressTimer) { clearInterval(jobProgressTimer); jobProgressTimer = null; }
  const runningJobs = sortedJobs.filter(j => j.status === 'running');
  if (runningJobs.length > 0) {
    jobProgressTimer = setInterval(() => {
      // If user navigated away, the cells will be gone; guard against null.
      runningJobs.forEach(j => {
        const row = document.querySelector(`tr[data-job-name="${CSS.escape(j.name)}"] .job-duration-cell`);
        if (row) row.innerHTML = renderProgressCell(j);
      });
    }, 1000);
  }
}


// --- Audit log ---
let auditFilter = 'all';
let _auditLastGood = null;
async function renderAudit(loading = true) {
  const _gen = _renderGen;
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
        <button class="btn btn-secondary btn-sm" onclick="renderAudit(true)" title="Re-fetch the events list from Kubernetes">&#8635; ${tr('buttons.refresh')}</button>
      </div>
    </div>
    <div style="display:flex;gap:6px;flex-wrap:wrap;margin-bottom:16px">
      ${categories.map(([cat, label, tip]) => `
        <button class="btn btn-${auditFilter === cat ? 'primary' : 'ghost'} btn-sm"
          onclick="setAuditFilter('${cat}')" title="${escAttr(tip)}">${label}</button>
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

window.setAuditFilter = function(cat) {
  auditFilter = cat;
  renderAudit(true);
};

function sortRuns(runs) {
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


// --- Target detail run-history cache ---
// Run history is the only storage-probing fetch on the detail page (the
// backend probes each destination, up to 12 s apiece). Blocking the page
// render on it made two things bad: (1) clicking a source showed nothing
// for many seconds even though the summary cards come from the fast
// /api/targets read, and (2) a language switch — a purely presentational
// re-render — re-fired the slow probe. This mirrors the dashboard's
// stale-while-revalidate caches (_fleetSummary / _slowProbes): render the
// summary immediately, fill the runs region from cache, and only re-probe
// storage when the cache is for a different target, expired, or explicitly
// invalidated by an SSE event. `runs == null` means "never loaded yet";
// `failed` records a probe error so the region can show a retry affordance
// instead of a misleading "No runs".
const RUN_HISTORY_TTL_MS = 30000;
let _targetRuns = { name: '', runs: null, lastFetch: 0, failed: false };
const _targetRunsInFlight = new Set(); // keyed by target name; dedups SSE-tick storms

// fetchTargetRuns refreshes the run-history cache for `name` in the
// background and repaints ONLY the runs region when done — never the whole
// page, so an in-flight probe can't yank the summary away. Per-name
// in-flight guard so rapid target switches each get their own slot while
// repeated events for the same target are deduped.
async function fetchTargetRuns(name) {
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
    // Keep any previously cached runs for this target; just flag the
    // failure so the region offers a retry rather than claiming emptiness.
    if (_targetRuns.name === name) _targetRuns.failed = true;
    else _targetRuns = { name, runs: null, lastFetch: 0, failed: true };
  } else {
    _targetRuns = { name, runs, lastFetch: Date.now(), failed: false };
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
window.reloadTargetRuns = function(name) {
  if (_targetRuns.name === name) { _targetRuns.lastFetch = 0; _targetRuns.failed = false; }
  fetchTargetRuns(name); // sets in-flight synchronously before we repaint
  const region = document.getElementById('target-runs-region');
  if (region) {
    const target = (_fastDataCache['/api/targets'] || []).find(t => t.Name === name);
    region.innerHTML = targetRunsRegionHTML(name, target);
  }
};

function runHistoryShell(inner) {
  return `<div class="table-card"><div class="table-card-header"><h2>${tr('target.runHistory')}</h2></div>${inner}</div>`;
}

// targetRunsRegionHTML renders the runs-dependent part of the detail page
// (size/heatmap charts, tables card, run-history table) from the cached
// _targetRuns state. Crucially it distinguishes three states a flat runs
// array can't: in-flight load, probe failure, and a genuinely empty
// history — so a slow/unreachable destination shows a spinner or a retry
// prompt, never a misleading "No runs".
function targetRunsRegionHTML(name, target) {
  const cached = _targetRuns.name === name ? _targetRuns : null;
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
        <button class="btn btn-secondary btn-sm" onclick="reloadTargetRuns('${escJS(name)}')">${tr('common.retry')}</button></div>`);
    }
    return runHistoryShell(`<div class="empty-state"><p>${tr('target.noRuns')}</p></div>`);
  }

  return `
    <div class="chart-grid-2">
      <div class="chart-card">
        <h3>${tr('target.dumpSizeTrend')}</h3>
        ${renderSizeChart(runs)}
      </div>
      <div class="chart-card">
        <h3>${tr('target.runStatusHeatmap')}</h3>
        ${renderStatusHeatmap(runs)}
      </div>
    </div>
    ${renderTablesCard(runs)}
    <div class="table-card">
      <div class="table-card-header"><h2>${tr('target.runHistory')}</h2></div>
      <table>
        <thead><tr>
          <th class="num row-num">#</th>
          <th class="sortable" onclick="toggleSort('runs','timestamp')">${tr('table.timestamp')}${sortIndicator('runs','timestamp')}</th>
          <th class="sortable" onclick="toggleSort('runs','status')">${tr('table.status')}${sortIndicator('runs','status')}</th>
          <th class="num sortable" onclick="toggleSort('runs','size')">${tr('table.size')}${sortIndicator('runs','size')}</th>
          <th>${tr('table.destinations')}</th>
          <th class="sortable" onclick="toggleSort('runs','verification')">${tr('table.verification')}${sortIndicator('runs','verification')}</th>
          <th class="sortable" onclick="toggleSort('runs','restoreVerification')">${tr('target.restoreLabel')}${sortIndicator('runs','restoreVerification')}</th>
          <th class="sortable" onclick="toggleSort('runs','schema')">Schema${sortIndicator('runs','schema')}</th>
          <th class="num sortable" onclick="toggleSort('runs','tables')">${tr('verification.table')}${sortIndicator('runs','tables')}</th>
          <th class="sortable" onclick="toggleSort('runs','anomalies')">${tr('table.anomalies')} / ${tr('table.error')}${sortIndicator('runs','anomalies')}</th>
          <th>${tr('row.download')}</th>
        </tr></thead>
        <tbody>${sortRuns(runs).map((r, i) => `<tr>
          <td class="num row-num">${i + 1}</td>
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
      </table>
    </div>`;
}

async function renderTargetDetail(name, loading = true) {
  const _gen = _renderGen;
  // Empty name shouldn't reach here from a normal click — hash is
  // `#/target/<name>` and currentParam returns the second segment. If
  // we got here with no name, the hash was malformed (e.g. someone
  // typed `#/target/` by hand). Show an explicit error rather than
  // silently bouncing to Dashboard, so the user can see what went
  // wrong instead of "I clicked and ended up somewhere else".
  if (!name) {
    if (loading) showLoading();
    if (isStaleRender(_gen)) return;
    content.innerHTML = `<div class="empty-state"><h3>Missing target name in URL</h3>
      <p>Hash was <code>${escAttr(location.hash)}</code> — expected <code>#/target/&lt;name&gt;</code>.</p>
      <a href="#/" class="btn btn-secondary">Back to Dashboard</a></div>`;
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
      <a href="#/" class="btn btn-secondary" title="Return to the Dashboard">Back to Dashboard</a></div>`;
    return;
  }

  // Run history: stale-while-revalidate (see _targetRuns above). Kick a
  // background refresh only when the cache is for a different target,
  // expired, or invalidated by SSE — NOT on a language switch or sort,
  // which reuse the cache and so re-render instantly. fetchTargetRuns sets
  // its in-flight flag synchronously, so the region below shows a spinner.
  const runsFresh = _targetRuns.name === name && !_targetRuns.failed &&
    (Date.now() - _targetRuns.lastFetch < RUN_HISTORY_TTL_MS);
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
        <button class="btn btn-secondary btn-sm" onclick="triggerBackup('${escJS(name)}')" title="Trigger a manual backup run for this target now — creates a one-off Job from the CronJob template">&#9654; ${tr('buttons.runNow')}</button>
        <button class="btn btn-secondary btn-sm" onclick="openSourceForm('${escJS(target.SecretName)}')" title="Edit this source's connection details and schedule">${tr('common.edit')}</button>
        <button class="btn btn-danger btn-sm" onclick="deleteSource('${escJS(target.SecretName)}','${escJS(name)}')" title="Permanently delete this source. The CronJob is cascaded; existing dumps remain in storage.">${tr('common.delete')}</button>
      </div>
    </div>
    ${runningJob ? `
    <div class="detail-card running-banner" data-job-name="${escAttr(runningJob.name)}" style="margin-bottom:16px;border-left:3px solid var(--accent,#3b82f6)">
      <h3 style="display:flex;align-items:center;gap:8px">
        <span class="badge badge-running">${tr('badge.running')}</span>
        ${tr('target.runningTitle') || 'Backup läuft gerade'}
      </h3>
      <div class="job-progress-host">${renderProgressCell(runningJob)}</div>
      <div style="font-size:11px;color:var(--text-muted);margin-top:6px">
        ${tr('table.startTime') || 'Started'}: ${new Date(runningJob.startTime).toLocaleString()} · Job: <code>${escHTML(runningJob.name)}</code>
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
  if (jobProgressTimer) { clearInterval(jobProgressTimer); jobProgressTimer = null; }
  if (runningJob) {
    jobProgressTimer = setInterval(() => {
      const host = document.querySelector(`.running-banner[data-job-name="${CSS.escape(runningJob.name)}"] .job-progress-host`);
      if (host) host.innerHTML = renderProgressCell(runningJob);
    }, 1000);
  }
}

function renderDownloadLinks(targetName, run, destNames) {
  const ts = escHTML(run.timestamp);
  const successDests = run.destinations ? run.destinations.filter(d => d.status === 'success') : [];
  if (successDests.length <= 1) {
    const destName = successDests.length === 1 ? successDests[0].name : '';
    const destParam = destName ? '?destination=' + encodeURIComponent(destName) : '';
    const destArg  = destName ? `'${escHTML(destName)}'` : 'null';
    return `<button class="btn btn-ghost btn-sm" style="font-size:11px" onclick="viewMeta('${escJS(targetName)}','${ts}',${destArg})" title="View the unencrypted meta.json sidecar (table stats, schema fingerprint, SHA256) in the browser">&#128065;</button>
      <a href="/download/${escAttr(targetName)}/${ts}/meta${destParam}" download="${escAttr(targetName)}-${ts}.meta.json" class="btn btn-ghost btn-sm" style="font-size:11px" title="Download the unencrypted meta.json sidecar">.json</a>
      <a href="/download/${escAttr(targetName)}/${ts}/dump${destParam}" class="btn btn-ghost btn-sm" style="font-size:11px" title="Download the age-encrypted dump (.sql.gz.age). Decrypt locally with backup-restore + your offline private key.">.age</a>`;
  }
  return `<div class="dropdown" style="display:inline-block">
    <button class="btn btn-ghost btn-sm" style="font-size:11px" onclick="this.nextElementSibling.classList.toggle('open')" title="This run was uploaded to multiple destinations — pick one to download or view from">Download &#9662;</button>
    <div class="dropdown-menu">${successDests.map(d =>
      `<a href="/download/${escAttr(targetName)}/${ts}/dump?destination=${encodeURIComponent(d.name)}" class="dropdown-item" style="font-size:12px" title="Download the age-encrypted dump from ${escAttr(d.name)}">
        ${escHTML(d.name)} <span style="opacity:0.6;font-size:10px">(${d.storageType})</span>
      </a>`
    ).join('')}
    <hr style="margin:4px 0;border:none;border-top:1px solid var(--border)">
    ${successDests.map(d =>
      `<button class="dropdown-item" style="font-size:11px;text-align:left;background:none;border:none;width:100%;cursor:pointer" onclick="viewMeta('${escJS(targetName)}','${ts}','${escJS(d.name)}')" title="View the meta.json sidecar from ${escAttr(d.name)} in the browser">
        &#128065; view: ${escHTML(d.name)}
      </button>`
    ).join('')}
    ${successDests.map(d =>
      `<a href="/download/${escAttr(targetName)}/${ts}/meta?destination=${encodeURIComponent(d.name)}" download="${escAttr(targetName)}-${ts}.meta.json" class="dropdown-item" style="font-size:11px;opacity:0.7" title="Download the meta.json sidecar from ${escAttr(d.name)}">
        meta: ${escHTML(d.name)}
      </a>`
    ).join('')}
    </div>
  </div>`;
}

// --- JSON viewer ---
window.viewMeta = async function(targetName, timestamp, destination) {
  const url = '/download/' + encodeURIComponent(targetName) + '/' + encodeURIComponent(timestamp) + '/meta' +
    (destination ? '?destination=' + encodeURIComponent(destination) : '');
  openModal(targetName + ' — ' + timestamp, '<div class="empty-state"><div class="spinner"></div></div>');
  try {
    const resp = await fetch(url);
    if (!resp.ok) throw new Error('HTTP ' + resp.status);
    const data = await resp.json();
    const pretty = JSON.stringify(data, null, 2);
    const fname = `${targetName}-${timestamp}.meta.json`;
    $('#modal-body').innerHTML = `
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:12px;gap:12px;flex-wrap:wrap">
        <span style="color:var(--text-muted);font-size:12px">${destination ? 'destination: <code>' + escHTML(destination) + '</code>' : ''}</span>
        <div style="display:flex;gap:6px">
          <button class="btn btn-ghost btn-sm" onclick="copyToClipboard(this, ${JSON.stringify(JSON.stringify(data, null, 2))})" title="Copy the full pretty-printed JSON to clipboard">Copy</button>
          <a href="${url}" download="${escAttr(fname)}" class="btn btn-secondary btn-sm" title="Save this meta.json as a file">↓ Download</a>
        </div>
      </div>
      <pre class="json-viewer">${jsonHighlight(pretty)}</pre>`;
  } catch(e) {
    $('#modal-body').innerHTML = `<div class="empty-state"><h3>${tr('card.failedToLoad')}</h3><p style="color:var(--danger)">${escHTML(e.message)}</p></div>`;
  }
};

function jsonHighlight(json) {
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

window.copyToClipboard = async function(btn, text) {
  try {
    await navigator.clipboard.writeText(text);
    const orig = btn.textContent;
    btn.textContent = 'Copied!';
    setTimeout(() => { btn.textContent = orig; }, 1500);
  } catch(e) {
    toast('Copy failed: ' + e.message, 'error');
  }
};

// Per-DB-type capability matrix for analysis features. "active" means the
// check is wired and meaningful; "n/a" means the engine doesn't model the
// concept (e.g. mongo has no DB-level charset). The matrix is the source of
// truth for what the Analysis Coverage card renders — keep in sync with the
// dumper / analyzer code paths.
const ANALYSIS_CAPS = {
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
function analysisStatus(toggleOn, supported) {
  if (!supported) return { cls: 'badge-pending', label: tr('analysis.status.na'), icon: '—' };
  if (!toggleOn)  return { cls: 'badge-warn',    label: tr('analysis.status.disabled'), icon: '⊘' };
  return                 { cls: 'badge-ok',      label: tr('analysis.status.active'), icon: '✓' };
}

function renderAnalysisCoverageCard(target) {
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
function renderScrubChip(h) {
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
function renderSchemaCharsetCell(r) {
  if (!r.report) return '—';
  const schemaBadge = r.report.schemaChanged
    ? `<span class="badge badge-failed">${tr('schemaCell.changed')}</span>`
    : `<span class="badge badge-ok">${tr('schemaCell.stable')}</span>`;
  const charsetBadge = r.report.charsetChanged
    ? ' <span class="badge badge-warn" style="font-size:10px" title="charset/collation changed since previous run">CS</span>'
    : '';
  return schemaBadge + charsetBadge;
}

// Charset / collation row. Renders nothing for runs without recorded encoding
// (mongo, redis, legacy metas) — keeps the detail card uncluttered for DB
// types where the field doesn't apply. Drift indicator pulls from
// report.charsetChanged so it lines up with the BackupCharsetChanged alert.
function renderCharsetRow(run) {
  if (!run || !run.stats) return '';
  const cs = run.stats.charset || '';
  const co = run.stats.collation || '';
  if (!cs && !co) return '';
  const drift = run.report && run.report.charsetChanged;
  const value = escHTML(cs) + (co ? ' / ' + escHTML(co) : '');
  const badge = drift
    ? '<span class="badge badge-warn" style="margin-left:8px;font-size:10px" title="character_set or collation differs from previous run — multibyte chars may truncate on restore">drift</span>'
    : '';
  return `<div class="detail-row"><span class="key">${tr('row.charset')}</span><span class="val" style="font-family:var(--font-mono,monospace);font-size:12px">${value}${badge}</span></div>`;
}

// "Schema unchanged for N days" — leverages meta.schemaChangedAt which is
// carried forward across runs, so a single meta tells you the schema's age.
// Hidden when the field is absent (legacy metas, mongo without schema hash).
function renderSchemaAgeRow(run) {
  if (!run || !run.schemaChangedAt) return '';
  const t = new Date(run.schemaChangedAt);
  if (isNaN(t.getTime())) return '';
  const days = Math.floor((Date.now() - t.getTime()) / 86400000);
  const label = days <= 0 ? tr('time.today') : days === 1 ? tr('time.oneDay') : tr('time.nDays', {n: days});
  const tip = ' title="Schema fingerprint last changed at ' + escAttr(t.toISOString()) + '. Old schemas may not match the current application."';
  return `<div class="detail-row"><span class="key">${tr('schemaAge.label')}</span><span class="val"${tip}>${tr('time.unchangedFor', {label})}</span></div>`;
}

// --- Verification ---
function renderVerificationBadge(v) {
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
function renderRestoreVerificationBadge(rv) {
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

function renderRestoreVerificationDetail(run) {
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
    ? `<div class="detail-row" style="align-items:flex-start"><span class="key">Stats error</span><pre class="val" style="color:var(--warn,#f0b400);font-size:12px;white-space:pre-wrap;word-break:break-word;margin:0;background:var(--bg-input);padding:8px;border-radius:4px;max-height:160px;overflow:auto" title="The pre-dump CollectStats call failed, so the verifier had no preTables to compare against. Fix the underlying access (DB permissions, network) — the analyzer toggle is unrelated.">${escHTML(run.statsError)}</pre></div>` : '';
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

function renderVerificationDetail(run) {
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

// --- Trigger ---
window.triggerBackup = async function(targetName) {
  try {
    await api('/api/trigger/' + targetName, { method: 'POST' });
    toast(tr('toast.triggered') + ': ' + targetName, 'success');
  } catch(e) { toast(tr('toast.triggerFailed', {error: e.message}), 'error'); }
};

// Pause/resume scheduled runs without deleting the source. Hits the
// dedicated suspend endpoint so we don't have to send the full source
// body — that endpoint only patches the suspended annotation.
window.toggleSourceSuspend = async function(secretName, displayName, suspend) {
  try {
    await api('/api/sources/' + secretName + '/suspend', {
      method: 'POST',
      body: JSON.stringify({ suspend }),
    });
    toast((suspend ? tr('toast.paused') : tr('toast.resumed')) + ': ' + displayName, 'success');
    renderPage(currentPage());
  } catch(e) {
    toast((suspend ? tr('buttons.pause') : tr('buttons.resume')) + ': ' + e.message, 'error');
  }
};


// --- Settings Wizard ---
let settingsStep = 0;
// Built fresh each call so the active language wins. The result is read
// dozens of times within a single render — keep callers light by using a
// local const.
function getSettingsSteps() {
  return [
    { id: 'schedule', title: tr('page.settings.section.schedule'), icon: '&#128339;' },
    { id: 'retention', title: tr('page.settings.section.retention'), icon: '&#128451;' },
    { id: 'resources', title: tr('page.settings.section.worker'), icon: '&#9881;' },
    { id: 'review', title: tr('page.settings.section.review'), icon: '&#10003;' }
  ];
}

window.renderSettings = renderSettings;
async function renderSettings(loading = true) {
  const _gen = _renderGen;
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
    window._currentSettings = null;
    content.innerHTML = `
      <div class="page-header">
        <div><h1>${tr('page.settings.title')}</h1><div class="subtitle">${tr('page.settings.subtitle')}</div></div>
      </div>
      <div class="empty-state">
        <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09"/></svg>
        <h3>${tr('page.settings.notAvailable')}</h3>
        <p>${tr('toast.loadFailed', {error: errorMsg || ''})}</p>
        <p style="margin-top:8px;font-size:0.9em;opacity:0.7">Ensure the operator is deployed with <code>ui.enabled=true</code> and the Docker image is rebuilt after code changes.</p>
        <button class="btn btn-primary" onclick="renderSettings(true)" style="margin-top:12px" title="Reload the settings ConfigMap from Kubernetes">${tr('buttons.refresh')}</button>
      </div>`;
    return;
  }

  window._currentSettings = settings;
  settingsStep = 0;
  renderSettingsPage(settings);
}

function renderSettingsPage(settings) {
  const settingsSteps = getSettingsSteps();
  content.innerHTML = `
    <div class="page-header">
      <div><h1>${tr('page.settings.title')}</h1><div class="subtitle">${tr('page.settings.wizardSubtitle')}</div></div>
      <div style="display:flex;gap:8px">
        <button class="btn btn-secondary" onclick="exportSettings()" title="Download a values.yaml snippet matching the current settings — drop into your Helm chart for GitOps-style management">&#8681; ${tr('buttons.exportValues')}</button>
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
            ${settingsStep > 0 ? '<button type="button" class="btn btn-secondary" onclick="goToStep(' + (settingsStep - 1) + ')" title="Return to the previous wizard step (your changes are preserved)">&#8592; ' + tr('buttons.back') + '</button>' : ''}
          </div>
          <div style="display:flex;gap:8px">
            ${settingsStep < settingsSteps.length - 1
              ? '<button type="button" class="btn btn-primary" onclick="goToStep(' + (settingsStep + 1) + ')" title="Continue to the next wizard step">' + tr('buttons.next') + ' &#8594;</button>'
              : '<button type="submit" class="btn btn-primary" title="Persist all settings to the operator\'s ConfigMap. Takes effect immediately for new runs.">' + tr('buttons.applySettings') + '</button>'}
          </div>
        </div>
      </form>
    </div>
    `;
  // Age-keys management moved to its own page (#/age-keys, see
  // renderAgeKeys) so the sidebar's active-link highlight and direct
  // bookmarking work. Keep loadAgeKeysSection() pure so renderAgeKeys
  // can reuse the existing async-load logic.
}


// --- Age Keys page ---
async function renderAgeKeys(loading = true) {
  const _gen = _renderGen;
  if (loading) showLoading();
  if (isStaleRender(_gen)) return;
  content.innerHTML = `
    <div class="page-header">
      <div>
        <h1>${tr('nav.ageKeys') || 'Age Keys'}</h1>
        <div class="subtitle">${tr('ageKeys.subtitle') || 'Public keys (age recipients) backups are encrypted to. Rotation: add new, wait one cycle, remove old.'}</div>
      </div>
    </div>
    <div id="age-keys-section" class="table-card"></div>
  `;
  loadAgeKeysSection();
}

// --- Age recipient (public key) management ---
async function loadAgeKeysSection() {
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
      <span style="color:var(--text-muted);font-size:12px">${keys.length} · Secret: <code>${escHTML(resp.secretName || '—')}</code></span>
    </div>
    <p style="padding:0 16px;color:var(--text-muted);font-size:13px;margin:0 0 12px">
      ${tr('ageKeys.intro')}
      ${canMutate ? '' : '<br><span style="color:var(--warning,#d97706)">' + tr('ageKeys.readOnly') + '</span>'}
    </p>
    ${keys.length === 0 ? `<div class="empty-state"><p>${tr('ageKeys.noKeys')} ${tr('ageKeys.noKeysHint')}</p></div>` : `
    <table>
      <thead><tr>
        <th class="num row-num">#</th>
        <th>${tr('row.fingerprint')}</th>
        <th>${tr('common.name')}</th>
        ${canMutate ? '<th style="width:1%"></th>' : ''}
      </tr></thead>
      <tbody>${keys.map((k, i) => `<tr>
        <td class="num row-num">${i + 1}</td>
        <td><code style="font-size:12px;background:var(--bg-input);padding:2px 6px;border-radius:4px">${escHTML(k.hash)}</code></td>
        <td><code style="font-size:11px;word-break:break-all">${escHTML(k.recipient)}</code></td>
        ${canMutate ? `<td style="white-space:nowrap">
          <button class="btn btn-ghost btn-sm" style="color:var(--danger)"
            onclick="removeAgeKey('${escJS(k.recipient)}','${escJS(k.hash)}')"
            title="Remove this public key. Future backups will no longer encrypt to it. The last recipient cannot be removed.">&#10005; ${tr('common.delete')}</button>
        </td>` : ''}
      </tr>`).join('')}</tbody>
    </table>`}
    ${canMutate ? `
    <div style="padding:16px;border-top:1px solid var(--border)">
      <h3 style="font-size:13px;margin:0 0 8px">${tr('ageKeys.addNew')}</h3>
      <form onsubmit="addAgeKey(event)">
        <div style="display:flex;gap:8px">
          <input type="text" name="recipient" required pattern="age1[0-9a-z]+"
            placeholder="age1qx0...your-recipient..."
            style="flex:1;font-family:ui-monospace,monospace;font-size:12px"
            title="An age X25519 recipient — starts with 'age1' followed by base32 characters. Generate with 'age-keygen' offline; paste only the public line here.">
          <button type="submit" class="btn btn-primary btn-sm"
            title="Add this recipient. Future backup runs will encrypt to it in addition to the existing recipients. Validation rejects malformed strings before saving.">+ ${tr('buttons.addKey')}</button>
        </div>
        <div class="hint">Paste the <code>age1...</code> public line from <code>age-keygen</code>. The private key must stay offline — never paste it here.</div>
      </form>
    </div>` : ''}`;
}

window.addAgeKey = async function(ev) {
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
    toast(tr('ageKeys.addFailed', {error: e.message}), 'error');
  }
};

window.removeAgeKey = async function(recipient, hash) {
  const ok = confirm(
    'Remove this age recipient?\n\n' +
    'Fingerprint: ' + hash + '\n\n' +
    'Future backups will no longer encrypt to this recipient. ' +
    'Make sure you still hold a private key for one of the other listed recipients — ' +
    'otherwise future backups become un-decryptable.\n\n' +
    'Existing dumps in storage are not affected.'
  );
  if (!ok) return;
  try {
    const resp = await api('/api/age-keys/' + encodeURIComponent(recipient), { method: 'DELETE' });
    toast(resp.message || tr('ageKeys.removed'), 'success');
    loadAgeKeysSection();
  } catch(e) {
    toast(tr('ageKeys.removeFailed', {error: e.message}), 'error');
  }
};

function renderSettingsStepContent(step, s) {
  switch(step) {
    case 0: return `
      <h3>${tr('page.settings.section.schedule')}</h3>
      <p class="wizard-desc">${tr('page.settings.wizard.scheduleDesc')}</p>
      <div class="form-group">
        <label for="defaultSchedule">${tr('page.settings.wizard.label.defaultSchedule')}</label>
        <input type="text" id="defaultSchedule" name="defaultSchedule" value="${escAttr(s.defaultSchedule)}" placeholder="0 2 * * *">
        <div class="hint">${tr('page.settings.wizard.hint.schedule')}</div>
      </div>
      <div class="form-group">
        <label for="runTimeoutSeconds">${tr('page.settings.wizard.label.runTimeout')}</label>
        <input type="number" id="runTimeoutSeconds" name="runTimeoutSeconds" value="${escAttr(s.runTimeoutSeconds)}" placeholder="3600" min="0">
        <div class="hint">${tr('page.settings.wizard.hint.timeout')}</div>
      </div>`;

    case 1: return `
      <h3>${tr('page.settings.section.retention')}</h3>
      <p class="wizard-desc">${tr('page.settings.wizard.retentionDesc')}</p>
      <div class="form-row">
        <div class="form-group">
          <label for="defaultRetentionDays">${tr('page.settings.wizard.label.retentionDays')}</label>
          <input type="number" id="defaultRetentionDays" name="defaultRetentionDays" value="${escAttr(s.defaultRetentionDays)}" placeholder="30" min="0">
          <div class="hint">${tr('page.settings.wizard.hint.retentionDays')}</div>
        </div>
        <div class="form-group">
          <label for="defaultMinKeep">${tr('page.settings.wizard.label.minKeep')}</label>
          <input type="number" id="defaultMinKeep" name="defaultMinKeep" value="${escAttr(s.defaultMinKeep)}" placeholder="3" min="0">
          <div class="hint">${tr('page.settings.wizard.hint.minKeep')}</div>
        </div>
      </div>
      <div class="form-row">
        <div class="form-group">
          <label for="tempDir">${tr('page.settings.wizard.label.tempDir')}</label>
          <input type="text" id="tempDir" name="tempDir" value="${escAttr(s.tempDir)}" placeholder="/tmp/backup-operator">
          <div class="hint">${tr('page.settings.wizard.hint.tempDir')}</div>
        </div>
        <div class="form-group">
          <label for="tempDirSize">${tr('page.settings.wizard.label.tempDirSize')}</label>
          <input type="text" id="tempDirSize" name="tempDirSize" value="${escAttr(s.tempDirSize)}" placeholder="10Gi">
          <div class="hint">${tr('page.settings.wizard.hint.tempDirSize')}</div>
        </div>
      </div>`;

    case 2: return `
      <h3>${tr('page.settings.section.worker')}</h3>
      <p class="wizard-desc">${tr('page.settings.wizard.workerDesc')}</p>
      <div class="form-section">
        <h4>${tr('page.settings.wizard.limits')}</h4>
        <div class="form-row">
          <div class="form-group">
            <label for="workerCpuLimit">${tr('page.settings.wizard.label.cpuLimit')}</label>
            <input type="text" id="workerCpuLimit" name="workerCpuLimit" value="${escAttr(s.workerCpuLimit)}" placeholder="2000m">
            <div class="hint">${tr('page.settings.wizard.hint.cpuLimit')}</div>
          </div>
          <div class="form-group">
            <label for="workerMemoryLimit">${tr('page.settings.wizard.label.memoryLimit')}</label>
            <input type="text" id="workerMemoryLimit" name="workerMemoryLimit" value="${escAttr(s.workerMemoryLimit)}" placeholder="2Gi">
            <div class="hint">${tr('page.settings.wizard.hint.memoryLimit')}</div>
          </div>
        </div>
      </div>
      <div class="form-section">
        <h4>${tr('page.settings.wizard.requests')}</h4>
        <div class="form-row">
          <div class="form-group">
            <label for="workerCpuRequest">${tr('page.settings.wizard.label.cpuRequest')}</label>
            <input type="text" id="workerCpuRequest" name="workerCpuRequest" value="${escAttr(s.workerCpuRequest)}" placeholder="250m">
            <div class="hint">${tr('page.settings.wizard.hint.cpuRequest')}</div>
          </div>
          <div class="form-group">
            <label for="workerMemoryRequest">${tr('page.settings.wizard.label.memoryRequest')}</label>
            <input type="text" id="workerMemoryRequest" name="workerMemoryRequest" value="${escAttr(s.workerMemoryRequest)}" placeholder="256Mi">
            <div class="hint">${tr('page.settings.wizard.hint.memoryRequest')}</div>
          </div>
        </div>
      </div>`;

    case 3:
      return `
      <h3>${tr('page.settings.section.review')}</h3>
      <p class="wizard-desc">${tr('page.settings.wizard.reviewDesc')}</p>
      <div class="review-grid">
        <div class="detail-card">
          <h3>${tr('page.settings.section.schedule')}</h3>
          <div class="detail-row"><span class="key">${tr('table.schedule')}</span><code class="val">${escHTML(s.defaultSchedule)}</code></div>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.timeout')}</span><span class="val">${escHTML(s.runTimeoutSeconds)}s</span></div>
        </div>
        <div class="detail-card">
          <h3>${tr('page.settings.section.retention')}</h3>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.retentionDays')}</span><span class="val">${escHTML(s.defaultRetentionDays)}</span></div>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.minKeep')}</span><span class="val">${escHTML(s.defaultMinKeep)}</span></div>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.tempDir')}</span><code class="val">${escHTML(s.tempDir)}</code></div>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.tempDirSize')}</span><span class="val">${escHTML(s.tempDirSize)}</span></div>
        </div>
        <div class="detail-card">
          <h3>${tr('page.settings.section.worker')}</h3>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.cpuLimit')}</span><span class="val">${escHTML(s.workerCpuLimit) || '—'}</span></div>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.memoryLimit')}</span><span class="val">${escHTML(s.workerMemoryLimit) || '—'}</span></div>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.cpuRequest')}</span><span class="val">${escHTML(s.workerCpuRequest) || '—'}</span></div>
          <div class="detail-row"><span class="key">${tr('page.settings.wizard.label.memoryRequest')}</span><span class="val">${escHTML(s.workerMemoryRequest) || '—'}</span></div>
        </div>
      </div>
      <div class="wizard-note">
        ${tr('page.settings.wizard.note')}
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
  settingsStep = Math.max(0, Math.min(n, getSettingsSteps().length - 1));
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
    toast(tr('toast.settingsSaved'), 'success');
  } catch(e) {
    console.error('[Settings] Save failed:', e.message);
    toast(tr('toast.saveFailed', {error: e.message}), 'error');
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
    toast(tr('toast.exportOk'), 'success');
  } catch(e) { toast(tr('toast.exportFail', {error: e.message}), 'error'); }
};

// --- Close dropdowns on outside click ---

document.addEventListener('click', function(e) {
  if (!e.target.closest('.dropdown')) {
    $$('.dropdown-menu.open').forEach(m => m.classList.remove('open'));
  }
});
