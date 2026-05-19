// charts.js — SVG chart renderers and visualization helpers
'use strict';

function chartOrLoading(data, inFlight, renderFn) {
  const empty = !data ||
                (Array.isArray(data) && data.length === 0) ||
                (typeof data === 'object' && !Array.isArray(data) && Object.keys(data).length === 0);
  if (empty && inFlight) {
    return `<div class="chart-loading"><div class="spinner"></div><div class="hint">${tr('chart.loading') || 'loading…'}</div></div>`;
  }
  return renderFn();
}
function refreshingDot(inFlight, hasData) {
  return (inFlight && hasData) ? '<span class="chart-refreshing" title="refreshing in background"></span>' : '';
}

// renderTargetSparkline is in pages.js (needs _fleetSummary).

function buildHourlyActivity(jobs) {
  const now = Date.now();
  const buckets = Array.from({length: 24}, () => ({ ok: 0, failed: 0, running: 0 }));
  for (const j of jobs || []) {
    const t = parseTsRFC(j.startTime);
    if (!t) continue;
    const hoursAgo = Math.floor((now - t) / 3600000);
    if (hoursAgo < 0 || hoursAgo >= 24) continue;
    const idx = 23 - hoursAgo;
    if (j.status === 'running') buckets[idx].running++;
    else if (j.status === 'failed') buckets[idx].failed++;
    else buckets[idx].ok++;
  }
  return buckets;
}

const _heroLastShown = { runs24h: 0, failed24h: 0, runningNow: 0, destinations: 0 };

// renderHeroPanel is the full-bleed status-at-a-glance card at the
// top of the dashboard. Replaces the five flat stat cards with:
//   - a status header that's green / amber / red based on 24h health
//   - four big metrics (runs, failures, running, destinations)
//   - a 24-bar hourly activity strip so the visitor sees the
//     fleet's rhythm without scrolling
// Numbers are wrapped in data-counter spans so animateCounter can
// tween them when SSE updates land.
function renderHeroPanel(targets, dests, jobs) {
  const buckets = buildHourlyActivity(jobs);
  const runs24h = buckets.reduce((s, b) => s + b.ok + b.failed + b.running, 0);
  const failed24h = buckets.reduce((s, b) => s + b.failed, 0);
  const runningNow = jobs.filter(j => j.status === 'running').length;
  const destCount = dests.length;
  const srcCount = targets.length;

  // Status tier: red if any 24h failure, amber if no recent run for
  // an active target (fleet quiet), green otherwise. The "quiet"
  // signal is "0 runs in 24h while there's >0 active targets" — that
  // shouts misconfig louder than a clean green.
  let statusKey = 'healthy';
  let statusMsg = tr('hero.status.healthy');
  let statusSub = tr('hero.status.healthySub', {sources: srcCount, dests: destCount});
  if (failed24h > 0) {
    statusKey = 'danger';
    statusMsg = tr('hero.status.failures', {count: failed24h});
    statusSub = tr('hero.status.failuresSub');
  } else if (srcCount > 0 && runs24h === 0) {
    statusKey = 'warn';
    statusMsg = tr('hero.status.quiet');
    statusSub = tr('hero.status.quietSub');
  }

  // 24h activity strip: each bar's height proportional to that hour's
  // total runs; colour = dominant status. Empty hours render a thin
  // baseline so the strip's shape stays readable.
  const maxRuns = Math.max(1, ...buckets.map(b => b.ok + b.failed + b.running));
  const stripW = 280, stripH = 36, barW = (stripW / 24) - 2;
  const stripBars = buckets.map((b, i) => {
    const total = b.ok + b.failed + b.running;
    const h = total === 0 ? 2 : 4 + (total / maxRuns) * (stripH - 4);
    const x = i * (stripW / 24) + 1;
    const y = stripH - h;
    let cls = 'hero-strip-bar';
    if (total === 0) cls += ' hero-strip-bar-empty';
    else if (b.failed > 0) cls += ' hero-strip-bar-failed';
    else if (b.running > 0) cls += ' hero-strip-bar-running';
    else cls += ' hero-strip-bar-ok';
    const tt = `${23-i}h ago: ${total} run(s)${b.failed > 0 ? `, ${b.failed} failed` : ''}${b.running > 0 ? `, ${b.running} running` : ''}`;
    return `<rect x="${x.toFixed(1)}" y="${y.toFixed(1)}" width="${barW.toFixed(1)}" height="${h.toFixed(1)}" rx="1" class="${cls}"><title>${escAttr(tt)}</title></rect>`;
  }).join('');

  return `<div class="hero hero-status-${statusKey}">
    <div class="hero-header">
      <span class="hero-pulse"></span>
      <div>
        <div class="hero-status-msg">${escHTML(statusMsg)}</div>
        <div class="hero-status-sub">${escHTML(statusSub)}</div>
      </div>
    </div>
    <div class="hero-metrics">
      <div class="hero-metric">
        <div class="hero-metric-value" data-counter="runs24h" data-value="${_heroLastShown.runs24h}">${_heroLastShown.runs24h}</div>
        <div class="hero-metric-label">${tr('hero.metric.runs24h')}</div>
      </div>
      <div class="hero-metric ${failed24h > 0 ? 'hero-metric-bad' : ''}">
        <div class="hero-metric-value" data-counter="failed24h" data-value="${_heroLastShown.failed24h}">${_heroLastShown.failed24h}</div>
        <div class="hero-metric-label">${tr('hero.metric.failed24h')}</div>
      </div>
      <div class="hero-metric ${runningNow > 0 ? 'hero-metric-running' : ''}">
        <div class="hero-metric-value" data-counter="runningNow" data-value="${_heroLastShown.runningNow}">${_heroLastShown.runningNow}</div>
        <div class="hero-metric-label">${tr('hero.metric.running')}${runningNow > 0 ? ' <span class="hero-metric-pulse"></span>' : ''}</div>
      </div>
      <div class="hero-metric">
        <div class="hero-metric-value" data-counter="destinations" data-value="${_heroLastShown.destinations}">${_heroLastShown.destinations}</div>
        <div class="hero-metric-label">${tr('hero.metric.destinations')}</div>
      </div>
    </div>
    <div class="hero-activity">
      <svg viewBox="0 0 ${stripW} ${stripH}" class="hero-strip" preserveAspectRatio="none" aria-label="24-hour activity">
        ${stripBars}
      </svg>
      <div class="hero-activity-label">${tr('hero.activity.label')}</div>
    </div>
  </div>`;
}


function fmtDurationShort(sec) {
  if (sec == null || !isFinite(sec) || sec < 0) return '';
  if (sec < 60) return `${Math.round(sec)}s`;
  const m = Math.floor(sec / 60), s = Math.round(sec % 60);
  if (m < 60) return s ? `${m}m ${s}s` : `${m}m`;
  const h = Math.floor(m / 60), mm = m % 60;
  return mm ? `${h}h ${mm}m` : `${h}h`;
}

// --- Target detail ---
// Pure-SVG line chart for the encrypted-dump-size trend on the target detail
// page. Failed runs are excluded (no size to plot). When fewer than 2
// successful runs are available, returns an empty-state message instead of
// a misleading single-point chart.
function renderSizeChart(runs) {
  const points = (runs || [])
    .filter(r => r.status !== 'failed' && r.encryptedSizeBytes > 0)
    .map(r => ({ ts: parseTsCompact(r.timestamp), size: r.encryptedSizeBytes }))
    .filter(p => p.ts !== null)
    .sort((a, b) => a.ts - b.ts);

  if (points.length < 2) {
    return '<div class="chart-empty">Not enough successful runs yet — at least 2 are needed to draw a trend.</div>';
  }

  const W = 700, H = 220;
  const ML = 64, MR = 16, MT = 12, MB = 30;
  const PW = W - ML - MR, PH = H - MT - MB;

  const xs = points.map(p => p.ts);
  const ys = points.map(p => p.size);
  const xMin = xs[0], xMax = xs[xs.length - 1];
  const xRange = (xMax - xMin) || 1;
  // niceMax: round yMax up so axis labels are tidy bytes (KiB/MiB step).
  const yMaxRaw = Math.max(...ys);
  const yMax = niceCeil(yMaxRaw * 1.1);

  const x = t => ML + ((t - xMin) / xRange) * PW;
  const y = v => MT + PH - (v / yMax) * PH;

  const nX = Math.min(6, points.length);
  const xTicks = [];
  for (let i = 0; i < nX; i++) {
    const t = xMin + (xRange * i) / (nX - 1);
    xTicks.push({ x: x(t), label: shortDate(t) });
  }
  const yTicks = [];
  for (let i = 0; i <= 4; i++) {
    const v = (yMax * i) / 4;
    yTicks.push({ y: y(v), label: humanBytes(v) });
  }

  const linePath = points.map((p, i) =>
    (i === 0 ? 'M' : 'L') + x(p.ts).toFixed(1) + ',' + y(p.size).toFixed(1)
  ).join(' ');
  // Area path: line + drop to bottom + close, gives a soft fill below.
  const areaPath = linePath +
    ' L' + x(points[points.length - 1].ts).toFixed(1) + ',' + (MT + PH).toFixed(1) +
    ' L' + x(points[0].ts).toFixed(1) + ',' + (MT + PH).toFixed(1) + ' Z';

  const dots = points.map(p => `<circle cx="${x(p.ts).toFixed(1)}" cy="${y(p.size).toFixed(1)}" r="3" class="chart-point"><title>${escHTML(new Date(p.ts).toLocaleString() + ' — ' + humanBytes(p.size))}</title></circle>`).join('');

  return `<svg viewBox="0 0 ${W} ${H}" class="chart-svg" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Dump size trend">
    ${yTicks.map(t => `<line x1="${ML}" y1="${t.y.toFixed(1)}" x2="${W - MR}" y2="${t.y.toFixed(1)}" class="chart-grid"/><text x="${ML - 8}" y="${(t.y + 4).toFixed(1)}" class="chart-axis-text" text-anchor="end">${escHTML(t.label)}</text>`).join('')}
    ${xTicks.map(t => `<text x="${t.x.toFixed(1)}" y="${(H - MB + 18).toFixed(1)}" class="chart-axis-text" text-anchor="middle">${escHTML(t.label)}</text>`).join('')}
    <line x1="${ML}" y1="${MT}" x2="${ML}" y2="${H - MB}" class="chart-axis"/>
    <line x1="${ML}" y1="${H - MB}" x2="${W - MR}" y2="${H - MB}" class="chart-axis"/>
    <path d="${areaPath}" class="chart-area"/>
    <path d="${linePath}" class="chart-line" fill="none"/>
    ${dots}
  </svg>`;
}

// niceCeil rounds up to a "human-friendly" number for axis maxes:
// next 1/2/5 × 10^n. Avoids labels like "1.234 GiB" on the axis.
function niceCeil(n) {
  if (n <= 0) return 1;
  const exp = Math.floor(Math.log10(n));
  const base = Math.pow(10, exp);
  const m = n / base;
  let nice;
  if (m <= 1)      nice = 1;
  else if (m <= 2) nice = 2;
  else if (m <= 5) nice = 5;
  else             nice = 10;
  return nice * base;
}
function shortDate(ms) {
  const d = new Date(ms);
  return (d.getMonth() + 1) + '/' + d.getDate();
}

// GitHub-style activity heatmap: one cell per day for the last 91 days
// (13 full weeks). A day is green when every run on that day succeeded,
// red when every run failed, amber when at least one of each, and dim
// when no run was recorded. Out-of-window padding cells (the leading
// days of the first column needed to align Sunday-first) render empty.
function renderStatusHeatmap(runs) {
  const days = 91;
  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const windowStart = new Date(today);
  windowStart.setDate(windowStart.getDate() - days + 1);

  const byDate = new Map();
  (runs || []).forEach(r => {
    const ts = parseTsCompact(r.timestamp);
    if (ts === null) return;
    const d = new Date(ts);
    d.setHours(0, 0, 0, 0);
    const key = isoDate(d);
    let b = byDate.get(key);
    if (!b) { b = { ok: 0, failed: 0 }; byDate.set(key, b); }
    if (r.status === 'failed') b.failed++; else b.ok++;
  });

  // Align grid to Sunday before windowStart so weeks line up cleanly.
  const gridStart = new Date(windowStart);
  gridStart.setDate(gridStart.getDate() - gridStart.getDay());

  const cells = [];
  for (let cur = new Date(gridStart); cur <= today; cur.setDate(cur.getDate() + 1)) {
    const key = isoDate(cur);
    const b = byDate.get(key);
    const inWindow = cur >= windowStart;
    let cls = 'empty';
    if (b && inWindow) {
      if (b.failed === 0) cls = 'ok';
      else if (b.ok === 0) cls = 'failed';
      else cls = 'mixed';
    }
    cells.push({
      date: new Date(cur), key, cls,
      ok: b ? b.ok : 0,
      failed: b ? b.failed : 0,
      visible: inWindow,
    });
  }

  if (cells.filter(c => c.visible && (c.ok + c.failed) > 0).length === 0) {
    return '<div class="chart-empty">No runs in the last ' + days + ' days.</div>';
  }

  const size = 12, gap = 3, step = size + gap;
  const ML = 28, MT = 18;
  const cols = Math.ceil(cells.length / 7);
  const W = ML + cols * step;
  const H = MT + 7 * step;

  const weekdayLabels = { 1: 'Mon', 3: 'Wed', 5: 'Fri' };
  const monthLabels = [];
  let lastMonth = -1;
  for (let c = 0; c < cols; c++) {
    const cell = cells[c * 7];
    if (!cell) continue;
    const m = cell.date.getMonth();
    if (m !== lastMonth) {
      monthLabels.push({
        x: ML + c * step,
        text: cell.date.toLocaleString('default', { month: 'short' }),
      });
      lastMonth = m;
    }
  }

  const cellSvg = cells.map((c, i) => {
    const col = Math.floor(i / 7), row = i % 7;
    const x = ML + col * step, y = MT + row * step;
    const total = c.ok + c.failed;
    const tip = !c.visible ? '' :
      total === 0 ? c.key + ' — no runs'
                  : c.key + ' — ' + c.ok + ' ok, ' + c.failed + ' failed';
    return `<rect x="${x}" y="${y}" width="${size}" height="${size}" rx="2" class="hm-cell hm-${c.cls}${c.visible ? '' : ' hm-out'}"><title>${escHTML(tip)}</title></rect>`;
  }).join('');

  const wdaySvg = Object.entries(weekdayLabels).map(([row, label]) =>
    `<text x="0" y="${MT + (+row) * step + size - 2}" class="chart-axis-text">${label}</text>`
  ).join('');

  const monthSvg = monthLabels.map(m =>
    `<text x="${m.x}" y="${MT - 6}" class="chart-axis-text">${escHTML(m.text)}</text>`
  ).join('');

  const legend = `<div class="hm-legend">
    <span>Less</span>
    <span class="hm-dot hm-empty"></span>
    <span class="hm-dot hm-ok"></span>
    <span class="hm-dot hm-mixed"></span>
    <span class="hm-dot hm-failed"></span>
    <span>More</span>
  </div>`;

  return `<svg viewBox="0 0 ${W} ${H}" class="chart-svg heatmap-svg" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Run status heatmap">
    ${wdaySvg}${monthSvg}${cellSvg}
  </svg>${legend}`;
}

function isoDate(d) {
  // Local-date YYYY-MM-DD; we bucket by local day so the heatmap matches
  // the user's timezone, not UTC.
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, '0');
  const day = String(d.getDate()).padStart(2, '0');
  return y + '-' + m + '-' + day;
}

// renderStorageDonut shows actual filesystem usage per destination from
// /api/destination-stats. Unlike renderStorageByDestination (which
// extrapolates "current snapshot × destinations"), the donut reflects
// what's really on disk — counting every retained run, not just the
// latest. Hovering shows file count and byte total per slice.
const STORAGE_DONUT_COLORS = [
  '#5b6eef', '#10b981', '#f59e0b', '#ef4444',
  '#8b5cf6', '#06b6d4', '#ec4899', '#84cc16',
];
function renderStorageDonut(stats) {
  const entries = (stats || []).filter(s => s && s.totalSizeBytes > 0)
    .map(s => ({ name: s.name, bytes: s.totalSizeBytes || 0, files: s.totalFiles || 0 }))
    .sort((a, b) => b.bytes - a.bytes);
  if (entries.length === 0) {
    return '<div class="chart-empty">' + tr('chart.storageDonut.empty') + '</div>';
  }
  const total = entries.reduce((s, e) => s + e.bytes, 0);
  const cx = 90, cy = 90, r = 70, rInner = 42;
  // Start at 12-o'clock; sweep clockwise. SVG arcs are easier if we
  // accumulate angles as we go.
  let acc = -Math.PI / 2;
  const segs = entries.map((e, i) => {
    const frac = e.bytes / total;
    const angle = frac * Math.PI * 2;
    const start = acc;
    const end = acc + angle;
    acc = end;
    const x1 = cx + Math.cos(start) * r,     y1 = cy + Math.sin(start) * r;
    const x2 = cx + Math.cos(end)   * r,     y2 = cy + Math.sin(end)   * r;
    const x3 = cx + Math.cos(end)   * rInner, y3 = cy + Math.sin(end)   * rInner;
    const x4 = cx + Math.cos(start) * rInner, y4 = cy + Math.sin(start) * rInner;
    const large = angle > Math.PI ? 1 : 0;
    const color = STORAGE_DONUT_COLORS[i % STORAGE_DONUT_COLORS.length];
    const pct = (frac * 100).toFixed(1);
    const path = `M${x1.toFixed(2)},${y1.toFixed(2)} A${r},${r} 0 ${large} 1 ${x2.toFixed(2)},${y2.toFixed(2)} L${x3.toFixed(2)},${y3.toFixed(2)} A${rInner},${rInner} 0 ${large} 0 ${x4.toFixed(2)},${y4.toFixed(2)} Z`;
    return {
      path, color, pct, ...e,
    };
  });
  return `<div style="display:flex;gap:16px;align-items:center;flex-wrap:wrap">
    <svg viewBox="0 0 180 180" class="chart-svg" style="max-width:180px;flex:0 0 180px" role="img" aria-label="Storage by destination">
      ${segs.map(s => `<path d="${s.path}" fill="${s.color}" stroke="var(--bg)" stroke-width="1"><title>${escAttr(s.name)} — ${humanBytes(s.bytes)} (${s.pct}%), ${s.files} files</title></path>`).join('')}
      <text x="${cx}" y="${cy - 4}" text-anchor="middle" class="chart-axis-text" style="font-size:11px;fill:var(--text-muted)">${tr('chart.storageDonut.total')}</text>
      <text x="${cx}" y="${cy + 12}" text-anchor="middle" style="font-size:13px;font-weight:600;fill:var(--text)">${humanBytes(total)}</text>
    </svg>
    <div style="flex:1;min-width:140px;font-size:12px">
      ${segs.map(s => `<div style="display:flex;align-items:center;gap:6px;padding:2px 0">
        <span style="display:inline-block;width:10px;height:10px;background:${s.color};border-radius:2px;flex:0 0 10px"></span>
        <span style="flex:1;color:var(--text);overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="${escAttr(s.name)}">${escHTML(s.name)}</span>
        <span style="color:var(--text-muted);font-size:11px">${humanBytes(s.bytes)}</span>
      </div>`).join('')}
    </div>
  </div>`;
}

// renderDurationDistribution draws a horizontal range-bar per target:
// the bar spans min→max with a vertical tick at the median and a small
// marker at p95. Sorted by median descending so the slowest targets
// sit at the top. Spots outliers — "every backup takes <2 min except
// target X which takes 45 min" — at a glance. Failed runs are
// excluded server-side (they fail fast and would skew the median to
// zero); the Count column shows how many successful runs informed the
// distribution.
function renderDurationDistribution(stats) {
  const rows = (stats || []).filter(s => s && s.count > 0).slice(0, 10);
  if (rows.length === 0) {
    return '<div class="chart-empty">' + tr('chart.durationDist.empty') + '</div>';
  }
  const W = 700, rowH = 22, padTop = 8, padBottom = 22, padL = 140, padR = 60;
  const PW = W - padL - padR;
  const H = padTop + rows.length * rowH + padBottom;
  // Single shared x-axis: 0 → max(max across all targets). Per-target
  // axes would make outliers invisible (the slow target's bar would
  // span the same visual length as a fast target's bar).
  const xMax = rows.reduce((m, r) => Math.max(m, r.max), 1);
  const xScale = v => padL + (v / xMax) * PW;

  // Hour/min ticks: 5 evenly-spaced labels
  const xTicks = [];
  for (let i = 0; i <= 4; i++) {
    const v = (xMax * i) / 4;
    xTicks.push({ x: xScale(v), label: fmtDurationShort(v) });
  }

  const bars = rows.map((r, i) => {
    const y = padTop + i * rowH;
    const cy = y + rowH / 2;
    const x1 = xScale(r.min), x2 = xScale(r.max);
    const xMed = xScale(r.median), xP95 = xScale(r.p95);
    return `<g>
      <a href="#/target/${escAttr(r.target)}" style="cursor:pointer">
        <text x="${(padL - 8).toFixed(1)}" y="${(cy + 4).toFixed(1)}" text-anchor="end" class="chart-axis-text" style="font-size:11px;fill:var(--accent)">${escHTML(r.target)}</text>
      </a>
      <line x1="${x1.toFixed(1)}" y1="${cy.toFixed(1)}" x2="${x2.toFixed(1)}" y2="${cy.toFixed(1)}" stroke="var(--accent, #5b6eef)" stroke-width="3" opacity="0.4" stroke-linecap="round"><title>${escAttr(r.target + ' — min ' + fmtDurationShort(r.min) + ', max ' + fmtDurationShort(r.max) + ', n=' + r.count)}</title></line>
      <line x1="${xMed.toFixed(1)}" y1="${(cy - 7).toFixed(1)}" x2="${xMed.toFixed(1)}" y2="${(cy + 7).toFixed(1)}" stroke="var(--accent, #5b6eef)" stroke-width="2.5"><title>median ${escAttr(fmtDurationShort(r.median))}</title></line>
      <circle cx="${xP95.toFixed(1)}" cy="${cy.toFixed(1)}" r="3.5" fill="var(--warning, #f59e0b)" stroke="var(--bg)" stroke-width="1"><title>p95 ${escAttr(fmtDurationShort(r.p95))}</title></circle>
      <text x="${(W - padR + 6).toFixed(1)}" y="${(cy + 4).toFixed(1)}" class="chart-axis-text" style="font-size:11px">n=${r.count}</text>
    </g>`;
  }).join('');

  return `<svg viewBox="0 0 ${W} ${H}" class="chart-svg" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Run duration distribution per target">
    ${xTicks.map(t => `<line x1="${t.x.toFixed(1)}" y1="${padTop}" x2="${t.x.toFixed(1)}" y2="${(H - padBottom + 2).toFixed(1)}" class="chart-grid"/><text x="${t.x.toFixed(1)}" y="${(H - padBottom + 14).toFixed(1)}" text-anchor="middle" class="chart-axis-text" style="font-size:10px">${escHTML(t.label)}</text>`).join('')}
    ${bars}
  </svg>`;
}

// renderVerificationTrend plots the daily restore-verification pass
// rate as a line over the 30-day window. Only runs with a
// RestoreVerification block contribute to the rate — sources with
// mode=off (the default) don't count toward the denominator at all,
// so a day with zero verifier-armed runs renders as a gap. The
// underlying area shading shows total verifier-armed-run volume so an
// operator can tell whether a 100% rate is "1 of 1" or "200 of 200".
function renderVerificationTrend(daily) {
  const data = (daily || []).filter(d => d && d.day);
  const armed = data.filter(d => (d.passed + d.failed) > 0);
  if (armed.length === 0) {
    return '<div class="chart-empty">' + tr('chart.verifyTrend.empty') + '</div>';
  }
  const W = 700, H = 220, ML = 56, MR = 50, MT = 12, MB = 28;
  const PW = W - ML - MR, PH = H - MT - MB;

  const xs = data.map((_, i) => ML + (i / Math.max(data.length - 1, 1)) * PW);
  // Two Y axes: left = pass rate 0-100%, right = volume (bar height).
  const yPct = pct => MT + PH - (pct / 100) * PH;
  const volMax = Math.max(1, ...data.map(d => d.passed + d.failed));
  const volH = v => (v / volMax) * (PH * 0.4); // bars take bottom 40% only

  // Volume bars (grey, behind the line)
  const bars = data.map((d, i) => {
    const total = d.passed + d.failed;
    if (total === 0) return '';
    const h = volH(total);
    const w = (PW / data.length) * 0.7;
    const x = xs[i] - w / 2;
    return `<rect x="${x.toFixed(1)}" y="${(MT + PH - h).toFixed(1)}" width="${w.toFixed(1)}" height="${h.toFixed(1)}" fill="var(--text-muted)" opacity="0.15"><title>${escAttr(d.day + ': ' + total + ' verifier-armed run(s)')}</title></rect>`;
  }).join('');

  // Pass-rate line — gaps where no verifier ran that day. Build path
  // by walking the array, starting a new sub-path after a gap.
  let path = '';
  let inPath = false;
  data.forEach((d, i) => {
    const total = d.passed + d.failed;
    if (total === 0) { inPath = false; return; }
    const pct = (d.passed / total) * 100;
    path += (inPath ? ' L' : ' M') + xs[i].toFixed(1) + ',' + yPct(pct).toFixed(1);
    inPath = true;
  });
  const dots = data.map((d, i) => {
    const total = d.passed + d.failed;
    if (total === 0) return '';
    const pct = (d.passed / total) * 100;
    const color = pct === 100 ? 'var(--success, #10b981)' : (pct >= 80 ? 'var(--warning, #f59e0b)' : 'var(--danger, #ef4444)');
    return `<circle cx="${xs[i].toFixed(1)}" cy="${yPct(pct).toFixed(1)}" r="3.5" fill="${color}" stroke="var(--bg)" stroke-width="1"><title>${escAttr(d.day + ': ' + pct.toFixed(1) + '% (' + d.passed + '/' + total + ')')}</title></circle>`;
  }).join('');

  // Y ticks at 0/50/100%
  const yTicks = [0, 50, 100].map(p => ({ y: yPct(p), label: p + '%' }));
  // X ticks every 7 days
  const xTicks = [];
  for (let i = 0; i < data.length; i += 7) {
    xTicks.push({ x: xs[i], label: data[i].day.slice(5) });
  }

  return `<svg viewBox="0 0 ${W} ${H}" class="chart-svg" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Restore-verification pass rate trend">
    ${yTicks.map(t => `<line x1="${ML}" y1="${t.y.toFixed(1)}" x2="${(W - MR).toFixed(1)}" y2="${t.y.toFixed(1)}" class="chart-grid"/><text x="${(ML - 6).toFixed(1)}" y="${(t.y + 4).toFixed(1)}" text-anchor="end" class="chart-axis-text" style="font-size:10px">${escHTML(t.label)}</text>`).join('')}
    ${bars}
    <path d="${path}" fill="none" stroke="var(--accent, #5b6eef)" stroke-width="2"/>
    ${dots}
    ${xTicks.map(t => `<text x="${t.x.toFixed(1)}" y="${(H - MB + 14).toFixed(1)}" text-anchor="middle" class="chart-axis-text" style="font-size:10px">${escHTML(t.label)}</text>`).join('')}
    <text x="${(W - MR + 6).toFixed(1)}" y="${(MT + PH - volH(volMax) - 4).toFixed(1)}" class="chart-axis-text" style="font-size:9px;opacity:0.6">${escHTML(tr('chart.verifyTrend.volume', {n: volMax}))}</text>
  </svg>`;
}

// renderStorageGrowth draws a stacked area chart of daily upload
// bytes per source over the heatmap window (30 days). NOT cumulative
// storage — that would require knowing each destination's retention
// policy and reconciling deletes, which we don't track. "Daily upload
// volume per source" answers the operationally relevant question:
// "which source is driving my storage growth?" Per-source is more
// actionable than per-DB-type for capacity planning — operators know
// their tech stack, they want to know which workload to investigate.
//
// With many sources the stack can get visually busy; that's the
// trade-off. Hash-based colours stay stable across reloads so the
// same source always gets the same slice of the rainbow.

// hashHue derives a deterministic 0-359 HSL hue from a string. Same
// string → same hue across reloads. Avoids the "every reload my
// chart looks different" problem of random palettes.
function hashHue(s) {
  let h = 0;
  for (let i = 0; i < s.length; i++) {
    h = ((h << 5) - h + s.charCodeAt(i)) | 0;
  }
  // Bias away from yellow/green range that clashes with the
  // success/warning UI colours.
  return ((h % 360) + 360) % 360;
}
function colorForTarget(name) {
  // 65% saturation + 58% lightness sits comfortably on the dark
  // background without bleaching out, and the HSL spacing means
  // adjacent stacked layers always look distinct from each other.
  return `hsl(${hashHue(name)}, 65%, 58%)`;
}

function renderStorageGrowth(points) {
  const days = (points || []).filter(p => p && p.day);
  if (days.length < 2) {
    return '<div class="chart-empty">' + tr('chart.storageGrowth.empty') + '</div>';
  }
  // Discover all targets that contributed any bytes in the window.
  // Sort by total volume descending so the largest source is at the
  // bottom of the stack (closest to the x-axis) — that's the
  // conventional reading order for stacked area, big-stuff-at-bottom.
  const totalsByTarget = new Map();
  days.forEach(d => {
    if (!d.perTarget) return;
    Object.entries(d.perTarget).forEach(([name, bytes]) => {
      if (bytes > 0) totalsByTarget.set(name, (totalsByTarget.get(name) || 0) + bytes);
    });
  });
  const targets = [...totalsByTarget.entries()].sort((a, b) => b[1] - a[1]).map(e => e[0]);
  if (targets.length === 0) {
    return '<div class="chart-empty">' + tr('chart.storageGrowth.empty') + '</div>';
  }

  const W = 700, H = 220, ML = 60, MR = 16, MT = 12, MB = 28;
  const PW = W - ML - MR, PH = H - MT - MB;
  const xs = days.map((_, i) => ML + (i / Math.max(days.length - 1, 1)) * PW);

  // Build cumulative-stack series, biggest-at-bottom (reverse the
  // targets array for the stack order so the largest layer sits
  // closest to the axis; legend stays in size-desc order separately).
  const stackOrder = targets.slice().reverse();
  const series = stackOrder.map(t => days.map(d => (d.perTarget && d.perTarget[t]) || 0));
  const cumTop = new Array(days.length).fill(0);
  const stacked = series.map(layer => layer.map((v, i) => {
    const top = cumTop[i] + v;
    const out = { bottom: cumTop[i], top };
    cumTop[i] = top;
    return out;
  }));
  const yMax = niceCeil(Math.max(1, ...cumTop) * 1.1);
  const y = v => MT + PH - (v / yMax) * PH;

  const layers = stacked.map((layer, li) => {
    const targetName = stackOrder[li];
    const fill = colorForTarget(targetName);
    const total = totalsByTarget.get(targetName) || 0;
    const topPath = layer.map((p, i) => (i === 0 ? 'M' : 'L') + xs[i].toFixed(1) + ',' + y(p.top).toFixed(1)).join(' ');
    const botPath = layer.slice().reverse().map((p, idx) => {
      const i = layer.length - 1 - idx;
      return 'L' + xs[i].toFixed(1) + ',' + y(p.bottom).toFixed(1);
    }).join(' ');
    return `<path d="${topPath} ${botPath} Z" fill="${fill}" opacity="0.78"><title>${escAttr(targetName + ' — ' + humanBytes(total) + ' in window')}</title></path>`;
  }).join('');

  const yTicks = [];
  for (let i = 0; i <= 4; i++) {
    const v = (yMax * i) / 4;
    yTicks.push({ y: y(v), label: humanBytes(v) });
  }
  const nX = Math.min(6, days.length);
  const xTicks = [];
  for (let i = 0; i < nX; i++) {
    const idx = Math.round((days.length - 1) * (i / (nX - 1)));
    xTicks.push({ x: xs[idx], label: days[idx].day.slice(5) });
  }

  // Legend in size-desc order so the biggest source is listed first
  // (matches the chart's bottom-to-top reading order). Click-link to
  // jump to that target's detail page — the chart becomes a nav
  // surface too.
  const legend = targets.map(t => {
    const color = colorForTarget(t);
    return `<a href="#/target/${escAttr(t)}" style="display:inline-flex;align-items:center;gap:4px;font-size:11px;margin-right:10px;text-decoration:none;color:var(--text)">
      <span style="display:inline-block;width:10px;height:10px;background:${color};border-radius:2px"></span>${escHTML(t)}
    </a>`;
  }).join('');

  return `<div>
    <svg viewBox="0 0 ${W} ${H}" class="chart-svg" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Daily upload bytes by source">
      ${yTicks.map(t => `<line x1="${ML}" y1="${t.y.toFixed(1)}" x2="${W - MR}" y2="${t.y.toFixed(1)}" class="chart-grid"/><text x="${ML - 6}" y="${(t.y + 4).toFixed(1)}" text-anchor="end" class="chart-axis-text">${escHTML(t.label)}</text>`).join('')}
      ${layers}
      ${xTicks.map(t => `<text x="${t.x.toFixed(1)}" y="${(MT + PH + 18).toFixed(1)}" text-anchor="middle" class="chart-axis-text" style="font-size:10px">${escHTML(t.label)}</text>`).join('')}
    </svg>
    <div style="padding:4px 8px 0;color:var(--text-muted)">${legend}</div>
  </div>`;
}

// renderAnomalyStream shows analyzer anomalies as colored dots on a
// 30-day horizontal timeline. Each dot is one anomaly; severity drives
// the colour. Hover for kind/subject/detail; click on the timestamp
// jumps to the target detail page where the full Report is rendered.
const ANOMALY_SEVERITY_COLOR = {
  critical: 'var(--danger, #ef4444)',
  warning:  'var(--warning, #f59e0b)',
  info:     'var(--accent, #5b6eef)',
};
function renderAnomalyStream(items) {
  const list = items || [];
  if (list.length === 0) {
    return '<div class="chart-empty" style="padding:32px 16px">' + tr('chart.anomalyStream.empty') + '</div>';
  }
  // Compute a 30-day axis ending today UTC. Anomalies older than that
  // are ignored (the cap on the server is also 30 days, but we don't
  // assume).
  const dayMs = 24 * 3600 * 1000;
  const endUTC = Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth(), new Date().getUTCDate());
  const startUTC = endUTC - 29 * dayMs;
  const W = 700, H = 200, ML = 60, MR = 16, MT = 16, MB = 28;
  const PW = W - ML - MR, PH = H - MT - MB;

  // Group anomalies by target row. Limit rows to 8 to keep the chart
  // legible; "+ N more" footer if truncated.
  const byTarget = new Map();
  list.forEach(a => {
    if (!byTarget.has(a.target)) byTarget.set(a.target, []);
    byTarget.get(a.target).push(a);
  });
  const targets = [...byTarget.keys()].sort();
  const visible = targets.slice(0, 8);
  const truncatedCount = targets.length - visible.length;

  const rowH = visible.length > 0 ? PH / visible.length : PH;

  const xs = ts => {
    const t = new Date(ts).getTime();
    const clamped = Math.max(startUTC, Math.min(endUTC + dayMs, t));
    return ML + ((clamped - startUTC) / (30 * dayMs)) * PW;
  };

  // Week ticks
  const xTicks = [];
  for (let i = 0; i < 5; i++) {
    const t = startUTC + i * 7 * dayMs;
    xTicks.push({ x: ML + ((t - startUTC) / (30 * dayMs)) * PW, label: new Date(t).toISOString().slice(5, 10) });
  }
  // Today marker
  const todayX = xs(new Date().toISOString());

  const rowsSvg = visible.map((tgt, ri) => {
    const cy = MT + (ri + 0.5) * rowH;
    const dots = byTarget.get(tgt).map(a => {
      const cx = xs(a.time);
      const color = ANOMALY_SEVERITY_COLOR[a.severity] || ANOMALY_SEVERITY_COLOR.info;
      const tt = `${a.target} · ${a.kind}${a.subject ? ' · ' + a.subject : ''}\n${new Date(a.time).toLocaleString()}${a.detail ? '\n' + a.detail : ''}`;
      return `<circle cx="${cx.toFixed(1)}" cy="${cy.toFixed(1)}" r="4" fill="${color}" stroke="var(--bg)" stroke-width="1"><title>${escAttr(tt)}</title></circle>`;
    }).join('');
    return `<g>
      <a href="#/target/${escAttr(tgt)}" style="cursor:pointer">
        <text x="${(ML - 6).toFixed(1)}" y="${(cy + 4).toFixed(1)}" text-anchor="end" class="chart-axis-text" style="font-size:11px;fill:var(--accent)">${escHTML(tgt)}</text>
      </a>
      <line x1="${ML}" y1="${cy.toFixed(1)}" x2="${(W - MR).toFixed(1)}" y2="${cy.toFixed(1)}" class="chart-grid"/>
      ${dots}
    </g>`;
  }).join('');

  return `<div>
    <svg viewBox="0 0 ${W} ${H}" class="chart-svg" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Analyzer anomaly timeline">
      ${xTicks.map(t => `<text x="${t.x.toFixed(1)}" y="${(MT - 4).toFixed(1)}" text-anchor="middle" class="chart-axis-text" style="font-size:10px">${escHTML(t.label)}</text>`).join('')}
      <line x1="${todayX.toFixed(1)}" y1="${MT}" x2="${todayX.toFixed(1)}" y2="${(H - MB).toFixed(1)}" stroke="var(--accent, #5b6eef)" stroke-width="1" stroke-dasharray="2,3" opacity="0.5"/>
      ${rowsSvg}
    </svg>
    ${truncatedCount > 0 ? `<div style="padding:4px 8px;color:var(--text-muted);font-size:11px">${tr('chart.anomalyStream.moreTargets', {count: truncatedCount})}</div>` : ''}
  </div>`;
}

// renderFleetHeatmap shows per-target, per-day backup status as a grid.
// Reads /api/dashboard/heatmap; each row is one target, columns are
// days (oldest left, today right). Cell colours mirror the GitHub-
// contributions style the per-target heatmap already uses, so an
// operator scanning the dashboard finds the same visual language.
//
// Status palette:
//   ok     → green
//   failed → red
//   mixed  → amber
//   none   → muted grey
//
// Click on any row's label routes to that target's detail page.
function renderFleetHeatmap(rows) {
  if (!rows || rows.length === 0) {
    return '<div class="chart-empty">' + tr('chart.fleetHeatmap.empty') + '</div>';
  }
  const days = rows[0].days ? rows[0].days.length : 30;
  // Cell + row geometry. Row height (cellSize + cellGap) MUST stay
  // ≥ the label font-size in px or labels overlap when many targets
  // are listed. 14 px cell + 4 px gap = 18 px row, comfortable for
  // an 11 px label.
  const cellSize = 14, cellGap = 4;
  const padL = 150, padTop = 24, padR = 16, padBottom = 32;
  const gridW = days * (cellSize + cellGap) - cellGap;
  const W = padL + gridW + padR;
  const H = padTop + rows.length * (cellSize + cellGap) - cellGap + padBottom;

  const statusFill = {
    ok:     'var(--success, #10b981)',
    failed: 'var(--danger, #ef4444)',
    mixed:  'var(--warning, #f59e0b)',
    none:   'var(--bg-input, #2a2a2a)',
  };

  // Truncate over-long target names so they fit padL without
  // bleeding into the grid. The full name is still visible on hover
  // via <title> on the link.
  const truncate = (s, n) => s.length > n ? s.slice(0, n - 1) + '…' : s;

  // Weekly tick on the day axis — too many labels make the row
  // unreadable. Pick every 7th day starting from the right (today).
  const xTicks = [];
  for (let i = days - 1; i >= 0; i -= 7) {
    const day = rows[0].days[i].day;
    const x = padL + i * (cellSize + cellGap) + cellSize / 2;
    const label = day.slice(5); // MM-DD
    xTicks.push({ x, label });
  }

  const rowSvg = rows.map((r, ri) => {
    const y = padTop + ri * (cellSize + cellGap);
    const labelY = y + cellSize / 2 + 4;
    const labelText = truncate(r.target, 20);
    const cells = r.days.map((c, ci) => {
      const x = padL + ci * (cellSize + cellGap);
      const fill = statusFill[c.status] || statusFill.none;
      const tt = `${r.target} · ${c.day}: ${c.runs > 0 ? c.runs + ' run(s), ' + c.status : 'no run'}`;
      return `<rect x="${x}" y="${y}" width="${cellSize}" height="${cellSize}" rx="2" fill="${fill}"><title>${escAttr(tt)}</title></rect>`;
    }).join('');
    return `<g>
      <a href="#/target/${escAttr(r.target)}" style="cursor:pointer">
        <title>${escAttr(r.target)}</title>
        <text x="${(padL - 8).toFixed(1)}" y="${labelY.toFixed(1)}" text-anchor="end" class="chart-axis-text" style="font-size:11px;fill:var(--accent)">${escHTML(labelText)}</text>
      </a>
      ${cells}
    </g>`;
  }).join('');

  // Legend at the bottom — flexbox-style horizontal stack with
  // explicit min-spacing so labels never overlap regardless of width.
  const legend = [
    ['ok', tr('chart.fleetHeatmap.legend.ok')],
    ['mixed', tr('chart.fleetHeatmap.legend.mixed')],
    ['failed', tr('chart.fleetHeatmap.legend.failed')],
    ['none', tr('chart.fleetHeatmap.legend.none')],
  ];
  const legendSpacing = Math.max(110, (W - padL) / legend.length);
  const legendSvg = legend.map(([k, label], i) => {
    const x = padL + i * legendSpacing;
    const y = H - 14;
    return `<rect x="${x}" y="${(y - 9).toFixed(1)}" width="10" height="10" rx="2" fill="${statusFill[k]}"/><text x="${(x + 14).toFixed(1)}" y="${y.toFixed(1)}" class="chart-axis-text" style="font-size:10px">${escHTML(label)}</text>`;
  }).join('');

  // Explicit height on the SVG (matching the natural H) overrides
  // the chart-svg max-height clamp; the min-width keeps the cell
  // grid at native px size on narrow viewports — the parent
  // .chart-card scrolls horizontally rather than scaling the SVG
  // down to where cells become microscopic. chart-svg-tall opts
  // out of the 260 px cap so labels stay readable.
  return `<svg viewBox="0 0 ${W} ${H}" class="chart-svg chart-svg-tall" preserveAspectRatio="xMidYMin meet" role="img" aria-label="Fleet backup heatmap" style="height:${H}px;max-height:${H}px;min-width:${W}px">
    ${xTicks.map(t => `<text x="${t.x.toFixed(1)}" y="${(padTop - 8).toFixed(1)}" text-anchor="middle" class="chart-axis-text" style="font-size:10px">${escHTML(t.label)}</text>`).join('')}
    ${rowSvg}
    ${legendSvg}
  </svg>`;
}

// renderNextRunGantt plots each target's NextRun on a 24-hour timeline
// starting at "now". Surfaces thundering-herd peaks (10 targets all
// scheduled at 02:00) and immediate upcoming work. Bar width is the
// estimated duration when available (median over last successful runs),
// minimum 4 px so a 30s backup doesn't disappear. Suspended sources are
// skipped — they have no nextRun.
function renderNextRunGantt(targets) {
  const now = Date.now();
  const horizonMs = 24 * 3600 * 1000;
  const rows = (targets || [])
    .filter(t => !t.Suspended && t.nextRun)
    .map(t => {
      const ms = new Date(t.nextRun).getTime();
      const durSec = (t.Latest && t.Latest.durationSeconds) ||
                     (t.Latest && t.Latest.encryptedSizeBytes ? 60 : 30);
      return { name: t.Name, dbType: t.DBType, startMs: ms, durSec };
    })
    .filter(r => isFinite(r.startMs) && r.startMs >= now - 60000 && r.startMs < now + horizonMs)
    .sort((a, b) => a.startMs - b.startMs);

  if (rows.length === 0) {
    return '<div class="chart-empty">' + tr('chart.nextRunGantt.empty') + '</div>';
  }
  const W = 700, rowH = 24, padTop = 24, padBottom = 22, padL = 96, padR = 16;
  const PW = W - padL - padR;
  const H = padTop + rows.length * rowH + padBottom;
  const xPos = ms => padL + ((ms - now) / horizonMs) * PW;

  // Hour-tick lines + labels (every 3h)
  const ticks = [];
  for (let h = 0; h <= 24; h += 3) {
    const t = now + h * 3600 * 1000;
    ticks.push({ x: xPos(t), label: new Date(t).toLocaleTimeString([], {hour: '2-digit', minute: '2-digit'}) });
  }

  // Count overlapping bars at any given minute → flag thundering-herd
  // peaks. Two or more bars whose start time falls within the same
  // 60-second window get a subtle warning border.
  const bucketMin = new Map();
  rows.forEach(r => {
    const k = Math.floor(r.startMs / 60000);
    bucketMin.set(k, (bucketMin.get(k) || 0) + 1);
  });

  const bars = rows.map((r, i) => {
    const x = xPos(r.startMs);
    const widthPx = Math.max(4, (r.durSec / 86400) * PW);
    const y = padTop + i * rowH;
    const tier = bucketMin.get(Math.floor(r.startMs / 60000)) || 1;
    const cls = tier > 1 ? 'gantt-bar gantt-bar-peak' : 'gantt-bar';
    const when = new Date(r.startMs).toLocaleString();
    const tooltip = `${r.name} (${r.dbType}) — ${when}, est ${fmtDurationShort(r.durSec)}${tier > 1 ? ` — ⚠ ${tier} targets fire in the same minute` : ''}`;
    return `<g>
      <text x="${padL - 6}" y="${(y + rowH/2 + 4).toFixed(1)}" text-anchor="end" class="chart-axis-text" style="font-size:11px"><tspan>${escHTML(r.name)}</tspan></text>
      <rect x="${x.toFixed(1)}" y="${(y + 5).toFixed(1)}" width="${widthPx.toFixed(1)}" height="${(rowH - 10).toFixed(1)}" rx="2" class="${cls}"><title>${escAttr(tooltip)}</title></rect>
    </g>`;
  }).join('');

  return `<svg viewBox="0 0 ${W} ${H}" class="chart-svg" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Upcoming backup runs">
    ${ticks.map(t => `<line x1="${t.x.toFixed(1)}" y1="${padTop - 4}" x2="${t.x.toFixed(1)}" y2="${(H - padBottom + 4).toFixed(1)}" class="chart-grid"/>
       <text x="${t.x.toFixed(1)}" y="${(H - padBottom + 16).toFixed(1)}" text-anchor="middle" class="chart-axis-text" style="font-size:10px">${escHTML(t.label)}</text>`).join('')}
    <line x1="${padL}" y1="${padTop - 4}" x2="${padL}" y2="${(H - padBottom + 4).toFixed(1)}" class="chart-axis"/>
    ${bars}
  </svg>`;
}

// Approximation: each destination receives every target's most recent
// successful dump. The bar shows that single snapshot, not historical
// retention. Good enough to spot "destination A holds 90% of the bytes".
function renderStorageByDestination(targets, dests) {
  if (!targets || targets.length === 0 || !dests || dests.length === 0) return '';

  const byDest = new Map();
  dests.forEach(d => byDest.set(d.name, []));
  targets.forEach(t => {
    if (!t.Latest || t.Latest.status === 'failed') return;
    const size = t.Latest.encryptedSizeBytes || 0;
    if (size === 0) return;
    const targetDests = (t.Destinations && t.Destinations.length > 0)
      ? t.Destinations
      : dests.map(d => d.name); // unrestricted source fans out everywhere
    targetDests.forEach(name => {
      if (!byDest.has(name)) byDest.set(name, []);
      byDest.get(name).push({ target: t.Name, size });
    });
  });

  const rows = [];
  for (const [name, items] of byDest) {
    items.sort((a, b) => b.size - a.size);
    const total = items.reduce((s, i) => s + i.size, 0);
    rows.push({ name, items, total });
  }
  rows.sort((a, b) => b.total - a.total);

  const maxTotal = rows.reduce((m, r) => Math.max(m, r.total), 0);
  if (maxTotal === 0) return '';

  return `<div class="chart-card" style="margin-bottom:16px">
    <h3>${tr('card.storageByDestination')} <span class="chart-card-sub">${tr('card.storageSubtitle')}</span></h3>
    <div class="stack-bar-list">
      ${rows.map(r => `
        <div class="stack-bar-row">
          <div class="stack-bar-label" title="${escAttr(r.name)}">${escHTML(r.name)}</div>
          <div class="stack-bar" style="width:${maxTotal > 0 ? (r.total / maxTotal * 100).toFixed(1) : 0}%">
            ${r.items.map(it => `<div class="stack-segment" style="width:${(it.size / r.total * 100).toFixed(2)}%;background:${colorForTarget(it.target)}" title="${escAttr(it.target + ' — ' + humanBytes(it.size))}"></div>`).join('')}
          </div>
          <div class="stack-bar-total">${humanBytes(r.total)}</div>
        </div>
      `).join('')}
    </div>
  </div>`;
}

// colorForTarget is defined once above (near hashHue) — do not duplicate.

// Per-table row-count trend on the target detail page. Builds a name→history
// map across all successful runs, then renders the latest run's tables sorted
// by current row count, with a tiny sparkline showing direction. Tables that
// only ever appeared in one run still get a single point.
function renderTablesCard(runs) {
  const sorted = (runs || [])
    .filter(r => r.status !== 'failed' && r.stats && Array.isArray(r.stats.tables))
    .map(r => ({ ts: parseTsCompact(r.timestamp), tables: r.stats.tables }))
    .filter(r => r.ts !== null)
    .sort((a, b) => a.ts - b.ts);
  if (sorted.length === 0) return '';

  const history = new Map();
  sorted.forEach(r => {
    r.tables.forEach(t => {
      if (!history.has(t.name)) history.set(t.name, []);
      history.get(t.name).push(t.rowCount);
    });
  });

  const latest = sorted[sorted.length - 1].tables.slice()
    .sort((a, b) => (b.rowCount || 0) - (a.rowCount || 0));
  if (latest.length === 0) return '';

  return `<div class="chart-card">
    <h3>${tr('card.tablesLatest')} <span class="chart-card-sub">${tr(latest.length === 1 ? 'card.tablesSubtitle' : 'card.tablesSubtitlePlural', {count: latest.length, runs: sorted.length})}</span></h3>
    <div class="table-scroll">
    <table class="tbl-compact">
      <thead><tr><th>${tr('verification.table')}</th><th class="num">${tr('table.runCount')}</th><th>Trend</th><th class="num">${tr('table.size')}</th></tr></thead>
      <tbody>${latest.map(t => `<tr>
        <td class="cell-mono">${escHTML(t.name)}</td>
        <td class="num">${formatNum(t.rowCount)}</td>
        <td>${renderSparkline(history.get(t.name) || [])}</td>
        <td class="num">${humanBytes(t.sizeBytes)}</td>
      </tr>`).join('')}</tbody>
    </table>
    </div>
  </div>`;
}

// Tiny inline SVG line — width 80, height 20, no axes. Direction-coloured:
// last value vs first → green if up >5%, red if down >5%, muted otherwise.
function renderSparkline(values) {
  if (!values || values.length === 0) return '';
  if (values.length === 1) {
    return '<svg viewBox="0 0 80 20" class="sparkline"><circle cx="40" cy="10" r="2" class="sparkline-point"/></svg>';
  }
  const W = 80, H = 20, pad = 2;
  const min = Math.min(...values), max = Math.max(...values);
  const range = (max - min) || 1;
  const xStep = (W - 2 * pad) / (values.length - 1);
  const path = values.map((v, i) => {
    const x = pad + i * xStep;
    const y = pad + (H - 2 * pad) - ((v - min) / range) * (H - 2 * pad);
    return (i === 0 ? 'M' : 'L') + x.toFixed(1) + ',' + y.toFixed(1);
  }).join(' ');
  const first = values[0], last = values[values.length - 1];
  let cls = 'sparkline-flat';
  if (first > 0) {
    if (last < first * 0.95) cls = 'sparkline-down';
    else if (last > first * 1.05) cls = 'sparkline-up';
  }
  return `<svg viewBox="0 0 ${W} ${H}" class="sparkline ${cls}"><path d="${path}"/></svg>`;
}

function formatNum(n) {
  if (n == null) return '—';
  if (n >= 1e9) return (n / 1e9).toFixed(1) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
  return String(n);
}
