// Test harness for the SPA. core.js and charts.js are classic scripts; the
// page renderers are ES modules (js/pages/*.js + js/pages.js) that reference
// the classic helpers through the global scope. Everything is loaded into one
// vm context with just enough of a DOM stub for the top-level statements to
// run: the modules are evaluated in dependency order with their import/export
// statements stripped, which mirrors how the browser resolves them against
// the same global scope. app.js is deliberately excluded — it boots the app
// (fetch, EventSource) on load.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import vm from 'node:vm';

const here = dirname(fileURLToPath(import.meta.url));
export const jsDir = join(here, '..');
export const staticDir = join(jsDir, '..');

function stubElement() {
  const el = {
    children: [], dataset: {}, style: {}, attributes: {},
    classList: { add() {}, remove() {}, toggle() { return false; }, contains() { return false; } },
    addEventListener() {}, removeEventListener() {}, appendChild(c) { this.children.push(c); return c; },
    remove() {}, setAttribute(k, v) { this.attributes[k] = v; }, getAttribute(k) { return this.attributes[k]; },
    removeAttribute(k) { delete this.attributes[k]; }, querySelector() { return null; }, querySelectorAll() { return []; },
    focus() {}, click() {}, contains() { return false; }, matches() { return false; }, closest() { return null; },
    textContent: '', innerHTML: '', value: '', hidden: false, offsetParent: null,
  };
  return el;
}

export function loadSPA() {
  const sandbox = {
    console,
    document: {
      querySelector: () => stubElement(), querySelectorAll: () => [], createElement: () => stubElement(),
      addEventListener() {}, documentElement: stubElement(), activeElement: null, hidden: false,
      getElementById: () => null, body: stubElement(),
    },
    localStorage: { getItem() { return null; }, setItem() {} },
    navigator: { language: 'en-US' },
    location: { hash: '' },
    performance: { now: () => Date.now() },
    requestAnimationFrame: fn => setTimeout(fn, 0),
    setTimeout, clearTimeout, setInterval, clearInterval,
    URL, URLSearchParams, AbortController, Date, Math, JSON, Number, String, Object, Array, Set, Map, Promise, Error, RegExp,
    encodeURIComponent, decodeURIComponent, isNaN, parseFloat, parseInt,
  };
  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  const ctx = vm.createContext(sandbox);
  for (const f of ['core.js', 'charts.js']) {
    vm.runInContext(readFileSync(join(jsDir, f), 'utf8'), ctx, { filename: f });
  }
  for (const f of moduleOrder) {
    vm.runInContext(stripModuleSyntax(readFileSync(join(jsDir, f), 'utf8')), ctx, { filename: f });
  }
  return ctx;
}

// Evaluation order for the page modules: state and router first (every page
// imports them), then the pages, then the thin entry module.
export const moduleOrder = [
  'pages/state.js', 'pages/router.js',
  'pages/dashboard.js', 'pages/sources.js', 'pages/destinations.js', 'pages/alerts.js',
  'pages/jobs.js', 'pages/audit.js', 'pages/target.js', 'pages/settings.js', 'pages/agekeys.js',
  'pages.js',
];

// stripModuleSyntax turns an ES module into a classic script for the vm
// context: single-line import statements and re-exports go away, `export`
// prefixes are dropped. Modules in this tree only use those forms.
export function stripModuleSyntax(src) {
  return src
    .replace(/^import .*;\s*$/gm, '')
    .replace(/^export \{[^}]*\} from .*;\s*$/gm, '')
    .replace(/^export (async function|function|const|let) /gm, '$1 ');
}

// Top-level const/let bindings of the loaded scripts live in the context's
// declarative record, not on the global object, so tests read them with evalIn.
export function evalIn(ctx, expr) { return vm.runInContext(expr, ctx); }

export const allJS = ['core.js', 'charts.js', ...moduleOrder, 'app.js'];

export function sources() {
  const files = allJS.map(f => [f, readFileSync(join(jsDir, f), 'utf8')]);
  files.push(['index.html', readFileSync(join(staticDir, 'index.html'), 'utf8')]);
  return files;
}
