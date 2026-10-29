// zmina ui — renders the shift's snapshot from /api/stream. Read-only except the todo, which writes notes to the
// dispatcher (POST /api/todo). Every text from the shift goes in with textContent, never as HTML.
"use strict";

const $ = (id) => document.getElementById(id);
const svgs = {};
let snap = null;
let lastTick = "";
let openRole = "";
let peekTimer = 0;
const pending = new Map(); // todo id -> {timer, action, text}
const nowSeen = new Map(); // role -> now text last rendered
const msSeen = new Map(); // role -> milestone text last rendered
const todoSeen = new Set();
let firstRender = true;
let filter = localStorage.getItem("zmina.crew") || "all";
const crewOpen = new Set();
const tickSeen = new Map(); // crew -> last tick rendered
const shows = (crew) => filter === "all" || filter === crew;

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function icon(name, cls) {
  const s = el("span", cls || "icon");
  s.setAttribute("aria-hidden", "true");
  if (svgs[name]) s.innerHTML = svgs[name]; // our own embedded assets, not shift data
  return s;
}

async function loadSVG(name, path) {
  try {
    const r = await fetch(path);
    if (r.ok) svgs[name] = await r.text();
  } catch (_) { /* the page still works without art */ }
}

function dur(ms) {
  const m = Math.max(0, Math.floor(ms / 60000));
  if (m < 1) return "<1m";
  if (m < 60) return m + "m";
  if (m < 48 * 60) return Math.floor(m / 60) + "h";
  return Math.floor(m / 1440) + "d";
}
const since = (t) => (t && !t.startsWith("0001") ? dur(Date.parse(snap.now) - Date.parse(t)) : "");
const hhmm = (t) => new Date(t).toTimeString().slice(0, 5);
const hms = (t) => new Date(t).toTimeString().slice(0, 8);
const tokens = (n) => (n >= 1e6 ? (n / 1e6).toFixed(n % 1e6 ? 1 : 0) + "m" : Math.round(n / 1000) + "k");

const tickerDead = (c) => !c.last_tick || Date.parse(snap.now) - Date.parse(c.last_tick) > 2 * 60000;
const crewName = (key) => (snap.crews.find((c) => c.key === key) || { name: key }).name;
const multi = () => snap.crews.length > 1;

function tag(key) {
  return el("span", "tag", crewName(key));
}

// classify is the row a role gets: need (prompt, stuck), busy, idle, outside, danger, down.
function classify(r) {
  const always = r.keep === "always";
  if (r.broken || ((r.state === "down" || r.state === "dead") && always)) return "danger";
  if (r.state === "outside") return "outside";
  if (r.state !== "up") return "down";
  if (r.prompt || r.stuck) return "need";
  if (r.status === "busy") return "busy";
  return "idle";
}
const order = { need: 0, busy: 1, idle: 2, outside: 3, danger: 4, down: 5 };

function stateWord(r, c) {
  const t = since(r.since);
  switch (c) {
    case "need": return r.prompt ? ["■", "prompt", "need"] : ["■", "stuck " + t, "need"];
    case "busy": return ["●", "busy " + t, "busy"];
    case "idle": return ["○", "idle " + (t || ""), "muted"];
    case "outside": return ["◌", "outside", "muted"];
    case "danger": return ["✕", r.broken ? "broken" : r.state, "danger"];
    default: return ["—", r.state, "faint"];
  }
}

function render() {
  if (!snap) return;
  if (filter !== "all" && !snap.crews.some((c) => c.key === filter)) filter = "all";
  const crews = snap.crews.map((c) => {
    const roles = c.roles.map((r) => ({ ...r, cls: classify(r) }));
    const count = (k) => roles.filter((r) => r.cls === k).length;
    return { ...c, roles, need: count("need"), busy: count("busy"), idle: count("idle"), dead: tickerDead(c) };
  });
  const seen = crews.filter((c) => shows(c.key));
  const sum = (k) => seen.reduce((n, c) => n + c[k], 0);
  const todo = snap.todo.filter((it) => shows(it.crew));
  const waiting = todo.length + sum("need");
  const stopped = crews.filter((c) => c.dead || c.stopped);

  renderChips();
  renderHeader(waiting, sum("busy"), sum("idle"), stopped);
  renderTodo(todo);
  renderCrews(seen);
  renderTasks();
  renderLog();
  foldLabels();
  $("version").textContent = "zmina " + snap.version;
  const lead = stopped.map((c) => c.name + " stopped").join(" · ");
  document.title = ["zmina", lead, waiting ? waiting + " waiting on you" : ""].filter(Boolean).join(" · ");
  favicon(stopped.length ? "#ff7b72" : waiting > 0 ? "#f0b45a" : "");
  firstRender = false;
}

function renderChips() {
  const nav = $("chips");
  nav.hidden = !multi();
  if (!multi()) return;
  nav.replaceChildren(...[["all", "All"], ...snap.crews.map((c) => [c.key, c.name])].map(([key, label]) => {
    const b = el("button", "chip", label);
    b.type = "button";
    b.setAttribute("aria-pressed", filter === key ? "true" : "false");
    b.onclick = () => { filter = key; localStorage.setItem("zmina.crew", key); render(); };
    return b;
  }));
}

function renderHeader(waiting, busy, idle, stopped) {
  // One mark for every crew: it redraws on a beat from any of them.
  const ticks = snap.crews.map((c) => c.last_tick || "").sort();
  const newest = ticks[ticks.length - 1] || "";
  $("tick").textContent = newest ? "ticker " + hms(newest) : "ticker —";
  if (newest && newest !== lastTick && lastTick) beat();
  lastTick = newest;

  const alarm = $("alarm");
  alarm.hidden = !snap.crews.length ? false : true;
  if (!snap.crews.length) alarm.textContent = "No crew under ~/.zmina — zmina init";

  const sum = $("summary");
  sum.replaceChildren();
  const parts = [[busy, "working", "crews"], [waiting, "waiting on you", "todo-sec"], [idle, "idle", "crews"]];
  parts.forEach(([n, word, target], i) => {
    if (i) sum.append(" · ");
    const a = el("a", "", n + " " + word);
    a.href = "#" + target;
    sum.append(a);
  });
  for (const c of stopped) {
    sum.append(" · ", el("span", "danger", c.name + ": " + (c.stopped ? "shift stopped" : "ticker stopped")));
  }
}

function beat() {
  const m = $("mark");
  m.classList.remove("beat");
  void m.offsetWidth;
  m.classList.add("beat");
}

function blink() {
  const m = $("mark");
  m.classList.add("event");
  setTimeout(() => m.classList.remove("event"), 300);
}

function favicon(color) {
  const z = '<path d="M5.6 7.6C5.4 3.9 18.6 3.7 18.1 7.4L6.2 16.8C5.4 20.4 18.6 20.2 18.4 16.4" fill="none" stroke="%23777" stroke-width="2.4" stroke-linecap="round"/>';
  const d = color ? '<circle cx="19" cy="5" r="5" fill="' + encodeURIComponent(color) + '"/>' : "";
  $("favicon").href = "data:image/svg+xml," + '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24">' + z + d + "</svg>";
}

// ---- todo

function renderTodo(items) {
  const list = $("todo");
  const keep = new Set();
  const kinds = { decision: "◆", check: "☐", waiting: "■" };

  for (const it of items) {
    const key = it.crew + "/" + it.id;
    keep.add(key);
    let li = list.querySelector(`[data-id="${CSS.escape(key)}"]`);
    if (!li) {
      li = el("li", "todo-item");
      li.dataset.id = key;
      if (!firstRender && !todoSeen.has(key)) li.classList.add("entering", "fresh");
      todoSeen.add(key);
      li.append(el("div", "in"));
    }
    li.classList.toggle("blocking", !!it.blocks);
    if (!li.dataset.busy) fillTodo(li.firstChild, it, kinds[it.kind] || "◆");
    list.append(li); // keeps the server's order
  }
  for (const li of [...list.children]) {
    if (!keep.has(li.dataset.id) && !li.dataset.busy) li.remove();
  }

  const empty = !list.children.length;
  $("empty").hidden = !empty;
  if (empty) {
    const ans = snap.log.find((e) => e.event === "answered");
    $("last-answer").textContent = ans ? "last answer " + hhmm(ans.ts) : "";
  }

  const parked = snap.later.filter((it) => shows(it.crew));
  const toggle = $("later-toggle");
  toggle.hidden = !parked.length;
  toggle.textContent = (toggle.getAttribute("aria-expanded") === "true" ? "▾ " : "▸ ") + "Later · " + parked.length;
  const later = $("later");
  later.replaceChildren(...parked.map((it) => {
    const li = el("li");
    li.append(el("div", "", it.text));
    if (it.note) li.append(el("div", "muted", it.note));
    const meta = el("div", "meta mono");
    if (multi()) meta.append(tag(it.crew), " · ");
    meta.append(it.id + " · " + since(it.created));
    meta.style.marginLeft = "0";
    li.append(meta);
    const acts = el("div", "actions");
    acts.style.marginLeft = "0";
    acts.append(button("Bring back", "", () => act(li, it, "back", "")), button("Drop", "", () => act(li, it, "drop", "")));
    li.append(acts);
    return li;
  }));
}

function button(label, cls, fn) {
  const b = el("button", "btn " + (cls || ""), label);
  b.type = "button";
  b.addEventListener("click", fn);
  return b;
}

function fillTodo(box, it, mark) {
  box.replaceChildren();
  const q = el("div", "q");
  q.append(el("span", "k", mark), el("span", "clip3", it.text));
  q.tabIndex = 0;
  q.setAttribute("role", "button");
  q.title = "Show all";
  const expand = () => box.parentElement.classList.toggle("expanded");
  q.onclick = expand;
  q.onkeydown = (e) => { if (e.key === "Enter") expand(); };
  box.append(q);
  if (it.rec) {
    const rec = el("div", "rec", "Recommended: " + it.rec);
    if (it.reason) rec.append(" ", el("span", "why", it.reason));
    box.append(rec);
  } else if (it.note) box.append(el("div", "rec why", it.note));
  box.querySelector(".rec")?.addEventListener("click", expand);

  const meta = el("div", "meta mono");
  if (multi()) meta.append(tag(it.crew), " · ");
  const bits = [it.role, it.task && it.task !== it.id ? it.task : it.id].filter(Boolean);
  meta.append(bits.join(" · ") + " · ");
  const ageEl = el("span", it.overdue ? "danger" : "", since(it.created));
  meta.append(ageEl);
  if (it.blocks) meta.append(" · ", el("span", "blocks", "blocks " + it.blocks));
  box.append(meta);

  const acts = el("div", "actions");
  const li = box.parentElement;
  const answer = () => openAnswer(li, it);
  if (it.kind === "decision") acts.append(button("Yes", "primary", () => act(li, it, "yes", "")), button("Answer…", "", answer));
  else if (it.kind === "check") acts.append(button("Done", "primary", () => act(li, it, "done", "")), button("Answer…", "", answer));
  else acts.append(button("Answer…", "primary", answer));
  acts.append(button("Later", "", () => act(li, it, "park", "")));
  box.append(acts);
}

function openAnswer(li, it) {
  li.dataset.busy = "1";
  const box = li.firstChild;
  box.querySelector(".actions").remove();
  const form = el("div", "answer");
  const ta = el("textarea");
  ta.rows = 3;
  ta.setAttribute("aria-label", "Answer to " + it.id);
  const acts = el("div", "actions");
  acts.style.margin = "0";
  const send = () => { if (ta.value.trim()) { form.remove(); act(li, it, "answer", ta.value.trim()); } };
  acts.append(button("Send", "primary", send), button("Cancel", "", () => { delete li.dataset.busy; form.remove(); render(); }));
  ta.addEventListener("keydown", (e) => { if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) send(); });
  form.append(ta, acts);
  box.append(form);
  ta.focus();
}

// act holds the answer five seconds (Undo), then sends it; the row leaves once the server took it.
function act(li, it, action, text) {
  const id = it.crew + "/" + it.id;
  li.dataset.busy = "1";
  const box = li.querySelector(".in") || li;
  box.querySelector(".actions")?.remove();
  box.querySelector(".error")?.remove();
  const sent = el("div", "sent", "Sent to dispatcher ·");
  const undo = el("button", "undo", "Undo");
  undo.type = "button";
  sent.append(undo, el("div", "bar5"));
  box.append(sent);
  const timer = setTimeout(async () => {
    pending.delete(id);
    try {
      const r = await fetch("api/todo", { method: "POST", headers: { "Content-Type": "application/json", "X-Zmina": "1" }, body: JSON.stringify({ crew: it.crew, id: it.id, action, text }) });
      if (!r.ok) throw new Error((await r.text()).trim());
      li.classList.add("leaving");
      setTimeout(() => { li.remove(); delete li.dataset.busy; }, 240);
    } catch (e) {
      sent.remove();
      box.append(el("div", "error", "Not sent: " + e.message));
      delete li.dataset.busy;
      setTimeout(render, 3000);
    }
  }, 5000);
  pending.set(id, timer);
  undo.addEventListener("click", () => { clearTimeout(timer); pending.delete(id); delete li.dataset.busy; sent.remove(); render(); });
}

// ---- crew

function renderCrews(crews) {
  const box = $("crews");
  const keep = new Set();
  for (const c of crews) {
    keep.add(c.key);
    let sec = box.querySelector(`[data-crew="${CSS.escape(c.key)}"]`);
    if (!sec) {
      sec = el("section", "crew");
      sec.dataset.crew = c.key;
      const h = el("div", "crew-h");
      const t = el("button", "fold phone-only");
      t.type = "button";
      t.onclick = () => { crewOpen.has(c.key) ? crewOpen.delete(c.key) : crewOpen.add(c.key); render(); };
      sec.append(h, t, el("ol", "rows"), el("p", "down-line"));
    }
    renderCrew(sec, c);
    box.append(sec);
  }
  for (const sec of [...box.children]) if (!keep.has(sec.dataset.crew)) sec.remove();
}

function renderCrew(sec, c) {
  sec.classList.toggle("dead", c.dead);
  sec.classList.toggle("open", crewOpen.has(c.key));
  const [h, toggle, list, downLine] = sec.children;

  h.replaceChildren();
  if (c.dead) {
    h.append(el("span", "cname", "✕ " + c.name + " · " + (c.last_tick ? "ticker stopped " + since(c.last_tick) + " ago" : "no ticker beat yet")));
  } else {
    h.append(el("span", "cname", c.name));
    const t = el("span", "ctick mono muted", "tick " + hms(c.last_tick));
    if (tickSeen.has(c.key) && tickSeen.get(c.key) !== c.last_tick) t.classList.add("fresh");
    h.append(t);
  }
  tickSeen.set(c.key, c.last_tick);
  if (c.stopped) h.append(el("span", "danger", "shift stopped — zmina start"));
  h.append(el("span", "csum", c.busy + " working · " + (c.waiting + c.need) + " waiting on you · " + c.idle + " idle"));
  if (c.error) h.append(el("span", "danger", c.error));

  const shown = c.roles.filter((r) => r.cls !== "down")
    .sort((a, b) => order[a.cls] - order[b.cls] || (Date.parse(a.since || snap.now) - Date.parse(b.since || snap.now)));
  const down = c.roles.filter((r) => r.cls === "down").map((r) => r.role);

  const before = new Map([...list.children].map((li) => [li.dataset.role, li.getBoundingClientRect().top]));
  const keep = new Set();
  for (const r of shown) {
    keep.add(r.role);
    let li = list.querySelector(`[data-role="${CSS.escape(r.role)}"]`);
    if (!li) { li = el("li"); li.dataset.role = r.role; }
    li.className = "r-" + r.cls;
    fillRole(li, r, c);
    list.append(li);
  }
  for (const li of [...list.children]) if (!keep.has(li.dataset.role)) li.remove();
  flip(list, before);

  downLine.textContent = down.length ? "down: " + down.join(", ") : "";
  toggle.textContent = (crewOpen.has(c.key) ? "▾ " : "▸ ") + c.busy + " busy · " + c.idle + " idle · " + down.length + " down";
}

// flip moves reordered rows from where they were (320 ms).
function flip(list, before) {
  if (matchMedia("(prefers-reduced-motion: reduce)").matches) return;
  for (const li of list.children) {
    const was = before.get(li.dataset.role);
    if (was === undefined) continue;
    const dy = was - li.getBoundingClientRect().top;
    if (!dy) continue;
    li.animate([{ transform: `translateY(${dy}px)` }, { transform: "none" }], { duration: 320, easing: "cubic-bezier(.2,.7,.2,1)" });
  }
}

function fillRole(li, r, c) {
  const id = c.key + "/" + r.role;
  const row = li.querySelector(".row") || el("button", "row");
  row.type = "button";
  row.onclick = () => toggleRole(id);
  row.setAttribute("aria-expanded", openRole === id ? "true" : "false");
  row.replaceChildren();

  const l1 = el("div", "line1");
  let [mk, word, color] = stateWord(r, r.cls);
  if (c.dead && r.state === "up") word = "last seen " + (c.last_tick ? hhmm(c.last_tick) : "—");
  const st = el("span", "state " + color);
  st.append(el("span", "", mk), el("span", "mono", word));
  l1.append(icon(r.icon), el("span", "name", r.role), st);
  if (r.tasks && r.tasks.length) l1.append(el("span", "mono muted grow", r.tasks.join(" ")));
  else l1.append(el("span", "grow"));
  if (r.context_max || r.context) {
    const m = el("span", "meter");
    const pct = r.context_max ? Math.min(100, (100 * r.context) / r.context_max) : 0;
    m.classList.toggle("hi", pct >= 75 && pct < 90);
    m.classList.toggle("max", pct >= 90);
    m.title = tokens(r.context || 0) + (r.context_max ? " / " + tokens(r.context_max) : "");
    const fill = el("i");
    fill.style.width = (r.context_max ? pct : 0) + "%";
    m.append(fill);
    l1.append(m);
  }
  row.append(l1);

  const task = r.tasks && r.tasks.length ? snap.tasks.find((t) => t.crew === c.key && t.id === r.tasks[0]) : null;
  const working = r.cls === "busy" || r.cls === "need";
  if (working && r.now) {
    const now = el("div", "now");
    const stale = r.now_at && Date.parse(snap.now) - Date.parse(r.now_at) >= 10 * 60000;
    now.classList.toggle("stale", !!stale);
    const txt = el("span", "txt clip2");
    if (stale) txt.append(el("span", "mono", since(r.now_at) + " ago · "));
    txt.append(r.now);
    now.append(txt);
    if (r.cls === "busy") {
      const w = el("span", "work");
      const seg = el("i");
      seg.style.animationDelay = -(Math.random() * 2.4).toFixed(2) + "s";
      w.append(seg);
      now.append(w);
    }
    if (nowSeen.has(id) && nowSeen.get(id) !== r.now) now.classList.add("changed");
    nowSeen.set(id, r.now);
    row.append(now);
  }
  if (working) {
    if (task) row.append(el("div", "sub clip", task.asked));
    else if (r.about && !r.now) row.append(el("div", "sub clip", r.about));
  }
  if (r.milestone) {
    const ms = el("div", r.cls === "idle" ? "sub clip" : "ms clip");
    if (r.cls === "idle") ms.textContent = "idle · last: " + r.milestone.text;
    else {
      ms.append(el("span", "mono ok", "✓ " + hhmm(r.milestone.at)), r.milestone.text);
      if (msSeen.has(id) && msSeen.get(id) !== r.milestone.text) ms.classList.add("arrived");
    }
    msSeen.set(id, r.milestone.text);
    row.append(ms);
  } else if (r.cls === "idle" || r.cls === "outside") {
    row.append(el("div", "sub clip", r.about || ""));
  }
  if (r.problem) row.append(el("div", "sub clip danger", r.problem));
  if (!li.contains(row)) li.prepend(row);

  let pv = li.querySelector(".preview");
  if (openRole === id && !pv) {
    pv = el("div", "preview");
    const pre = el("pre", "", "…");
    const at = el("div", "attach");
    const cmd = "zmina " + (c.key === "default" ? "" : "--config " + c.key + " ") + "attach " + r.role;
    at.append(el("span", "mono", cmd), button("Copy", "", () => navigator.clipboard?.writeText(cmd)));
    pv.append(pre, at);
    li.append(pv);
    peek(id, pre);
  } else if (openRole !== id && pv) pv.remove();
}

function toggleRole(id) {
  openRole = openRole === id ? "" : id;
  clearInterval(peekTimer);
  render();
  if (openRole) {
    peekTimer = setInterval(() => {
      const [crew, role] = openRole.split("/");
      const pre = document.querySelector(`[data-crew="${CSS.escape(crew)}"] [data-role="${CSS.escape(role)}"] .preview pre`);
      if (pre) peek(openRole, pre);
    }, 2000);
  }
}

// peek shows a role's screen; id is crew/role.
async function peek(id, pre) {
  const [crew, role] = id.split("/");
  try {
    const r = await fetch("api/peek?crew=" + encodeURIComponent(crew) + "&role=" + encodeURIComponent(role));
    const text = await r.text();
    const atEnd = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 4;
    pre.textContent = text.replace(/\s+$/, "");
    if (atEnd) pre.scrollTop = pre.scrollHeight;
  } catch (_) { /* next refresh */ }
}

// ---- tasks, log

let openTask = "";

function renderTasks() {
  const list = $("tasks");
  const tasks = snap.tasks.filter((t) => shows(t.crew));
  list.replaceChildren(...tasks.map((t) => {
    const key = t.crew + "/" + t.id;
    const li = el("li", "task");
    const row = el("button", "row");
    row.type = "button";
    row.setAttribute("aria-expanded", openTask === key ? "true" : "false");
    if (multi()) row.append(tag(t.crew));
    row.append(el("span", "mono", t.id), el("span", "muted", t.role), el("span", t.state === "waiting" ? "need" : "muted", t.state),
      el("span", "mono muted", since(t.created)), el("span", "clip", t.asked));
    row.onclick = () => { openTask = openTask === key ? "" : key; renderTasks(); };
    li.append(row);
    if (openTask === key) taskDetail(li, t.crew, t.id);
    return li;
  }));
  if (!tasks.length) list.append(el("li", "muted row empty-row", "No open tasks."));
}

async function taskDetail(li, crew, id) {
  const box = el("div", "detail", "…");
  li.append(box);
  try {
    const r = await fetch("api/task?crew=" + encodeURIComponent(crew) + "&id=" + encodeURIComponent(id));
    const d = await r.json();
    box.replaceChildren(el("div", "", d.task.asked));
    if (d.task.note) box.append(el("div", "", "note: " + d.task.note));
    if (d.task.worktree) box.append(el("div", "mono", d.task.worktree));
    const ol = el("ol");
    for (const e of d.log.slice().reverse()) ol.append(el("li", "mono", hhmm(e.ts) + " " + (e.by || e.role || "") + " " + e.event + (e.detail ? ": " + e.detail : "")));
    box.append(ol);
  } catch (e) { box.textContent = String(e); }
}

// foldLabels: on a phone, Tasks and Log are 44 px rows that open — «▸ Tasks · 10».
function foldLabels() {
  const phone = matchMedia("(max-width: 599px)").matches;
  for (const b of document.querySelectorAll(".fold-h")) {
    const n = b.closest("section").querySelector("ol").querySelectorAll(":scope > li:not(.empty-row)").length;
    const open = b.getAttribute("aria-expanded") === "true";
    b.textContent = phone ? (open ? "▾ " : "▸ ") + b.dataset.label + " · " + n : b.dataset.label;
  }
}

function renderLog() {
  $("log").replaceChildren(...snap.log.filter((e) => shows(e.crew) && e.event !== "watch-run").slice(0, 20).map((e) => {
    const li = el("li");
    const row = el("div", "row");
    if (multi()) row.append(tag(e.crew));
    row.append(el("span", "mono muted", hhmm(e.ts)), el("span", "", e.role || e.by || ""), el("span", "muted", e.event),
      el("span", "clip muted", [e.task, e.detail].filter(Boolean).join(" · ")));
    li.append(row);
    return li;
  }));
}

// ---- keys, folds, stream

document.addEventListener("keydown", (e) => {
  if (e.target.closest("textarea, input")) return;
  if (e.key === "Escape" && openRole) toggleRole(openRole);
  if (e.key === "j" || e.key === "k") {
    const rows = [...document.querySelectorAll("button.row")];
    const i = rows.indexOf(document.activeElement);
    const next = rows[Math.max(0, Math.min(rows.length - 1, i + (e.key === "j" ? 1 : -1)))];
    next?.focus();
  }
});

$("later-toggle").addEventListener("click", (e) => {
  const open = e.currentTarget.getAttribute("aria-expanded") !== "true";
  e.currentTarget.setAttribute("aria-expanded", open);
  $("later").hidden = !open;
  render();
});
for (const b of document.querySelectorAll(".fold-h")) {
  const sec = b.closest("section");
  if (matchMedia("(max-width: 599px)").matches) { sec.classList.add("folded"); b.setAttribute("aria-expanded", "false"); }
  b.addEventListener("click", () => { b.setAttribute("aria-expanded", !sec.classList.toggle("folded")); foldLabels(); });
}
document.addEventListener("visibilitychange", () => document.body.classList.toggle("hidden", document.hidden));

function connect() {
  const es = new EventSource("api/stream");
  es.addEventListener("state", (e) => {
    snap = JSON.parse(e.data);
    $("mark").classList.remove("lost");
    $("live").textContent = "";
    blink();
    render();
  });
  es.onerror = () => {
    $("mark").classList.add("lost");
    $("live").textContent = "reconnecting…";
  };
}

(async () => {
  await Promise.all([
    loadSVG("mark", "assets/mark.svg"), loadSVG("cup", "assets/empty-cup.svg"),
    ...["dispatcher", "designer", "developer", "director", "reviewer", "generic"].map((n) => loadSVG(n, "assets/roles/" + n + ".svg")),
  ]);
  $("mark").innerHTML = svgs.mark || "";
  $("cup").innerHTML = svgs.cup || "";
  connect();
})();
