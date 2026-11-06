// Renders rows of the real static/app.js against a minimal fake DOM, under Node (no browser, no network):
//   node internal/ui/testdata/app-labels.js internal/ui/static/app.js [phone]
// Prints one JSON object: each case's rows as their visible text (with «phone»: the phone tabs' cases).
// app_labels_test.go reads it.
"use strict";

const fs = require("fs");
const vm = require("vm");

class FakeElement {
  constructor(tag) {
    this.tagName = tag;
    this.children = [];
    this.dataset = {};
    this.style = { setProperty() {}, removeProperty() {} };
    this.attrs = {};
    this.className = "";
    this.text = "";
    this.value = "";
    const self = this;
    this.classList = {
      add: (...c) => { self.className = [...new Set([...self.className.split(" ").filter(Boolean), ...c])].join(" "); },
      remove: (...c) => { self.className = self.className.split(" ").filter((x) => x && !c.includes(x)).join(" "); },
      toggle: (c, on) => { (on ?? !self.className.split(" ").includes(c)) ? self.classList.add(c) : self.classList.remove(c); },
      contains: (c) => self.className.split(" ").includes(c),
    };
  }
  set textContent(v) { this.text = String(v); this.children = []; }
  get textContent() { return this.text + this.children.map((c) => (typeof c === "string" ? c : c.textContent)).join(""); }
  set innerHTML(v) { this.text = ""; this.children = []; }
  get childNodes() { return this.children; }
  get lastElementChild() { return this.children[this.children.length - 1] || new FakeElement("span"); }
  append(...xs) { for (const x of xs) if (x !== null && x !== undefined) this.children.push(typeof x === "string" ? x : x); }
  prepend(...xs) { this.children.unshift(...xs.filter((x) => x !== null && x !== undefined)); }
  replaceChildren(...xs) { this.text = ""; this.children = []; this.append(...xs); }
  appendChild(x) { this.append(x); return x; }
  remove() {}
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return this.attrs[k] ?? null; }
  removeAttribute(k) { delete this.attrs[k]; }
  addEventListener() {}
  removeEventListener() {}
  querySelector() { return null; }
  querySelectorAll() { return []; }
  closest() { return null; }
  focus() {}
  getBoundingClientRect() { return { left: 0, top: 0, width: 0, height: 0 }; }
}

// cls is every descendant (and the node) that carries the class, depth first.
function cls(node, c, out = []) {
  if (typeof node === "string") return out;
  if (node.className.split(" ").includes(c)) out.push(node);
  for (const ch of node.children) cls(ch, c, out);
  return out;
}

const byId = {};
const document = {
  getElementById: (id) => (byId[id] = byId[id] || new FakeElement("div")),
  createElement: (tag) => new FakeElement(tag),
  createElementNS: (_, tag) => new FakeElement(tag),
  addEventListener() {},
  querySelector: () => null,
  querySelectorAll: () => [],
  documentElement: new FakeElement("html"),
  body: new FakeElement("body"),
  activeElement: null,
  hidden: false,
};
const store = {};
const phone = process.argv[3] === "phone";
const listeners = {};
const media = { matches: phone, addEventListener() {} };
const ctx = vm.createContext({
  addEventListener: (name, fn) => { (listeners[name] = listeners[name] || []).push(fn); },
  document,
  window: {},
  localStorage: { getItem: (k) => store[k] ?? null, setItem: (k, v) => { store[k] = String(v); }, removeItem: (k) => { delete store[k]; } },
  setInterval: () => 0,
  setTimeout: () => 0,
  clearTimeout: () => {},
  fetch: () => new Promise(() => {}),
  EventSource: class { addEventListener() {} },
  matchMedia: () => media,
  location: { hash: "", search: "", origin: "http://127.0.0.1" },
  history: { pushState() {}, replaceState() {} },
  requestAnimationFrame: () => 0,
  scrollTo() {},
  scrollY: 0,
  innerHeight: 844,
  innerWidth: 390,
  navigator: {},
  screen: { width: 390, height: 844 },
  console,
  Date,
  JSON,
  Math,
  Promise,
});

// patch.js stands in as plain replacement: these cases read what is drawn, not how it is patched.
if (phone) Object.assign(ctx, { patch: (el, kids) => el.replaceChildren(...kids), setText: (el, t) => { el.textContent = t; }, focusMark: () => null, refocus() {} });
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), ctx, { filename: "app.js" });

if (phone) {
  // D-09: whom Talk opens after the scope changes, Alarms with no alarm, Back to the entry the page opened on.
  ctx.fire = (name, e) => (listeners[name] || []).forEach((fn) => fn(e || {}));
  ctx.media = media;
  ctx.byId = byId;
  const out = vm.runInContext(`
    const role = (r) => ({ role: r, state: "up", status: "idle" });
    snap = {
      self: "node-a", nodes: [{ name: "node-a", self: true }, { name: "node-b" }], tints: {}, alarms: [],
      crews: [{ key: "default", name: "default", node: "node-a", dispatcher: "dispatcher", roles: [role("dispatcher"), role("dev")] },
        { key: "node-b/ops", name: "ops", node: "node-b", remote: true, dispatcher: "dispatcher", roles: [role("dispatcher")] }],
    };
    const res = {};
    filter = "all";
    lastTalk = { kind: "talk", crew: "default", role: "dev", scope: "all" };
    res.sameScope = phoneTalk();
    filter = "node-b/ops";
    res.otherScope = phoneTalk();
    renderAlarms();
    res.alarms = byId.alarms.className;
    const s = snap;
    snap = null; // the tab switches below draw nothing
    goTab("going");
    location.hash = "";
    fire("popstate");
    res.back = { tab, body: document.body.dataset.tab };
    // The conversation's field says the keyboard is up: the bar steps aside on the phone, never on the desktop.
    const typing = (on) => fire("message", { origin: location.origin, source: undefined, data: on ? "zmina:typing" : "zmina:typed" });
    typing(true);
    res.keyboardPhone = document.body.className;
    typing(false);
    media.matches = false;
    typing(true);
    res.keyboardDesktop = document.body.className;
    typing(false);
    media.matches = true;
    snap = s;
    JSON.stringify(res);
  `, ctx);
  process.stdout.write(out + "\n");
  process.exit(0);
}

// Two nodes, each with its default crew, each asking the same question as A-1.
const ask = (crew) => ({ crew, id: "A-1", kind: "decision", title: "Approve release", text: "Approve release", rec: "yes", reason: "Ready", created: new Date().toISOString() });
ctx.cases = { asks: [ask("default"), ask("node-b/default")] };
ctx.cls = cls;
const out = vm.runInContext(`
  snap = {
    self: "node-a",
    nodes: [{ name: "node-a", self: true }, { name: "node-b" }],
    crews: [{ key: "default", name: "default", node: "node-a" }, { key: "node-b/default", name: "default", node: "node-b", remote: true }],
    tints: {},
  };
  const words = (row) => cls(row, "words")[0].textContent;
  const labels = (row) => cls(row, "cl").map((e) => e.textContent);
  const views = {};
  for (const f of ["all", "node:node-b", "node-b/default"]) {
    filter = f;
    views[f] = {
      cockpit: cases.asks.map((it) => ({ words: words(moveRow(it, false)), labels: labels(moveRow(it, false)) })),
      lean: cases.asks.map((it) => ({ words: words(askRow(it)), labels: labels(askRow(it)) })),
    };
  }
  JSON.stringify(views);
`, ctx);
process.stdout.write(out + "\n");
