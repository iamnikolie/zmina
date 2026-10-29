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

// ---- words: the server's language (zmina ui --lang, else its locale); ?lang=ru|en overrides and is remembered

const qLang = new URLSearchParams(location.search).get("lang");
if (qLang === "ru" || qLang === "en") localStorage.setItem("zmina.lang", qLang);
let plural = new Intl.PluralRules("en");
const pick = (n, forms) => forms[plural.select(n)] || forms.other;

const DICT = {
  ru: {
    hold: "Ты держишь", ask: "Ответить", go: "Идёт", later: "Отложено", others: "Ждут других", all: "Всё",
    sayHold: (n) => "Ты держишь " + n, sayAsk: (n) => "ответить " + n, sayGo: (n) => "идёт " + n, sayLater: (n) => "отложено " + n,
    noHold: "никого не держишь", noAsk: "вопросов нет", noGo: "сейчас ничего не идёт",
    yes: "Да", no: "Нет", noText: "нет", done: "Сделано", answer: "Ответить…", park: "Позже", ignore: "Забей", send: "Отправить", cancel: "Отмена",
    sentTo: "Отправлено диспетчеру", parkedTo: "Отложено", ignored: "Убрано", broughtBack: "Возвращено", dropped: "Выброшено",
    undo: "отменить", back: "Вернуть", drop: "Выбросить",
    notSent: "Не отправлено: ", rec: "Рекомендую: ", more: (n) => "ещё " + n, less: "свернуть", hands: "Руками:",
    waits: (r) => "ждёт " + r, waitOn: (n, who) => n + " " + pick(n, { one: "ждёт", other: "ждут" }) + " " + who,
    setAside: (n) => n + " " + pick(n, { one: "убрана", other: "убраны" }) + " тобой",
    doneToday: "сделано сегодня: ", history: "история", historyH: "Что было",
    stopped: (c, t) => c + ": тикер стоит" + (t ? " " + t : ""), shiftStopped: (c) => c + ": смена остановлена",
    lost: "переподключение…", noCrew: "Нет смены в ~/.zmina — zmina init",
    min: "мин", h: "ч", d: "дн", lessMin: "<1 мин", busyLive: "работает", notLive: "не работает",
    ev: {
      new: "начата: ", waitingOn: "ждёт тебя: ", waitingWho: (w) => "ждёт " + w + ": ", done: "готово", dropped: "брошена",
      reopened: "снова в работе", asked: "вопрос тебе: ", answered: "ты ответил: ", parked: "отложено: ",
      blocked: "застрял: ", watchFailed: "наблюдатель упал: ", start: "смена запущена", stop: "смена остановлена",
    },
  },
  en: {
    hold: "You hold", ask: "Answer", go: "Going", later: "Parked", others: "Waiting on others", all: "All",
    sayHold: (n) => "You hold " + n, sayAsk: (n) => "answer " + n, sayGo: (n) => "going " + n, sayLater: (n) => "parked " + n,
    noHold: "you hold nobody up", noAsk: "no questions", noGo: "nothing goes right now",
    yes: "Yes", no: "No", noText: "no", done: "Done", answer: "Answer…", park: "Later", ignore: "Ignore", send: "Send", cancel: "Cancel",
    sentTo: "Sent to the dispatcher", parkedTo: "Parked", ignored: "Set aside", broughtBack: "Brought back", dropped: "Dropped",
    undo: "undo", back: "Bring back", drop: "Drop",
    notSent: "Not sent: ", rec: "Recommended: ", more: (n) => n + " more", less: "show less", hands: "By hand:",
    waits: (r) => "waits on " + r, waitOn: (n, who) => n + " waiting on " + who,
    setAside: (n) => n + " set aside by you",
    doneToday: "done today: ", history: "history", historyH: "What happened",
    stopped: (c, t) => c + ": ticker stopped" + (t ? " " + t + " ago" : ""), shiftStopped: (c) => c + ": shift stopped",
    lost: "reconnecting…", noCrew: "No crew under ~/.zmina — zmina init",
    min: "min", h: "h", d: "d", lessMin: "<1 min", busyLive: "working", notLive: "not working",
    ev: {
      new: "started: ", waitingOn: "waits on you: ", waitingWho: (w) => "waits on " + w + ": ", done: "done", dropped: "dropped",
      reopened: "back in work", asked: "asks you: ", answered: "you answered: ", parked: "parked: ",
      blocked: "stuck: ", watchFailed: "watch failed: ", start: "shift started", stop: "shift stopped",
    },
  },
};
let W = DICT.en;

function setLang(server) {
  const lang = localStorage.getItem("zmina.lang") || server || "en";
  W = DICT[lang] || DICT.en;
  plural = new Intl.PluralRules(W === DICT.ru ? "ru" : "en");
  document.documentElement.lang = W === DICT.ru ? "ru" : "en";
}

// ---- state

let snap = null;
let got = 0; // when the snapshot came, so ages move between snapshots
let first = true;
let filter = localStorage.getItem("zmina.crew") || "all";
let lastTick = "";
let hover = null;
let showAllAsks = false;
let openHand = "";
let openRow = ""; // the one row whose actions are open
let draft = { k: "", text: "" }; // the reply being typed, kept across renders
const groups = new Set(); // «waiting on others» groups open
const seen = new Set();
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
const crewName = (key) => (snap.crews.find((c) => c.key === key) || { name: key }).name;
const multi = () => snap.crews.length > 1;
const who = (crew, role) => (role ? (multi() && crew !== "default" ? crewName(crew) + "/" : "") + role : "");
const tickerDead = (c) => zero(c.last_tick) || nowMs() - T(c.last_tick) > 2 * 60000;
const key = (crew, id) => crew + "/" + id;
const shows = (crew) => filter === "all" || filter === crew;
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
  if (filter !== "all" && !snap.crews.some((c) => c.key === filter)) filter = "all";

  // Keys the server took leave once its snapshot no longer has them.
  for (const k of sentKeys) {
    if (![...snap.todo, ...snap.later].some((it) => key(it.crew, it.id) === k)) sentKeys.delete(k);
  }
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
  renderOthers(others);
  renderFoot();
  if (!$("history").hidden) renderFeed();
  applyHover();
  if (typing >= 0) {
    const f = document.querySelector(`[data-k="${CSS.escape(openRow)}"] .panel .field`);
    f?.focus();
    f?.setSelectionRange(typing, typing);
  }
  document.body.classList.remove("sk");
  first = false;
}

function renderSay(hold, asks, going, parked) {
  const say = $("say");
  say.replaceChildren();
  if (!snap.crews.length) {
    say.append(el("span", "x", W.noCrew));
    return;
  }
  const long = hold.some((it) => nowMs() - T(it.created) > HOUR);
  const parts = [[W.sayHold(hold.length), long ? "x" : "n", "hold"], [W.sayAsk(asks.length), "", "ask"],
    [W.sayGo(going.length), "b", "go"], [W.sayLater(parked.length), "m", "later"]];
  parts.forEach(([text, cls, target], i) => {
    if (i) say.append(" · ");
    const s = el("span", "k " + cls, text);
    s.onclick = () => $(target).scrollIntoView({ behavior: "smooth", block: "start" });
    say.append(s);
  });
  const dead = [];
  for (const c of snap.crews) {
    if (!zero(c.stopped)) dead.push(W.shiftStopped(c.name));
    else if (tickerDead(c)) dead.push(W.stopped(c.name, zero(c.last_tick) ? "" : ago(c.last_tick)));
  }
  for (const d of dead) say.append(" · ", el("span", "k x", d));

  $("clock").classList.toggle("dead", dead.length > 0);
  $("clock").lastElementChild.textContent = hhmm(nowMs());
  document.title = ["zmina", ...dead, hold.length ? W.sayHold(hold.length) : "", asks.length ? W.sayAsk(asks.length) : ""]
    .filter(Boolean).join(" · ");
  favicon(dead.length || long ? "#ff7b72" : hold.length + asks.length ? "#f0b45a" : "");

  // One mark for every crew: it redraws on a beat from any of them.
  const newest = snap.crews.map((c) => c.last_tick || "").sort().pop() || "";
  if (newest && lastTick && newest !== lastTick) beat();
  lastTick = newest;
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

function renderChips() {
  const nav = $("chips");
  nav.hidden = !multi();
  if (!multi()) return;
  nav.replaceChildren(...[["all", W.all], ...snap.crews.map((c) => [c.key, c.name])].map(([k, label]) => {
    const b = button(filter === k ? "on" : "", label, () => { filter = k; localStorage.setItem("zmina.crew", k); render(); });
    b.setAttribute("aria-pressed", filter === k ? "true" : "false");
    return b;
  }));
  // The edge fades only when the chips scroll.
  nav.classList.toggle("over", nav.scrollWidth > nav.clientWidth + 1);
}

function heading(id, label, n, cls) {
  $(id).replaceChildren(label + " ", el("b", cls || "", String(n)));
}

// fresh marks a row that came after the first render, so it enters from the top.
function fresh(row, k) {
  if (!first && !seen.has(k)) row.classList.add("enter");
  seen.add(k);
  return row;
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
  if (openRow) document.querySelector(`[data-k="${CSS.escape(openRow)}"] .panel input, [data-k="${CSS.escape(openRow)}"] .panel textarea`)?.focus();
}

// rowShell is the focusable row: its words, its age, an optional chip, the «›»; hov the in-place actions (≥ 860),
// panel what opens under it.
function rowShell(k, cls, words, age, chip, hov, panel) {
  const row = fresh(el("div", "row" + (cls ? " " + cls : "")), k);
  row.dataset.k = k;
  row.tabIndex = 0;
  row.setAttribute("role", "button");
  const isOpen = openRow === k;
  row.setAttribute("aria-expanded", isOpen ? "true" : "false");
  row.classList.toggle("open", isOpen);
  row.addEventListener("click", (e) => {
    if (e.target.closest("button, a, input, textarea")) return;
    toggleRow(k);
  });
  row.addEventListener("keydown", (e) => {
    if (e.target !== row) return;
    if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggleRow(k); }
  });
  row.append(words, age || el("span"), chip || el("span"), el("span", "chev", "›"));
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

function act(it, action, text, word) {
  const k = key(it.crew, it.id);
  openRow = "";
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

// toast says what was done, with «отменить» for 5 s; one without undo (an error) leaves after 5 s too.
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

async function post(body) {
  const r = await fetch("api/todo", { method: "POST", headers: { "Content-Type": "application/json", "X-Zmina": "1" }, body: JSON.stringify(body) });
  if (!r.ok) throw new Error((await r.text()).trim());
}

// reply is the inline field: Enter (⌘Enter in the long one) sends the words as the answer.
function reply(it, long) {
  const k = key(it.crew, it.id);
  const f = el(long ? "textarea" : "input", "field");
  if (long) f.rows = 2;
  f.placeholder = W.answer;
  f.setAttribute("aria-label", W.answer);
  if (draft.k === k) f.value = draft.text;
  f.addEventListener("input", () => { draft = { k, text: f.value }; });
  const send = () => { if (f.value.trim()) act(it, "answer", f.value.trim(), W.sentTo); };
  f.addEventListener("keydown", (e) => {
    if (e.key === "Escape") { e.preventDefault(); draft = { k: "", text: "" }; toggleRow(k); }
    if (e.key === "Enter" && (!long || e.metaKey || e.ctrlKey)) { e.preventDefault(); send(); }
  });
  return [f, send];
}

function detail(lines, url) {
  const d = el("div", "detail", lines.filter(Boolean).join("\n"));
  if (url) {
    const link = el("a", "", url);
    link.href = url;
    link.target = "_blank";
    link.rel = "noreferrer";
    d.append("\n", link);
  }
  return d;
}

function itemWords(it, holds) {
  const words = el("div", "words");
  if (it.project) words.append(el("span", "pj", it.project));
  words.append(el("span", "tt", it.title || it.text));
  if (holds) words.append(el("span", "gr", W.waits(who(it.crew, it.blocks || it.role) || "—")));
  else if (!it.rec && it.note) words.append(el("span", "gr", it.note));
  return words;
}

function itemDetail(it, holds) {
  return detail([it.title && it.text !== it.title ? it.text : "", it.rec ? W.rec + it.rec + (it.reason ? " — " + it.reason : "") : "",
    it.note && (holds || it.rec) ? it.note : "", [who(it.crew, it.role), it.task || it.id].filter(Boolean).join(" · ")], it.url);
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

// ---- L1 · you hold (b3): the row is the button; a tap opens the reply to the session that waits

function renderHold(hold) {
  heading("hold-h", W.hold, hold.length, hold.some((it) => nowMs() - T(it.created) > HOUR) ? "x" : "n");
  const rows = hold.filter((it) => !gone(key(it.crew, it.id))).map((it) => {
    const k = key(it.crew, it.id);
    const hov = el("div");
    const panel = el("div");
    panel.append(itemDetail(it, true));
    const [f, send] = reply(it, true);
    const ctl = el("div", "ctl");
    ctl.append(f, button("q", W.send, send));
    if (it.kind === "check") ctl.append(button("q", W.done, () => act(it, "done", "", W.sentTo)));
    ctl.append(button("q m", W.park, () => act(it, "park", "", W.parkedTo)), button("q m", W.ignore, () => act(it, "ignore", "", W.ignored)));
    panel.append(ctl);
    hov.append(button("q m", W.park, () => act(it, "park", "", W.parkedTo)), button("q m", W.ignore, () => act(it, "ignore", "", W.ignored)));
    const row = rowShell(k, "", itemWords(it, true), ageOf(it), chip(it), hov, panel);
    if (it.task) row.dataset.w = key(it.crew, it.task);
    return tint(row, it.project);
  });
  if (!rows.length) rows.push(el("p", "none", W.noHold));
  $("hold-rows").replaceChildren(...rows);
}

// ---- L2 · answer (b2): one chip with the recommendation; open: the field, «Позже», «Нет»

function askRow(it) {
  const k = key(it.crew, it.id);
  const hov = el("div");
  const panel = el("div");
  panel.append(itemDetail(it, false));
  const [f] = reply(it, false);
  const ctl = el("div", "ctl");
  ctl.append(f);
  if (it.kind === "check") ctl.append(button("q", W.done, () => act(it, "done", "", W.sentTo)));
  ctl.append(button("q", W.park, () => act(it, "park", "", W.parkedTo)));
  if (it.kind !== "check") ctl.append(button("q", W.no, () => act(it, "answer", W.noText, W.sentTo)));
  panel.append(ctl);
  hov.append(button("q", W.answer, () => toggleRow(k)));
  if (it.kind === "check") hov.append(button("q", W.done, () => act(it, "done", "", W.sentTo)));
  hov.append(button("q", W.park, () => act(it, "park", "", W.parkedTo)));
  if (it.kind !== "check") hov.append(button("q", W.no, () => act(it, "answer", W.noText, W.sentTo)));
  const row = rowShell(k, "", itemWords(it, false), ageOf(it), chip(it), hov, panel);
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
  const rows = shown.map(askRow);
  if (decisions.length > cap + 1) {
    rows.push(button("morerow", showAllAsks ? W.less : W.more(decisions.length - cap), () => { showAllAsks = !showAllAsks; render(); }));
  }
  if (!live.length) rows.push(el("p", "none", W.noAsk));
  $("ask-rows").replaceChildren(...rows);

  // The checks: one line; a tap on one opens its row under the line.
  const hands = $("hands");
  hands.hidden = !checks.length;
  if (!checks.some((it) => key(it.crew, it.id) === openHand) || openRow !== openHand) openHand = "";
  hands.replaceChildren(el("span", "hl", W.hands));
  checks.forEach((it, i) => {
    const k = key(it.crew, it.id);
    if (i) hands.append(" · ");
    const link = el("span", "hk" + (openHand === k ? " on" : ""), it.title || it.text);
    link.tabIndex = 0;
    link.setAttribute("role", "button");
    const flip = () => { openHand = openHand === k ? "" : k; openRow = openHand; render(); };
    link.onclick = flip;
    link.onkeydown = (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); flip(); } };
    hands.append(tint(link, it.project));
  });
  const opened = checks.find((it) => key(it.crew, it.id) === openHand);
  if (opened) hands.append(askRow(opened));
}

// ---- L3 · going: open work, and work waiting on a role of the crew

// nowLine is the latest plain note; the role's own status line only when no note came for half an hour, its ids
// and pids taken out.
function nowLine(w) {
  const note = w.milestone && w.milestone.text;
  const stale = !w.milestone || nowMs() - T(w.milestone.at) > 30 * 60000;
  if (stale && w.status === "busy" && w.now) {
    return w.now.replace(/\b(pid|PID)\s*\d+/g, "").replace(/\b[0-9a-f]{7,40}\b/g, "").replace(/\b[ATL]-\d+\b/g, "")
      .replace(/\(\s*[,;]?\s*\)/g, "").replace(/\s{2,}/g, " ").trim();
  }
  return note || w.why || "";
}

function renderGo(going) {
  const live = (w) => w.role_state === "up" && w.status === "busy" && w.state === "open";
  going.sort((a, b) => live(b) - live(a) || T(b.updated) - T(a.updated));
  heading("go-h", W.go, going.length, "b");
  const rows = going.map((w) => {
    const k = key(w.crew, w.id);
    const words = el("div", "words");
    const dot = el("span", "dot" + (live(w) ? " live" : ""));
    dot.title = live(w) ? W.busyLive : W.notLive;
    words.append(dot, el("span", "who", who(w.crew, w.role)));
    if (w.project) words.append(el("span", "pj", w.project));
    words.append(el("span", "tt", w.title), el("span", "gr", nowLine(w)));
    const meta = el("span", "age m");
    if (w.on) meta.append(el("span", "wait", W.waits(who(w.crew, w.on))), " ");
    meta.append(ago(w.created));
    const panel = el("div");
    panel.append(detail([w.why, w.milestone && w.milestone.text], w.url));
    const row = rowShell(k, "go", words, meta, null, null, panel);
    row.dataset.w = k;
    return tint(row, w.project);
  });
  if (!rows.length) rows.push(el("p", "none", W.noGo));
  $("go-rows").replaceChildren(...rows);
}

// ---- L4 · parked (b4): rows carry nothing; open: «Вернуть», «Выбросить» (muted, no confirm, 5 s undo)

function renderLater(parked) {
  const live = parked.filter((it) => !gone(key(it.crew, it.id)));
  $("later").hidden = !parked.length;
  heading("later-h", W.later, parked.length);
  $("later-rows").replaceChildren(...live.map((it) => {
    const k = key(it.crew, it.id);
    const a = nowMs() - T(it.created);
    const old = a > 7 * 24 * HOUR;
    const words = el("div", "words");
    words.append(el("span", "tt", it.text));
    if (it.note) words.append(el("span", "gr", it.note));
    const acts = () => {
      const box = el("div", "ctl");
      box.append(button("q", W.back, () => act(it, "back", "", W.broughtBack)), button("q m", W.drop, () => act(it, "drop", "", W.dropped)));
      return box;
    };
    const hov = el("div");
    hov.append(...acts().childNodes);
    const panel = el("div");
    panel.append(detail([it.text, it.note]), acts());
    return rowShell(k, old ? "old" : "", words, el("span", "age" + (old ? "" : " m"), dur(a)), null, hov, panel);
  }));
}

// ---- L5 · waiting on others: one line per person, open on click

function renderOthers(others) {
  $("others").hidden = !others.length;
  heading("others-h", W.others, others.length);
  const by = new Map();
  for (const w of others) {
    const g = w.on || "";
    if (!by.has(g)) by.set(g, []);
    by.get(g).push(w);
  }
  const out = [];
  for (const [g, ws] of [...by].sort((a, b) => b[1].length - a[1].length)) {
    const isOpen = groups.has(g);
    const head = button("grp", "", () => { isOpen ? groups.delete(g) : groups.add(g); render(); });
    head.setAttribute("aria-expanded", isOpen ? "true" : "false");
    head.append(el("span", "chev", isOpen ? "▾" : "▸"));
    const words = el("span");
    const n = String(ws.length);
    const text = g ? W.waitOn(ws.length, g) : W.setAside(ws.length);
    words.append(el("b", "", n), text.slice(n.length));
    head.append(words);
    out.push(head);
    if (!isOpen) continue;
    const sub = el("div", "sub");
    for (const w of ws.sort((a, b) => T(a.updated) - T(b.updated))) {
      const row = el("div", "row");
      row.dataset.w = key(w.crew, w.id);
      tint(row, w.project);
      const words2 = el("div", "words");
      if (w.project) words2.append(el("span", "pj", w.project));
      words2.append(el("span", "tt", w.title), el("span", "gr", (w.milestone && w.milestone.text) || ""));
      words2.title = (w.milestone && w.milestone.text) || w.title;
      row.append(words2, el("span", "age m", ago(w.updated)), el("span"));
      sub.append(row);
    }
    out.push(sub);
  }
  $("others-rows").replaceChildren(...out);
}

// ---- L6 · the bottom line; the history drawer

function renderFoot() {
  const done = snap.crews.filter((c) => shows(c.key)).reduce((n, c) => n + (c.done_today || 0), 0);
  const line = el("span", "", W.doneToday);
  line.append(el("b", "", String(done)));
  const h = button("r", W.history, () => { $("history").hidden = !$("history").hidden; render(); });
  h.setAttribute("aria-expanded", $("history").hidden ? "false" : "true");
  $("foot").replaceChildren(line, h);
}

function taskChange(detail) {
  let state = "", note = "", on = "";
  let rest = detail;
  for (;;) {
    const i = rest.indexOf(", ");
    const head = i < 0 ? rest : rest.slice(0, i);
    if (["open", "waiting", "waiting-mako", "done", "dropped"].includes(head)) state = head;
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
      if (state === "waiting" || state === "waiting-mako") text = (on ? W.ev.waitingWho(on) : W.ev.waitingOn) + (note || title);
      else if (state === "done") text = W.ev.done + ": " + (note || title);
      else if (state === "dropped") text = W.ev.dropped + ": " + (note || title);
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
    case "start": text = W.ev.start; break;
    case "stop": text = W.ev.stop; break;
  }
  return text ? { ...e, text } : null;
}

function renderFeed() {
  $("history-h").textContent = W.historyH;
  const evs = snap.log.filter((e) => shows(e.crew));
  // An ask and its answer are one line: the answer.
  const answered = new Set(evs.filter((e) => e.event === "answered").map((e) => e.crew + "/" + ((e.detail || "").match(/^(A-\d+)/) || [])[1]));
  $("feed").replaceChildren(...evs.map((e) => feedLine(e, answered)).filter(Boolean).map((e) => {
    const row = el("div", "e");
    const t = el("time", "", hhmm(e.ts));
    t.title = new Date(e.ts).toLocaleString();
    const s = el("span");
    s.append(tint(el("span", "d"), e.project));
    const name = e.project || (multi() ? crewName(e.crew) : "");
    if (name) s.append(el("b", "", name), " · ");
    s.append(e.text);
    const by = e.role || e.by;
    if (by) s.append(el("i", "", " · " + who(e.crew, by)));
    row.append(t, s);
    return row;
  }));
}

$("history-close").addEventListener("click", () => { $("history").hidden = true; render(); });
document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  if (!$("history").hidden) { $("history").hidden = true; render(); } else if (openRow) {
    const k = openRow;
    openRow = "";
    openHand = "";
    render();
    document.querySelector(`[data-k="${CSS.escape(k)}"]`)?.focus();
  }
});

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

function connect() {
  const es = new EventSource("api/stream");
  es.addEventListener("state", (e) => {
    snap = JSON.parse(e.data);
    got = Date.now();
    snap.tints = snap.tints || {};
    if (first) setLang(snap.lang);
    $("mark").classList.remove("lost");
    render();
  });
  es.onerror = () => {
    $("mark").classList.add("lost");
    $("clock").lastElementChild.textContent = W.lost;
  };
}

(async () => {
  try {
    const r = await fetch("assets/mark.svg");
    if (r.ok) $("mark").innerHTML = await r.text(); // our own embedded asset, not shift data
  } catch (_) { /* the page works without the mark */ }
})();
connect();
