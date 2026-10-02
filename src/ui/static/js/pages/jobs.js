// pages/jobs.js — Jobs page, next-run formatting, progress bars.
// Shared helpers ($, tr, api, escHTML, …) come from the classic core.js /
// charts.js scripts via the global scope; cross-page state lives in state.js.
import { state, isStaleRender } from './state.js';
import { registerPage } from './router.js';
import { parseDurationSec } from './target.js';

// --- Jobs ---


// fmtNextRun renders a CronJob's next fire time as "in 4h 32m (02:00)"
// or "in 12d (Mon 02:00)" depending on horizon. Pass an ISO timestamp
// string from the target's NextRun field. Returns '' if input is falsy
// or already in the past (next tick is imminent / SSE catch-up race).
export function fmtNextRun(iso) {
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
export function renderProgressCell(j) {
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
  return `<span style="color:var(--text-muted)">${tr('target.progressNoEstimate', {elapsed: fmtDurationShort(elapsed)})}</span>`;
}

export let _jobsLastGood = null;
export async function renderJobs(loading = true) {
  const _gen = state.renderGen;
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
          <th class="sortable" ${sortableTh('jobs','name')}>${tr('nav.jobs')}${sortIndicator('jobs','name')}</th>
          <th class="sortable" ${sortableTh('jobs','target')}>${tr('table.target')}${sortIndicator('jobs','target')}</th>
          <th class="sortable" ${sortableTh('jobs','status')}>${tr('table.status')}${sortIndicator('jobs','status')}</th>
          <th class="sortable" ${sortableTh('jobs','startTime')}>${tr('table.started')}${sortIndicator('jobs','startTime')}</th>
          <th class="sortable" ${sortableTh('jobs','duration')}>${tr('table.duration')}${sortIndicator('jobs','duration')}</th>
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
  if (state.jobProgressTimer) { clearInterval(state.jobProgressTimer); state.jobProgressTimer = null; }
  const runningJobs = sortedJobs.filter(j => j.status === 'running');
  if (runningJobs.length > 0) {
    state.jobProgressTimer = setInterval(() => {
      // If user navigated away, the cells will be gone; guard against null.
      runningJobs.forEach(j => {
        const row = document.querySelector(`tr[data-job-name="${CSS.escape(j.name)}"] .job-duration-cell`);
        if (row) row.innerHTML = renderProgressCell(j);
      });
    }, 1000);
  }
}
registerPage('jobs', loading => renderJobs(loading));
