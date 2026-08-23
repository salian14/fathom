// Checks for internal/web/static/evals.js, run by `make check` alongside
// app_test.mjs and skipped the same way when node is not on PATH.
//
//   node internal/web/evals_test.mjs      # from front/
//
// Loads app.js and THEN evals.js into one vm realm, which exercises the
// cross-file dependency as a side effect: evals.js calls app.js's rendering
// helpers, and if that ever stops working this file throws on load.
//
// What is tested is the logic with no server-side counterpart: which hash maps
// to which view, how an outcome maps onto the three semantic colours, the empty
// and null shapes the database really produces, and that model output reaches
// the page as text.

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import vm from 'node:vm';
import assert from 'node:assert/strict';

import { makeContext, check, report, textOf, assertNoMarkup } from './domstub.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));

function load(overrides) {
  const ctx = makeContext(overrides);
  vm.createContext(ctx);
  vm.runInContext(readFileSync(path.join(here, 'static', 'app.js'), 'utf8'), ctx);
  vm.runInContext(readFileSync(path.join(here, 'static', 'evals.js'), 'utf8'), ctx);
  return ctx;
}

const ctx = load();

// --- routing --------------------------------------------------------------

check('the empty hash is the chat view, so old bookmarks still work', () => {
  for (const hash of ['', '#', '#/', '#/chat']) {
    assert.equal(ctx.parseRoute(hash).view, 'chat', hash);
  }
});

check('#/evals is the run list', () => {
  assert.equal(ctx.parseRoute('#/evals').view, 'runs');
});

check('a run and a result deep link carry a numeric id', () => {
  const run = ctx.parseRoute('#/evals/runs/12');
  assert.equal(run.view, 'run');
  assert.equal(run.id, 12);

  const result = ctx.parseRoute('#/evals/results/941');
  assert.equal(result.view, 'result');
  assert.equal(result.id, 941);
});

check('a non-numeric id is unknown, not NaN', () => {
  // Rendering a "result NaN" pane would be worse than saying the link is stale.
  assert.equal(ctx.parseRoute('#/evals/results/abc').view, 'unknown');
  assert.equal(ctx.parseRoute('#/evals/nonsense').view, 'unknown');
});

// --- the outcome vocabulary ------------------------------------------------

check('ceiling and no_answer are warnings, not errors', () => {
  // They are real measurements of the agent -- it ran out of turns, or declined.
  assert.equal(ctx.outcomeClass('ceiling'), 'warn');
  assert.equal(ctx.outcomeClass('no_answer'), 'warn');
});

check('fixture_error is an error -- nothing was measured', () => {
  // The undocumented sixth outcome: the fixture never reached its broken state.
  assert.equal(ctx.outcomeClass('fixture_error'), 'error');
  assert.equal(ctx.outcomeClass('timeout'), 'error');
  assert.equal(ctx.outcomeClass('ok'), 'ok');
});

check('an outcome this file has not heard of gets a neutral pill', () => {
  // The harness may learn a new outcome before the UI does; a neutral pill still
  // reads correctly, where a guessed colour would assert something false.
  assert.equal(ctx.outcomeClass('something_new'), '');
});

// --- the shapes an empty run really produces -------------------------------

check('a run with no results reads as em dash, never NaN%', () => {
  assert.equal(ctx.passRate(0, 0), '—');
  assert.equal(ctx.passRate(7, 10), '7/10');
});

check('pass rates are counts, so ten thin samples look thin', () => {
  // "70%" hides that a 7/10 rate has roughly a plus or minus 25 point interval.
  assert.ok(!ctx.passRate(7, 10).includes('%'));
});

check('a missing duration is em dash', () => {
  assert.equal(ctx.fmtDuration(null), '—');
  assert.equal(ctx.fmtDuration(undefined), '—');
  assert.equal(ctx.fmtDuration(42), '42.0s');
});

check('a missing score is em dash, not zero', () => {
  // A judgement that errored has a null score. Rendering it as 0 would say the
  // model scored badly when in fact it was never asked.
  assert.equal(ctx.fmtScore(null), '—');
  assert.equal(ctx.fmtScore(3.25), '3.3');
});

check('an unfinished run has no duration', () => {
  assert.equal(ctx.elapsedSince('2026-08-22T01:00:00Z', null), '—');
});

// --- rendering -------------------------------------------------------------

check('a bar with nothing recorded is empty rather than full', () => {
  const bar = ctx.barNode(0, 0);
  assert.equal(bar.children[0].style.width, '0.0%');
});

check('a judgement that errored shows the error, not a verdict', () => {
  const rendered = ctx.judgementNode({
    verdict: null, score: null, remediation_score: null,
    error: 'HTTPError: timed out', rubric_version: 'answer_v2-1a2b3c4d',
  });
  const text = textOf(rendered);
  assert.ok(text.includes('no verdict'), text);
  assert.ok(text.includes('HTTPError'), text);
  assert.ok(!text.includes('null'), 'a null score must not render as "null": ' + text);
});

check('a v1 judgement renders without a remediation score', () => {
  // Rows judged under the older rubric have remediation_score IS NULL.
  const text = textOf(ctx.judgementNode({
    verdict: 'pass', score: 4, remediation_score: null,
    rubric_version: 'answer_v1-99887766',
  }));
  assert.ok(text.includes('fix —'), text);
});

check('a turn at the budget is flagged as measuring the timeout', () => {
  const flagged = ctx.elapsedNode(180);
  assert.equal(flagged.className, 'warn-text');
  assert.ok(flagged.title.includes('timeout'));
  // A normal turn is a plain string, not a node.
  assert.equal(typeof ctx.elapsedNode(42), 'string');
});

check('an outcome breakdown omits the zeroes', () => {
  const text = textOf(ctx.outcomesNode({ ok: 8, ceiling: 2, timeout: 0, error: 0 }));
  assert.ok(text.includes('ok 8') && text.includes('ceiling 2'), text);
  assert.ok(!text.includes('timeout'), 'zero counts are noise: ' + text);
});

check('an all-zero breakdown still renders something', () => {
  assert.equal(textOf(ctx.outcomesNode({})), '—');
});

// --- the rule that matters -------------------------------------------------

check('a judge rationale is rendered as text, never as markup', () => {
  // Judge rationales are model output about model output. Same rule as app.js.
  const nasty = '<img src=x onerror="alert(1)">';
  const rendered = ctx.judgementNode({
    verdict: 'fail', score: 2, remediation_score: 1, rationale: nasty,
  });
  assertNoMarkup(rendered, 'judgement');
  assert.ok(textOf(rendered).includes(nasty), 'the tag must survive as literal text');
});

check('a check detail is rendered as text', () => {
  const nasty = '<script>alert(1)</script>';
  const table = ctx.tableNode(['Detail'], [[nasty]]);
  assertNoMarkup(table, 'check table');
  assert.ok(textOf(table).includes(nasty));
});

check('a table cell given null renders em dash rather than "null"', () => {
  assert.ok(textOf(ctx.tableNode(['A'], [[null]])).includes('—'));
});

check('a table cell given a node keeps it as a node', () => {
  // Regression: the cell builder tells a node from a string by its nodeType, and
  // a stub without one made every link and pill render as "[object Object]" --
  // in the harness only, which is worse, because the tests still passed.
  const link = ctx.linkNode('#/evals/runs/1', '#1');
  const table = ctx.tableNode(['Run'], [[link]]);
  const cell = table.children[0].children[1].children[0].children[0];
  assert.equal(cell.children.length, 1, 'the cell should contain the node');
  assert.equal(cell.children[0].tagName, 'a');
  assert.ok(!textOf(table).includes('[object Object]'));
});

report();
