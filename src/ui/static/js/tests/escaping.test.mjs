import { test } from 'node:test';
import assert from 'node:assert/strict';
import { loadSPA } from './_harness.mjs';

const g = loadSPA();

test('escHTML escapes element-content metacharacters and nothing else', () => {
  assert.equal(g.escHTML('<img src=x onerror=alert(1)>&'), '&lt;img src=x onerror=alert(1)&gt;&amp;');
  assert.equal(g.escHTML(null), '');
  assert.equal(g.escHTML(`it's "quoted"`), `it's "quoted"`);
});

test('escAttr escapes both quote styles and backtick', () => {
  assert.equal(g.escAttr(`a"b'c\`d<e>&`), 'a&quot;b&#39;c&#96;d&lt;e&gt;&amp;');
  assert.equal(g.escAttr(undefined), '');
});

test('escJS unicode-escapes every string/attribute breakout character', () => {
  const dangerous = ["'", '"', '<', '>', '&', '\n', '\r', '\\', '\u2028', '\u2029', '`'];
  const out = g.escJS('x' + dangerous.join('') + 'y');
  // Every dangerous char must have become a \uXXXX sequence; once those are
  // removed, nothing from the dangerous set may remain (the sequences
  // themselves contain a backslash, hence the strip-first).
  const residue = out.replace(/\\u[0-9a-f]{4}/g, '');
  assert.equal(residue, 'xy', `escJS output ${out} left unescaped characters`);
  assert.equal(g.escJS("a'b"), 'a\\u0027b');
  assert.equal(g.escJS(null), '');
});

test('tr() HTML-escapes interpolated vars; trText() does not', () => {
  g._i18nInstall('en', { t: { msg: 'Failed: {error}', plain: 'Hello' } });
  const evil = '<img src=x onerror=alert(1)>';
  assert.equal(g.tr('t.msg', { error: evil }), 'Failed: &lt;img src=x onerror=alert(1)&gt;');
  assert.equal(g.trText('t.msg', { error: evil }), 'Failed: ' + evil);
  assert.equal(g.tr('t.plain'), 'Hello');
  assert.equal(g.tr('missing.key'), 'missing.key', 'missing keys fall back to the key itself');
  assert.equal(g.tr('t.msg', {}), 'Failed: {error}', 'missing vars are left visible');
});

test('actionAttrs renders escaped data-arg attributes in order', () => {
  const out = g.actionAttrs('view-meta', 'a"b', 'ts', null);
  assert.equal(out, 'data-action="view-meta" data-arg="a&quot;b" data-arg2="ts" data-arg3=""');
  assert.equal(g.actionAttrs('x'), 'data-action="x"');
});

test('targetHref URL-encodes free-form target names', () => {
  assert.equal(g.targetHref('my db/#?%'), '#/target/my%20db%2F%23%3F%25');
  assert.equal(decodeURIComponent(g.targetHref('a.b-c').slice('#/target/'.length)), 'a.b-c');
});

test('jsonHighlight escapes markup inside JSON values before wrapping in spans', () => {
  const out = g.jsonHighlight(JSON.stringify({ '<k>': '<v>&' }, null, 2));
  assert.ok(!out.includes('<k>') && !out.includes('<v>'), out);
  assert.ok(out.includes('json-key') && out.includes('json-str'));
});
