// pages/state.js — the one place cross-page mutable state lives.
// Page modules are ES modules (private scope); anything two pages or the
// router mutate goes through this object so there is exactly one instance.
export const state = {
  // Monotonic counter bumped ONLY on user-initiated navigation. Async
  // renderers capture it at entry and drop their write when it moved on,
  // so a slow fetch for a page the user already left cannot clobber the
  // page they are looking at now. SSE re-renders reuse the current gen.
  renderGen: 0,
  // Per-second progress-bar tick shared by the Jobs page and target detail.
  jobProgressTimer: null,
  // Dashboard caches (stale-while-revalidate; see dashboard.js).
  slowProbes: { health: [], consistency: [], lastFetch: 0 },
  fleetSummary: {
    heatmap: [], storage: [], anomalies: [],
    durations: [], verificationDaily: [],
    lastFetch: 0,
  },
  // Destination stats cache shared by the dashboard donut and the
  // destinations page.
  destStatsCache: { stats: [], lastFetch: 0 },
  // Target-detail run history cache (keyed by target name).
  targetRuns: { name: '', runs: null, lastFetch: 0, failed: false },
};
export function newRenderGen() { return ++state.renderGen; }
export function isStaleRender(gen) { return gen !== state.renderGen; }
