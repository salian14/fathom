// Checks for internal/web/static/app.js, run by `make check` when node is on
// PATH and skipped when it is not -- the same graceful degradation the helm
// lint uses. The UI has no build step and no package.json, and it is not worth
// acquiring either; this needs nothing but a node binary.
//
// What is tested here is the logic that has no server-side counterpart and
// therefore no other guard: which prior turns get replayed (the model's context
// is finite and the client owns the transcript), which error envelope a failure
// arrived in, and that the chat list survives its own edge cases.
//
//   node internal/web/app_test.mjs      # from fathom-front/

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import vm from 'node:vm';
import assert from 'node:assert/strict';

// Values built inside the vm realm have that realm's prototypes, so
// deepStrictEqual rejects structurally identical objects. Compare as JSON.
const same = (a, b, msg) => assert.equal(JSON.stringify(a), JSON.stringify(b), msg);

// Minimal DOM stub -- enough that app.js can load, wire itself up and render.
// If the render path touches something that is not here, this throws, which is
// itself the point.
function mkEl(tag = 'div') {
  const el = {
    tagName: tag, children: [], _text: '', style: {}, dataset: {},
    className: '', title: '', value: '', disabled: false, hidden: false,
    scrollTop: 0, scrollHeight: 0, type: '',
    append(...n) { this.children.push(...n); },
    replaceChildren(...n) { this.children = n; },
    remove() {},
    addEventListener() {},
    setAttribute() {},
    hasChildNodes() { return this.children.length > 0; },
    requestSubmit() {},
    focus() {},
  };
  Object.defineProperty(el, 'textContent', {
    get() { return this._text; },
    set(v) { this._text = String(v); this.children = []; },
  });
  return el;
}

const ids = {};
const ctx = {
  console,
  document: {
    createElement: (t) => mkEl(t),
    createTextNode: (t) => ({ nodeType: 3, textContent: String(t) }),
    getElementById: (id) => (ids[id] ||= mkEl()),
  },
  localStorage: {
    store: {},
    getItem(k) { return k in this.store ? this.store[k] : null; },
    setItem(k, v) { this.store[k] = String(v); },
    removeItem(k) { delete this.store[k]; },
  },
  crypto: { randomUUID: () => 'id-' + Math.random().toString(16).slice(2) },
  fetch: async () => ({ ok: true, status: 200, text: async () => 'ok' }),
  setInterval: () => 0,
  setTimeout: () => 0,
  clearInterval: () => {},
  clearTimeout: () => {},
  AbortController: class { constructor() { this.signal = {}; } abort() {} },
  Date, Math, JSON, String, Object, Array, Error, Promise,
};
ctx.globalThis = ctx;
vm.createContext(ctx);
const here = path.dirname(fileURLToPath(import.meta.url));
vm.runInContext(readFileSync(path.join(here, 'static', 'app.js'), 'utf8'), ctx);

let pass = 0;
const check = (name, fn) => { fn(); console.log('  ok  ' + name); pass++; };

// --- buildHistory ---------------------------------------------------------

check('replays prior turns as {role, content} only', () => {
  const { history, truncated } = ctx.buildHistory([
    { role: 'user', content: 'a' },
    { role: 'assistant', content: 'b', meta: { steps: [] } },
  ]);
  same(history, [
    { role: 'user', content: 'a' },
    { role: 'assistant', content: 'b' },
  ], 'meta must not go on the wire');
  assert.equal(truncated, false);
});

check('drops failed turns -- the agent never produced them', () => {
  const { history } = ctx.buildHistory([
    { role: 'user', content: 'a' },
    { role: 'assistant', content: '502 — agent unreachable', error: true },
  ]);
  same(history, [{ role: 'user', content: 'a' }]);
});

check('caps at 12 messages and reports the truncation', () => {
  const many = Array.from({ length: 30 }, (_, i) => ({
    role: i % 2 ? 'assistant' : 'user', content: 'm' + i,
  }));
  const { history, truncated } = ctx.buildHistory(many);
  assert.equal(history.length, 12);
  assert.equal(history[11].content, 'm29', 'must keep the most recent turns');
  assert.equal(truncated, true);
});

check('caps on characters too, so a few huge turns still fit', () => {
  const huge = Array.from({ length: 6 }, (_, i) => ({
    role: 'user', content: 'x'.repeat(9000) + i,
  }));
  const { history, truncated } = ctx.buildHistory(huge);
  const chars = history.reduce((n, m) => n + m.content.length, 0);
  assert.ok(chars <= 24000 + 9001, 'character budget not enforced: ' + chars);
  assert.ok(history.length < 6);
  assert.equal(truncated, true);
});

check('never empties the history down to nothing', () => {
  const { history } = ctx.buildHistory([{ role: 'user', content: 'y'.repeat(100000) }]);
  assert.equal(history.length, 1, 'a single oversized turn must survive the loop');
});

check('empty transcript is an empty replay', () => {
  const { history, truncated } = ctx.buildHistory([]);
  same(history, []);
  assert.equal(truncated, false);
});

// --- errorMessage ---------------------------------------------------------

check('reads the agent/proxy {"error": ...} envelope', () => {
  assert.equal(ctx.errorMessage({ error: 'turn exceeded 180s' }, 504), 'turn exceeded 180s');
});

check('reads FastAPI\'s {"detail": [...]} envelope', () => {
  assert.equal(
    ctx.errorMessage({ detail: [{ msg: 'String should have at least 1 character' }] }, 422),
    'String should have at least 1 character');
});

check('falls back rather than rendering "undefined"', () => {
  assert.match(ctx.errorMessage({}, 422), /invalid/);
  assert.match(ctx.errorMessage(null, 500), /failed/);
});

// --- rendering ------------------------------------------------------------

check('fences become pre/code, prose becomes paragraphs', () => {
  const parent = mkEl();
  ctx.formatInto(parent, 'before\n\nnext\n\n```yaml\nkey: value\n```\nafter');
  const tags = parent.children.map((c) => c.tagName);
  same(tags, ['p', 'p', 'pre', 'p']);
  const code = parent.children[2].children[0];
  assert.equal(code.textContent, 'key: value', 'language tag must be dropped');
});

check('empty answer still renders a node', () => {
  const parent = mkEl();
  ctx.formatInto(parent, '');
  assert.equal(parent.children.length, 1);
});

check('a step with no result is shown as unfinished', () => {
  const node = ctx.metaNode({ steps: [{ tool: 'k8s_get', args: { resource: 'pods' } }], iterations: 1 });
  const text = JSON.stringify(node);
  assert.ok(text.includes('no result'), 'an unanswered tool call must say so');
});

// --- chat lifecycle -------------------------------------------------------

// `let chats` is a lexical binding, so it is not reachable on the vm global.
// Going through the accessors is the better test anyway -- it exercises the
// same surface the buttons do.

check('a first chat exists and persists after load', () => {
  const chat = ctx.activeChat();
  assert.ok(chat, 'no active chat after load');
  assert.equal(chat.messages.length, 0);
  const stored = JSON.parse(ctx.localStorage.getItem('fathom.chats'));
  assert.equal(stored.length, 1, 'the chat was not written to localStorage');
});

check('new chat switches the active transcript', () => {
  const first = ctx.activeChat().id;
  ctx.newChat();
  const second = ctx.activeChat().id;
  assert.notEqual(first, second);
  assert.equal(JSON.parse(ctx.localStorage.getItem('fathom.chats')).length, 2);
  assert.equal(ctx.localStorage.getItem('fathom.activeChat'), second);
});

check('deleting the active chat falls back to another', () => {
  const doomed = ctx.activeChat().id;
  ctx.deleteChat(doomed);
  const now = ctx.activeChat();
  assert.ok(now, 'no chat became active after deleting the active one');
  assert.notEqual(now.id, doomed);
});

check('deleting the last chat leaves no active id', () => {
  ctx.deleteChat(ctx.activeChat().id);
  assert.equal(ctx.activeChat(), null);
  assert.equal(ctx.localStorage.getItem('fathom.activeChat'), null);
});

check('corrupt storage is survivable', () => {
  ctx.localStorage.setItem('fathom.chats', 'not json at all');
  ctx.load();
  assert.equal(ctx.activeChat(), null, 'load() must not throw or invent a chat');
});

console.log('\n' + pass + ' checks passed');
