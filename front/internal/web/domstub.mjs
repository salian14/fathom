// The DOM stub shared by app_test.mjs and evals_test.mjs.
//
// Extracted rather than copied: two hand-maintained stubs drift, and the drift
// shows up as one suite passing on a page shape the other has already changed.
//
// Deliberately minimal. If a render path touches something that is not here, it
// throws, and that is the point -- the stub is also an assertion about how small
// the UI's DOM surface is. Both scripts are constrained to getElementById and
// createElement for the same reason: a querySelector would mean shipping a
// selector engine in here.

import assert from 'node:assert/strict';

export function mkEl(tag = 'div') {
  const el = {
    // nodeType 1 because a real element has it, and code that distinguishes a
    // node from a string checks exactly that -- tableNode does. Without it the
    // stub silently stringifies elements to "[object Object]", and the harness
    // stops testing the path the browser actually takes.
    nodeType: 1,
    tagName: tag, children: [], _text: '', style: {}, dataset: {},
    className: '', title: '', value: '', disabled: false, hidden: false,
    scrollTop: 0, scrollHeight: 0, type: '', attributes: {},
    append(...n) { this.children.push(...n); },
    replaceChildren(...n) { this.children = n; },
    remove() { this.removed = true; },
    addEventListener() {},
    setAttribute(k, v) { this.attributes[k] = String(v); },
    getAttribute(k) { return k in this.attributes ? this.attributes[k] : null; },
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

// Values built inside the vm realm have that realm's prototypes, so
// deepStrictEqual rejects structurally identical objects. Compare as JSON.
export const same = (a, b, msg) => assert.equal(JSON.stringify(a), JSON.stringify(b), msg);

// Flattens a rendered tree to the text a reader would see.
export const textOf = (node) =>
  node.children && node.children.length
    ? node.children.map(textOf).join(' ')
    : String(node.textContent || '');

// Walks a rendered tree asserting nothing anywhere used an HTML-string sink.
// The Go test greps the source; this checks the built nodes.
export function assertNoMarkup(node, label) {
  assert.equal(node.innerHTML, undefined, label + ': innerHTML must never be set');
  for (const child of node.children || []) {
    if (child && child.children) assertNoMarkup(child, label);
  }
}

export function makeContext(overrides = {}) {
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
    location: { hash: '' },
    crypto: { randomUUID: () => 'id-' + Math.random().toString(16).slice(2) },
    fetch: async () => ({ ok: true, status: 200, text: async () => 'ok' }),
    setInterval: () => 0,
    setTimeout: () => 0,
    clearInterval: () => {},
    clearTimeout: () => {},
    AbortController: class { constructor() { this.signal = {}; } abort() {} },
    Date, Math, JSON, String, Object, Array, Error, Promise, Number, RegExp,
    ...overrides,
  };
  ctx.window = ctx;
  ctx.globalThis = ctx;
  ctx.ids = ids;
  return ctx;
}

let pass = 0;
export const check = (name, fn) => { fn(); console.log('  ok  ' + name); pass++; };
export const report = () => { console.log('\n' + pass + ' checks passed'); };
