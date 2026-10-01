import { test } from 'node:test';
import assert from 'node:assert/strict';
import { loadSPA, sources } from './_harness.mjs';

// The strict CSP (script-src 'self') forbids inline handlers, so every
// clickable element is wired through data-action / data-submit / data-change
// and a registry in core.js / pages.js. These tests keep the two sides in sync.

test('no inline event-handler attributes remain in templates or the shell', () => {
  // Attribute form only (`onclick="…"`): prose in comments may mention
  // `onerror=` when explaining an XSS payload.
  const re = /\son(click|change|submit|input|keydown|keyup|load|error|focus|blur|mouseover)\s*=\s*["']/i;
  for (const [name, src] of sources()) {
    const hit = src.match(re);
    assert.ok(hit === null, `${name} still has an inline handler: ${hit && hit[0]}`);
  }
});

test('every data-action/data-submit/data-change used in a template is registered', () => {
  const g = loadSPA();
  const registered = new Set(g.registeredActions());
  const used = new Set();
  for (const [, src] of sources()) {
    for (const m of src.matchAll(/data-(?:action|submit|change)="([a-z0-9-]+)"/g)) used.add(m[1]);
    for (const m of src.matchAll(/actionAttrs\('([a-z0-9-]+)'/g)) used.add(m[1]);
  }
  assert.ok(used.size > 20, 'template scan found suspiciously few actions: ' + used.size);
  for (const name of used) {
    assert.ok(registered.has(name), `data-action "${name}" is used in a template but never registered`);
  }
});

test('sortableTh carries the sort action, keyboard reachability and aria-sort', () => {
  const g = loadSPA();
  const out = g.sortableTh('runs', 'timestamp');
  assert.match(out, /data-action="sort" data-arg="runs" data-arg2="timestamp"/);
  assert.match(out, /role="button" tabindex="0" aria-sort="descending"/);
  assert.match(g.sortableTh('runs', 'status'), /aria-sort="none"/);
});
