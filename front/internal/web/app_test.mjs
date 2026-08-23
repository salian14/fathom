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

import { makeContext, mkEl, same, textOf, check, report } from './domstub.mjs';

// The DOM stub lives in domstub.mjs so this file and evals_test.mjs cannot drift
// apart on what the page looks like.
const ctx = makeContext();
vm.createContext(ctx);
const here = path.dirname(fileURLToPath(import.meta.url));
vm.runInContext(readFileSync(path.join(here, 'static', 'app.js'), 'utf8'), ctx);

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

// --- remediation ----------------------------------------------------------

// Flatten a rendered node to the text it would show, so a check can assert on
// content without knowing the element tree.

check('renders the proposed fix, its target and its commands', () => {
  const node = ctx.remediationNode({
    action_required: true,
    summary: 'Raise the memory limit on oom-app.',
    target_kind: 'Pod',
    target_name: 'oom-app',
    namespace: 'fathom-eval-oomkill',
    commands: ['kubectl -n fathom-eval-oomkill set resources pod/oom-app --limits=memory=256Mi'],
  });
  const text = textOf(node);
  assert.ok(text.includes('Suggested fix'), text);
  assert.ok(text.includes('Raise the memory limit on oom-app.'), text);
  assert.ok(text.includes('Pod oom-app in fathom-eval-oomkill'), text);
  assert.ok(text.includes('--limits=memory=256Mi'), text);
});

check('a healthy cluster does not get an incident treatment', () => {
  const node = ctx.remediationNode({
    action_required: false,
    summary: 'Nothing is wrong; no action needed.',
    commands: ['kubectl delete pod healthy-app'],
  });
  assert.ok(node.className.includes('none'), node.className);
  const text = textOf(node);
  assert.ok(text.includes('No action needed'), text);
  // Commands are suppressed when there is nothing to act on -- rendering one
  // beside "no action needed" invites the reader to run it.
  assert.ok(!text.includes('kubectl delete'), text);
});

check('the fix is rendered as text, never as markup', () => {
  // Same rule as the rest of the render path: this is model output.
  const node = ctx.remediationNode({
    action_required: true,
    summary: '<img src=x onerror="alert(1)">',
    commands: ['<script>alert(2)</script>'],
  });
  const walk = (n) => {
    assert.equal(n.innerHTML, undefined, 'innerHTML must never be assigned');
    n.children.forEach(walk);
  };
  walk(node);
  assert.ok(textOf(node).includes('<img src=x'), 'the tag must survive as literal text');
});

check('the remediation never goes back on the wire', () => {
  // /chat is stateless and the client owns the transcript, so replaying a fix we
  // were given would feed the model its own output as if the user had said it.
  const { history } = ctx.buildHistory([
    { role: 'user', content: 'why?' },
    { role: 'assistant', content: 'because.', remediation: { action_required: true } },
  ]);
  same(history, [
    { role: 'user', content: 'why?' },
    { role: 'assistant', content: 'because.' },
  ]);
});

report();
