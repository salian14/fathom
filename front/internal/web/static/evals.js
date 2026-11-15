// Fathom's eval view: inspecting what the nightly harness measured.
//
// Loaded after app.js, and depends on it. Both are classic scripts sharing one
// global scope, which is what lets this file call remediationNode(), metaNode(),
// formatInto(), readBody() and errorMessage() directly -- the answers and fixes
// stored by the harness are the same shapes the chat view already renders, so
// there is nothing to reimplement. index.html's script order is pinned by a Go
// test for exactly that reason.
//
// The same rule as app.js applies here, and applies harder: everything below
// builds DOM with createElement and textContent. Nothing assigns innerHTML. An
// answer in the results database is model output that read a live cluster, and a
// judge rationale is model output about model output. A Go test greps every file
// in static/ for the HTML sinks.
//
// Routing is the hash, because the page is served by http.FileServerFS, which
// 404s any path that is not a file -- a real URL would break on reload.

'use strict';

// ------------------------------------------------------------------ constants

// Relative on purpose -- see the note on fetch('api/chat') in app.js.
const EVAL_BASE = 'api/eval';

// Outcomes onto the three semantic colours the stylesheet already defines.
// `ceiling` and `no_answer` are --warn rather than --error on purpose: the agent
// ran out of turns or declined, which is a real measurement of the agent, not a
// fault in the harness. `fixture_error` is --error because the fixture never
// reached its broken state and nothing was measured at all.
const OUTCOME_CLASS = {
  ok: 'ok',
  ceiling: 'warn',
  no_answer: 'warn',
  timeout: 'error',
  error: 'error',
  fixture_error: 'error',
};

// outcomeClass wraps the table above so it is reachable from the test harness,
// which runs this file in a vm realm where a top-level `const` is a lexical
// binding and never lands on the global. app_test.mjs notes the same thing about
// app.js's `let chats`.
//
// An unknown outcome gets no class rather than a guess: the harness may learn a
// new one before this file does, and a neutral pill still reads correctly.
function outcomeClass(outcome) {
  return OUTCOME_CLASS[outcome] || '';
}

// elapsed_seconds at or above this means the agent's own 180s budget was what
// ended the turn -- the timeout is being measured, not the agent.
const TURN_BUDGET_SECONDS = 175;

const evalEl = {
  view: document.getElementById('eval-view'),
  chatView: document.getElementById('chat-view'),
  switcher: document.getElementById('view-switcher'),
  chatLink: document.getElementById('view-chat'),
  evalsLink: document.getElementById('view-evals'),
  chatList: document.getElementById('chat-list'),
  runList: document.getElementById('run-list'),
};

let evalEnabled = false;

// --------------------------------------------------------------------- routing

// parseRoute maps a location.hash onto a view. Anything it does not recognise is
// 'unknown' rather than a guess, so a stale deep link renders a message instead
// of an empty pane.
function parseRoute(hash) {
  const path = String(hash || '').replace(/^#/, '');
  if (path === '' || path === '/' || path === '/chat') return { view: 'chat' };
  if (path === '/evals') return { view: 'runs' };

  const run = path.match(/^\/evals\/runs\/(\d+)$/);
  if (run) return { view: 'run', id: Number(run[1]) };

  const result = path.match(/^\/evals\/results\/(\d+)$/);
  if (result) return { view: 'result', id: Number(result[1]) };

  if (path.startsWith('/evals')) return { view: 'unknown' };
  return { view: 'chat' };
}

function route() {
  const target = parseRoute(location.hash);

  // Chat is the fallback for everything when eval is not configured, including
  // a bookmarked #/evals from an install that used to have it.
  const onEval = evalEnabled && target.view !== 'chat';

  evalEl.chatView.hidden = onEval;
  evalEl.view.hidden = !onEval;
  evalEl.chatList.hidden = onEval;
  evalEl.runList.hidden = !onEval;
  evalEl.chatLink.setAttribute('aria-current', onEval ? 'false' : 'page');
  if (evalEl.evalsLink) evalEl.evalsLink.setAttribute('aria-current', onEval ? 'page' : 'false');

  if (!onEval) return;
  if (target.view === 'runs') return showRuns();
  if (target.view === 'run') return showRun(target.id);
  if (target.view === 'result') return showResult(target.id);
  renderEvalMessage('That is not a page here.', 'Pick a run from the list.');
}

// ---------------------------------------------------------------------- fetch

// evalFetch returns the parsed body, or throws with the message the UI shows.
// Reuses readBody/errorMessage from app.js: the eval service answers with the
// same {"error": ...} envelope the agent and the proxy use, deliberately, so
// there is one shape to parse rather than one per origin.
async function evalFetch(path) {
  let response;
  try {
    response = await fetch(EVAL_BASE + path, { cache: 'no-store' });
  } catch (err) {
    throw new Error('could not reach the eval service: ' + err.message);
  }
  const payload = await readBody(response);
  if (!response.ok) throw new Error(errorMessage(payload, response.status));
  return payload;
}

// ------------------------------------------------------------------ formatting

function passRate(passed, total) {
  // Counts, never a bare percentage. Ten samples per fixture is thin -- a 7/10
  // pass rate has roughly a plus or minus 25 point interval -- and "70%" hides
  // that in a way "7/10" does not.
  if (!total) return '—';
  return passed + '/' + total;
}

function fmtDuration(seconds) {
  if (typeof seconds !== 'number' || Number.isNaN(seconds)) return '—';
  if (seconds < 90) return seconds.toFixed(1) + 's';
  const mins = Math.floor(seconds / 60);
  if (mins < 60) return mins + 'm ' + Math.round(seconds % 60) + 's';
  return Math.floor(mins / 60) + 'h ' + (mins % 60) + 'm';
}

function fmtTime(iso) {
  if (!iso) return '—';
  const at = new Date(iso);
  return Number.isNaN(at.getTime()) ? String(iso) : at.toLocaleString();
}

function fmtScore(value) {
  return typeof value === 'number' ? value.toFixed(1) : '—';
}

function elapsedSince(startedAt, finishedAt) {
  if (!startedAt || !finishedAt) return '—';
  const seconds = (new Date(finishedAt) - new Date(startedAt)) / 1000;
  return Number.isNaN(seconds) ? '—' : fmtDuration(seconds);
}

// ---------------------------------------------------------------- DOM builders

function node(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined && text !== null) el.textContent = String(text);
  return el;
}

function linkNode(href, text, className) {
  const a = node('a', className, text);
  a.setAttribute('href', href);
  return a;
}

function pillNode(text, kind) {
  return node('span', 'pill' + (kind ? ' ' + kind : ''), text);
}

// barNode is two divs and a width. No markup strings, and no percentage in the
// label -- the caller shows counts.
function barNode(passed, total) {
  const wrap = node('div', 'eval-bar');
  const fill = node('div', 'eval-bar-fill');
  const ratio = total ? passed / total : 0;
  fill.style.width = (ratio * 100).toFixed(1) + '%';
  if (total) fill.className += ratio === 1 ? ' ok' : ratio === 0 ? ' bad' : ' partial';
  wrap.append(fill);
  return wrap;
}

function tableNode(headers, rows) {
  const table = node('table', 'eval-table');
  const thead = node('thead');
  const hrow = node('tr');
  for (const h of headers) hrow.append(node('th', null, h));
  thead.append(hrow);
  table.append(thead);

  const tbody = node('tbody');
  for (const cells of rows) {
    const tr = node('tr');
    for (const cell of cells) {
      const td = node('td');
      if (cell === null || cell === undefined) td.textContent = '—';
      else if (typeof cell === 'object' && cell.nodeType) td.append(cell);
      else td.textContent = String(cell);
      tr.append(td);
    }
    tbody.append(tr);
  }
  table.append(tbody);

  // Wrapped so a wide table scrolls inside itself rather than pushing the page
  // sideways, which is what the 720px rule needs.
  const scroll = node('div', 'eval-scroll');
  scroll.append(table);
  return scroll;
}

function outcomesNode(outcomes) {
  const wrap = node('span', 'outcomes');
  for (const [name, count] of Object.entries(outcomes || {})) {
    if (!count) continue;
    wrap.append(pillNode(name + ' ' + count, outcomeClass(name)));
  }
  if (!wrap.hasChildNodes()) wrap.textContent = '—';
  return wrap;
}

function crumbNode(parts) {
  const wrap = node('nav', 'eval-crumb');
  parts.forEach((part, i) => {
    if (i) wrap.append(node('span', 'sep', '/'));
    if (part.href) wrap.append(linkNode(part.href, part.text));
    else wrap.append(node('span', null, part.text));
  });
  return wrap;
}

function renderEvalMessage(title, detail) {
  const wrap = node('div', 'eval-empty');
  wrap.append(node('h2', null, title));
  if (detail) wrap.append(node('p', null, detail));
  evalEl.view.replaceChildren(wrap);
}

function renderEvalError(err) {
  const wrap = node('div', 'eval-empty');
  wrap.append(node('h2', null, 'Could not load'));
  // Inline, in this pane. Deliberately not setStatus() -- that drives the
  // sidebar dot, which reports the *agent's* readiness, and an eval read failing
  // is not an agent outage.
  wrap.append(node('p', 'error', err.message || String(err)));
  evalEl.view.replaceChildren(wrap);
}

// A run that has written nothing yet, or a database that does not exist at all,
// is a correct state of the system rather than a failure -- the service says so
// with {"empty": true} and a 200.
function isEmpty(payload) {
  return payload && payload.empty === true;
}

// ------------------------------------------------------------- level 1: runs

async function showRuns() {
  evalEl.view.replaceChildren(node('div', 'eval-empty', 'Loading…'));
  let payload;
  try {
    payload = await evalFetch('/runs');
  } catch (err) {
    return renderEvalError(err);
  }

  if (isEmpty(payload)) {
    renderRunList([]);
    return renderEvalMessage(
      'No results yet',
      payload.reason || 'The evaluation CronJob has not completed a run.'
    );
  }

  const runs = payload.runs || [];
  renderRunList(runs);

  if (!runs.length) {
    return renderEvalMessage('No runs recorded', 'The database is there, but empty.');
  }

  const wrap = node('div', 'eval-page');
  wrap.append(node('h2', null, 'Evaluation runs'));

  const rows = runs.map((run) => [
    linkNode('#/evals/runs/' + run.id, '#' + run.id),
    pillNode(run.status, run.status === 'ok' ? 'ok' : run.status === 'running' ? 'warn' : 'error'),
    fmtTime(run.started_at),
    elapsedSince(run.started_at, run.finished_at),
    passRate(run.passed, run.results),
    outcomesNode(run.outcomes),
    fmtDuration(run.mean_elapsed),
    run.agent_model,
    // Shown because results either side of a question rewording are not
    // comparable, and these are the only markers of which side a run is on.
    run.harness_version,
    run.git_sha,
  ]);

  wrap.append(
    tableNode(
      ['Run', 'Status', 'Started', 'Took', 'Passed', 'Outcomes', 'Mean turn', 'Model', 'Harness', 'Commit'],
      rows
    )
  );
  evalEl.view.replaceChildren(wrap);
}

function renderRunList(runs) {
  const items = runs.slice(0, 20).map((run) => {
    const li = node('li');
    const a = linkNode('#/evals/runs/' + run.id, '');
    a.append(node('span', 'run-id', '#' + run.id));
    a.append(node('span', 'run-when', fmtTime(run.started_at)));
    a.append(node('span', 'run-rate', passRate(run.passed, run.results)));
    li.append(a);
    return li;
  });
  evalEl.runList.replaceChildren(...items);
}

// -------------------------------------------------------- level 2: one run

async function showRun(runId) {
  evalEl.view.replaceChildren(node('div', 'eval-empty', 'Loading…'));
  let payload;
  try {
    payload = await evalFetch('/runs/' + runId);
  } catch (err) {
    return renderEvalError(err);
  }
  if (isEmpty(payload)) return renderEvalMessage('No results yet', payload.reason);

  const run = payload.run;
  const wrap = node('div', 'eval-page');
  wrap.append(crumbNode([{ text: 'Runs', href: '#/evals' }, { text: 'Run #' + run.id }]));

  const head = node('div', 'eval-head');
  head.append(node('h2', null, 'Run #' + run.id));
  head.append(
    pillNode(run.status, run.status === 'ok' ? 'ok' : run.status === 'running' ? 'warn' : 'error')
  );
  head.append(node('span', 'dim', fmtTime(run.started_at)));
  head.append(node('span', 'dim', elapsedSince(run.started_at, run.finished_at)));
  head.append(node('span', 'dim', run.agent_model || ''));
  head.append(node('span', 'dim', 'harness ' + (run.harness_version || '?')));
  wrap.append(head);

  wrap.append(node('h3', null, 'By fixture'));
  wrap.append(
    tableNode(
      ['Fixture', 'Passed', '', 'Outcomes', 'Mean turn', 'Mean tools', 'Isolation'],
      (payload.fixtures || []).map((f) => [
        f.fixture,
        passRate(f.passed, f.results),
        barNode(f.passed, f.results),
        outcomesNode(f.outcomes),
        fmtDuration(f.mean_elapsed),
        typeof f.mean_tool_calls === 'number' ? f.mean_tool_calls.toFixed(1) : '—',
        f.isolation,
      ])
    )
  );

  const judge = run.judge || [];
  if (judge.length) {
    wrap.append(node('h3', null, 'Judge'));
    // One row per rubric version, never averaged together: a score's scale is
    // defined by its rubric, and v1 rows carry no remediation_score at all.
    wrap.append(
      tableNode(
        ['Rubric', 'Judged', 'Passed', 'Errors', 'Mean score', 'Mean fix'],
        judge.map((j) => [
          j.rubric_version,
          j.judged,
          j.passes,
          j.errors ? pillNode(String(j.errors), 'error') : '0',
          fmtScore(j.mean_score),
          fmtScore(j.mean_remediation_score),
        ])
      )
    );
  }

  const checks = payload.checks || [];
  if (checks.length) {
    wrap.append(node('h3', null, 'Checks'));
    wrap.append(
      tableNode(
        ['Fixture', 'Category', 'Check', 'Passed', ''],
        checks.map((c) => [
          c.fixture,
          c.category,
          node('code', null, c.name),
          passRate(c.passed, c.total),
          barNode(c.passed, c.total),
        ])
      )
    );
  }

  wrap.append(node('h3', null, 'Results'));
  if (payload.rubric) {
    wrap.append(node('p', 'dim', 'Judge columns show ' + payload.rubric));
  }
  wrap.append(
    tableNode(
      ['', 'Fixture', 'Question', 'Iter', 'Outcome', 'Checks', 'Verdict', 'Score', 'Fix', 'Took'],
      (payload.results || []).map((r) => [
        linkNode('#/evals/results/' + r.id, '#' + r.id),
        r.fixture,
        r.question_id,
        r.iteration,
        pillNode(r.outcome, outcomeClass(r.outcome)),
        passRate(r.total_checks - r.failed_checks, r.total_checks),
        r.judge_verdict ? pillNode(r.judge_verdict, r.judge_verdict === 'pass' ? 'ok' : 'error') : null,
        r.judge_score,
        r.judge_remediation_score,
        elapsedNode(r.elapsed_seconds),
      ])
    )
  );

  evalEl.view.replaceChildren(wrap);
}

// elapsedNode flags a turn that ran into the agent's own budget, because that
// number is measuring the timeout rather than the agent.
function elapsedNode(seconds) {
  const text = fmtDuration(seconds);
  if (typeof seconds === 'number' && seconds >= TURN_BUDGET_SECONDS) {
    const span = node('span', 'warn-text', text);
    span.title = 'at the turn budget -- this is measuring the timeout, not the agent';
    return span;
  }
  return text;
}

// ----------------------------------------------------- level 3: one result

async function showResult(resultId) {
  evalEl.view.replaceChildren(node('div', 'eval-empty', 'Loading…'));
  let payload;
  try {
    payload = await evalFetch('/results/' + resultId);
  } catch (err) {
    return renderEvalError(err);
  }
  if (isEmpty(payload)) return renderEvalMessage('No results yet', payload.reason);

  const r = payload.result;
  const wrap = node('div', 'eval-page');
  wrap.append(
    crumbNode([
      { text: 'Runs', href: '#/evals' },
      { text: 'Run #' + r.run_id, href: '#/evals/runs/' + r.run_id },
      { text: 'Result #' + r.id },
    ])
  );

  const head = node('div', 'eval-head');
  head.append(node('h2', null, r.fixture + ' · ' + r.question_id + ' · iteration ' + r.iteration));
  head.append(pillNode(r.outcome, outcomeClass(r.outcome)));
  head.append(pillNode(r.passed ? 'all checks passed' : 'checks failed', r.passed ? 'ok' : 'error'));
  wrap.append(head);

  if (r.json_errors) {
    for (const problem of r.json_errors) {
      wrap.append(node('p', 'error', 'stored JSON could not be parsed: ' + problem));
    }
  }

  wrap.append(node('h3', null, 'Question'));
  wrap.append(node('p', 'eval-question', r.question_text || '—'));

  wrap.append(node('h3', null, 'Answer'));
  if (r.answer) {
    const body = node('div', 'eval-answer');
    formatInto(body, r.answer); // app.js: fences and paragraphs, as text
    wrap.append(body);
  } else {
    wrap.append(node('p', 'dim', r.error || 'no answer was recorded'));
  }

  // The exact shape app.js already renders for a live turn.
  if (r.remediation) wrap.append(remediationNode(r.remediation));

  if ((r.steps || []).length) {
    wrap.append(
      metaNode({
        steps: r.steps,
        iterations: r.tool_calls,
        elapsed_seconds: r.elapsed_seconds,
        model: (payload.run && payload.run.agent_model) || '',
      })
    );
  }

  wrap.append(node('h3', null, 'Checks'));
  wrap.append(
    tableNode(
      ['', 'Category', 'Check', 'Detail'],
      (payload.checks || []).map((c) => [
        pillNode(c.passed ? 'pass' : 'fail', c.passed ? 'ok' : 'error'),
        c.category,
        node('code', null, c.name),
        c.detail || '',
      ])
    )
  );

  const judgements = payload.judgements || [];
  if (judgements.length) {
    wrap.append(node('h3', null, 'Judge'));
    for (const j of judgements) wrap.append(judgementNode(j));
  }

  evalEl.view.replaceChildren(wrap);
}

// judgementNode handles the failure shape as well as the verdict: a judgement
// whose model call failed, or whose reply would not parse, is stored with a null
// verdict and a populated error.
function judgementNode(j) {
  const wrap = node('div', 'judgement');
  const head = node('div', 'judgement-head');

  if (j.verdict) {
    head.append(pillNode(j.verdict, j.verdict === 'pass' ? 'ok' : 'error'));
    head.append(node('span', 'dim', 'diagnosis ' + (j.score ?? '—') + '/5'));
    head.append(node('span', 'dim', 'fix ' + (j.remediation_score ?? '—') + '/5'));
  } else {
    head.append(pillNode('no verdict', 'warn'));
  }
  head.append(node('span', 'dim', j.rubric_version || ''));
  head.append(node('span', 'dim', j.judge_model || ''));
  wrap.append(head);

  if (j.error) wrap.append(node('p', 'error', j.error));
  if (j.rationale) wrap.append(node('p', null, j.rationale));
  return wrap;
}

// ------------------------------------------------------------------- bootstrap

// initEvals asks the server whether the eval service is configured before showing
// anything. The page is embedded in the binary and cannot read an environment
// variable, so asking is the only honest mechanism -- and doing it before the
// first render is what keeps an install without eval from flashing a dead tab.
async function initEvals() {
  try {
    const response = await fetch('api/features', { cache: 'no-store' });
    const features = await readBody(response);
    evalEnabled = response.ok && features && features.eval === true;
  } catch {
    evalEnabled = false;
  }

  if (evalEnabled) {
    evalEl.evalsLink.hidden = false;
  } else if (evalEl.evalsLink) {
    // Removed, not just hidden: the DOM should carry no trace of a feature this
    // deployment does not have.
    evalEl.evalsLink.remove();
    evalEl.evalsLink = null;
  }

  route();
}

if (typeof window !== 'undefined' && window.addEventListener) {
  window.addEventListener('hashchange', route);
}
initEvals();
