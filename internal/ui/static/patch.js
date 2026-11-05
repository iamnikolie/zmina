// zmina ui — patching. A snapshot comes every time anything in any crew moves, every couple of seconds. Each section
// is drawn anew and patched into the page, so what did not change stays the very element it was: its scroll, its
// focus and an agent's reference to it hold.
//
// Handlers are the catch: a button's handler closes over what it was drawn for, which its markup need not say (two
// checks with one title, the «+2 more» of whichever group is first). So a kept subtree never keeps a handler of its
// own: every element with one is swapped for the one just drawn, which carries the current handler. The one exception
// is a row (.row): its only handler opens it by its data-k, which is in its markup, so equal markup means the same
// row — and a row that differs is swapped whole, its panel's field and buttons with it.
"use strict";

// handles reports an element that carries a handler or holds state a handler reads.
const handles = (n) => n.nodeType === 1 && (["BUTTON", "A", "INPUT", "TEXTAREA", "SELECT"].includes(n.nodeName)
  || n.classList.contains("row") || n.classList.contains("panel") || !!n.onclick || !!n.onkeydown);

// keyed is a handler element whose handler is told by its markup: a row, by its data-k.
const keyed = (n) => n.nodeType === 1 && n.classList.contains("row") && !!n.dataset.k;

// patch makes parent's children the nodes given (strings are text), keeping each old child equal to its new one, and
// those of the same data-k or place where only plain containers differ.
function patch(parent, nodes) {
  const want = nodes.filter((n) => n !== null && n !== undefined && n !== false)
    .map((n) => (typeof n === "string" ? document.createTextNode(n) : n));
  const old = [...parent.childNodes];
  const byKey = new Map(old.filter((n) => n.dataset?.k).map((n) => [n.dataset.k, n]));
  const used = new Set();
  want.forEach((n, i) => {
    let o = n.dataset?.k ? byKey.get(n.dataset.k) : old[i];
    if (!o || used.has(o) || (o.dataset?.k || "") !== (n.dataset?.k || "")) o = null;
    let keep = n;
    if (o) {
      used.add(o);
      keep = morph(o, n);
    }
    if (parent.childNodes[i] !== keep) parent.insertBefore(keep, parent.childNodes[i] || null);
  });
  while (parent.childNodes.length > want.length) parent.lastChild.remove();
}

// morph turns old into nu and returns the node that stays: old itself when only plain containers differ, with every
// handler element in it swapped for the new one (adopt).
function morph(old, nu) {
  // An open panel's indent is measured after it is drawn (indent): the new one starts where the old one stands.
  const op = old.nodeType === 1 && old.querySelector(":scope > .panel");
  const np = op && nu.querySelector(":scope > .panel");
  if (np) np.style.setProperty("--in", op.style.getPropertyValue("--in"));
  const swap = () => { if (old.parentNode) old.replaceWith(nu); return nu; };
  if (old.nodeType !== nu.nodeType || old.nodeName !== nu.nodeName) return swap();
  if (handles(old) || handles(nu)) {
    if (keyed(old) && keyed(nu) && old.dataset.k === nu.dataset.k && old.isEqualNode(nu)) { adopt(old, nu); return old; }
    return swap();
  }
  if (old.isEqualNode(nu)) { adopt(old, nu); return old; }
  if (old.nodeType !== 1) return swap();
  for (const a of [...old.attributes]) if (!nu.hasAttribute(a.name)) old.removeAttribute(a.name);
  for (const a of [...nu.attributes]) if (old.getAttribute(a.name) !== a.value) old.setAttribute(a.name, a.value);
  patch(old, [...nu.childNodes]);
  return old;
}

// adopt walks two equal trees together and swaps every handler element of the old one, but a keyed row, for its new
// twin: the markup holds, the handlers are the current ones.
function adopt(old, nu) {
  const oc = [...old.childNodes];
  const nc = [...nu.childNodes];
  oc.forEach((o, i) => {
    const n = nc[i];
    if (!n || o.nodeType !== 1) return;
    if (handles(o) && !keyed(o)) o.replaceWith(n);
    else adopt(o, n);
  });
}

// setText writes a text only when it changes: an equal write is still a mutation, and a live region reads it out.
function setText(e, t) { if (e.textContent !== t) e.textContent = t; }

// focusMark notes what has the focus, as something a redrawn page can find again: its row or section, its kind and
// its name. refocus puts the focus back on that element when it was swapped for a new one.
const focusSig = (x) => x.nodeName + "|" + x.className + "|" + (x.getAttribute("aria-label") || x.textContent.trim());

function focusMark() {
  const a = document.activeElement;
  if (!a || a === document.body || a.classList.contains("field")) return null;
  const host = a.closest("[data-k]") || a.closest("[id]");
  if (!host) return null;
  return { el: a, host: host.dataset.k ? `[data-k="${CSS.escape(host.dataset.k)}"]` : "#" + CSS.escape(host.id), sig: focusSig(a) };
}

function refocus(m) {
  if (!m || m.el.isConnected) return;
  for (const h of document.querySelectorAll(m.host)) {
    if (!h.offsetParent && h.id !== "full") continue;
    const f = [h, ...h.querySelectorAll("button, a, [tabindex]")].find((x) => focusSig(x) === m.sig);
    if (f) { f.focus({ preventScroll: true }); return; }
  }
}
