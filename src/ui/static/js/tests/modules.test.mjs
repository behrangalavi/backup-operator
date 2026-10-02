import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { loadSPA, sources, moduleOrder, jsDir, staticDir, evalIn } from './_harness.mjs';

// The page renderers are ES modules split along the old pages.js section
// markers. These tests guard the seams: every page registers itself with the
// router, every module in pages/ is part of the load order, and the thin
// pages.js entry re-exports what app.js imports.

test('every page module is in the harness load order and registers a renderer', () => {
  const onDisk = readdirSync(join(jsDir, 'pages')).filter(f => f.endsWith('.js')).map(f => 'pages/' + f).sort();
  assert.deepEqual(onDisk, moduleOrder.filter(f => f.startsWith('pages/')).sort(), 'js/pages/*.js and moduleOrder differ');
  const g = loadSPA();
  const pages = new Set(g.registeredPages());
  for (const p of ['dashboard', 'sources', 'destinations', 'jobs', 'target', 'alerts', 'audit', 'age-keys', 'settings']) {
    assert.ok(pages.has(p), `page "${p}" is not registered with the router`);
  }
  assert.equal(pages.size, 9);
});

test('shared state is a single object and the render-gen guard works', () => {
  const g = loadSPA();
  const state = evalIn(g, 'state');
  assert.equal(typeof state, 'object');
  for (const k of ['renderGen', 'jobProgressTimer', 'slowProbes', 'fleetSummary', 'destStatsCache', 'targetRuns']) {
    assert.ok(k in state, `state.${k} missing`);
  }
  const gen = g.newRenderGen();
  assert.equal(g.isStaleRender(gen), false);
  g.newRenderGen();
  assert.equal(g.isStaleRender(gen), true);
  // No page module may keep its own copy of the shared caches.
  for (const [name, src] of sources()) {
    if (!name.startsWith('pages/') || name === 'pages/state.js') continue;
    assert.doesNotMatch(src, /^let (_fleetSummary|_slowProbes|_targetRuns|_destStatsCache|jobProgressTimer|_renderGen)\b/m, name + ' redeclares shared state');
  }
});

test('app.js imports only what pages.js re-exports', () => {
  const app = readFileSync(join(jsDir, 'app.js'), 'utf8');
  const pagesJs = readFileSync(join(jsDir, 'pages.js'), 'utf8');
  const m = app.match(/import \{([^}]*)\} from '\.\/pages\.js';/);
  assert.ok(m, 'app.js must import from ./pages.js');
  const exported = [...pagesJs.matchAll(/export \{([^}]*)\} from/g)].flatMap(x => x[1].split(',').map(s => s.trim()));
  for (const name of m[1].split(',').map(s => s.trim()).filter(Boolean)) {
    assert.ok(exported.includes(name), `app.js imports ${name} which pages.js does not re-export`);
  }
});

test('no English tooltip literals remain in JS templates; every tooltip key exists in en, de and fr', () => {
  const dicts = Object.fromEntries(['en', 'de', 'fr'].map(l => [l, JSON.parse(readFileSync(join(staticDir, 'i18n', l + '.json'), 'utf8'))]));
  const lookup = (d, key) => key.split('.').reduce((o, k) => (o && typeof o === 'object') ? o[k] : undefined, d);
  const used = new Set();
  for (const [name, src] of sources()) {
    if (!name.endsWith('.js')) continue;
    // A static title="…" whose value starts with a letter is a hard-coded
    // English string; dynamic values start with "${" or are concatenated ('"…).
    const hit = src.match(/title="[A-Za-z][^"]*"/);
    assert.equal(hit, null, `${name} still has a literal tooltip: ${hit && hit[0]}`);
    for (const m of src.matchAll(/tr(?:Text)?\('(tooltip\.[a-zA-Z0-9_.]+)'/g)) used.add(m[1]);
  }
  assert.ok(used.size >= 40, 'suspiciously few tooltip keys found: ' + used.size);
  for (const key of used) {
    for (const [lang, d] of Object.entries(dicts)) {
      assert.equal(typeof lookup(d, key), 'string', `${key} missing in ${lang}.json`);
    }
  }
  // Static shell tooltips go through data-i18n-title.
  const html = readFileSync(join(staticDir, 'index.html'), 'utf8');
  for (const m of html.matchAll(/<(?:a|button)[^>]*title="[^"]+"[^>]*>/g)) {
    assert.match(m[0], /data-i18n-title=/, 'index.html tooltip without data-i18n-title: ' + m[0].slice(0, 80));
  }
});

test('theme: light token blocks are identical, toggle is registered, charts use tokens', () => {
  const css = readFileSync(join(staticDir, 'style.css'), 'utf8');
  const blocks = [...css.matchAll(/(?::root:not\(\[data-theme="dark"\]\)|:root\[data-theme="light"\]) \{([\s\S]*?)\n\s*\}/g)].map(m => m[1].replace(/\s+/g, ' ').trim());
  assert.equal(blocks.length, 2, 'expected a prefers-color-scheme block and a data-theme=light block');
  assert.equal(blocks[0], blocks[1], 'the two light token blocks drifted apart');
  const darkTokens = [...css.slice(css.indexOf(':root {'), css.indexOf('}', css.indexOf(':root {'))).matchAll(/--([a-z0-9-]+):/g)].map(m => m[1]);
  for (const t of darkTokens) {
    if (['sidebar-w', 'radius', 'radius-sm'].includes(t)) continue;
    assert.ok(blocks[0].includes('--' + t + ':'), `light theme is missing token --${t}`);
  }
  // No hard-coded colours outside the token blocks.
  const body = css.slice(css.indexOf(':root[data-theme="light"]'));
  const afterTokens = body.slice(body.indexOf('}') + 1);
  assert.equal(afterTokens.match(/#[0-9a-fA-F]{3,6}\b/), null, 'style.css has a hard-coded hex colour outside the token blocks');
  const g = loadSPA();
  assert.ok(new Set(g.registeredActions()).has('set-theme'), 'theme picker action not registered');
  const charts = readFileSync(join(jsDir, 'charts.js'), 'utf8');
  assert.equal(charts.match(/#[0-9a-fA-F]{3,6}\b/), null, 'charts.js still hard-codes a hex colour');
  const html = readFileSync(join(staticDir, 'index.html'), 'utf8');
  assert.match(html, /id="themePicker"[^>]*data-change="set-theme"/);
});
