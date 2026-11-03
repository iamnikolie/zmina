// Renders rows of the real static/app.js against a minimal fake DOM, under Node (no browser, no network):
//   node internal/ui/testdata/app-labels.js internal/ui/static/app.js
// Prints one JSON object: each case's rows as their visible text. app_labels_test.go reads it.
"use strict";

const fs = require("fs");
const vm = require("vm");

class FakeElement {
  constructor(tag) {
    this.tagName = tag;
    this.children = [];
    this.dataset = {};
    this.style = {};
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
const ctx = vm.createContext({
  document,
  window: {},
  localStorage: { getItem: (k) => store[k] ?? null, setItem: (k, v) => { store[k] = String(v); }, removeItem: (k) => { delete store[k]; } },
  setInterval: () => 0,
  setTimeout: () => 0,
  clearTimeout: () => {},
  fetch: () => new Promise(() => {}),
  EventSource: class { addEventListener() {} },
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  console,
  Date,
  JSON,
  Math,
  Promise,
});

vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), ctx, { filename: "app.js" });

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
