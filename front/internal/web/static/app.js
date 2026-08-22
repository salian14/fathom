// Fathom chat UI.
//
// Two things shape everything below.
//
// The agent's POST /chat is stateless: it holds no conversation, so the
// transcript lives here, in localStorage, and every turn replays the prior
// messages. "New chat" is therefore a purely local act -- there is no session
// to open or close on the server.
//
// And it does not stream. One request blocks for the whole turn, typically
// 30-180s, then returns the finished answer in one JSON body. So the waiting
// state has to be honest about how long it has been waiting, and it has to be
// cancellable.

'use strict';

// --------------------------------------------------------------- constants

const STORE_KEY = 'fathom.chats';
const ACTIVE_KEY = 'fathom.activeChat';

// History replay caps. The model's context is 16k tokens (ollama/config.env,
// FATHOM_NUM_CTX) and a single tool result may be 8k characters of it, so an
// unbounded transcript stops fitting long before it stops feeling short. Both
// caps apply; whichever bites first wins.
const MAX_HISTORY_MESSAGES = 12;
const MAX_HISTORY_CHARS = 24000;

// Backstop only. The agent's own budget is 180s and the front's proxy waits
// 200s, so reaching this means both of those failed to fire and the user is
// owed an answer rather than a spinner.
const REQUEST_TIMEOUT_MS = 210000;

const READY_POLL_MS = 15000;

// ------------------------------------------------------------------- state

/** @type {{id: string, title: string, messages: Array, updated: number}[]} */
let chats = [];
let activeId = null;
let inFlight = null; // AbortController while a turn is running

const el = {
  list: document.getElementById('chat-list'),
  thread: document.getElementById('thread'),
  form: document.getElementById('composer'),
  question: document.getElementById('question'),
  send: document.getElementById('send'),
  stop: document.getElementById('stop'),
  newChat: document.getElementById('new-chat'),
  statusDot: document.getElementById('status-dot'),
  statusText: document.getElementById('status-text'),
};

// ----------------------------------------------------------------- storage

function load() {
  try {
    const raw = JSON.parse(localStorage.getItem(STORE_KEY) || '[]');
    chats = Array.isArray(raw) ? raw.filter(isChat) : [];
  } catch {
    // Corrupt or unreadable storage is not worth a broken page. Start clean.
    chats = [];
  }
  activeId = localStorage.getItem(ACTIVE_KEY);
  if (!chats.some((c) => c.id === activeId)) {
    activeId = chats.length ? chats[0].id : null;
  }
}

function isChat(c) {
  return c && typeof c.id === 'string' && Array.isArray(c.messages);
}

function save() {
  try {
    localStorage.setItem(STORE_KEY, JSON.stringify(chats));
    if (activeId) localStorage.setItem(ACTIVE_KEY, activeId);
    else localStorage.removeItem(ACTIVE_KEY);
  } catch (err) {
    // Quota, or a private window that refuses to persist. The conversation in
    // front of the user still works; only its survival across reloads is lost.
    console.warn('could not persist chats:', err);
  }
}

function activeChat() {
  return chats.find((c) => c.id === activeId) || null;
}

function newChat() {
  const chat = { id: newId(), title: 'New chat', messages: [], updated: Date.now() };
  chats.unshift(chat);
  activeId = chat.id;
  save();
  renderList();
  renderThread();
  el.question.focus();
}

function deleteChat(id) {
  chats = chats.filter((c) => c.id !== id);
  if (activeId === id) activeId = chats.length ? chats[0].id : null;
  save();
  renderList();
  renderThread();
}

function newId() {
  if (crypto.randomUUID) return crypto.randomUUID();
  return String(Date.now()) + Math.random().toString(16).slice(2);
}

// --------------------------------------------------------------- rendering

// Everything below builds DOM with createElement and textContent. Nothing in
// this file assigns innerHTML: the answer is model output, which is to say it
// is whatever the model read out of the cluster, and cluster objects can hold
// arbitrary strings that someone else put there.

function renderList() {
  el.list.replaceChildren();
  for (const chat of chats) {
    const li = document.createElement('li');
    if (chat.id === activeId) li.className = 'active';

    const open = document.createElement('button');
    open.type = 'button';
    open.className = 'title';
    open.textContent = chat.title;
    open.title = chat.title;
    open.addEventListener('click', () => {
      activeId = chat.id;
      save();
      renderList();
      renderThread();
    });

    const remove = document.createElement('button');
    remove.type = 'button';
    remove.className = 'delete';
    remove.textContent = '×';
    remove.title = 'Delete this chat';
    remove.setAttribute('aria-label', 'Delete chat: ' + chat.title);
    remove.addEventListener('click', (e) => {
      e.stopPropagation();
      deleteChat(chat.id);
    });

    li.append(open, remove);
    el.list.append(li);
  }
}

function renderThread() {
  el.thread.replaceChildren();
  const chat = activeChat();

  if (!chat || chat.messages.length === 0) {
    const empty = document.createElement('div');
    empty.className = 'empty';
    const h = document.createElement('p');
    h.textContent = 'Ask about the cluster.';
    const sub = document.createElement('p');
    sub.textContent =
      'Read-only: the agent can look at pods, deployments, events, logs and metrics. It cannot change anything.';
    empty.append(h, sub);
    el.thread.append(empty);
    return;
  }

  chat.messages.forEach((msg, i) => el.thread.append(messageNode(msg, i)));
  scrollToEnd();
}

function messageNode(msg, index) {
  const wrap = document.createElement('div');
  wrap.className = 'msg ' + msg.role + (msg.error ? ' error' : '');

  const who = document.createElement('div');
  who.className = 'who';
  who.textContent = msg.role === 'user' ? 'You' : msg.error ? 'Error' : 'Fathom';
  wrap.append(who);

  // The fix leads, because it is the thing the reader acts on. The prose below it
  // is the evidence for it.
  if (msg.remediation) wrap.append(remediationNode(msg.remediation));

  const body = document.createElement('div');
  body.className = 'body';
  formatInto(body, msg.content);
  wrap.append(body);

  if (msg.truncated) {
    const note = document.createElement('div');
    note.className = 'notice';
    note.textContent =
      'Earlier turns were left out of this request to fit the model’s context.';
    wrap.append(note);
  }

  if (msg.meta) wrap.append(metaNode(msg.meta));
  if (msg.error) wrap.append(retryNode(index));

  return wrap;
}

// remediationNode renders the structured fix the agent proposed. Same rule as the
// rest of this file: textContent only, never innerHTML -- this is model output.
function remediationNode(fix) {
  const wrap = document.createElement('div');
  wrap.className = 'remediation' + (fix.action_required ? '' : ' none');

  const label = document.createElement('div');
  label.className = 'who';
  label.textContent = fix.action_required ? 'Suggested fix' : 'No action needed';
  wrap.append(label);

  if (fix.summary) {
    const p = document.createElement('p');
    p.textContent = fix.summary;
    wrap.append(p);
  }

  // Only meaningful when there is something to act on; on a healthy cluster the
  // label above has already said everything.
  if (fix.action_required) {
    const target = [fix.target_kind, fix.target_name].filter(Boolean).join(' ');
    if (target || fix.namespace) {
      const p = document.createElement('p');
      p.className = 'target';
      p.textContent = [target, fix.namespace && 'in ' + fix.namespace]
        .filter(Boolean)
        .join(' ');
      wrap.append(p);
    }

    for (const command of fix.commands || []) {
      const pre = document.createElement('pre');
      const code = document.createElement('code');
      code.textContent = command;
      pre.append(code);
      wrap.append(pre);
    }
  }

  return wrap;
}

function metaNode(meta) {
  const details = document.createElement('details');
  details.className = 'meta';

  const summary = document.createElement('summary');
  const bits = [];
  if (typeof meta.elapsed_seconds === 'number') bits.push(meta.elapsed_seconds + 's');
  if (typeof meta.iterations === 'number') {
    bits.push(meta.iterations + (meta.iterations === 1 ? ' tool call' : ' tool calls'));
  }
  if (meta.model) bits.push(meta.model);
  summary.textContent = bits.join(' · ') || 'details';
  details.append(summary);

  const steps = Array.isArray(meta.steps) ? meta.steps : [];
  if (steps.length === 0) {
    const none = document.createElement('p');
    none.textContent = 'The model answered without calling any tools.';
    details.append(none);
    return details;
  }

  const ul = document.createElement('ul');
  ul.className = 'steps';
  for (const step of steps) {
    const li = document.createElement('li');

    const name = document.createElement('span');
    name.className = 'tool';
    name.textContent = step.tool || '(unnamed tool)';
    li.append(name, document.createTextNode(' ' + summariseArgs(step.args)));

    // A step with no result_chars never came back -- the model stopped while
    // still calling tools, which is what hitting the iteration ceiling looks
    // like from here.
    if (step.result_chars === undefined) {
      li.append(document.createTextNode(' — no result'));
    } else {
      const status = document.createElement('span');
      // An error step is routine, not a failure: the MCP server writes its
      // errors as advice and the model retries with a better query. Shown in
      // the warn colour, not the error one, so it does not read as a fault.
      if (step.status && step.status !== 'success') status.className = 'status-error';
      status.textContent = ' — ' + step.result_chars + ' chars, ' + (step.status || 'success');
      li.append(status);
    }

    ul.append(li);
  }
  details.append(ul);
  return details;
}

function summariseArgs(args) {
  if (!args || typeof args !== 'object') return '';
  return (
    '(' +
    Object.entries(args)
      .map(([k, v]) => k + '=' + (typeof v === 'string' ? v : JSON.stringify(v)))
      .join(', ') +
    ')'
  );
}

function retryNode(index) {
  const actions = document.createElement('div');
  actions.className = 'actions';
  const button = document.createElement('button');
  button.type = 'button';
  button.textContent = 'Retry';
  button.addEventListener('click', () => retryFrom(index));
  actions.append(button);
  return actions;
}

// formatInto renders text into parent, honouring triple-backtick fences and
// blank-line paragraphs. Deliberately not a markdown parser: this is a fence
// splitter and a paragraph splitter, which covers what the agent actually
// emits (prose with the occasional YAML or log excerpt) without pulling in a
// dependency that would need its own escaping story.
function formatInto(parent, text) {
  const parts = String(text == null ? '' : text).split(/```/);
  parts.forEach((part, i) => {
    if (i % 2 === 1) {
      const pre = document.createElement('pre');
      const code = document.createElement('code');
      // Drop a language tag on the opening fence; keep everything else.
      code.textContent = part.replace(/^[^\n]*\n/, '').replace(/\n$/, '');
      pre.append(code);
      parent.append(pre);
      return;
    }
    for (const para of part.split(/\n{2,}/)) {
      if (!para.trim()) continue;
      const p = document.createElement('p');
      p.textContent = para.trim();
      parent.append(p);
    }
  });

  if (!parent.hasChildNodes()) {
    const p = document.createElement('p');
    p.textContent = String(text == null ? '' : text);
    parent.append(p);
  }
}

function scrollToEnd() {
  el.thread.scrollTop = el.thread.scrollHeight;
}

// -------------------------------------------------------------------- turn

// buildHistory returns what to replay, and whether anything was dropped. Only
// {role, content} goes on the wire -- the agent rejects any role other than
// user or assistant, and the meta we keep locally has no counterpart in its
// request schema.
function buildHistory(messages) {
  const usable = messages.filter((m) => !m.error && (m.role === 'user' || m.role === 'assistant'));
  let kept = usable.slice(-MAX_HISTORY_MESSAGES);

  let chars = kept.reduce((n, m) => n + m.content.length, 0);
  while (kept.length > 1 && chars > MAX_HISTORY_CHARS) {
    chars -= kept[0].content.length;
    kept = kept.slice(1);
  }

  return {
    history: kept.map((m) => ({ role: m.role, content: m.content })),
    truncated: kept.length < usable.length,
  };
}

async function ask(question) {
  const chat = activeChat();
  if (!chat || inFlight) return;

  if (chat.messages.length === 0) {
    chat.title = question.length > 40 ? question.slice(0, 40).trimEnd() + '…' : question;
    renderList();
  }

  const { history, truncated } = buildHistory(chat.messages);
  chat.messages.push({ role: 'user', content: question, truncated });
  chat.updated = Date.now();
  save();
  renderThread();

  await run(chat, question, history);
}

// retryFrom drops a failed exchange and asks the same question again. The user
// message immediately before the error is the one that failed.
function retryFrom(errorIndex) {
  const chat = activeChat();
  if (!chat || inFlight) return;

  const userIndex = errorIndex - 1;
  const failed = chat.messages[userIndex];
  if (!failed || failed.role !== 'user') return;

  const question = failed.content;
  chat.messages.splice(userIndex, chat.messages.length - userIndex);
  const { history, truncated } = buildHistory(chat.messages);
  chat.messages.push({ role: 'user', content: question, truncated });
  save();
  renderThread();

  run(chat, question, history);
}

async function run(chat, question, history) {
  const controller = new AbortController();
  inFlight = controller;
  setBusy(true);

  const waiting = waitingNode();
  el.thread.append(waiting.node);
  scrollToEnd();

  const timer = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);

  try {
    const response = await fetch('/api/chat', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ question, history }),
      signal: controller.signal,
    });

    const payload = await readBody(response);

    if (!response.ok) {
      chat.messages.push({
        role: 'assistant',
        error: true,
        content: response.status + ' — ' + errorMessage(payload, response.status),
      });
    } else {
      chat.messages.push({
        role: 'assistant',
        content: payload && payload.answer ? payload.answer : '(the agent returned no answer)',
        remediation: payload && payload.remediation,
        meta: {
          steps: payload && payload.steps,
          iterations: payload && payload.iterations,
          model: payload && payload.model,
          elapsed_seconds: payload && payload.elapsed_seconds,
        },
      });
    }
  } catch (err) {
    chat.messages.push({
      role: 'assistant',
      error: true,
      content:
        err && err.name === 'AbortError'
          ? 'Stopped. The agent may still be working on this turn.'
          : 'Could not reach the front end: ' + (err && err.message ? err.message : String(err)),
    });
  } finally {
    clearTimeout(timer);
    waiting.stop();
    inFlight = null;
    setBusy(false);
    chat.updated = Date.now();
    save();
    renderThread();
    el.question.focus();
  }
}

// readBody parses JSON if that is what came back, and falls back to text --
// the agent's probes answer in text/plain, and a proxy or ingress failure may
// answer in HTML.
async function readBody(response) {
  const text = await response.text();
  try {
    return JSON.parse(text);
  } catch {
    return { error: text.trim() };
  }
}

// errorMessage covers the two envelopes in play: {"error": "..."} from the
// agent's own handlers and from this service's proxy, and FastAPI's
// {"detail": [...]} from request validation -- an empty question, most often.
function errorMessage(payload, status) {
  if (!payload) return 'the request failed';
  if (typeof payload.error === 'string' && payload.error) return payload.error;

  const detail = payload.detail;
  if (typeof detail === 'string') return detail;
  if (Array.isArray(detail)) {
    const msgs = detail.map((d) => (d && d.msg) || JSON.stringify(d)).filter(Boolean);
    if (msgs.length) return msgs.join('; ');
  }
  return status === 422 ? 'the request was rejected as invalid' : 'the request failed';
}

// waitingNode is the whole answer to "no streaming": a placeholder that counts
// up, so a 90-second turn looks like progress rather than a hang.
function waitingNode() {
  const wrap = document.createElement('div');
  wrap.className = 'msg assistant';

  const who = document.createElement('div');
  who.className = 'who';
  who.textContent = 'Fathom';

  const body = document.createElement('div');
  body.className = 'body';
  const p = document.createElement('p');
  body.append(p);
  wrap.append(who, body);

  const started = Date.now();
  const tick = () => {
    const seconds = Math.round((Date.now() - started) / 1000);
    p.textContent = 'Thinking… ' + seconds + 's';
  };
  tick();
  const interval = setInterval(tick, 1000);

  return {
    node: wrap,
    stop() {
      clearInterval(interval);
      wrap.remove();
    },
  };
}

function setBusy(busy) {
  el.send.disabled = busy;
  el.question.disabled = busy;
  el.stop.hidden = !busy;
  // Deliberately not disabled while busy: switching chats mid-turn would land
  // the answer in the wrong transcript, since `run` holds a reference to the
  // chat it started in.
  el.newChat.disabled = busy;
}

// ------------------------------------------------------------------ status

// The status line reports the agent's readiness, not this service's -- an
// unreachable Ollama or MCP server is the failure worth knowing about before
// typing a question, and /readyz names which one it is.
async function pollReady() {
  try {
    const response = await fetch('/readyz', { cache: 'no-store' });
    const text = (await response.text()).trim();
    if (response.ok) {
      setStatus('ready', 'agent ready');
    } else {
      setStatus('down', text || 'agent not ready');
    }
  } catch {
    setStatus('down', 'front end unreachable');
  }
}

function setStatus(state, text) {
  el.statusDot.dataset.state = state;
  el.statusText.textContent = text;
  el.statusText.title = text;
}

// ------------------------------------------------------------------- wiring

el.form.addEventListener('submit', (e) => {
  e.preventDefault();
  const question = el.question.value.trim();
  if (!question || inFlight) return;
  el.question.value = '';
  autosize();
  ask(question);
});

el.question.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    el.form.requestSubmit();
  }
});

el.question.addEventListener('input', autosize);

function autosize() {
  el.question.style.height = 'auto';
  el.question.style.height = el.question.scrollHeight + 'px';
}

el.stop.addEventListener('click', () => {
  if (inFlight) inFlight.abort();
});

el.newChat.addEventListener('click', newChat);

load();
if (!chats.length) newChat();
renderList();
renderThread();
pollReady();
setInterval(pollReady, READY_POLL_MS);
el.question.focus();
