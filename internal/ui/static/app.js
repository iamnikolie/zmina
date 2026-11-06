// zmina ui — the lean page: what the owner holds up, what to answer, what goes, what is parked, who waits on
// others; history in a drawer. Every crew under ~/.zmina, live from /api/stream. The page writes only answers and
// the parked list's moves (POST /api/todo, to the item's own crew's dispatcher). Shift text goes in as textContent.
"use strict";

const $ = (id) => document.getElementById(id);
const el = (tag, cls, text) => {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
};

// ---- patching: patch.js (patch, setText, focusMark, refocus), loaded before this file.

// ---- words: English only; task notes and todo text are shown as written

// A language picked in this browser before the page went English-only is forgotten.
localStorage.removeItem("zmina.lang");

const W = {
  hold: "You hold", ask: "Answer", go: "In progress", later: "Parked", others: "Waiting on others", all: "All",
  sayHold: (n) => "You hold " + n, sayAsk: (n) => "answer " + n, sayGo: (n) => "in progress " + n, sayLater: (n) => "parked " + n,
  noHold: "you hold nobody up", noAsk: "no questions", noGo: "nothing goes right now",
  yes: "Yes", no: "No", noText: "no", done: "Done", answer: "Answer…", park: "Later", ignore: "Hide", send: "Send", cancel: "Cancel",
  sentTo: "Sent to the dispatcher", parkedTo: "Parked", ignored: "Hidden", broughtBack: "Brought back", dropped: "Discarded",
  undo: "undo", back: "Bring back", drop: "Discard", mine: "Take it back", madeMine: "Back with you",
  notSent: "Not sent: ", rec: "Recommended: ", more: (n) => n + " more", less: "show less", hands: "By hand:",
  waits: (r) => "waits on " + r, from: (r) => "from " + r, doneText: "done", waitOn: (n, who) => n + " waiting on " + who,
  setAside: (n) => n + " set aside by you",
  doneToday: "done today: ", history: "history", historyH: "What happened", andHistory: "and history",
  allN: (n) => "All " + n, olderWeek: "Older than a week", thisWeek: "This week", comesBack: "Comes back", doneTab: "Done today", historyTab: "History",
  shift: "shift", kb: "tasks", kbGo: "in progress", kbYou: "waiting on you", kbOthers: "waiting on others", kbLater: "parked", kbNone: "empty", stack: "stack", spread: "spread", moreN: (n) => "+" + n + " more", stackIdle: "stack idle", idleN: (n) => "+" + n + " idle",
  fullView: "open on the whole screen", close: "close", fvNow: "Now", fvAsked: "Asked", fvResult: "Result", fvNoResult: "No result recorded.", fvReported: "Worker report", fvLog: "Timeline", fvMove: "Your move",
  fvDetails: "Details", fvScreen: "Screen", fvHolder: "held by", fvWaits: "waits on", fvProject: "project", fvCrew: "crew",
  fvCreated: "created", fvUpdated: "changed", fvRepo: "repo", fvBranch: "branch", fvWorktree: "worktree", fvLink: "link",
  fvDown: "not up", fvNoScreen: "the session is not up", fvLoading: "…", fvNoLog: "nothing in the log yet", ctxOf: (n, max) => n + " of " + max + " before compact", ctxPast: " · compacts when idle", move: "your move", roDone: "done today", oldest: "oldest · ", nobody: "nobody",
  withRec: (n) => n + " with an answer ready", upOf: (u, t) => u + " up of " + t, rolesWorking: (n) => n + (n === 1 ? " role" : " roles") + " working",
  bandCounts: (w, u) => w + " working · " + u + " up", parkedLine: (n, old, back) => "Parked " + n + (old ? " · oldest " + old : "") + (back ? " · " + back + " come back" : ""), noDue: "no due date",
  people: (n) => n + " " + (n === 1 ? "person" : "people"),
  roSpent: "spent today", spentWeek: (v) => "7 days " + v, spentNone: "API-equivalent",
  spentRole: (v) => v + " today", spentTip: "API-equivalent dollars: what these tokens would cost at API rates; the crews run on a subscription", fvCost: "cost",
  sessions: "sessions", upN: (n) => n + " up", downN: (n) => n + " not started",
  notUp: "not started: ", tick: "tick ", always: "always", working: "working", free: "idle",
  onScreen: "question on screen", stuck: "stuck", check: "check", nobodyUp: "nobody up",
  stopped: (c, t) => c + ": ticker stopped" + (t ? " " + t + " ago" : ""), shiftStopped: (c) => c + ": shift stopped",
  lost: "reconnecting…", noCrew: "No crew under ~/.zmina — zmina init",
  min: "min", h: "h", d: "d", lessMin: "<1 min", busyLive: "working", notLive: "not working",
  alarms: "Alarms", alSet: "Set", alNew: "Remind me", alWhat: "What to remind you of", alWhen: "tomorrow 09:00, +2h", alWhenLabel: "When: tomorrow 09:00, +2h, thu, a date",
  alMove: "Move", alOff: "Off", alMoved: "Moved", alOffDone: "Taken off", alSetDone: "Set", alNone: "no alarms set",
  alFor: (f) => (f === "you" ? "" : "for " + f), alWakes: (r) => r + " wakes", alBack: "back: ", alIn: (d) => "in " + d,
  alAsWords: (w) => "set as «" + w + "»", alDue: "due now", backTag: "back",
  seen: (a) => "seen " + a, staleN: (a) => "stale" + (a ? " " + a : ""), stale: "stale", offline: "offline", stoppedList: "stopped:",
  silent: (a) => "ticker stopped" + (a ? " " + a : ""),
  spare: "spare", retiredW: "retired", choicesN: (n) => n + " choices", chose: (r, l) => r + ": " + l, notUpN: (n) => "+" + n + " not up", fixTicker: (c) => "zmina" + (c === "default" ? "" : " --config " + c.split("/").pop()) + " ticker status",
  fixDown: (r) => "start it, or hand its task on: zmina up " + r, onScreenN: (n) => n + " on screen", crewsN: (n) => n + " crews", nothingOn: (w) => "Nothing waits on you on " + w + ".",
  elsewhere: (h, q) => "Elsewhere: " + h + " hold · " + q + " answer → All", downHolds: (r, ts) => r + ": down, holds " + ts,
  answerOn: (n) => "answer on " + n, detailsOn: (n) => "details on " + n,
  talk: "Talk", talkTo: (r) => "Talk to " + r,
  tabAgents: "Agents", tabLater: "Later", overMesh: " · over the mesh", upW: "up",
  waitingOn: "Waiting on you", workingW: "Working", idleW: "Idle", yoursByHand: "Yours by hand", crewsNodes: "Crews and nodes",
  nothingWaits: "Nothing waits on you", busyNoTask: "busy, no task", downHoldsP: (ts) => "Down · holds " + ts,
  tickerNoWake: "Ticker stopped · the crew does not wake", asksQ: (q) => "Asks: " + q, nWaiting: (n) => n + " waiting",
  oldestW: (d) => "oldest " + d, nextAt: (w) => "next " + w, raisedBy: (r) => "raised by " + r, moveTo: "Move to…", alarmW: "Alarm",
  nDown: (n) => n + " down", roleDown: (r) => r + " down", nWorking: (n) => n + " working", tickerStopped: "ticker stopped",
  stoppedW: "stopped", doneTodayN: "Done today", parkedN: (n, old) => n + (old ? " · oldest " + old : ""),
  ev: {
    alarm: "alarm: ", sleep: "asleep ",
    new: "started: ", waitingOn: "waits on you: ", waitingWho: (w) => "waits on " + w + ": ", done: "done", dropped: "dropped",
    reopened: "back in work", asked: "asks you: ", answered: "you answered: ", parked: "parked: ",
    blocked: "stuck: ", watchFailed: "watch failed: ", start: "shift started", stop: "shift stopped",
  },
};

// ---- state

let snap = null;
let cost = null; // /api/cost: API-equivalent dollars today and over the week, read every minute
let got = 0; // when the snapshot came, so ages move between snapshots
let first = true;
let filter = localStorage.getItem("zmina.crew") || "all";
let lastTick = "";
let hover = null;
let showAllAsks = false;
let stale = false; // a snapshot came while a reply was being typed: it is drawn once the field is left
let pointerDown = false;
let openHand = "";
let openRow = ""; // the one row whose actions are open
let draft = { k: "", text: "" }; // the reply being typed, kept across renders
let alDraft = { text: "", when: "" }; // the reminder being set, kept across renders
let full = null; // the task opened on the whole screen: {crew, id, data: /api/task, peek: its holder's screen}
const groups = new Set(); // «waiting on others» groups open
const stacked = new Set(JSON.parse(localStorage.getItem("zmina.stacked") || "[]")); // board columns folded into a stack
const seen = new Set();
const seenNow = new Set(); // the rows this render drew: both layouts draw a row, and both may enter
const pending = new Map(); // row key -> the action held for its 5 s undo; the row is off the page meanwhile
const sentKeys = new Set(); // rows the server took, kept off until the snapshot drops them

const T = (t) => Date.parse(t);
const zero = (t) => !t || t.startsWith("0001");
const nowMs = () => T(snap.now) + (Date.now() - got);
const hhmm = (t) => new Date(t).toTimeString().slice(0, 5);
function dur(ms) {
  const m = Math.max(0, Math.floor(ms / 60000));
  if (m < 1) return W.lessMin;
  if (m < 60) return m + " " + W.min;
  if (m < 48 * 60) return Math.floor(m / 60) + " " + W.h;
  return Math.floor(m / 1440) + " " + W.d;
}
const ago = (t) => (zero(t) ? "" : dur(nowMs() - T(t)));
const crewOf = (key) => snap.crews.find((c) => c.key === key);
const crewName = (key) => (crewOf(key) || { name: key }).name;
const multiNode = () => (snap.nodes || []).length > 1;
const multi = () => snap.crews.length > 1 || multiNode();
// remote is a peer's crew: shown read-only. nodeOf is the mesh node a crew runs on.
const remote = (crew) => !!crewOf(crew)?.remote;
const nodeOf = (crew) => crewOf(crew)?.node || crew.split("/")[0];
// crewLabel names a crew on the page: its name; with more than one node, node/crew, the node alone for «default».
function crewLabel(crew) {
  const c = crewOf(crew);
  const moved = c && c.retired && c.moved_to ? " → " + c.moved_to : "";
  if (!multiNode()) return crewName(crew) + moved;
  const full = remote(crew) || !snap.self ? crew : snap.self + "/" + crew;
  return full.replace(/\/default$/, "") + moved;
}
// shownLabel is the label a row carries: «always» also for the node's default crew when there is one node. With more
// than one node it drops what the crew switch already says: a crew drops all of it, a node drops the node.
function shownLabel(crew, always) {
  if (!multi()) return "";
  if (!multiNode()) return always || crew !== "default" ? crewName(crew) : "";
  if (filter === crew) return "";
  const l = crewLabel(crew);
  const n = nodeOf(crew);
  return filter === "node:" + n ? (l === n ? "" : l.slice(n.length + 1)) : l;
}
const who = (crew, role) => (role ? (shownLabel(crew) ? shownLabel(crew) + "/" : "") + role : "");
// crewTag is that label in the page's quiet mono; null when there is none to show.
function crewTag(crew, always) {
  const l = shownLabel(crew, always);
  return l ? el("span", "cl", l) : null;
}
// whoTag is a role with its crew label before it.
function whoTag(crew, role) {
  const s = el("span");
  const l = shownLabel(crew);
  if (l) s.append(el("span", "cl", l + "/"));
  s.append(role);
  return s;
}
const tickerDead = (c) => zero(c.last_tick) || nowMs() - T(c.last_tick) > 2 * 60000;
const key = (crew, id) => crew + "/" + id;
// otherKey is a «waiting on others» row: taken back, the same task comes up in «you hold» under its own key.
const otherKey = (w) => "others:" + key(w.crew, w.id);
// A crew stopped on purpose with nothing waiting on the owner (dormant) is folded away from «all» (unless every crew
// is); its name in the crew switch, or its node's chip, brings it back.
const folded = (c) => (!!c.dormant || (c.retired && !c.moved_to)) && snap.crews.some((x) => !x.dormant && !x.retired);
// away is a crew the owner retired into another (D-07 s4): gone from the page, the switch and every count.
const away = (crew) => { const c = crewOf(crew); return !!(c && c.retired && c.moved_to); };
// successor is the page's key of the crew a retired one went on as: this node's crews go by their bare name.
const successor = (c) => (snap.self && c.moved_to.startsWith(snap.self + "/") ? c.moved_to.slice(snap.self.length + 1) : c.moved_to);
const shows = (crew) => {
  // A retired crew leaves the switch, the sessions and the counts of crews, but what it still holds stays in sight:
  // under All and under the crew it went on as, labelled «crew → successor», answered on its own crew (D-07 s4).
  if (away(crew)) {
    const s = successor(crewOf(crew));
    return filter === "all" || filter === s || (filter.startsWith("node:") && nodeOf(s) === filter.slice(5));
  }
  if (filter === "all") { const c = crewOf(crew); return !c || !folded(c); }
  if (filter.startsWith("node:")) return nodeOf(crew) === filter.slice(5);
  return filter === crew;
};
const validFilter = (f) => f === "all" || (f.startsWith("node:")
  ? (snap.nodes || []).some((n) => n.name === f.slice(5)) || snap.crews.some((c) => c.node === f.slice(5)) : !!crewOf(f));
// stl marks the rows of a crew whose last read is old.
const stl = (crew) => (crewOf(crew)?.stale ? " stale" : "");
const HOUR = 3600e3;

function tint(node, project) {
  const t = project && snap.tints[project];
  if (!t) return node;
  node.dataset.p = project;
  node.style.setProperty("--t", "var(--" + t + ")");
  return node;
}

function button(cls, label, fn, title) {
  const b = el("button", cls, label);
  b.type = "button";
  if (title) b.title = title;
  b.addEventListener("click", (e) => { e.stopPropagation(); fn(); });
  return b;
}

// ---- render

function render() {
  if (!snap) return;
  // A reply being typed keeps its focus and caret through a re-render.
  const typing = document.activeElement?.classList.contains("field") ? document.activeElement.selectionStart : -1;
  const focused = focusMark();
  // The saved choice holds while it matches something: a peer's crews may come after the first snapshot.
  const saved = localStorage.getItem("zmina.crew") || "all";
  // The phone has no switch (D-12 h1): it shows every crew, each row saying where it is.
  filter = phone.matches || !validFilter(saved) ? "all" : saved;

  // Keys the server took leave once its snapshot no longer has them.
  const held = new Set([...snap.todo, ...snap.later].map((it) => key(it.crew, it.id)));
  for (const a of snap.alarms || []) held.add(alarmKey(a));
  for (const w of snap.work) if (w.lane === "others") held.add(otherKey(w));
  for (const k of sentKeys) if (!held.has(k)) sentKeys.delete(k);
  const todo = snap.todo.filter((it) => shows(it.crew));
  const age = (it) => nowMs() - T(it.created);
  const hold = todo.filter((it) => it.holds).sort((a, b) => age(b) - age(a));
  const asks = todo.filter((it) => !it.holds);
  const work = snap.work.filter((w) => shows(w.crew));
  const going = work.filter((w) => w.lane === "go");
  const others = work.filter((w) => w.lane === "others");
  const parked = snap.later.filter((it) => shows(it.crew));

  renderSay(hold, asks, going, parked);
  renderChips();
  renderHold(hold);
  renderAsks(asks);
  renderGo(going);
  renderLater(parked);
  renderAlarms();
  renderOthers(others);
  renderQuiet();
  renderFoot();
  renderCockpit(hold, asks, others, parked);
  renderPhone();
  for (const k of seenNow) seen.add(k);
  seenNow.clear();
  if (!$("history").hidden) renderFeed();
  renderFull();
  renderSide();
  applyHover();
  indent();
  refocus(focused);
  if (typing >= 0) {
    // Both layouts draw the open row; the one on screen takes the caret.
    const f = full ? $("full").querySelector(".field")
      : [...document.querySelectorAll(`[data-k="${CSS.escape(openRow)}"] .panel .field`)].find((x) => x.offsetParent);
    f?.focus();
    f?.setSelectionRange(typing, typing);
  }
  document.body.classList.remove("sk");
  first = false;
}

function renderSay(hold, asks, going, parked) {
  // Drawn aside and patched in: the line is a live region, read out again only when it says something new.
  const say = el("div");
  try { sayParts(say, hold, asks, going, parked); } finally { patch($("say"), [...say.childNodes]); }
}

function sayParts(say, hold, asks, going, parked) {
  if (!snap.crews.length) {
    say.append(el("span", "x", W.noCrew));
    return;
  }
  const long = hold.some((it) => nowMs() - T(it.created) > HOUR);
  const parts = [[W.sayHold(hold.length), long ? "x" : "n", "hold"], [W.sayAsk(asks.length), "", "ask"],
    [W.sayGo(going.length), "b", "go"], [W.sayLater(parked.length), "m", "later"]];
  parts.forEach(([text, cls, target], i) => {
    if (i) say.append(" · ");
    // The line is a live region: its parts carry no handler of their own (one listener on the line below), so an
    // update that changes nothing in it changes nothing in the page.
    const s = el("span", "k " + cls, text);
    s.dataset.to = target;
    say.append(s);
  });
  const dead = redList().map((d) => d.text);
  for (const d of dead) say.append(" · ", el("span", "k x", d));

  $("clock").classList.toggle("dead", dead.length > 0);
  $("mark").classList.toggle("stopped", dead.length > 0);
  setText($("clock").lastElementChild, hhmm(nowMs()));
  document.title = ["zmina", ...dead, hold.length ? W.sayHold(hold.length) : "", asks.length ? W.sayAsk(asks.length) : ""]
    .filter(Boolean).join(" · ");
  favicon(dead.length || long ? "#ff7b72" : hold.length + asks.length ? "#f0b45a" : "");

  // One mark for every crew: it redraws on a beat from any of them.
  const newest = snap.crews.map((c) => c.last_tick || "").sort().pop() || "";
  if (newest && lastTick && newest !== lastTick) beat();
  lastTick = newest;
}

// redList is what is red, act now (D-07 s5): a live crew's ticker overdue, a role down that holds an active task (a peer
// node offline is red on its chip). A stopped, retired or spare crew is never red; a peer read long ago says «stale».
// Each names what to do: the crew's dispatcher to open, or the role.
function redList(all) {
  const out = [];
  for (const c of snap.crews.filter((c) => !folded(c) && !c.stale && !c.retired && !c.spare && (all || shows(c.key)))) {
    if (!zero(c.stopped)) continue;
    if (tickerDead(c)) {
      out.push({ text: W.stopped(crewLabel(c.key) || c.name, zero(c.last_tick) ? "" : ago(c.last_tick)), crew: c.key,
        role: c.dispatcher || "dispatcher", fix: W.fixTicker(c.key), kind: "ticker" });
    }
    for (const r of c.roles || []) {
      if (r.state !== "up" && (r.tasks || []).length && zero(r.asleep || "")) {
        out.push({ text: W.downHolds(who(c.key, r.role) || r.role, r.tasks.join(", ")), crew: c.key, role: r.role, fix: W.fixDown(r.role), kind: "down" });
      }
    }
  }
  return out;
}

function beat() {
  const m = $("mark");
  m.classList.remove("beat");
  void m.offsetWidth;
  m.classList.add("beat");
}

function favicon(color) {
  const z = '<path d="M5.6 7.6C5.4 3.9 18.6 3.7 18.1 7.4L6.2 16.8C5.4 20.4 18.6 20.2 18.4 16.4" fill="none" stroke="%23777" stroke-width="2.4" stroke-linecap="round"/>';
  const d = color ? '<circle cx="19" cy="5" r="5" fill="' + encodeURIComponent(color) + '"/>' : "";
  $("favicon").href = "data:image/svg+xml," + '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24">' + z + d + "</svg>";
}

// crewChip is one choice of the crew switch: «all», a node (node:name) or a crew; note is a word after the name.
function crewChip(k, label, cls, note) {
  const b = button([filter === k ? "on" : "", cls].filter(Boolean).join(" "), label, () => { filter = k; menuNode = ""; localStorage.setItem("zmina.crew", k); render(); });
  b.setAttribute("aria-pressed", filter === k ? "true" : "false");
  const c = crewOf(k);
  if (c && c.spare) b.append(el("span", "ns spare", W.spare));
  else if (note) b.append(el("span", "ns bad", note));
  return b;
}

// nodeChip is a node with how it was read: a green dot while fresh («seen 3 min» is in the tooltip), a hollow one and
// «stale 14 min» when the crews shown are an old read, a red one and «offline» (the reason in the tooltip) when it was
// never read. This node says nothing. Never amber: amber is what waits on you.
function nodeChip(n) {
  const b = crewChip("node:" + n.name, n.name, "nd");
  const mark = (cls, text) => { b.append(el("i", "nsd " + cls)); if (text) b.append(el("span", "ns " + cls, text)); };
  if (n.state === "fresh") mark("ok");
  else if (n.state === "stale") mark("hollow", W.staleN(ago(n.seen)));
  else if (n.state === "offline") mark("bad", W.offline);
  const seen = n.state === "fresh" || n.state === "stale" ? W.seen(ago(n.seen)) : "";
  b.title = [seen, n.error].filter(Boolean).join(" · ");
  return b;
}

// silent is the note on a crew whose ticker has been silent for long without being stopped.
const silent = (c) => (c.silent ? W.silent(ago(c.last_tick)) : "");

function renderChips() {
  const nav = $("chips");
  nav.hidden = !multi();
  if (!multi()) return;
  nav.classList.toggle("mesh", multiNode());
  const items = [crewChip("all", W.all)];
  const menu = $("chipmenu");
  if (multiNode()) {
    // D-07 h3: the switch is nodes. A node with more than one crew reads «box-ops 3 crews ▾» and opens a menu of its crews
    // (spare and silent words, then its stopped line); a crew chosen shows as «box-ops / acme ✕».
    for (const n of snap.nodes) {
      const g = el("span", "ng");
      const crews = (n.crews || []).map(crewOf).filter((c) => c && !away(c.key));
      g.append(nodeChip(n));
      const chosen = crewOf(filter);
      if (chosen && chosen.node === n.name && crews.length > 1) {
        g.append(crewChip("node:" + n.name, "/ " + chosen.key.split("/").pop() + " ✕", "on"));
      } else if (crews.length > 1) {
        const m = button("nm", W.crewsN(crews.length) + " ▾", () => { menuNode = menuNode === n.name ? "" : n.name; render(); });
        m.dataset.node = n.name;
        m.setAttribute("aria-expanded", menuNode === n.name ? "true" : "false");
        m.setAttribute("aria-haspopup", "menu");
        g.append(m);
      }
      items.push(g);
    }
  } else items.push(...snap.crews.filter((c) => !folded(c) && !away(c.key)).map((c) => crewChip(c.key, c.name, "", silent(c))));
  // The folded crews: one muted line, each name brings its crew back (with nodes, in each node's menu).
  const off = multiNode() ? [] : snap.crews.filter(folded);
  if (off.length) {
    const g = el("span", "ng off");
    g.append(el("span", "", W.stoppedList));
    for (const c of off) {
      const age = ago(zero(c.stopped) ? c.last_tick : c.stopped);
      g.append(crewChip(c.key, crewLabel(c.key) + (c.retired ? " " + W.retiredW : age ? " " + age : "")));
    }
    items.push(g);
  }
  patch(nav, [...items]);
  renderChipMenu(menu);
  // The edge fades only when the chips scroll.
  nav.classList.toggle("over", nav.scrollWidth > nav.clientWidth + 1);
}

let menuNode = "";

// renderChipMenu draws the open node's crews under its «N crews ▾».
function renderChipMenu(menu) {
  const n = (snap.nodes || []).find((x) => x.name === menuNode);
  const opener = n && document.querySelector(`.chips .nm[data-node="${CSS.escape(n.name)}"]`);
  if (!n || !opener) { menu.hidden = true; return; }
  const crews = (n.crews || []).map(crewOf).filter((c) => c && !away(c.key));
  const items = crews.filter((c) => !folded(c)).map((c) => crewChip(c.key, c.key.split("/").pop(), "", silent(c)));
  const off = crews.filter(folded);
  if (off.length) {
    const g = el("div", "off");
    g.append(el("span", "", W.stoppedList));
    for (const c of off) {
      const age = ago(zero(c.stopped) ? c.last_tick : c.stopped);
      g.append(crewChip(c.key, c.key.split("/").pop() + (c.retired ? " " + W.retiredW : age ? " " + age : "")));
    }
    items.push(g);
  }
  patch(menu, items);
  const r = opener.getBoundingClientRect();
  menu.style.top = r.bottom + 6 + "px";
  menu.style.left = Math.max(8, Math.min(r.left, innerWidth - 260)) + "px";
  menu.hidden = false;
}

document.addEventListener("click", (e) => {
  if (menuNode && !e.target.closest(".chipmenu, .chips .nm")) { menuNode = ""; render(); }
});

function heading(id, label, n, cls) {
  patch($(id), [label + " ", el("b", cls || "", String(n))]);
}

// fresh marks a row that came after the first render, so it enters from the top.
function fresh(row, k) {
  if (!first && !seen.has(k)) row.classList.add("enter");
  seenNow.add(k);
  return row;
}

// indent starts the open row's panel at its title's left edge (p5); below 860 it takes the full width.
function indent() {
  for (const r of document.querySelectorAll(".row.open")) {
    const tt = r.querySelector(":scope > .words .tt");
    const p = r.querySelector(":scope > .panel");
    if (!tt || !p) continue;
    p.style.setProperty("--in", "0px");
    if (matchMedia("(min-width: 861px)").matches) p.style.setProperty("--in", Math.max(0, tt.getBoundingClientRect().left - p.getBoundingClientRect().left) + "px");
  }
}

// ---- rows (b1): no button bars. A row is one focusable element ending in «›»; a tap opens its actions under it,
// one row at a time; a second tap or Esc closes it. From 860 px, hover or focus shows the actions in place.

const gone = (k) => pending.has(k) || sentKeys.has(k);

function toggleRow(k) {
  if (openRow === k) {
    openRow = "";
    if (openHand === k) openHand = "";
  } else openRow = k;
  render();
}

// rowShell is a row: its words, its age, an optional chip, the «›»; hov the in-place actions (≥ 860), panel what
// opens under it. A click anywhere on it opens it; the «›» is the row's own button, named by its title, so the
// keyboard and assistive tech reach it and the actions beside it — never inside a button, where they would be lost.
function rowShell(k, cls, words, age, chip, hov, panel) {
  const row = fresh(el("div", "row" + (cls ? " " + cls : "")), k);
  row.dataset.k = k;
  const isOpen = openRow === k;
  row.classList.toggle("open", isOpen);
  row.addEventListener("click", (e) => {
    if (e.target.closest("button, a, input, textarea")) return;
    toggleRow(k);
  });
  const toggle = button("chev", "›", () => toggleRow(k));
  toggle.setAttribute("aria-expanded", isOpen ? "true" : "false");
  toggle.setAttribute("aria-label", (words.querySelector(".tt") || words).textContent.trim().slice(0, 160));
  row.append(words, age || el("span"), chip || el("span"), toggle);
  if (hov && hov.childNodes.length) {
    hov.className = "hov";
    row.append(hov);
  }
  if (isOpen && panel) {
    panel.classList.add("panel");
    row.append(panel);
  }
  return row;
}

// act sends the action after a 5 s undo; k is the row it hides, the item's own key unless that row has another.
function act(it, action, text, word, k = key(it.crew, it.id)) {
  openRow = "";
  full = null;
  draft = { k: "", text: "" };
  const t = toast(word, () => { clearTimeout(timer); pending.delete(k); render(); });
  const timer = setTimeout(async () => {
    pending.delete(k);
    sentKeys.add(k);
    t.remove();
    try {
      await post({ crew: it.crew, id: it.id, action, text });
    } catch (e) {
      sentKeys.delete(k);
      toast(W.notSent + e.message);
    }
    render();
  }, 5000);
  pending.set(k, timer);
  render();
}

// toast says what was done, with «undo» for 5 s; one without undo (an error) leaves after 5 s too.
function toast(word, undo) {
  const t = el("div", "toast");
  t.append(el("span", "", word));
  if (undo) {
    t.append(" · ", button("", W.undo, () => { t.remove(); undo(); }));
    t.append(el("div", "bar5"));
  }
  $("toasts").append(t);
  if (!undo) setTimeout(() => t.remove(), 5000);
  return t;
}

async function post(body) { return postTo("api/todo", body); }

async function postTo(url, body) {
  const r = await fetch(url, { method: "POST", headers: { "Content-Type": "application/json", "X-Zmina": "1" }, body: JSON.stringify(body) });
  if (!r.ok) throw new Error((await r.text()).trim());
}

// ---- the opened row (D-02 p1–p5): the title wraps in place; under it the note as one sentence, one faint meta
// line with the link named by its source, then one action line — a field where the row takes words, one primary,
// the rest muted text.

// says is whether the why tells something the title does not.
function says(why, title) {
  const n = (s) => (s || "").toLowerCase().replace(/[^\p{L}\p{N}]+/gu, " ").trim();
  return n(why) !== "" && !n(title).includes(n(why));
}

// taskBody is a task's opened text: its latest note says where it stands; the why only when no note came yet.
const taskBody = (why, title, note) => (note ? [note] : [says(why, title) ? why : ""]);

// source names a link by where it goes: the MR or PR number, else the service, else the host.
function source(url) {
  let u;
  try { u = new URL(url); } catch { return url; }
  const n = u.pathname.match(/\/merge_requests\/(\d+)/) || u.pathname.match(/\/pull\/(\d+)/);
  if (n) return (u.pathname.includes("/pull/") ? "#" : "!") + n[1];
  const host = u.hostname.replace(/^www\./, "");
  const named = [["fibery", "Fibery"], ["gitlab", "GitLab"], ["github", "GitHub"], ["slack", "Slack"], ["figma", "Figma"], ["notion", "Notion"]];
  return (named.find(([h]) => host.includes(h)) || [, u.host])[1];
}

// ICONS are the panel's actions in lucide's shapes (24 grid, stroke only), drawn inline: no icon package.
const ICONS = {
  check: '<path d="M20 6 9 17l-5-5"/>',
  send: '<path d="M14.536 21.686a.5.5 0 0 0 .937-.024l6.5-19a.496.496 0 0 0-.635-.635l-19 6.5a.5.5 0 0 0-.024.937l7.93 3.18a2 2 0 0 1 1.112 1.11z"/><path d="m21.854 2.147-10.94 10.939"/>',
  clock: '<circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/>',
  eyeOff: '<path d="M10.733 5.076a10.744 10.744 0 0 1 11.205 6.575 1 1 0 0 1 0 .696 10.747 10.747 0 0 1-1.444 2.49"/><path d="M14.084 14.158a3 3 0 0 1-4.242-4.242"/><path d="M17.479 17.499a10.75 10.75 0 0 1-15.417-5.151 1 1 0 0 1 0-.696 10.75 10.75 0 0 1 4.446-5.143"/><path d="m2 2 20 20"/>',
  x: '<path d="M18 6 6 18"/><path d="m6 6 12 12"/>',
  undo: '<path d="M9 14 4 9l5-5"/><path d="M4 9h10.5a5.5 5.5 0 0 1 5.5 5.5a5.5 5.5 0 0 1-5.5 5.5H11"/>',
  trash: '<path d="M3 6h18"/><path d="M19 6v14c0 1-1 2-2 2H7c-1 0-2-1-2-2V6"/><path d="M8 6V4c0-1 1-2 2-2h4c1 0 2 1 2 2v2"/><path d="M10 11v6"/><path d="M14 11v6"/>',
  mine: '<path d="M9 14 4 9l5-5"/><path d="M20 20v-7a4 4 0 0 0-4-4H4"/>',
  stack: '<path d="m12.83 2.18a2 2 0 0 0-1.66 0L2.6 6.08a1 1 0 0 0 0 1.83l8.58 3.91a2 2 0 0 0 1.66 0l8.58-3.9a1 1 0 0 0 0-1.83Z"/><path d="m22 17.65-9.17 4.16a2 2 0 0 1-1.66 0L2 17.65"/><path d="m22 12.65-9.17 4.16a2 2 0 0 1-1.66 0L2 12.65"/>',
  spread: '<path d="M12 22v-6"/><path d="M12 8V2"/><path d="M4 12H2"/><path d="M10 12H8"/><path d="M16 12h-2"/><path d="M22 12h-2"/><path d="m15 19-3 3-3-3"/><path d="m15 5-3-3-3 3"/>',
  full: '<path d="M15 3h6v6"/><path d="M9 21H3v-6"/><path d="m21 3-7 7"/><path d="m3 21 7-7"/>',
  refresh: '<path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/>',
  link: '<path d="M15 3h6v6"/><path d="M10 14 21 3"/><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h3"/>',
};

function icon(name) {
  const s = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  for (const [k, v] of Object.entries({ viewBox: "0 0 24 24", width: 18, height: 18, fill: "none", stroke: "currentColor",
    "stroke-width": 1.75, "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true", class: "ic" })) s.setAttribute(k, v);
  s.innerHTML = ICONS[name];
  return s;
}

// labelled fills a button with its icon and its word.
function labelled(b, ic, label) {
  b.replaceChildren(icon(ic), el("span", "", label));
  return b;
}

// panel builds the opened row. o: body (lines, empty ones dropped), meta (words), url; reply (the item the field
// answers), primary ({label, fn, icon}: what the primary does while the field is empty), muted ([label, fn, icon]:
// quiet buttons, a 16 px icon and its short word — one label per control, A-11). crew: the item's crew; a peer's is
// read-only, so the field and the buttons give way to «answer on <node>».
function panel(o) {
  const p = el("div");
  for (const line of (o.body || []).filter(Boolean)) p.append(el("p", "pn", line));
  const meta = (o.meta || []).filter(Boolean);
  if (meta.length || o.url) {
    const m = el("p", "meta", meta.join(" · "));
    if (o.url) {
      const a = el("a", "", source(o.url));
      a.append(icon("link"));
      a.href = o.url;
      a.title = o.url;
      a.target = "_blank";
      a.rel = "noreferrer";
      m.append(meta.length ? " · " : "", a);
    }
    p.append(m);
  }
  // The field, then the buttons as one group: when the line is too narrow the group moves under the field whole
  // and the field takes the width (f2). In a column narrower still, the muted ones move under the primary together;
  // a button never wraps alone.
  const line = el("div", "ctl");
  const acts = el("div", "acts");
  if (o.reply) {
    const it = o.reply;
    const k = key(it.crew, it.id);
    const f = el("textarea", "field");
    f.rows = 1;
    f.placeholder = W.answer;
    f.setAttribute("aria-label", W.answer);
    if (draft.k === k) f.value = draft.text;
    const prim = button("btn", "", () => (f.value.trim() ? act(it, "answer", f.value.trim(), W.sentTo) : o.primary && o.primary.fn()));
    const sync = () => {
      const typed = f.value.trim() !== "";
      if (typed || !o.primary) labelled(prim, "send", W.send);
      else labelled(prim, o.primary.icon, o.primary.label);
      prim.disabled = !typed && !o.primary;
    };
    sync();
    f.addEventListener("input", () => { draft = { k, text: f.value }; sync(); });
    f.addEventListener("keydown", (e) => {
      if (e.key === "Escape") { e.preventDefault(); draft = { k: "", text: "" }; if (full) closeFull(); else toggleRow(k); }
      if (e.key === "Enter" && !e.shiftKey && f.value.trim()) { e.preventDefault(); prim.click(); }
    });
    line.append(f);
    acts.append(prim);
  } else if (o.primary) {
    // Without a field to stand beside, the primary is a light outline: the row is read more than acted on.
    acts.append(labelled(button("btn lite", "", o.primary.fn), o.primary.icon, o.primary.label));
  }
  const mute = el("div", "mute");
  for (const [label, fn, ic] of o.muted || []) {
    // A narrow card keeps only the icon: the word stays in the tooltip and the button's name.
    const b = labelled(button("ib", "", fn, label), ic, label);
    b.setAttribute("aria-label", label);
    mute.append(b);
  }
  if (mute.childNodes.length) acts.append(mute);
  if (acts.childNodes.length) line.append(acts);
  if (line.childNodes.length) p.append(line);
  return p;
}

// done says «done» to the session that waits: a check or an ask is closed as done, a waiting task hears «done».
function done(it) {
  return it.id.startsWith("T-") ? act(it, "answer", W.doneText, W.sentTo) : act(it, "done", "", W.sentTo);
}

// itemPanel is an ask's or a held task's opened row; its primary is the recommendation, else «done» where the item
// can be done (a check, anything the owner holds).
function itemPanel(it, holds) {
  if (it.kind === "later") return backPanel(it);
  const rec = it.kind === "decision" && it.rec;
  const primary = rec ? { label: it.rec, fn: () => act(it, "yes", "", W.sentTo), icon: "check" }
    : holds || it.kind === "check" ? { label: W.done, fn: () => done(it), icon: "check" } : null;
  const muted = [[W.park, () => act(it, "park", "", W.parkedTo), "clock"]];
  if (holds) muted.push([W.ignore, () => act(it, "ignore", "", W.ignored), "eyeOff"]);
  else if (it.kind !== "check") muted.push([W.no, () => act(it, "answer", W.noText, W.sentTo), "x"]);
  return panel({
    body: it.id.startsWith("T-") ? taskBody(it.text, it.title, it.note)
      : [says(it.text, it.title || it.text) ? it.text : "", rec ? it.reason : it.note],
    meta: [holds ? W.waits(who(it.crew, it.blocks || it.role) || "—") : it.role && W.from(who(it.crew, it.role))], url: it.url,
    reply: it, primary, muted, crew: it.crew,
  });
}

// backPanel is a parked item its alarm brought back: «Bring back» hands it to the dispatcher; it can be parked again
// or discarded.
function backPanel(it) {
  return panel({ body: [it.note], primary: { label: W.back, fn: () => act(it, "back", "", W.broughtBack), icon: "undo" }, crew: it.crew,
    muted: [[W.park, () => act(it, "park", "", W.parkedTo), "clock"], [W.drop, () => act(it, "drop", "", W.dropped), "trash"]] });
}

// ids is what the row's title attribute carries: the ids leave the screen.
const ids = (...xs) => [...new Set(xs.filter(Boolean))].join(" · ");
// idOf is an id as it reads where its crew is not in the same row (D-07 l1): «acme L-5» once more than one crew shows.
const idOf = (crew, id) => (multi() && filter === "all" ? (crew.split("/").pop() + " ") : "") + id;

function itemWords(it, holds) {
  const words = el("div", "words");
  if (it.project) words.append(el("span", "pj", it.project));
  words.append(el("span", "tt", it.title || it.text));
  // Whose it is, while the switch shows more than one crew: the same question from two nodes reads apart.
  const cl = crewTag(it.crew, true);
  if (cl) words.append(cl);
  // What the owner holds says what to do: the latest note, else the why; who waits is in the opened row.
  if (holds) words.append(el("span", "gr", it.note || (it.text !== it.title ? it.text : "")));
  else if (!it.rec && it.note) words.append(el("span", "gr", it.note));
  return words;
}

function ageOf(it) {
  const a = nowMs() - T(it.created);
  return el("span", "age" + (a >= 24 * HOUR ? " late" : ""), dur(a));
}

// chip is the recommended answer in its own words; a tap takes it (b2).
function chip(it) {
  if (!(it.kind === "decision" && it.rec)) return null;
  const c = button("chip", it.rec.length > 28 ? [...it.rec].slice(0, 27).join("") + "…" : it.rec, () => act(it, "yes", "", W.sentTo), W.rec + it.rec);
  c.setAttribute("aria-label", W.yes + ": " + it.rec);
  return c;
}

// ownHov is the in-place actions of a row. A peer's crew has them too: the answer goes to that node, signed with the
// owner's key, and is done there (POST /api/todo routes it).
function ownHov(crew, hov) {
  return hov;
}

// ---- L1 · you hold (b3): the row is the button; a tap opens the reply to the session that waits

function renderHold(hold) {
  heading("hold-h", W.hold, hold.length, hold.some((it) => nowMs() - T(it.created) > HOUR) ? "x" : "n");
  const rows = hold.filter((it) => !gone(key(it.crew, it.id))).map((it) => {
    const k = key(it.crew, it.id);
    const hov = el("div");
    hov.append(button("q m", W.park, () => act(it, "park", "", W.parkedTo)), button("q m", W.ignore, () => act(it, "ignore", "", W.ignored)));
    const row = rowShell(k, stl(it.crew), itemWords(it, true), ageOf(it), chip(it), ownHov(it.crew, hov), itemPanel(it, true));
    row.title = ids(idOf(it.crew, it.id), it.task && idOf(it.crew, it.task));
    if (it.task) row.dataset.w = key(it.crew, it.task);
    return tint(row, it.project);
  });
  $("hold").classList.toggle("empty", !rows.length);
  if (!rows.length) rows.push(el("p", "none", W.noHold));
  patch($("hold-rows"), [...rows]);
}

// ---- L2 · answer (b2): one chip with the recommendation; open: the field, «Later», «No»

function askRow(it) {
  const k = key(it.crew, it.id);
  const hov = el("div");
  if (it.kind === "later") hov.append(button("q", W.back, () => act(it, "back", "", W.broughtBack)));
  else hov.append(button("q", W.answer, () => toggleRow(k)));
  if (it.kind === "check") hov.append(button("q", W.done, () => act(it, "done", "", W.sentTo)));
  hov.append(button("q", W.park, () => act(it, "park", "", W.parkedTo)));
  if (it.kind !== "check") hov.append(button("q", W.no, () => act(it, "answer", W.noText, W.sentTo)));
  const words = itemWords(it, false);
  const row = rowShell(k, stl(it.crew), words, ageOf(it), chip(it), ownHov(it.crew, hov), itemPanel(it, false));
  row.title = ids(idOf(it.crew, it.id), it.task && idOf(it.crew, it.task));
  if (it.task) row.dataset.w = key(it.crew, it.task);
  return tint(row, it.project);
}

function renderAsks(asks) {
  heading("ask-h", W.ask, asks.length);
  const live = asks.filter((it) => !gone(key(it.crew, it.id)));
  const decisions = live.filter((it) => it.kind !== "check");
  const checks = live.filter((it) => it.kind === "check");
  const cap = 6;
  const shown = showAllAsks || decisions.length <= cap + 1 ? decisions : decisions.slice(0, cap);
  const rows = shown.map((it) => askRow(it));
  if (decisions.length > cap + 1) {
    rows.push(button("morerow", showAllAsks ? W.less : W.more(decisions.length - cap), () => { showAllAsks = !showAllAsks; render(); }));
  }
  $("ask").classList.toggle("empty", !live.length);
  if (!live.length) rows.push(el("p", "none", W.noAsk));
  patch($("ask-rows"), [...rows]);

  // The checks: one line; a tap on one opens its row under the line.
  const hands = $("hands");
  hands.hidden = !checks.length;
  if (!checks.some((it) => key(it.crew, it.id) === openHand) || openRow !== openHand) openHand = "";
  const kids = [el("span", "hl", W.hands)];
  checks.forEach((it, i) => {
    const k = key(it.crew, it.id);
    if (i) kids.push(" · ");
    const link = el("span", "hk" + (openHand === k ? " on" : "") + stl(it.crew), it.title || it.text);
    link.tabIndex = 0;
    link.setAttribute("role", "button");
    link.setAttribute("aria-expanded", openHand === k ? "true" : "false");
    const flip = () => { openHand = openHand === k ? "" : k; openRow = openHand; render(); };
    link.onclick = flip;
    link.onkeydown = (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); flip(); } };
    kids.push(tint(link, it.project));
  });
  const opened = checks.find((it) => key(it.crew, it.id) === openHand);
  if (opened) kids.push(askRow(opened));
  patch(hands, kids);
}

// ---- L3 · going: open work, and work waiting on a role of the crew

// nowLine is the latest plain note; the role's own status line only when no note came for half an hour, its ids
// and pids taken out.
function nowLine(w) {
  const note = w.milestone && w.milestone.text;
  const stale = !w.milestone || nowMs() - T(w.milestone.at) > 30 * 60000;
  if (stale && w.status === "busy" && w.now) return plain(w.now);
  return note || w.why || "";
}

// plain is a status line with its ids and pids taken out.
const plain = (s) => (s || "").replace(/\b(pid|PID)\s*\d+/g, "").replace(/\b[0-9a-f]{7,40}\b/g, "").replace(/\b[ATL]-\d+\b/g, "")
  .replace(/\(\s*[,;]?\s*\)/g, "").replace(/\s{2,}/g, " ").trim();

function renderGo(going) {
  const live = (w) => w.role_state === "up" && w.status === "busy" && w.state === "open";
  going.sort((a, b) => live(b) - live(a) || T(b.updated) - T(a.updated));
  heading("go-h", W.go, going.length, "b");
  const rows = going.map((w) => {
    const k = key(w.crew, w.id);
    const words = el("div", "words");
    const dot = el("span", "dot" + (live(w) ? " live" : ""));
    dot.title = live(w) ? W.busyLive : W.notLive;
    const wh = el("span", "who");
    wh.append(whoTag(w.crew, w.role));
    words.append(dot, wh);
    if (w.project) words.append(el("span", "pj", w.project));
    const now = nowLine(w);
    words.append(el("span", "tt", w.title), el("span", "gr", now === w.title ? "" : now));
    const meta = el("span", "age m");
    if (w.on) meta.append(el("span", "wait", W.waits(who(w.crew, w.on))), " ");
    meta.append(ago(w.created));
    const row = rowShell(k, "go" + stl(w.crew), words, meta, null, null, panel({
      body: taskBody(w.why, w.title, w.milestone && w.milestone.text),
      meta: [w.on ? W.waits(who(w.crew, w.on)) : ""], url: w.url,
    }));
    row.title = idOf(w.crew, w.id);
    row.dataset.w = k;
    return tint(row, w.project);
  });
  $("go").classList.toggle("empty", !rows.length);
  if (!rows.length) rows.push(el("p", "none", W.noGo));
  patch($("go-rows"), [...rows]);
}

// ---- L4 · parked (b4): rows carry nothing; open: «Bring back», «Drop» (muted, no confirm, 5 s undo)

function renderLater(parked) {
  const live = parked.filter((it) => !gone(key(it.crew, it.id)));
  $("later").hidden = !parked.length;
  // D-07 r4: one line, «Parked 53 · oldest 12 d · 4 come back ›», that opens the list (the whole screen on a phone).
  patch($("later-h"), []);
  const oldest = live.length ? dur(Math.max(...live.map((it) => nowMs() - T(it.created)))) : "";
  const back = live.filter((it) => (snap.alarms || []).some((a) => a.crew === it.crew && a.later === it.id)).length;
  const line = button("parked-line", W.parkedLine(live.length, oldest, back) + " ›", () => openList("parked"));
  line.setAttribute("aria-expanded", side && side.kind === "parked" ? "true" : "false");
  patch($("later-rows"), [line]);
}

// laterRow is a parked item on the lean page; opened, its whole text shows in place, with «Bring back» and a muted
// «Drop».
function laterRow(it) {
  const a = nowMs() - T(it.created);
  const old = a > 7 * 24 * HOUR;
  const words = el("div", "words");
  words.append(el("span", "tt", it.text));
  if (it.note) words.append(el("span", "gr", it.note));
  const hov = el("div");
  hov.append(button("q", W.back, () => act(it, "back", "", W.broughtBack)), button("q m", W.drop, () => act(it, "drop", "", W.dropped)));
  const row = rowShell(key(it.crew, it.id), "parked" + (old ? " old" : "") + stl(it.crew), words, el("span", "age" + (old ? "" : " m"), dur(a)), null, ownHov(it.crew, hov), laterPanel(it));
  row.title = idOf(it.crew, it.id);
  return row;
}

// laterPanel is a parked item opened: its note, «Bring back», and a muted «Drop» (b4 toast).
function laterPanel(it) {
  return panel({ body: [it.note], primary: { label: W.back, fn: () => act(it, "back", "", W.broughtBack), icon: "undo" }, crew: it.crew,
    muted: [[W.drop, () => act(it, "drop", "", W.dropped), "trash"]] });
}

// ---- alarms: reminders (for you, a role, another crew), roles asleep, parked items set to come back. A row opens to
// a field that moves it («tomorrow 09:00», «+2h») and a muted «Off»; «Set» on the heading opens a new reminder for
// you. Ringing is the ticker's: a reminder lands on your move, a role is started and told.

const alarmKey = (a) => "alarm:" + key(a.crew, a.id);
const NEW_ALARM = "alarm:new";

// alarmWhen is when it rings: the time today, else the day and the time.
function alarmWhen(a) {
  const t = new Date(a.at);
  const now = new Date(nowMs());
  const sameDay = t.toDateString() === now.toDateString();
  const day = sameDay ? "" : t.toLocaleDateString("en-GB", { weekday: "short", day: "numeric", month: "short" }) + " ";
  return day + hhmm(a.at);
}

function alarmText(a) {
  if (a.kind === "sleep") return W.alWakes(who(a.crew, a.role));
  if (a.kind === "later") return W.alBack + a.text;
  return a.text;
}

function alarmHead() {
  const live = (snap.alarms || []).filter((a) => shows(a.crew) && !gone(alarmKey(a)));
  const b = button("hl", W.alSet, () => toggleRow(NEW_ALARM));
  b.setAttribute("aria-expanded", openRow === NEW_ALARM ? "true" : "false");
  // A reminder is set on this node: no «Set» while a peer's crew or node is shown.
  return [el("span", "", W.alarms + (live.length ? " " + live.length : "")), ...(alarmCrew() ? [b] : [])];
}

// alarmCrew is the crew a new reminder goes to: the one shown, else the shown node's (or this node's) default, else its
// first. A peer's crew takes it on its own node (POST /api/alarm routes it there).
function alarmCrew() {
  const f = crewOf(filter);
  if (f) return f.key;
  const node = filter.startsWith("node:") ? filter.slice(5) : "";
  const own = snap.crews.filter((c) => (node ? c.node === node : !c.remote));
  const awake = own.filter((c) => !folded(c));
  const from = awake.length ? awake : own;
  return (from.find((c) => c.key === "default") || from[0] || {}).key || "";
}

function alarmRows(cls) {
  const live = (snap.alarms || []).filter((a) => shows(a.crew) && !gone(alarmKey(a)));
  const rows = live.map((a) => alarmRow(a, cls));
  if (openRow === NEW_ALARM && alarmCrew()) rows.unshift(newAlarmRow(cls));
  return rows;
}

function alarmRow(a, cls) {
  const k = alarmKey(a);
  const words = el("div", "words");
  words.append(el("span", "tt", alarmText(a)));
  if (a.kind !== "sleep") words.append(el("span", "gr", W.alFor(who(a.crew, a.for === "you" ? "" : a.for) || "you")));
  const left = T(a.at) - nowMs();
  const when = el("span", "age m al-t", alarmWhen(a));
  when.title = left > 0 ? W.alIn(dur(left)) : W.alDue;
  const row = rowShell(k, cls + stl(a.crew), words, when, null, null, alarmPanel(a, k));
  row.title = a.id + (a.later ? " · " + a.later : "");
  return row;
}

// alarmPanel: the note, when it was set as, a field for the new time with «Move», and «Off».
function alarmPanel(a, k) {
  const p = el("div");
  if (a.note) p.append(el("p", "pn", a.note));
  const left = T(a.at) - nowMs();
  p.append(el("p", "meta", [left > 0 ? W.alIn(dur(left)) : W.alDue, a.when ? W.alAsWords(a.when) : ""].filter(Boolean).join(" · ")));
  const line = el("div", "ctl");
  const f = whenField(k, (v) => { draft = { k, text: v }; });
  if (draft.k === k) f.value = draft.text;
  const prim = labelled(button("btn", "", () => f.value.trim() && alarmAct(a, "move", f.value.trim(), "", W.alMoved, k)), "clock", W.alMove);
  const sync = () => { prim.disabled = !f.value.trim(); };
  sync();
  f.addEventListener("input", sync);
  f.addEventListener("keydown", (e) => { if (e.key === "Enter" && f.value.trim()) { e.preventDefault(); prim.click(); } });
  const acts = el("div", "acts");
  const mute = el("div", "mute");
  const off = labelled(button("ib", "", () => alarmAct(a, "off", "", "", W.alOffDone, k), W.alOff), "x", W.alOff);
  off.setAttribute("aria-label", W.alOff);
  mute.append(off);
  acts.append(prim, mute);
  line.append(f, acts);
  p.append(line);
  return p;
}

function whenField(k, keep) {
  const f = el("textarea", "field when");
  f.rows = 1;
  f.placeholder = W.alWhen;
  f.setAttribute("aria-label", W.alWhenLabel);
  f.title = W.alWhenLabel;
  f.addEventListener("input", () => keep(f.value));
  f.addEventListener("keydown", (e) => { if (e.key === "Escape") { e.preventDefault(); draft = { k: "", text: "" }; toggleRow(k); } });
  return f;
}

// newAlarmRow is a reminder for you being set: what, when, «Set». It goes to the crew shown, else the first.
function newAlarmRow(cls) {
  const words = el("div", "words");
  words.append(el("span", "tt", W.alNew));
  const p = el("div");
  const line = el("div", "ctl");
  const what = el("textarea", "field");
  what.rows = 1;
  what.placeholder = W.alWhat;
  what.setAttribute("aria-label", W.alWhat);
  what.value = alDraft.text;
  const when = whenField(NEW_ALARM, (v) => { alDraft.when = v; sync(); });
  when.value = alDraft.when;
  const prim = labelled(button("btn", "", () => {
    const crew = alarmCrew();
    if (!crew) return;
    alarmAct({ crew, id: "" }, "set", alDraft.when.trim(), alDraft.text.trim(), W.alSetDone, NEW_ALARM);
  }), "clock", W.alSet);
  const sync = () => { prim.disabled = !(alDraft.text.trim() && alDraft.when.trim()); };
  what.addEventListener("input", () => { alDraft.text = what.value; sync(); });
  what.addEventListener("keydown", (e) => { if (e.key === "Escape") { e.preventDefault(); toggleRow(NEW_ALARM); } });
  for (const f of [what, when]) f.addEventListener("keydown", (e) => { if (e.key === "Enter" && !prim.disabled) { e.preventDefault(); prim.click(); } });
  sync();
  const acts = el("div", "acts");
  acts.append(prim);
  line.append(what, when, acts);
  p.append(line);
  return rowShell(NEW_ALARM, cls + " new", words, null, null, null, p);
}

// alarmAct posts set, move or off at once (a time typed is seen before it goes); off waits the 5 s undo.
function alarmAct(a, action, when, text, word, k) {
  openRow = "";
  draft = { k: "", text: "" };
  const go = async () => {
    try {
      await postTo("api/alarm", { crew: a.crew, id: a.id, action, when, text });
      if (action === "set") alDraft = { text: "", when: "" };
      if (action !== "off") toast(word);
    } catch (e) {
      sentKeys.delete(k);
      if (action === "set") openRow = NEW_ALARM;
      toast(W.notSent + e.message);
    }
    render();
  };
  if (action !== "off") { go(); render(); return; }
  const t = toast(word, () => { clearTimeout(timer); pending.delete(k); render(); });
  const timer = setTimeout(() => { pending.delete(k); sentKeys.add(k); t.remove(); go(); }, 5000);
  pending.set(k, timer);
  render();
}

function renderAlarms() {
  const head = alarmHead();
  patch($("alarms-h"), [...head]);
  const rows = alarmRows("");
  if (!rows.length) rows.push(el("p", "none", W.alNone));
  patch($("alarms-rows"), [...rows]);
}

// ---- L5 · waiting on others: one line per person, open on click; a row opens like a going one, with «Take it back»

function renderOthers(others) {
  $("others").hidden = !others.length;
  heading("others-h", W.others, others.length);
  const by = new Map();
  for (const w of others.filter((w) => !gone(otherKey(w)))) {
    const g = w.on || "";
    if (!by.has(g)) by.set(g, []);
    by.get(g).push(w);
  }
  const out = [];
  for (const [g, ws] of [...by].sort((a, b) => b[1].length - a[1].length)) {
    const isOpen = groups.has(g);
    const head = button("grp", "", () => { isOpen ? groups.delete(g) : groups.add(g); render(); });
    head.setAttribute("aria-expanded", isOpen ? "true" : "false");
    head.append(el("span", "chev", "›"));
    const words = el("span");
    const n = String(ws.length);
    const text = g ? W.waitOn(ws.length, g) : W.setAside(ws.length);
    words.append(el("b", "", n), text.slice(n.length));
    head.append(words);
    out.push(head);
    if (!isOpen) continue;
    const sub = el("div", "sub");
    sub.append(...ws.sort((a, b) => T(a.updated) - T(b.updated)).map(otherRow));
    out.push(sub);
  }
  patch($("others-rows"), [...out]);
}

// otherRow is a task waiting on someone else; it opens to its note, whom it waits on, and «Take it back».
function otherRow(w) {
  const k = otherKey(w);
  const words = el("div", "words");
  if (w.project) words.append(el("span", "pj", w.project));
  words.append(el("span", "tt", w.title), el("span", "gr", (w.milestone && w.milestone.text) || ""));
  const row = rowShell(k, stl(w.crew), words, el("span", "age m", ago(w.updated)), null, null, panel({
    body: taskBody(w.why, w.title, w.milestone && w.milestone.text), crew: w.crew,
    meta: [w.on ? W.waits(w.on) : ""], url: w.url,
    primary: { label: W.mine, fn: () => act(w, "mine", "", W.madeMine, k), icon: "mine" },
  }));
  row.title = idOf(w.crew, w.id);
  row.dataset.w = key(w.crew, w.id);
  return tint(row, w.project);
}

// quiet (w3): from 860 px the empty «you hold», «answer» and «going» are not drawn; one muted line names them, where
// the first of them stood. Below 860 the sections keep their own empty lines.
function renderQuiet() {
  const empty = [["hold", W.noHold], ["ask", W.noAsk], ["go", W.noGo]].filter(([id]) => $(id).classList.contains("empty"));
  const q = $("quiet");
  q.hidden = !empty.length;
  if (!empty.length) return;
  setText(q, empty.map(([, words]) => words).join(" · "));
  $(empty[0][0]).before(q);
}

// ---- the cockpit (D-03 c1–c6), from 1400 px: a header with the last event, six readouts, and three zones —
// your move (the only place with actions), the crews, and who waits on others with the parked list. Below 1400 the
// lean page above is the page. Motion is only a working role's lamp and the heartbeat.

function renderCockpit(hold, asks, others, parked) {
  setText($("brand-s"), W.shift);
  renderEv();
  const crews = snap.crews.filter((c) => shows(c.key) && !away(c.key));
  const screens = crews.flatMap((c) => (c.roles || []).filter((r) => r.state === "up" && r.prompt).map((r) => [c, r]));
  const roles = crews.flatMap((c) => c.roles);
  const up = roles.filter((r) => r.state === "up");
  const persons = new Set(others.map((w) => w.on || "")).size;
  const oldest = hold.length ? Math.max(...hold.map((it) => nowMs() - T(it.created))) : 0;
  const cells = [
    [W.hold, hold.length + screens.length, [hold.length ? W.oldest + dur(oldest) : "", screens.length ? W.onScreenN(screens.length) : ""].filter(Boolean).join(" · ") || W.nobody, "need"],
    [W.ask, asks.length, asks.length ? W.withRec(asks.filter((it) => it.rec).length) : W.noAsk, "ink"],
    [W.go, snap.work.filter((w) => w.lane === "go" && shows(w.crew)).length, W.rolesWorking(up.filter((r) => r.status === "busy").length), "busy"],
    [W.others, others.length, others.length ? W.people(persons) : W.nobody, "dim"],
    [W.later, parked.length, parked.length ? W.oldest.trim() + " " + dur(Math.max(...parked.map((it) => nowMs() - T(it.created)))) : W.noDue, "dim", "", "", "parked"],
    [W.roDone, crews.reduce((n, c) => n + (c.done_today || 0), 0), W.andHistory, "dim", "", "", "done"],
  ];
  // Spent today is a readout like the others: dark at nothing, its week and «API-equivalent» under it.
  const today = spent(crews, "today");
  const week = spent(crews, "week");
  cells.push([W.roSpent, today, week ? W.spentWeek(money(week)) : W.spentNone, "dim", money(today), W.spentTip]);
  patch($("strip"), cells.map(([label, n, sub, cls, shown, tip, opens]) => {
    const ro = el(opens ? "button" : "div", "ro " + cls + (n ? " lit" : " dark") + (opens ? " opens" : ""));
    if (opens) {
      ro.type = "button";
      ro.setAttribute("aria-expanded", side && side.kind === opens ? "true" : "false");
      ro.addEventListener("click", (e) => (side && side.kind === opens ? closeTalk() : openList(opens, e.currentTarget)));
    }
    ro.append(el("span", "ro-l", label), el("span", "ro-n", shown || String(n)), el("span", "ro-s", sub));
    if (tip) ro.title = tip;
    return ro;
  }));

  // A session that shows a question on its screen waits on the owner: first in his move.
  // Your move: holds first, then the questions and checks, one line each with the recommendation as the chip.
  setText($("move-h"), W.move);
  const live = (it) => !gone(key(it.crew, it.id));
  const holds = [...screens.map(([c, r]) => screenRow(c, r)), ...hold.filter(live).map((it) => moveRow(it, true))];
  if (!holds.length && !asks.filter(live).length && filter !== "all") {
    const where = filter.startsWith("node:") ? filter.slice(5) : crewLabel(filter) || filter;
    holds.push(el("p", "dnone", W.nothingOn(where)));
    const all = snap.todo.filter((it) => !away(it.crew));
    const h = all.filter((it) => it.holds).length, q = all.length - h;
    if (all.length) {
      const b = button("q elsewhere", W.elsewhere(h, q), () => { filter = "all"; localStorage.setItem("zmina.crew", "all"); render(); });
      holds.push(b);
    }
  } else if (!holds.length) holds.push(el("p", "dnone", W.noHold));
  patch($("move-rows"), [...holds]);
  const qs = [...asks.filter(live).filter((it) => it.kind !== "check"), ...asks.filter(live).filter((it) => it.kind === "check")];
  setText($("move-ask-h"), qs.length ? W.ask : "");
  patch($("move-asks"), [...qs.map((it) => moveRow(it, false))]);
  patch($("move-al-h"), [...alarmHead()]);
  patch($("move-alarms"), [...alarmRows("mv al")]);

  renderBand(crews);
  renderBoard(parked);
}

// money is API-equivalent dollars the way the page shows them: $4.20, $37, $1.6k.
function money(v) {
  if (v >= 1000) return "$" + (v / 1000).toFixed(1).replace(/\.0$/, "") + "k";
  if (v >= 10) return "$" + Math.round(v);
  return "$" + v.toFixed(2);
}

// spent is the shown crews' dollars over a window (today or week), 0 until the first read is in.
function spent(crews, window) {
  if (!cost || !cost.ready) return 0;
  return crews.reduce((n, c) => n + ((cost.crews[c.key] || {})[window] || 0), 0);
}

// loadCost reads the cost figures; the server reads the transcripts in the background, so the first answer may be
// empty and is asked again soon.
async function loadCost() {
  try {
    cost = await getJSON("api/cost");
  } catch (_) { /* the page works without the figures */ }
  if (snap) render();
  setTimeout(loadCost, cost && cost.ready ? 60000 : 5000);
}

// renderEv is the newest event in words, in the header.
function renderEv() {
  const evs = snap.log.filter((e) => shows(e.crew));
  const answered = new Set(evs.filter((e) => e.event === "answered").map((e) => e.crew + "/" + ((e.detail || "").match(/^(A-\d+)/) || [])[1]));
  const e = evs.map((x) => feedLine(x, answered)).find(Boolean);
  const whole = e ? [who(e.crew, e.role || e.by), e.text].filter(Boolean).join(" · ") : "";
  const red = redList();
  $("ev").classList.toggle("red", red.length > 0);
  if (red.length) {
    patch($("ev"), red.map((d) => {
      const b = button("redb", d.text, () => openTalk(d.crew, d.role), d.fix);
      return b;
    }));
    $("ev").title = whole;
    return;
  }
  patch($("ev"), e ? [el("i"), clause(whole, 60)] : []);
  $("ev").title = e ? hhmm(e.ts) + " · " + whole : "";
}

// clause is a text's first sentence, cut at a word to n characters with «…» (D-07 h1).
function clause(t, n) {
  const first = t.split(/(?<=[.!?;])\s/)[0];
  if (first.length <= n) return first;
  const cut = first.slice(0, n - 1);
  return (cut.lastIndexOf(" ") > n / 2 ? cut.slice(0, cut.lastIndexOf(" ")) : cut) + "…";
}

$("ev").addEventListener("click", (e) => { if (!e.target.closest("button")) openHistory(); });

// moveRow is a row of your move: a hold carries its note and «ops · waits on dispatcher» under the title, a question its
// reason beside it; both open like the lean page's rows (b1, p1–p5).
function moveRow(it, holds) {
  const words = el("div", "words");
  const head = el("span", "tt");
  if (it.project) head.append(pdot(it.project));
  head.append(it.title || it.text);
  words.append(head);
  if (holds) {
    const note = it.note || (it.text !== it.title ? it.text : "");
    if (note) words.append(el("span", "gr", note));
    const meta = el("span", "mvm");
    const cl = crewTag(it.crew, true);
    if (cl) meta.append(cl);
    meta.append(el("span", "", W.waits(it.blocks || it.role || "—")));
    if (it.project) meta.append(el("span", "", it.project));
    words.append(meta);
  } else {
    // A question carries its node/crew too: the same title from two nodes reads apart.
    const cl = crewTag(it.crew, true);
    if (cl) words.append(cl);
    const why = it.kind === "decision" ? it.reason : it.note;
    if (why) words.append(el("span", "gr", why));
  }
  const tag = chip(it) || (it.kind === "later" ? el("span", "chip ghost", W.backTag) : null);
  const row = rowShell(key(it.crew, it.id), "mv " + (holds ? "hold" : "ask") + stl(it.crew), words, holds ? ageOf(it) : null, tag, null, itemPanel(it, holds));
  row.title = ids(idOf(it.crew, it.id), it.task && idOf(it.crew, it.task));
  if (it.task) row.dataset.w = key(it.crew, it.task);
  if (it.project) row.dataset.p = it.project;
  return row;
}

// screenRow is a session with a question on its screen: amber like a hold. When the screen reads as a question with
// numbered choices, the row says the question and, opened, offers each choice as a button: zmina moves the dialog's
// cursor there and takes it (chat/action choose), on this node or a peer's, and refuses if the question changed.
function screenRow(c, r) {
  const d = r.dialog;
  const words = el("div", "words");
  words.append(el("span", "tt", r.role + " · " + (d && d.question ? d.question : W.onScreen)));
  const meta = el("span", "mvm");
  const cl = crewTag(c.key, true);
  if (cl) meta.append(cl);
  meta.append(el("span", "", d ? W.choicesN(d.options.length) : plain(r.now || "")));
  words.append(meta);
  const p = el("div");
  if (d) {
    if (d.question) p.append(el("p", "pn", d.question));
    const ch = el("div", "choices");
    for (const o of d.options) {
      const b = button("btn choice", o.n + ". " + o.label, () => chooseDialog(c, r, o));
      if (o.detail) b.append(el("span", "cd", o.detail));
      ch.append(b);
    }
    p.append(ch);
  } else if (r.now) p.append(el("p", "pn", r.now));
  p.append(button("q", W.talkTo(r.role), () => openTalk(c.key, r.role)));
  return rowShell("screen:" + c.key + "/" + r.role, "mv hold screen", words, el("span", "age", ago(r.since)), null, null, p);
}

// chooseDialog answers a session's question with one of its choices, on the node the session runs on.
async function chooseDialog(c, r, o) {
  const node = c.remote ? nodeOf(c.key) : "";
  const crew = node ? c.key.slice(node.length + 1) : c.key;
  openRow = "";
  try {
    await postTo("api/chat/action", { node, crew, role: r.role, action: "choose:" + o.n + ":" + r.dialog.id });
    toast(W.chose(r.role, o.label));
  } catch (e) {
    toast(W.notSent + e.message);
  }
  render();
}

// glyphs holds the role glyphs (assets/roles/*.svg, our own embedded files), read once at start.
const glyphs = {};

// glyph is a role's line glyph in currentColor; an unknown name takes the generic one.
function glyph(name) {
  const g = el("span", "glyph");
  g.innerHTML = glyphs[name] || glyphs.generic || "";
  return g;
}

// pdot is a project as the cockpit shows it: a 6 px dot in its tint, never a bar (k5).
function pdot(project) {
  const d = tint(el("span", "pd"), project);
  d.title = project;
  return d;
}

// renderBand (A-13) is every session that is up, across the crews shown, as one band of equal cards: a crew with
// one session takes one card, never a box of its own. Its line: how many are up and not started, each crew's tick.
function renderBand(crews) {
  const up = crews.flatMap((c) => c.roles.filter((r) => r.state === "up").map((r) => [c, r]));
  const down = crews.flatMap((c) => c.roles.filter((r) => r.state !== "up").map((r) => who(c.key, r.role)));
  const h = el("div", "dh");
  const counts = el("span", "dc", W.bandCounts(up.filter(([, r]) => r.status === "busy").length, up.length));
  if (down.length) counts.title = W.notUp + down.join(", ");
  h.append(el("span", "dl", W.sessions), counts);
  // Only the crews whose tick is not fresh, or that are spare, stand on the line; the fresh ones are in its tooltip
  // (D-07 x3).
  const fresh = crews.filter((c) => !c.spare && !c.stale && !tickerDead(c));
  if (crews.length > 1 && fresh.length) h.title = fresh.map((c) => crewLabel(c.key) + " " + hhmm(c.last_tick)).join(" · ");
  for (const c of crews.length > 1 ? crews.filter((c) => !fresh.includes(c)) : crews) {
    const tick = el("span", "tick" + (c.spare ? " spare" : c.stale ? " stale" : tickerDead(c) ? " dead" : ""));
    const said = c.spare ? W.spare : tickerDead(c) && !c.stale ? W.silent(zero(c.last_tick) ? "" : ago(c.last_tick)) : zero(c.last_tick) ? "—" : hhmm(c.last_tick);
    tick.append(el("i"), (crews.length > 1 ? crewLabel(c.key) + " · " : W.tick) + said + (c.stale ? " · " + W.stale : ""));
    tick.title = W.tick + crewLabel(c.key);
    h.append(tick);
  }
  // The working ones and those that need you stand in the band; the idle ones lie in one stack after them, the one
  // that went idle last on top, until the owner spreads them (kept in the browser).
  const awake = (r) => r.status === "busy" || r.prompt || r.stuck;
  const lit = up.filter(([, r]) => awake(r)).sort((a, b) => (b[1].prompt || b[1].stuck ? 1 : 0) - (a[1].prompt || a[1].stuck ? 1 : 0));
  const idle = up.filter(([, r]) => !awake(r)).sort((a, b) => T(b[1].since) - T(a[1].since));
  const spread = stacked.has("sessions") || idle.length < 2;
  if (idle.length > 1) {
    const t = labelled(button("kb-st", "", () => { spread ? stacked.delete("sessions") : stacked.add("sessions"); keepStacked(); render(); }),
      spread ? "stack" : "spread", spread ? W.stackIdle : W.spread);
    t.setAttribute("aria-pressed", spread ? "false" : "true");
    h.insertBefore(t, counts.nextSibling);
  }
  const band = el("div", "band");
  band.append(...lit.map(([c, r]) => sessionCard(c, r)));
  if (spread) band.append(...idle.map(([c, r]) => sessionCard(c, r)));
  else {
    const pile = el("div", "sstack");
    pile.append(...stackOf([sessionCard(...idle[0])], idle.length - 1, () => { stacked.add("sessions"); keepStacked(); render(); }, W.idleN));
    band.append(pile);
  }
  patch($("crew-col"), [h, up.length ? band : el("p", "dnone", W.nobodyUp)]);
}

// sessionCard is one session: the role's glyph as its lamp (D-04 i3), the role, its crew while every crew shows, the
// context ring once it nears a trim; the state word and since when; what it says it does. A working one is lit, an idle
// one dimmed unless it needs you.
function sessionCard(c, r) {
  const busy = r.status === "busy";
  const need = r.prompt || r.stuck;
  const card = el("button", "role card " + (busy ? "busy" : "idle") + (need ? " need" : "") + stl(c.key));
  card.type = "button";
  card.dataset.k = "card:" + c.key + "/" + r.role;
  card.addEventListener("click", (e) => openTalk(c.key, r.role, e.currentTarget));
  const tip = r.keep === "always" ? [W.always] : [];
  const h = el("div", "role-h");
  h.append(glyph(r.icon), el("span", "role-n", r.role));
  const where = multiNode() ? shownLabel(c.key, true) : filter === "all" && multi() ? c.name : "";
  if (where) h.append(el("span", "keep", where));
  h.append(el("span", "fill"));
  if (r.context && r.context_max) {
    // The ring is the share of context_max, where the role compacts — not the model's limit. It shows only once it
    // matters, from 75 % (D-04 i4); below that its numbers are in the card's tooltip.
    const share = r.context / r.context_max;
    const p = Math.min(1, share);
    const said = W.ctxOf(Math.round(r.context / 1000) + "k", Math.round(r.context_max / 1000) + "k") + (share > 1 ? W.ctxPast : "");
    if (p < 0.75) tip.push(said);
    else {
      const ring = el("span", "ring need" + (share > 1 ? " over" : ""));
      ring.style.setProperty("--deg", Math.round(p * 360) + "deg");
      ring.title = said;
      ring.append(el("span", "", share > 1 ? "100+" : String(Math.round(share * 100))));
      h.append(el("span", "ctx", "ctx"), ring);
    }
  }
  const word = r.prompt ? W.onScreen : r.stuck ? W.stuck : busy ? W.working : W.free;
  const line = el("div", "role-w" + (r.prompt ? " need" : r.stuck ? " bad" : ""), word + (zero(r.since) ? "" : " · " + ago(r.since)));
  // What the session spent today, API-equivalent, once it is a cent (copies count with their role).
  const usd = cost && cost.ready ? cost.roles[c.key + "/" + r.role.split("#")[0]] || 0 : 0;
  if (usd >= 0.01 && !r.role.includes("#")) {
    const m = el("span", "role-c", W.spentRole(money(usd)));
    m.title = W.spentTip;
    line.append(m);
  }
  card.setAttribute("aria-label", W.talkTo(r.role) + (where ? " · " + where : ""));
  card.append(h, line);
  const now = nowLine(r) || plain(r.now);
  card.append(el("div", "role-now", now || ""));
  if (tip.length) card.title = tip.join(" · ");
  return card;
}

// ---- the board (D-03 k1–k5): every active task in four columns — in progress, waiting on you, waiting on
// others, parked — as cards that open in place. Waiting on others is grouped by person, one group open at a time.
// An empty column folds to a strip with its label and 0; each column scrolls on its own.

function renderBoard(parked) {
  setText($("kb-h"), W.kb);
  const work = snap.work.filter((w) => shows(w.crew) && !gone(key(w.crew, w.id)) && !gone(otherKey(w)));
  const busy = (w) => w.role_state === "up" && w.status === "busy";
  const cols = [
    ["go", W.kbGo, work.filter((w) => w.lane === "go").sort((a, b) => busy(b) - busy(a) || T(b.updated) - T(a.updated))],
    ["others", W.kbOthers, work.filter((w) => w.lane === "others")],
  ];
  // Two columns (D-07 r2, r3): what waits on you is first in your move, parked is a readout that opens its list.
  $("kb-cols").style.gridTemplateColumns = cols.map(([lane, , ws]) => (!ws.length ? "3rem" : lane === "go" ? "minmax(0, 1.4fr)" : "minmax(0, 1fr)")).join(" ");
  patch($("kb-cols"), cols.map(([lane, label, ws]) => {
    const col = el("div", "kb-col" + (ws.length ? "" : " empty"));
    const h = el("div", "kb-ch");
    h.append(el("span", "", label), el("b", lane === "you" && ws.length ? "n" : "", String(ws.length)));
    const units = !ws.length ? [] : lane === "others" ? personGroups(ws) : lane === "later" ? ws.map((it) => [laterBoardRow(it)]) : ws.map((w) => [boardRow(w, lane, busy(w))]);
    // waiting on others stacks per person instead (personGroups)
    const isStack = lane !== "others" && stacked.has(lane) && units.length > 1;
    if (lane !== "others" && units.length > 1) {
      const t = labelled(button("kb-st", "", () => { stacked.has(lane) ? stacked.delete(lane) : stacked.add(lane); keepStacked(); render(); }),
        isStack ? "spread" : "stack", isStack ? W.spread : W.stack);
      t.setAttribute("aria-pressed", isStack ? "true" : "false");
      h.append(t);
    }
    if (lane === "later" && ws.length) h.append(button("hl", W.history, openHistory));
    col.append(h);
    if (units.length) {
      const rows = el("div", "rows kb-rows");
      rows.append(...(isStack ? stackOf(units[0], units.length - 1, () => { stacked.delete(lane); keepStacked(); render(); }) : units.flat()));
      col.append(rows);
    }
    return col;
  }));
}

// stackOf is cards folded into a stack: the top one, the edges of the rest under it, and «+N more», which spreads
// them (spread).
function stackOf(top, n, spread, word = W.moreN) {
  const pile = el("div", "kb-stack");
  pile.append(...top);
  const more = button("kb-more", word(n), spread);
  more.title = W.spread;
  return [pile, more];
}

const keepStacked = () => localStorage.setItem("zmina.stacked", JSON.stringify([...stacked]));

// personGroups: one stack per person who holds work up, the most first, under a line with their name, how many and
// the oldest; the oldest card on top. Opening one (its line or «+N more») spreads its cards and folds the others, so
// a long group never buries the rest.
function personGroups(ws) {
  const by = new Map();
  for (const w of ws) {
    const g = w.on || "";
    if (!by.has(g)) by.set(g, []);
    by.get(g).push(w);
  }
  return [...by].sort((a, b) => b[1].length - a[1].length).map(([g, items]) => {
    const gk = "kb:" + g;
    const isOpen = groups.has(gk);
    const toggle = () => {
      for (const o of [...groups]) if (o.startsWith("kb:")) groups.delete(o);
      if (!isOpen) groups.add(gk);
      render();
    };
    items.sort((a, b) => T(a.created) - T(b.created));
    const head = button("kb-grp", "", toggle);
    head.setAttribute("aria-expanded", isOpen ? "true" : "false");
    head.append(el("span", "chev", "›"), el("span", "kb-gn", g || W.setAside(items.length).replace(/^\d+\s*/, "")),
      el("span", "kb-gc", String(items.length)), el("span", "kb-ga", W.oldest + ago(items[0].created)));
    const cards = items.map((w) => boardRow(w, "others", false));
    // Every item shows, a row each (Fable 9): a stack hid the second item of every person behind a click.
    return [head, ...cards];
  });
}

// boardCard is a task on the board: the project's dot, the id and the age on top; the title, two lines; the latest
// note, one line; who has it. In progress it carries the holder's lamp and «working» while the holder works;
// waiting on you, the amber bar of your move and what it needs from you.
function boardRow(w, lane, isBusy) {
  const top = el("div", "kc-h");
  if (w.project) top.append(pdot(w.project));
  top.append(el("span", "kb-id", w.id), el("span", "kc-age", ago(w.updated)));
  const big = button("kc-x", "", () => openFull(w), W.fullView);
  big.setAttribute("aria-label", W.fullView);
  big.append(icon("full"));
  top.append(big);
  const words = el("div", "words kc");
  words.append(top, el("span", "tt", w.title));
  const note = lane === "go" ? nowLine(w) : lane === "you" ? (w.needs && w.needs.note) || (w.milestone && w.milestone.text) : w.milestone && w.milestone.text;
  if (note && note !== w.title) words.append(el("span", "gr kc-n", note));
  const holder = el("div", "kc-f" + (isBusy ? " busy" : ""));
  holder.append(whoTag(w.crew, w.role), lane === "go" && isBusy ? " · " + W.working : lane === "go" && w.on ? " · " + W.waits(who(w.crew, w.on)) : "");
  words.append(holder);
  const row = rowShell("kb:" + key(w.crew, w.id), "kbr " + lane + stl(w.crew), words, null, null, null, boardPane(w, lane));
  row.title = idOf(w.crew, w.id);
  row.dataset.w = key(w.crew, w.id);
  if (w.project) row.dataset.p = w.project;
  return row;
}

// boardPane is what a task card opens to: its latest note, who it waits on, the link, and what the owner can do
// from that column.
function boardPane(w, lane) {
  const body = taskBody(w.why, w.title, w.milestone && w.milestone.text);
  return lane === "you" && w.needs ? itemPanel(w.needs, true)
    : lane === "others" ? panel({ body, meta: [W.waits(w.on || "—")], url: w.url, crew: w.crew,
      primary: { label: W.mine, fn: () => act(w, "mine", "", W.madeMine, otherKey(w)), icon: "mine" } })
    : panel({ body, meta: [w.on ? W.waits(who(w.crew, w.on)) : who(w.crew, w.role)], url: w.url });
}

// laterBoardRow is a parked item on the board as a card: its L-id and how long since it changed; its text, three
// lines; its crew while every crew shows.
function laterBoardRow(it) {
  const top = el("div", "kc-h");
  top.append(el("span", "kb-id", it.id), el("span", "kc-age", ago(it.updated)));
  const words = el("div", "words kc");
  words.append(top, el("span", "tt", it.text));
  const cl = shownLabel(it.crew, true);
  if (cl) words.append(el("div", "kc-f cl", cl));
  const row = rowShell(key(it.crew, it.id), "kbr later parked" + stl(it.crew), words, null, null, null, laterPanel(it));
  row.title = idOf(it.crew, it.id);
  return row;
}

// ---- a task on the whole screen: what was asked, where it stands, its timeline from the event log, its details,
// its holder's screen, and the same actions as its card. Esc or ✕ closes it.

async function getJSON(path) {
  const r = await fetch(path);
  if (!r.ok) throw new Error((await r.text()).trim());
  return r.json();
}

function openFull(w) {
  full = { crew: w.crew, id: w.id, data: null, peek: null, from: document.activeElement };
  if (!remote(w.crew)) loadFull(); // the server knows only this node's crews: a peer's sheet is the snapshot's
  render();
  $("full").querySelector(".fv-x")?.focus();
}

function closeFull() {
  const back = full && full.from;
  full = null;
  render();
  if (back && back.isConnected) back.focus();
}

// loadFull reads the task and its log, then the holder's screen while it is up.
async function loadFull() {
  const f = full;
  if (!f || f.loading || remote(f.crew)) return;
  f.loading = true;
  try {
    f.data = await getJSON(`api/task?crew=${encodeURIComponent(f.crew)}&id=${encodeURIComponent(f.id)}`);
  } catch (e) {
    f.data = { error: e.message };
  }
  f.loading = false;
  if (full === f) render();
  loadPeek();
}

async function loadPeek() {
  const f = full;
  const t = f && f.data && f.data.task;
  if (!t) return;
  const r = roleOf(f.crew, t.role);
  if (!r || r.state !== "up") { f.peek = ""; if (full === f) render(); return; }
  try {
    const res = await fetch(`api/peek?crew=${encodeURIComponent(f.crew)}&role=${encodeURIComponent(t.role)}`);
    f.peek = res.ok ? (await res.text()).replace(/\s+$/, "") : "";
  } catch {
    f.peek = "";
  }
  if (full === f) render();
}

const roleOf = (crew, role) => snap.crews.find((c) => c.key === crew)?.roles.find((r) => r.role === role);

// when is a past moment as the timeline shows it: the time today, the day and the time before.
function when(t) {
  const d = new Date(t);
  return d.toDateString() === new Date(nowMs()).toDateString() ? hhmm(t) : d.toLocaleDateString("en-GB", { day: "numeric", month: "short" }) + " " + hhmm(t);
}

function fvSection(label, ...kids) {
  const s = el("section", "fv-s");
  s.append(el("h3", "fv-l", label), ...kids);
  return s;
}

function renderFull() {
  const box = $("full");
  if (!full) { box.hidden = true; box.replaceChildren(); document.body.classList.remove("fv-on"); return; }
  box.hidden = false;
  document.body.classList.add("fv-on");
  const rem = remote(full.crew);
  const w = snap.work.find((x) => x.crew === full.crew && x.id === full.id);
  const own = (snap.tasks || []).find((x) => x.crew === full.crew && x.id === full.id);
  const t = (full.data && full.data.task) || (rem && own) || null;
  // A task that changed since it was read is read again.
  const latest = w || own;
  if (!rem && latest && t && T(latest.updated) > T(t.updated) && !full.loading) loadFull();
  const title = (w && w.title) || (t && (t.title || t.asked)) || full.id;
  const project = (w && w.project) || (t && t.project);

  const head = el("div", "fv-h");
  if (project) head.append(pdot(project));
  head.append(el("span", "kb-id", full.id));
  const laneWord = !w ? (t ? t.state : "") : w.lane === "go" ? W.kbGo : w.lane === "you" ? W.kbYou : W.waits(w.on || "—");
  if (laneWord) head.append(el("span", "fv-st" + (w && w.lane === "you" ? " n" : ""), laneWord));
  if (multi()) head.append(el("span", "fv-crew", crewLabel(full.crew)));
  head.append(el("span", "fill"));
  const x = button("fv-x ib", "", closeFull, W.close);
  x.setAttribute("aria-label", W.close);
  x.append(icon("x"), el("span", "", "esc"));
  head.append(x);

  const main = el("div", "fv-main");
  const now = w ? nowLine(w) : t && t.note;
  if (now && now !== title) main.append(fvSection(W.fvNow, el("p", "fv-now", now)));
  const asked = (t && t.asked) || (w && w.why);
  if (asked) main.append(fvSection(W.fvAsked, el("p", "fv-asked", asked)));
  if (full.data && !full.data.error) {
    const result = full.data.result;
    const contents = [];
    if (full.data.result_error) contents.push(el("p", "fv-asked", full.data.result_error));
    else if (!result) contents.push(el("p", "fv-asked", W.fvNoResult));
    else {
      contents.push(el("p", "fv-asked", W.fvReported + " · " + result.role + " · " + when(result.at)));
      for (const section of result.shape.sections) {
        const label = section.name.replaceAll("_", " ").replaceAll("-", " ");
        contents.push(el("h4", "fv-l", label.charAt(0).toUpperCase() + label.slice(1)),
          el("p", "fv-asked", result.values[section.name] || "—"));
      }
    }
    main.append(fvSection(W.fvResult, ...contents));
  }
  const log = el("ol", "fv-log");
  if (!full.data) log.append(el("li", "fv-none", W.fvLoading));
  else if (full.data.error) log.append(el("li", "fv-none", full.data.error));
  else {
    const evs = [...(full.data.log || [])].sort((a, b) => T(b.ts) - T(a.ts));
    if (!evs.length) log.append(el("li", "fv-none", W.fvNoLog));
    for (const e of evs) {
      const line = feedLine({ ...e, crew: full.crew, title }, new Set());
      const li = el("li");
      li.append(el("span", "fv-when", when(e.ts)), el("span", "fv-who", e.by || e.role || ""),
        el("span", "fv-what", line ? line.text : [e.event, e.detail].filter(Boolean).join(": ")));
      li.title = e.event;
      log.append(li);
    }
  }
  main.append(rem ? el("p", "fv-none", W.detailsOn(nodeOf(full.crew))) : fvSection(W.fvLog, log));

  const side = el("div", "fv-side");
  // The card's actions, without its note and link: those stand on the sheet already.
  if (w && w.lane !== "go") {
    const pane = boardPane(w, w.lane);
    pane.classList.add("panel", "fv-pane");
    side.append(fvSection(W.fvMove, pane));
  }
  const dl = el("dl", "fv-dl");
  const fact = (k, v) => { if (v) dl.append(el("dt", "", k), typeof v === "string" ? el("dd", "", v) : (() => { const d = el("dd"); d.append(v); return d; })()); };
  const role = (w && w.role) || (t && t.role);
  const r = roleOf(full.crew, role);
  const rs = !r ? "" : r.state !== "up" ? W.fvDown : (r.status === "busy" ? W.working : W.free) + (zero(r.since) ? "" : " · " + ago(r.since));
  if (role) {
    const holder = el("span", "", [who(full.crew, role), rs].filter(Boolean).join(" · ") + " ");
    const crewKey = full.crew;
    holder.append(button("q", W.talkTo(role), () => openTalk(crewKey, role)));
    fact(W.fvHolder, holder);
  }
  if (t && t.delivery) fact("Handoff", t.delivery.accepted_at ? "Accepted" : t.delivery.sent_at ? "Sent, awaiting acceptance" : "Queued, awaiting acceptance");
  fact(W.fvWaits, (w && w.on) || (t && t.on));
  fact(W.fvProject, project);
  fact(W.fvCost, full.data && full.data.cost);
  if (multi()) fact(W.fvCrew, crewLabel(full.crew));
  if (t) {
    fact(W.fvCreated, when(t.created) + " · " + ago(t.created));
    fact(W.fvUpdated, when(t.updated) + " · " + ago(t.updated));
    fact(W.fvRepo, t.repo);
    fact(W.fvBranch, t.branch && t.branch + (t.base ? " ← " + t.base : ""));
    fact(W.fvWorktree, t.worktree);
  }
  const url = (w && w.url) || (t && t.url);
  if (url) {
    const a = el("a", "", source(url));
    a.append(icon("link"));
    a.href = url;
    a.title = url;
    a.target = "_blank";
    a.rel = "noreferrer";
    fact(W.fvLink, a);
  }
  side.append(fvSection(W.fvDetails, dl));
  const scr = fvSection(W.fvScreen + (role ? " · " + who(full.crew, role) : ""));
  const again = button("fv-re ib", "", () => { full.peek = null; render(); loadPeek(); }, W.fvScreen);
  again.setAttribute("aria-label", "refresh");
  again.append(icon("refresh"));
  scr.querySelector(".fv-l").append(again);
  scr.append(full.peek === null ? el("p", "fv-none", W.fvLoading) : full.peek ? el("pre", "fv-pre", full.peek) : el("p", "fv-none", W.fvNoScreen));
  if (!rem) side.append(scr);

  const grid = el("div", "fv-g");
  grid.append(main, side);
  const sheet = el("div", "fv");
  sheet.append(head, el("h2", "fv-t", title), grid);
  sheet.setAttribute("role", "dialog");
  sheet.setAttribute("aria-modal", "true");
  sheet.setAttribute("aria-label", title);
  box.replaceChildren(sheet);
}

function openHistory() { openList("done", null, "history"); }

// ---- L6 · the bottom line; the history drawer

function renderFoot() {
  const done = snap.crews.filter((c) => shows(c.key)).reduce((n, c) => n + (c.done_today || 0), 0);
  const line = el("span", "", W.doneToday);
  line.append(el("b", "", String(done)));
  const h = button("r", W.history, () => (side && side.kind === "done" ? closeTalk() : openHistory()));
  h.setAttribute("aria-expanded", side && side.kind === "done" ? "true" : "false");
  patch($("foot"), [line, h]);
}

function taskChange(detail) {
  let state = "", note = "", on = "";
  let rest = detail;
  for (;;) {
    const i = rest.indexOf(", ");
    const head = i < 0 ? rest : rest.slice(0, i);
    // waiting-<owner> is the first name of waiting, still in old logs.
    if (["open", "waiting", "done", "dropped"].includes(head)) state = head;
    else if (head.startsWith("waiting-")) state = "waiting";
    else if (head.startsWith("parked ")) state = "parked";
    else if (head.startsWith("on ")) on = head.slice(3);
    else if (!head.startsWith("role ")) {
      if (rest.startsWith("note: ")) {
        note = rest.slice(6);
        for (const f of [", title: ", ", title cleared", ", project: ", ", project cleared", ", url: ", ", url cleared"]) {
          const j = note.indexOf(f);
          if (j >= 0) note = note.slice(0, j);
        }
      }
      return { state, note, on };
    }
    if (i < 0) return { state, note, on };
    rest = rest.slice(i + 2);
  }
}

// feedLine is an event in words, or null when it is not news (an ask's edit, a title set, an ask already answered).
function feedLine(e, answered) {
  const d = e.detail || "";
  const title = e.title || "";
  const askID = (d.match(/^(A-\d+)/) || [])[1];
  const afterId = (s) => s.replace(/^[AT]-\d+\s*(·\s*)?/, "");
  let text = "";
  switch (e.event) {
    case "task": {
      if (d.startsWith("new: ")) { text = W.ev.new + (title || d.slice(5)); break; }
      const { state, note, on } = taskChange(d);
      if (state === "waiting") text = (on ? W.ev.waitingWho(on) : W.ev.waitingOn) + (note || title);
      else if (state === "done") text = W.ev.done + ": " + (note || title);
      else if (state === "dropped") text = W.ev.dropped + ": " + (note || title);
      else if (state === "parked") text = W.ev.parked + (note || title);
      else if (note) text = note;
      else if (state === "open") text = W.ev.reopened;
      break;
    }
    case "asked":
      if (/ edited: /.test(d) || answered.has(e.crew + "/" + askID)) return null;
      text = W.ev.asked + afterId(d).replace(/^(decision|check):\s*/, "");
      break;
    case "answered": text = W.ev.answered + afterId(d); break;
    case "later":
      if (!d.startsWith("parked: ")) return null;
      text = W.ev.parked + d.slice(8);
      break;
    case "blocked": text = W.ev.blocked + d.replace(/^[^:]+:\s*/, ""); break;
    case "watch-failed": text = W.ev.watchFailed + d; break;
    case "alarm": text = W.ev.alarm + d.replace(/^R-\d+:?\s*/, ""); break;
    case "sleep": text = W.ev.sleep + d.replace(/^R-\d+\s*/, ""); break;
    case "start": text = W.ev.start; break;
    case "stop": text = W.ev.stop; break;
  }
  return text ? { ...e, text } : null;
}

function renderFeed() {
  setText($("history-h"), W.historyH);
  patch($("feed"), feedRows());
}

// feedRows is the history, a line per event, newest first; only shows those today's Done list keeps.
function feedRows(only) {
  const evs = snap.log.filter((e) => shows(e.crew) && (!only || only(e)));
  // An ask and its answer are one line: the answer.
  const answered = new Set(evs.filter((e) => e.event === "answered").map((e) => e.crew + "/" + ((e.detail || "").match(/^(A-\d+)/) || [])[1]));
  return evs.map((e) => feedLine(e, answered)).filter(Boolean).map((e) => {
    const row = el("div", "e");
    const t = el("time", "", hhmm(e.ts));
    t.title = new Date(e.ts).toLocaleString();
    const s = el("span");
    s.append(tint(el("span", "d"), e.project));
    const cl = e.project ? null : crewTag(e.crew, true);
    if (e.project) s.append(el("b", "", e.project), " · ");
    else if (cl) s.append(cl, " · ");
    s.append(e.text);
    const by = e.role || e.by;
    if (by) s.append(el("i", "", " · " + who(e.crew, by)));
    row.append(t, s);
    return row;
  });
}

$("say").addEventListener("click", (e) => {
  const to = e.target.closest?.("[data-to]")?.dataset.to;
  if (to) $(to).scrollIntoView({ behavior: "smooth", block: "start" });
});
$("history-close").addEventListener("click", () => { $("history").hidden = true; render(); });

// ---- the side panel (D-07 p1–p7): a role's conversation (Chat) as a layer over the right zone; your move, the readouts
// and the header stay in view and keep updating. Any role of any node: a peer's over the mesh, signed with the owner's
// key, as Chat does. One panel at a time; Esc and ✕ close it and the focus goes back to what opened it.

let side = null; // {crew, role}: what the panel shows
let sideFrom = null; // the element that opened it
let sideMark = null; // the same, as something a redrawn page finds again (patch.js focusMark)
let lastTalk = null; // the last conversation, for the header's Talk

function placeSide() {
  const mid = document.querySelector(".dcol.mid");
  const r = mid && mid.offsetParent ? mid.getBoundingClientRect() : null;
  const pane = $("side");
  pane.style.setProperty("--side-top", r ? Math.max(0, r.top) + "px" : "0px");
  pane.style.setProperty("--side-left", r ? r.left + "px" : "0px");
}

// talkTarget is where the header's Talk goes: the last conversation, else the dispatcher of the crew the switch shows
// (All: this node's default crew).
function talkTarget() {
  return lastTalk || scopeDispatcher();
}

// scopeDispatcher is the dispatcher of the crew the switch shows (All: this node's default crew).
function scopeDispatcher() {
  const f = crewOf(filter);
  const node = filter.startsWith("node:") ? filter.slice(5) : "";
  const c = f || snap.crews.find((x) => x.key === "default" && !x.remote && !node)
    || snap.crews.find((x) => (node ? x.node === node : !x.remote)) || snap.crews[0];
  if (!c) return null;
  const d = c.roles.find((r) => r.role === c.dispatcher) || c.roles.find((r) => /dispatcher/.test(r.role)) || c.roles[0];
  return d ? { crew: c.key, role: d.role } : null;
}

function openTalk(crewKey, role, from) {
  side = { kind: "talk", crew: crewKey, role, scope: filter };
  lastTalk = side;
  sideFrom = from || document.activeElement;
  sideMark = markOf(sideFrom);
  const c = crewOf(crewKey);
  const node = c && c.remote ? nodeOf(crewKey) : "";
  const crew = node ? crewKey.slice(node.length + 1) : crewKey;
  const q = new URLSearchParams({ node, crew, role });
  const frame = $("side-frame");
  const src = "chat.html?" + q + "&embed=1";
  if (frame.getAttribute("src") !== src) frame.setAttribute("src", src);
  $("side-full").href = "chat.html?" + q;
  $("side-term").href = "terminals.html?" + new URLSearchParams({ node, crew, role, view: "terminal" });
  $("history").hidden = true;
  // D-09 k1: on the phone a conversation is the Talk tab.
  if (phone.matches && tab !== "talk") goTab("talk");
  showSide(true);
  $("talk-open").setAttribute("aria-expanded", "true");
}

// openList opens Parked or Done today in the panel; tab picks Done's History.
function openList(kind, from, tab) {
  side = { kind, f: "all", tab: tab || "done" };
  sideFrom = from || document.activeElement;
  sideMark = markOf(sideFrom);
  $("talk-open").setAttribute("aria-expanded", "false");
  showSide(false);
}

function showSide(talking) {
  $("side-frame").hidden = !talking;
  $("side-list").hidden = talking;
  $("side-pick").parentElement.hidden = !talking;
  $("side-term").hidden = $("side-full").hidden = !talking;
  placeSide();
  $("side").hidden = false;
  render();
  // D-08 amendment c: the panel takes the focus, not its Close (a ring on ✕ at every open reads as a fault).
  $("side").focus();
}

function closeTalk() {
  const talking = side && side.kind === "talk";
  side = null;
  $("side").hidden = true;
  $("talk-open").setAttribute("aria-expanded", "false");
  // On the phone the Talk tab is the conversation: closing it goes back to the tab the owner came from.
  if (talking && phone.matches && tab === "talk") goTab(tabFrom);
  render();
  if (sideFrom && sideFrom.isConnected) sideFrom.focus();
  else if (sideMark) refocus(sideMark);
}

// markOf is focusMark for a given element: its row or section, its kind and its name.
function markOf(e) {
  if (!e || !e.closest) return null;
  const host = e.closest("[data-k]") || e.closest("[id]");
  if (!host) return null;
  return { el: e, host: host.dataset.k ? `[data-k="${CSS.escape(host.dataset.k)}"]` : "#" + CSS.escape(host.id), sig: focusSig(e) };
}

// renderSide is the panel's head: the role's glyph as its lamp, its name, node/crew, its state and since when, and the
// picker of every role of every node, grouped by node/crew, up ones first.
function renderSide() {
  if (!snap) return;
  if (!side || side.kind !== "talk") {
    // The header's Talk says whom it opens.
    const t = talkTarget();
    setText($("talk-open").lastElementChild, t ? W.talk + " · " + t.role + " " + (shownLabel(t.crew, true) || crewLabel(t.crew)) : W.talk);
  }
  if (!side) return;
  placeSide();
  if (side.kind !== "talk") { renderList(); return; }
  const r = roleOf(side.crew, side.role);
  const busy = r && r.status === "busy";
  const need = r && (r.prompt || r.stuck);
  const lamp = $("side-lamp");
  lamp.hidden = false;
  lamp.className = "lamp" + (busy ? " busy" : "");
  const g = glyph(r ? r.icon : "generic");
  if (g) patch(lamp, [g]);
  const st = !r || r.state !== "up" ? W.fvDown : (r.prompt ? W.onScreen : busy ? W.working : W.free) + (zero(r.since) ? "" : " · " + ago(r.since));
  // D-08 h1–h2: the role name is the picker; node/crew is the whole destination, its tooltip says how it is reached.
  setText($("side-role"), side.role);
  const c = crewOf(side.crew), where = remote(side.crew) || !snap.self ? side.crew : snap.self + "/" + side.crew;
  const k = el("span", "k", where);
  k.title = c && c.remote ? where + " · over the mesh, signed" : where + " · local";
  patch($("side-who"), [k, el("span", "st" + (need ? " need" : busy ? " busy" : ""), st)]);
  if (!$("side-menu").hidden) renderPick();
  setText($("talk-open").lastElementChild, W.talk + " · " + side.role + " " + (shownLabel(side.crew, true) || crewLabel(side.crew)));
}

// renderPick is the panel's role list (Fable 3): every crew of every node, up roles first with their state and since
// when, the ones not up folded into «+N not up»; typing filters it.
let pickAll = "";
function renderPick() {
  const q = $("side-filter").value.trim().toLowerCase();
  const out = [];
  for (const c of snap.crews.filter((c) => (c.roles || []).length && !away(c.key))) {
    const label = shownLabel(c.key, true) || crewLabel(c.key) || c.name;
    const match = (r) => !q || (label + " " + r.role).toLowerCase().includes(q);
    const up = c.roles.filter((r) => r.state === "up" && match(r));
    const down = c.roles.filter((r) => r.state !== "up" && match(r));
    if (!up.length && !down.length) continue;
    out.push(el("span", "pg", label));
    const item = (r) => {
      const st = r.state !== "up" ? W.fvDown : (r.prompt ? W.onScreen : r.status === "busy" ? W.working : W.free) + (zero(r.since) ? "" : " · " + ago(r.since));
      const b = button("pi" + (side && side.crew === c.key && side.role === r.role ? " on" : ""), "", () => { $("side-menu").hidden = true; openTalk(c.key, r.role, sideFrom); });
      b.setAttribute("role", "option");
      b.append(el("span", "pr", r.role), el("span", "ps" + (r.prompt ? " need" : r.status === "busy" ? " busy" : ""), st));
      return b;
    };
    out.push(...up.map(item));
    if (down.length && (q || pickAll === c.key)) out.push(...down.map(item));
    else if (down.length) out.push(button("pmore", W.notUpN(down.length), () => { pickAll = c.key; renderPick(); }));
  }
  patch($("side-roles"), out);
}

// renderList is the panel's Parked (filter chips, older than a week first) or Done today (by crew) with its History tab.
function renderList() {
  const head = side.kind === "parked" ? W.later : W.doneTab;
  $("side-lamp").hidden = true;
  setText($("side-role"), "");
  patch($("side-who"), [head]);
  const out = [];
  if (side.kind === "parked") {
    const all = snap.later.filter((it) => shows(it.crew) && !gone(key(it.crew, it.id)));
    const old = (it) => nowMs() - T(it.created) > 7 * 24 * HOUR;
    // Comes back: an alarm (later add --until) will bring it back to the owner's todo.
    const back = (it) => (snap.alarms || []).some((a) => a.crew === it.crew && a.later === it.id);
    const fl = el("div", "fl");
    for (const [f, label, n] of [["all", W.allN(all.length)], ["old", W.olderWeek + " " + all.filter(old).length], ["back", W.comesBack + " " + all.filter(back).length]]) {
      const b = button("", label + (n || ""), () => { side.f = f; render(); });
      b.setAttribute("aria-pressed", side.f === f ? "true" : "false");
      fl.append(b);
    }
    out.push(fl);
    const shown = all.filter((it) => (side.f === "old" ? old(it) : side.f === "back" ? back(it) : true)).sort((a, b) => T(a.created) - T(b.created));
    for (const [label, items] of [[W.olderWeek, shown.filter(old)], [W.thisWeek, shown.filter((it) => !old(it))]]) {
      if (!items.length) continue;
      out.push(el("div", "gh", label));
      const rows = el("div", "rows");
      rows.append(...items.map(laterRow));
      out.push(rows);
    }
  } else {
    const fl = el("div", "fl");
    for (const [t, label] of [["done", W.doneTab], ["history", W.historyTab]]) {
      const b = button("", label, () => { side.tab = t; render(); });
      b.setAttribute("aria-pressed", side.tab === t ? "true" : "false");
      fl.append(b);
    }
    out.push(fl);
    if (side.tab === "history") out.push(...feedRows());
    else {
      for (const c of snap.crews.filter((c) => shows(c.key) && (c.done || []).length)) {
        out.push(el("div", "gh", (crewLabel(c.key) || c.name) + " " + c.done.length));
        for (const d of [...c.done].sort((a, b) => T(b.at) - T(a.at))) {
          const row = el("div", "e");
          row.append(el("time", "", hhmm(d.at)));
          const t = el("span");
          t.append(el("b", "", d.id), " · " + d.title, el("i", "", d.role ? " · " + d.role : ""));
          row.append(t);
          out.push(row);
        }
      }
    }
  }
  patch($("side-list"), out);
}

$("side-pick").addEventListener("click", () => {
  const m = $("side-menu");
  m.hidden = !m.hidden;
  $("side-pick").setAttribute("aria-expanded", m.hidden ? "false" : "true");
  if (!m.hidden) { $("side-filter").value = ""; pickAll = ""; renderPick(); $("side-filter").focus(); }
});
$("side-filter").addEventListener("input", renderPick);
// D-11: on the phone node/crew stands under the role's name, and a tap there opens the picker too.
$("side-who").addEventListener("click", () => { if (phone.matches && side && side.kind === "talk") $("side-pick").click(); });
$("side-close").addEventListener("click", closeTalk);
// The conversation is a frame of this page: its Esc arrives as a message, accepted only from that frame and this origin.
globalThis.addEventListener?.("message", (e) => {
  if (e.origin !== location.origin || e.source !== $("side-frame").contentWindow) return;
  if (e.data === "zmina:close" && side) closeTalk();
  if (e.data === "zmina:typing" || e.data === "zmina:typed") {
    frameTyping = e.data === "zmina:typing";
    kbCheck();
    // iOS scrolls the page to the field and, in the home-screen app, leaves it there when the keyboard goes: the
    // bar would float above a dead strip. The Talk tab has nothing to scroll, so it goes back to the top.
    if (!frameTyping && phone.matches && tab === "talk") setTimeout(() => scrollTo(0, 0), 50);
  }
});
$("side-back").addEventListener("click", closeTalk);
$("talk-open").addEventListener("click", (e) => {
  if (side) { closeTalk(); return; }
  const t = talkTarget();
  if (t) openTalk(t.crew, t.role, e.currentTarget);
});
globalThis.addEventListener?.("resize", () => { if (side) placeSide(); });
document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  if (sheet && !$("psheet").hidden) closeSheet();
  else if (side) closeTalk();
  else if (full) closeFull();
  else if (!$("history").hidden) { $("history").hidden = true; render(); } else if (openRow) {
    const k = openRow;
    openRow = "";
    openHand = "";
    render();
    [...document.querySelectorAll(`[data-k="${CSS.escape(k)}"] > .chev`)].find((x) => x.offsetParent)?.focus();
  }
});

// ---- the phone (D-12): three tabs at the bottom — Agents · Talk · Later — in place of the header stack. Below 860 px
// only. Agents is a row per role that waits on you, works or idles; a tap is Talk with that role. Later is what is not
// «now», as link rows; an item there opens as a bottom sheet with one action.

const phone = matchMedia("(max-width: 860px)");
const TABS = ["agents", "talk", "later"];
// D-09's tabs, kept in a browser or a bookmark, open where their content went: Move and Going on Agents, Crews on Later.
const OLD_TABS = { move: "agents", going: "agents", crews: "later" };
const tabOf = (t) => (TABS.includes(t) ? t : OLD_TABS[t] || "");
const tabSaved = () => { try { return localStorage.getItem("zmina.tab"); } catch (_) { return null; } };
let tab = [location.hash.slice(1), tabSaved()].map(tabOf).find(Boolean) || "agents";
const tab0 = tab; // the tab of the history entry the page opened on, which may have no hash
let tabFrom = "agents"; // where a closed Talk returns to
let laterSub = ""; // the Later list open: "" (the links), parked, alarms, hand, crews
let idleOpen = false; // a3: the idle roles laid out in place of their one line
let sheet = null; // l1: the item opened in the bottom sheet: {kind, crew, id, moving}
let sheetFrom = null; // the row that opened it
const tabScroll = {};
const badges = {};
document.body.dataset.tab = tab;

// goTab switches the open tab: its scroll is kept, the choice is kept in localStorage and in the URL hash (n1).
function goTab(t, push = true) {
  if (t === tab) return;
  tabScroll[tab] = scrollY;
  if (tab !== "talk") tabFrom = tab;
  tab = t;
  try { localStorage.setItem("zmina.tab", t); } catch (_) { /* the tab is kept for this visit only */ }
  if (push && location.hash !== "#" + t) history.pushState(null, "", "#" + t);
  document.body.dataset.tab = t;
  sheet = null;
  if (typeof pinPanel === "function") pinPanel();
  requestAnimationFrame(() => scrollTo(0, tabScroll[t] || 0));
}

// setTab is a tap on the bar: Talk opens the last conversation, else the dispatcher; another tab puts the panel away.
function setTab(t, push = true) {
  goTab(t, push);
  if (t === "talk") {
    if (!side || side.kind !== "talk") { const x = phoneTalk(); if (x) openTalk(x.crew, x.role, null); }
  } else if (side) {
    side = null;
    frameTyping = false;
    document.body.classList.remove("keyboard");
    $("side").hidden = true;
    $("talk-open").setAttribute("aria-expanded", "false");
  }
  render();
}

// openLater opens one of Later's lists (Agents' by-hand and alarms lines lead there).
function openLater(sub) {
  laterSub = sub;
  tabScroll.later = 0;
  if (tab === "later") { scrollTo(0, 0); render(); } else setTab("later");
}

// The Later tab always opens on its links.
for (const b of document.querySelectorAll("#tabbar [data-tab]")) {
  b.addEventListener("click", () => { if (b.dataset.tab === "later") laterSub = ""; setTab(b.dataset.tab); });
}
globalThis.addEventListener?.("popstate", () => {
  const h = tabOf(location.hash.slice(1)) || tab0;
  if (h !== tab) setTab(h, false);
});
$("pback").addEventListener("click", () => { laterSub = ""; render(); });
phone.addEventListener?.("change", () => { render(); kbCheck(); });

// k2: while a field has the keyboard up the bar steps aside; it never rides up on the keyboard.
// A field in the conversation says so itself (zmina:typing): iOS does not show the parent a focus inside the frame,
// and in the home-screen app the visual viewport does not shrink either.
let frameTyping = false;
function kbCheck() {
  const vv = globalThis.visualViewport;
  const a = document.activeElement;
  const field = /^(INPUT|TEXTAREA)$/.test(a?.tagName || "");
  // «keyboard», not «kb»: .kb is the cockpit's board. The phone only; the desktop has no bar to move.
  document.body.classList.toggle("keyboard", phone.matches && (frameTyping || (!!vv && (field || a === $("side-frame")) && vv.height < innerHeight - 120)));
}
globalThis.visualViewport?.addEventListener("resize", kbCheck);

// pinPanel holds the Talk tab's panel to what is on screen while the keyboard is up: iOS pans the page under it to the
// field, and a panel fixed to the page went up with it — its head under the clock, the conversation jumping.
function pinPanel() {
  const vv = globalThis.visualViewport, p = $("side");
  const up = !!vv && phone.matches && tab === "talk" && vv.height < innerHeight - 120;
  if (!up) {
    if (p.style.height) { p.style.top = p.style.height = p.style.bottom = ""; scrollTo(0, 0); }
    return;
  }
  p.style.top = vv.offsetTop + "px";
  p.style.height = vv.height + "px";
  p.style.bottom = "auto";
}
globalThis.visualViewport?.addEventListener("resize", pinPanel);
globalThis.visualViewport?.addEventListener("scroll", pinPanel);
document.addEventListener("focusin", () => setTimeout(kbCheck, 0));
document.addEventListener("focusout", () => setTimeout(kbCheck, 0));
globalThis.addEventListener?.("blur", () => setTimeout(kbCheck, 0));

// phoneTalk is whom the Talk tab opens (k1): the last conversation while the scope is the one it was opened under, else the scoped
// crew's dispatcher. The phone has no scope of its own (D-12 h1): it shows every crew.
function phoneTalk() {
  return lastTalk && lastTalk.scope === filter ? lastTalk : scopeDispatcher();
}

// phoneWhere is a row's crew tag (D-12 a1): the crew alone for this machine's crews, «crew · node» for a peer's.
function phoneWhere(crew) {
  const c = crewOf(crew);
  if (!c) return crew;
  return c.remote ? c.name + " · " + nodeOf(crew) : c.name;
}

// dispatcherOf is a crew's dispatcher role, where an item with no role of the crew is talked about.
function dispatcherOf(crew) {
  const c = crewOf(crew);
  if (!c) return "dispatcher";
  const d = c.roles.find((r) => r.role === c.dispatcher) || c.roles.find((r) => /dispatcher/.test(r.role));
  return d ? d.role : c.dispatcher || "dispatcher";
}

// firstLine is a question's first line.
const firstLine = (s) => (s || "").split("\n").map((x) => x.trim()).find(Boolean) || "";

// phoneLists is what the phone's tabs read, over every crew shown (All: the phone has no switch).
function phoneLists() {
  const live = (it) => shows(it.crew) && !gone(key(it.crew, it.id));
  const old = (a, b) => T(a.created) - T(b.created);
  const todo = snap.todo.filter(live);
  return {
    waiting: todo.filter((it) => it.kind !== "check" && it.kind !== "later"),
    // Yours by hand: the checks, and a parked item its alarm brought back.
    hand: todo.filter((it) => it.kind === "check" || it.kind === "later").sort(old),
    parked: snap.later.filter(live).sort(old),
    alarms: (snap.alarms || []).filter((a) => shows(a.crew) && !gone(alarmKey(a))).sort((a, b) => T(a.at) - T(b.at)),
    crews: snap.crews.filter((c) => shows(c.key) && !away(c.key) && !c.spare),
  };
}

// agents (D-12 a1–a3) is one row per role (node/crew/role), never per item, in three groups. Waiting on you: a role
// down that holds a task or a crew whose ticker stopped (red, first), a question on a session's screen, then by the
// oldest item that waits on the owner. Working: the busy roles. Idle: the rest of the roles that are up. Down roles that
// hold nothing and spare crews never show.
function agents(lists) {
  const rows = new Map();
  const get = (crew, role) => {
    const k = crew + "/" + role;
    if (!rows.has(k)) {
      const r = roleOf(crew, role);
      rows.set(k, { crew, role, icon: r ? r.icon : "generic", items: [], red: "", q: null });
    }
    return rows.get(k);
  };
  for (const d of redList()) {
    const tasks = (roleOf(d.crew, d.role)?.tasks || []).join(", ");
    get(d.crew, d.role).red = d.kind === "down" ? W.downHoldsP(tasks) : W.tickerNoWake;
  }
  for (const c of lists.crews) {
    for (const r of c.roles || []) if (r.state === "up" && r.prompt) get(c.key, r.role).q = firstLine(r.dialog && r.dialog.question);
  }
  // An item is the role's it holds up, else the role's that asked; one with no role of its crew is its dispatcher's.
  for (const it of lists.waiting) {
    const c = crewOf(it.crew);
    const known = (n) => n && (!c || c.roles.some((r) => r.role === n));
    get(it.crew, [it.blocks, it.role].find(known) || dispatcherOf(it.crew)).items.push(it);
  }
  const waiting = [...rows.values()].map((a) => {
    a.items.sort((x, y) => T(x.created) - T(y.created));
    const top = a.items[0];
    const wait = top ? nowMs() - T(top.created) : 0;
    return Object.assign(a, {
      kind: a.red ? "x" : "n", n: a.items.length, wait,
      line: a.red || (a.q !== null ? W.asksQ(a.q || W.onScreen) : top.title || top.text),
      age: a.red || !top ? "" : dur(wait), late: wait > 3 * 24 * HOUR,
    });
  }).sort((a, b) => !!b.red - !!a.red || (b.q !== null) - (a.q !== null) || b.wait - a.wait);
  const ts = (a) => (zero(a.r.since) ? 0 : T(a.r.since));
  const since = (a, b) => ts(b) - ts(a);
  const up = lists.crews.flatMap((c) => (c.roles || []).filter((r) => r.state === "up" && !rows.has(c.key + "/" + r.role))
    .map((r) => ({ crew: c.key, role: r.role, icon: r.icon, r, age: ago(r.since) })));
  const working = up.filter((a) => a.r.status === "busy").sort(since).map((a) => Object.assign(a, { kind: "busy", line: taskOf(a.crew, a.r) || W.busyNoTask }));
  const idle = up.filter((a) => a.r.status !== "busy").sort(since).map((a) => Object.assign(a, { kind: "idle", line: "" }));
  return { waiting, working, idle };
}

// taskOf is the title of the task a role works on: its open work, else the first task it holds.
function taskOf(crew, r) {
  const w = snap.work.find((x) => x.crew === crew && x.role === r.role && x.state === "open");
  if (w) return w.title;
  const id = (r.tasks || [])[0];
  const t = id && (snap.tasks || []).find((x) => x.crew === crew && x.id === id);
  return t ? t.title || t.asked : "";
}

// agentRow (a1, a2, a4) is a role: its glyph in a circle coloured by its state, with how many items wait on the
// owner as a pill once there are two; who and where; what it waits on you for (one line for a working or idle role);
// the oldest wait. A tap is Talk with that role. No buttons, chips or fields inside it.
function agentRow(a) {
  const one = a.kind === "busy" || a.kind === "idle";
  const b = button("ag " + a.kind + (one ? " one" : "") + stl(a.crew), "", () => openTalk(a.crew, a.role, b));
  b.dataset.k = "agent:" + a.crew + "/" + a.role;
  const av = el("span", "av " + a.kind);
  av.append(glyph(a.icon));
  if (a.n >= 2) av.append(el("span", "cnt", String(a.n)));
  const w = el("span", "w");
  const rn = el("span", "rn");
  const where = phoneWhere(a.crew);
  rn.append(el("span", "r", a.role), el("small", "", where));
  if (a.kind === "busy") rn.append(el("span", "tk", " · " + a.line));
  w.append(rn);
  if (!one) w.append(el("span", "ask", a.line));
  b.append(av, w, el("span", "age" + (one ? " m" : a.late ? " x" : ""), a.age || ""), el("span", "chev", "›"));
  b.setAttribute("aria-label", [W.talkTo(a.role), where, a.line, a.n >= 2 ? W.nWaiting(a.n) : "", a.age].filter(Boolean).join(" · "));
  if (a.items && a.items.length) b.title = a.items.map((it) => idOf(it.crew, it.id)).join(" · ");
  return b;
}

// agentSection is a group of rows under its heading; fold makes the heading the way to fold it back.
function agentSection(label, rows, cls, fold) {
  const s = el("section", "ags");
  const h = fold ? button("lab fold", "", fold) : el("h2", "lab");
  h.append(label + " ", el("b", cls, String(rows.length)));
  if (fold) h.setAttribute("aria-expanded", "true");
  const list = el("div", "rows2");
  list.append(...rows.map(agentRow));
  s.append(h, list);
  return s;
}

// lineTo is a one-line opener: a name and its count, «›» (a3, l1).
function lineTo(name, value, fn, cls) {
  const b = button("ln" + (cls ? " " + cls : ""), "", fn);
  b.append(el("span", "n", name), el("span", "v", (value ? value + " " : "") + "›"));
  return b;
}

const oldestOf = (xs) => (xs.length ? dur(nowMs() - T(xs[0].created)) : "");

function renderAgents(lists, a) {
  const out = [];
  if (a.waiting.length) out.push(agentSection(W.waitingOn, a.waiting, a.waiting.some((x) => x.red) ? "x" : "n"));
  else out.push(el("p", "pnone", W.nothingWaits));
  if (a.working.length) out.push(agentSection(W.workingW, a.working, "b"));
  if (a.idle.length) {
    if (idleOpen) out.push(agentSection(W.idleW, a.idle, "", () => { idleOpen = false; render(); }));
    else {
      const l = lineTo(W.idleW, String(a.idle.length), () => { idleOpen = true; render(); }, "fold");
      l.setAttribute("aria-expanded", "false");
      out.push(l);
    }
  }
  const tail = [];
  if (lists.hand.length) tail.push(lineTo(W.yoursByHand, lists.hand.length + " · " + W.oldestW(oldestOf(lists.hand)), () => openLater("hand")));
  if (lists.alarms.length) tail.push(lineTo(W.alarms, lists.alarms.length + " · " + W.nextAt(alarmWhen(lists.alarms[0])), () => openLater("alarms")));
  if (tail.length) {
    const t = el("div", "lns");
    t.append(...tail);
    out.push(t);
  }
  patch($("pagents"), out);
}

// ---- Later (D-12 l1): 48 px link rows; Parked, Alarms and Yours by hand open their lists, whose items open as a
// bottom sheet; Done today and History open the panel's lists; Crews and nodes lists every crew.

const SUBS = { parked: () => W.later, alarms: () => W.alarms, hand: () => W.yoursByHand, crews: () => W.crewsNodes };

function renderLaterTab(lists) {
  if (laterSub && !SUBS[laterSub]) laterSub = "";
  if (laterSub) { patch($("plater"), laterList(lists)); return; }
  const done = lists.crews.reduce((n, c) => n + (c.done_today || 0), 0);
  const red = redList();
  const al = lists.alarms;
  patch($("plater"), [
    lineTo(W.later, lists.parked.length ? W.parkedN(lists.parked.length, oldestOf(lists.parked)) : "0", () => openLater("parked")),
    lineTo(W.alarms, al.length ? al.length + " · " + W.nextAt(alarmWhen(al[0])) : "0", () => openLater("alarms")),
    lineTo(W.yoursByHand, lists.hand.length ? lists.hand.length + " · " + W.oldestW(oldestOf(lists.hand)) : "0", () => openLater("hand")),
    lineTo(W.doneTodayN, String(done), () => openList("done")),
    lineTo(W.historyTab, "", openHistory),
    lineTo(W.crewsNodes, lists.crews.length + (red.length ? " · " + W.nDown(red.length) : ""), () => openLater("crews")),
  ]);
}

// laterItem is an item of a Later list: its title in two lines at most, where and how old, «›»; a tap opens its sheet.
function laterItem(k, title, meta, open) {
  const b = button("li", "", () => open(b));
  b.dataset.k = k;
  const w = el("span", "w");
  w.append(el("span", "t", title), el("span", "m", meta));
  b.append(w, el("span", "chev", "›"));
  return b;
}

function laterList(lists) {
  const sheetOf = (s) => (from) => openSheet(s, from);
  if (laterSub === "parked") {
    return lists.parked.map((it) => laterItem(key(it.crew, it.id), it.text, phoneWhere(it.crew) + " · " + dur(nowMs() - T(it.created)),
      sheetOf({ kind: "parked", crew: it.crew, id: it.id })));
  }
  if (laterSub === "alarms") {
    return lists.alarms.map((a) => laterItem(alarmKey(a), alarmText(a), phoneWhere(a.crew) + " · " + alarmWhen(a),
      sheetOf({ kind: "alarm", crew: a.crew, id: a.id })));
  }
  if (laterSub === "hand") {
    return lists.hand.map((it) => laterItem(key(it.crew, it.id), it.title || it.text, phoneWhere(it.crew) + " · " + dur(nowMs() - T(it.created)),
      sheetOf({ kind: "hand", crew: it.crew, id: it.id })));
  }
  // Crews and nodes (D-09 c1's list): a group per node, a row per crew with its lamp and state; a tap is Talk with
  // its dispatcher.
  const out = [];
  const red = redList(true);
  const nodes = new Map();
  for (const c of snap.crews.filter((c) => !away(c.key))) {
    const n = c.node || snap.self || "";
    if (!nodes.has(n)) nodes.set(n, []);
    nodes.get(n).push(c);
  }
  for (const [n, crews] of nodes) {
    if (n) out.push(el("p", "grp", n + (crews.some((c) => c.remote) ? W.overMesh : "")));
    for (const c of crews) {
      const [lamp, state, stCls] = crewState(c, red);
      const b = button("cr", "", () => openTalk(c.key, dispatcherOf(c.key), b));
      b.append(el("span", "lamp " + lamp), el("span", "n", c.name), el("span", "s " + stCls, state));
      b.setAttribute("aria-label", W.talkTo(dispatcherOf(c.key)) + " · " + c.name + " · " + state);
      out.push(b);
    }
  }
  return out;
}

// crewState is a crew's lamp, its words and their colour: down first, then working, stale, spare, stopped.
function crewState(c, red) {
  const mine = red.filter((d) => d.crew === c.key);
  const busy = (c.roles || []).filter((r) => r.state === "up" && r.status === "busy").length;
  if (c.stale) {
    const n = (snap.nodes || []).find((x) => x.name === c.node);
    return ["h", W.stale + (n && !zero(n.seen || "") ? " · " + ago(n.seen) : ""), "f"];
  }
  if (c.spare) return ["h", W.spare, "f"];
  if (c.retired || !zero(c.stopped)) return ["h", W.stoppedW, "f"];
  if (mine.length) {
    const words = mine.map((d) => (d.kind === "down" ? W.roleDown(d.role) : W.tickerStopped));
    if (busy) words.push(W.nWorking(busy));
    return ["x", words.join(" · "), "x"];
  }
  if (busy) return ["b", W.nWorking(busy), "b"];
  return ["", W.upW, ""];
}

// ---- the sheet (l1): what the item is, who raised it, where and how old; its title and its real text; ONE neutral
// primary (by hand: Done; an alarm: Move to…, which opens the when-field in the sheet; parked: Bring back) and
// «Talk to <role>». Swipe down, the shade or Esc closes it.

function openSheet(s, from) {
  sheet = s;
  sheetFrom = from || null;
  $("psheet").dataset.k = "";
  render();
  $("psheet").focus();
}

function closeSheet() {
  if (!sheet) return;
  sheet = null;
  render();
  if (sheetFrom && sheetFrom.isConnected) sheetFrom.focus();
  else if (sheetFrom?.dataset.k) [...document.querySelectorAll(`#plater [data-k="${CSS.escape(sheetFrom.dataset.k)}"]`)][0]?.focus();
}

// sheetItem is what the sheet shows, as the snapshot has it now; gone once it was dealt with.
function sheetItem(s) {
  if (s.kind === "alarm") {
    const a = (snap.alarms || []).find((x) => x.crew === s.crew && x.id === s.id);
    return a && !gone(alarmKey(a)) ? a : null;
  }
  const it = (s.kind === "parked" ? snap.later : snap.todo).find((x) => x.crew === s.crew && x.id === s.id);
  return it && !gone(key(it.crew, it.id)) ? it : null;
}

// The sheet is drawn once per item (and once more when Move to… opens its field): a snapshot never redraws a field
// being typed in.
function renderSheet() {
  const sh = $("psheet");
  const it = phone.matches && sheet && tab === "later" ? sheetItem(sheet) : null;
  if (!it) {
    if (sheet && phone.matches) sheet = null;
    sh.hidden = $("pshade").hidden = true;
    sh.dataset.k = "";
    return;
  }
  sh.hidden = $("pshade").hidden = false;
  const k = sheet.kind + ":" + key(sheet.crew, sheet.id) + (sheet.moving ? ":moving" : "");
  if (sh.dataset.k === k) return;
  sh.dataset.k = k;
  const s = sheet;
  const where = phoneWhere(s.crew);
  let kind, by, age, title, text, primary, talkRole;
  if (s.kind === "alarm") {
    const left = T(it.at) - nowMs();
    kind = W.alarmW;
    by = it.by || "";
    age = left > 0 ? W.alIn(dur(left)) : W.alDue;
    title = alarmText(it);
    text = [it.note, it.for && it.for !== "you" ? W.alFor(it.for) : ""].filter(Boolean).join(" · ");
    talkRole = it.kind === "sleep" && it.role ? it.role : by && roleOf(s.crew, by) ? by : dispatcherOf(s.crew);
  } else {
    kind = s.kind === "parked" ? W.later : W.yoursByHand;
    by = it.role || "";
    age = dur(nowMs() - T(it.created));
    title = s.kind === "parked" ? it.text : it.title || it.text;
    text = s.kind === "parked" ? it.note : says(it.text, title) ? it.text : it.note;
    talkRole = by && roleOf(s.crew, by) ? by : dispatcherOf(s.crew);
  }
  const kids = [el("span", "grab")];
  kids.push(el("p", "sk", [kind, by ? W.raisedBy(by) : "", where, age].filter(Boolean).join(" · ")));
  const h = el("h3", "", title);
  h.id = "psheet-h";
  kids.push(h);
  if (text) kids.push(el("p", "sn", text));
  const shut = () => { sheet = null; };
  if (s.kind === "hand") primary = button("pri", W.done, () => { shut(); doneHand(it); });
  else if (s.kind === "parked") primary = button("pri", W.back, () => { shut(); act(it, "back", "", W.broughtBack); });
  else if (!s.moving) primary = button("pri", W.moveTo, () => { sheet.moving = true; render(); $("psheet").querySelector(".field")?.focus(); });
  else {
    const f = el("textarea", "field when");
    f.rows = 1;
    f.placeholder = W.alWhen;
    f.setAttribute("aria-label", W.alWhenLabel);
    primary = button("pri", W.alMove, () => {
      const v = f.value.trim();
      if (!v) return;
      shut();
      alarmAct(it, "move", v, "", W.alMoved, alarmKey(it));
    });
    const sync = () => { primary.disabled = !f.value.trim(); };
    sync();
    f.addEventListener("input", sync);
    f.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && f.value.trim()) { e.preventDefault(); primary.click(); }
      if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); sheet.moving = false; render(); }
    });
    kids.push(f);
  }
  kids.push(primary, button("sec", W.talkTo(talkRole), () => { const c = s.crew; shut(); openTalk(c, talkRole, null); }));
  patch(sh, kids);
}

// doneHand closes a by-hand item: a check is done, a parked item its alarm brought back is done with.
function doneHand(it) {
  return it.kind === "later" ? act(it, "done", "", W.sentTo) : done(it);
}

$("pshade").addEventListener("click", closeSheet);
// Swipe down closes it: the sheet follows the finger, and a pull past 60 px lets it go.
{
  let y0 = null;
  const sh = $("psheet");
  sh.addEventListener("touchstart", (e) => { y0 = e.target.closest?.("textarea") ? null : e.touches[0].clientY; }, { passive: true });
  sh.addEventListener("touchmove", (e) => {
    if (y0 === null) return;
    sh.style.transform = "translateY(" + Math.max(0, e.touches[0].clientY - y0) + "px)";
  }, { passive: true });
  sh.addEventListener("touchend", (e) => {
    if (y0 === null) return;
    const dy = e.changedTouches[0].clientY - y0;
    y0 = null;
    sh.style.transform = "";
    if (dy > 60) closeSheet();
  });
}

function renderPhone() {
  if (!phone.matches) { renderSheet(); return; }
  const lists = phoneLists();
  const a = agents(lists);
  const sub = tab === "later" && laterSub && SUBS[laterSub];
  setText($("phead-h"), sub ? sub() : tab === "later" ? W.tabLater : tab === "talk" ? W.talk : W.tabAgents);
  $("pback").hidden = !sub;
  renderAgents(lists, a);
  renderLaterTab(lists);
  renderSheet();
  renderTabs(a.waiting);
  // Opened on #talk (or kept there): the conversation comes with the first snapshot.
  if (tab === "talk" && !side) setTimeout(() => { const x = !side && tab === "talk" && phoneTalk(); if (x) openTalk(x.crew, x.role, null); }, 0);
}

// t1 · the badges: Agents counts the roles that wait on you (red while any of them is down), Talk dots while its role
// waits on you; Later carries none. A badge that changes pops once.
function renderTabs(waiting) {
  const set = (t, on, text, cls) => {
    const b = document.querySelector(`#tabbar [data-tab="${t}"]`);
    if (!b) return;
    if (tab === t) b.setAttribute("aria-current", "page");
    else b.removeAttribute("aria-current");
    const mark = b.querySelector(".bd, .dot");
    if (!mark) return;
    mark.hidden = !on;
    if (text !== undefined) setText(mark, text);
    if (cls !== undefined) mark.classList.toggle("x", cls);
    const v = on ? String(text ?? "") + (cls ? "x" : "") : "";
    if (badges[t] !== undefined && badges[t] !== v && on) { mark.classList.remove("pop"); void mark.offsetWidth; mark.classList.add("pop"); }
    badges[t] = v;
  };
  set("agents", waiting.length > 0, String(waiting.length), waiting.some((a) => a.red));
  const t = side && side.kind === "talk" ? side : phoneTalk();
  const r = t && roleOf(t.crew, t.role);
  set("talk", !!(r && (r.prompt || r.stuck)));
  set("later");
}

// ---- hover: a tinted row raises its project everywhere

function setHover(node) {
  const p = node?.closest?.(".row[data-p], .hk[data-p]")?.dataset.p || null;
  if (hover === p) return;
  hover = p;
  applyHover();
}

function applyHover() {
  document.body.classList.toggle("hp", !!hover);
  for (const n of document.querySelectorAll(".row")) n.classList.toggle("lit", !!hover && n.dataset.p === hover);
}

document.addEventListener("pointerover", (e) => setHover(e.target));
document.addEventListener("focusin", (e) => setHover(e.target));
document.documentElement.addEventListener("mouseleave", () => setHover(null));
document.addEventListener("visibilitychange", () => document.body.classList.toggle("hidden", document.hidden));
// Ages and the clock move between snapshots.
setInterval(() => { if (snap && !document.hidden && !document.querySelector(".field:focus")) render(); }, 30000);

// ---- stream

// settle draws a held snapshot once no field has the caret and no press is under way (a press on the field's own
// button must reach it before the panel is drawn again).
function settle() {
  if (!stale || pointerDown || document.activeElement?.classList.contains("field")) return;
  stale = false;
  render();
}
document.addEventListener("focusout", (e) => { if (e.target.classList?.contains("field")) setTimeout(settle, 0); });
document.addEventListener("pointerdown", () => { pointerDown = true; }, true);
document.addEventListener("pointerup", () => { pointerDown = false; setTimeout(settle, 0); }, true);

function connect() {
  const es = new EventSource("api/stream");
  es.addEventListener("state", (e) => {
    snap = JSON.parse(e.data);
    got = Date.now();
    snap.tints = snap.tints || {};
    $("mark").classList.remove("lost");
    document.body.classList.remove("lost");
    // While a reply is being typed the page holds still: the snapshot is drawn when the field is left.
    if (document.activeElement?.classList.contains("field")) stale = true;
    else render();
  });
  es.onerror = () => {
    $("mark").classList.add("lost");
    document.body.classList.add("lost");
    $("clock").lastElementChild.textContent = W.lost;
  };
}

(async () => {
  try {
    const r = await fetch("assets/mark.svg");
    if (r.ok) $("mark").innerHTML = await r.text(); // our own embedded asset, not shift data
  } catch (_) { /* the page works without the mark */ }
  await Promise.all(["designer", "developer", "director", "dispatcher", "generic", "reviewer"].map(async (n) => {
    try {
      const r = await fetch(`assets/roles/${n}.svg`);
      if (r.ok) glyphs[n] = await r.text();
    } catch (_) { /* a role without its glyph shows the generic one, or none */ }
  }));
  render();
})();
connect();
loadCost();
