/* D-15: a task (T-n), parked item (L-n) or ask (A-n) named in a message is a link; it opens as a card — a bottom sheet
   on the phone, a popover on the desktop — that says in plain words what it is, and offers the one answer the owner
   can give and a conversation about it. Ids resolve on the crew the conversation is with (GET /api/items). */
(() => {
  "use strict";
  // An id, with an optional node/crew prefix that stays part of it; never inside a word, a path or a longer id.
  const ID = /(?<![\w/.-])(?:([A-Za-z0-9][\w.-]*)\/([A-Za-z0-9][\w.-]*)\/)?([TLA]-\d{1,7})(?![\w-])/g;
  const TTL = 30000, MAX = 200;
  const cache = new Map(); // key → {card, at} | {error, at}
  const waiting = new Set();
  let hooks = null, timer = 0, fetching = false;

  const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; };
  const keyOf = (dest, id) => (dest.node || "") + "|" + dest.crew + "|" + id;
  const phone = () => document.body.classList.contains("phone") || matchMedia("(max-width: 860px)").matches;
  const clock = new Intl.DateTimeFormat(undefined, {hour: "2-digit", minute: "2-digit"});
  const today = (d) => d.toDateString() === new Date().toDateString();
  const days = (d) => Math.max(1, Math.round((Date.now() - d.getTime()) / 86400000)) + " d";
  const when = (at) => { const d = new Date(at); return Number.isNaN(d.getTime()) || d.getFullYear() < 2000 ? "" : today(d) ? clock.format(d) : days(d); };

  // i2: the short title — the part before «: » in its first 24 characters dropped, cut at 34 on a word.
  function short(title) {
    let t = String(title || "").replace(/\s+/g, " ").trim();
    const c = t.indexOf(": ");
    if (c > 0 && c < 24) t = t.slice(c + 2);
    if (t.length <= 34) return t;
    const cut = t.slice(0, 34), sp = cut.lastIndexOf(" ");
    return (sp > 12 ? cut.slice(0, sp) : cut).replace(/[\s,.;:—-]+$/, "") + "…";
  }
  const rootTitle = (card) => card.kind === "ask" ? (card.ask.title || card.ask.text) : card.chain[0].title;
  const chainKey = (card) => card.kind === "ask" ? card.id : card.chain[0].id;

  // linkify turns the ids in an element's text into links: not inside code, pre, links or a link already made.
  function linkify(root, dest) {
    const walk = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {acceptNode: (n) =>
      n.parentElement.closest("code, pre, a, button, .xid, .xre") ? NodeFilter.FILTER_REJECT : (ID.lastIndex = 0, ID.test(n.data)) ? NodeFilter.FILTER_ACCEPT : NodeFilter.FILTER_REJECT});
    const nodes = [];
    while (walk.nextNode()) nodes.push(walk.currentNode);
    for (const n of nodes) {
      const frag = document.createDocumentFragment();
      let last = 0;
      n.data.replace(ID, (m, nodeName, crew, id, at) => {
        // A prefix is a node/crew only when the node is one the mesh knows (this one, or a peer). Anything else —
        // src/acme/T-7, a host's path, a peer's id before the node list is here — stays text: never this
        // conversation's id, whose card could answer another item. The page links again once the nodes are known.
        const node = nodeName ? hooks?.node(nodeName) : undefined;
        if (nodeName && node === undefined) return m;
        frag.append(n.data.slice(last, at));
        const b = el("button", "xid", m);
        b.type = "button";
        b.dataset.id = id;
        b.dataset.node = nodeName ? node : dest.node || "";
        b.dataset.crew = nodeName ? crew : dest.crew;
        b.setAttribute("aria-haspopup", "dialog");
        frag.append(b);
        last = at + m.length;
        return m;
      });
      frag.append(n.data.slice(last));
      n.replaceWith(frag);
    }
  }

  const destOf = (b) => ({node: b.dataset.node, crew: b.dataset.crew});
  function cached(dest, id) { const c = cache.get(keyOf(dest, id)); return c && c.card; }

  // want asks for the ids whose cards are missing or old, in one call per crew, a beat later.
  function want(dest, id) {
    const c = cache.get(keyOf(dest, id));
    if (c && Date.now() - c.at < TTL) return;
    waiting.add(JSON.stringify([dest.node || "", dest.crew, id]));
    if (!timer) timer = setTimeout(load, 60);
  }
  async function load() {
    timer = 0;
    if (fetching) { timer = setTimeout(load, 300); return; }
    const groups = new Map();
    for (const w of waiting) { const [node, crew, id] = JSON.parse(w); const k = node + "|" + crew; if (!groups.has(k)) groups.set(k, {node, crew, ids: []}); if (groups.get(k).ids.length < MAX) groups.get(k).ids.push(id); }
    waiting.clear();
    fetching = true;
    try {
      await Promise.all([...groups.values()].map(async (g) => {
        const at = Date.now();
        try {
          const r = await fetch("/api/items?" + new URLSearchParams({node: g.node, crew: g.crew, ids: g.ids.join(",")}), {cache: "no-store"});
          if (!r.ok) throw new Error((await r.text()).trim() || r.statusText);
          const data = await r.json();
          for (const id of g.ids) if (data.items?.[id]) cache.set(keyOf(g, id), {card: data.items[id], at});
        } catch (e) {
          for (const id of g.ids) { const old = cache.get(keyOf(g, id)); cache.set(keyOf(g, id), {card: old?.card, error: e.message, at}); }
        }
      }));
    } finally { fetching = false; }
    hooks?.redraw();
    if (open) fill();
  }

  // decorate links every id of the conversation and, in each run of the role's messages, gives the first mention
  // of a piece of work its short title (i2) and its dot (i3). The owner's bubbles get links only.
  function decorate(list, dest) {
    let seen = new Set();
    for (const article of list.querySelectorAll(".chat-message")) {
      const body = article.querySelector(".message-body");
      if (!body) continue;
      const mine = article.classList.contains("user");
      if (article.classList.contains("event")) continue;
      if (mine) seen = new Set();
      if (body._xids !== article._text) { linkify(body, dest); body._xids = article._text; }
      for (const b of body.querySelectorAll(".xid")) {
        const d = destOf(b), card = cached(d, b.dataset.id);
        want(d, b.dataset.id);
        const live = card && card.found && (card.state === "need" || card.state === "busy") ? card.state : "";
        b.classList.toggle("need", live === "need");
        b.classList.toggle("busy", live === "busy");
        if (mine) continue;
        let t = b.nextSibling && b.nextSibling.classList?.contains("xt") ? b.nextSibling : null;
        const k = card && card.found ? d.node + "|" + d.crew + "|" + chainKey(card) : "";
        const words = k && !seen.has(k) ? " · " + short(rootTitle(card)) : "";
        if (k) seen.add(k);
        if (!words) { t?.remove(); continue; }
        if (!t) { t = el("span", "xt"); b.after(t); }
        if (t.textContent !== words) t.textContent = words;
      }
    }
  }

  // ---- the card ----
  let open = null; // {b: the id's link, dest, id, pinned, box, shade}
  let showTimer = 0, hideTimer = 0, returning = false;
  const stateWord = {need: "Waiting on you", busy: "Working", queued: "Queued", parked: "Parked", done: "Done", dropped: "Dropped"};
  const kindWord = {task: "task", later: "parked", ask: "ask"};

  // split reads a note as what was done and what is left («… · left: …»).
  function split(text) {
    const m = /(^|[\s·;,.])left:\s*/i.exec(text || "");
    if (!m) return {latest: (text || "").trim(), left: ""};
    return {latest: text.slice(0, m.index).replace(/[\s·;,.]+$/, "").trim(), left: text.slice(m.index + m[0].length).trim()};
  }
  function row(label, text, cls) {
    const box = el("div", "xrow");
    const l = el("p", "xlab");
    if (Array.isArray(label)) l.append(label[0], el("span", "xtime", label[1])); else l.textContent = label;
    box.append(l, el("p", "xtext" + (cls ? " " + cls : ""), text));
    return box;
  }

  // fill draws the card (Loading… until its data is here) and puts a popover in its place.
  function fill() {
    if (!open) return;
    // A redraw replaces the card's children: the focus that was in it stays in it.
    const had = open.box.contains(document.activeElement);
    draw();
    if (had && !open.box.contains(document.activeElement)) open.box.focus({preventScroll: true});
    if (open && !open.sheet) place();
  }
  function draw() {
    const {box, dest, id} = open;
    const c = cache.get(keyOf(dest, id)), card = c && c.card;
    const out = [];
    if (open.sheet) out.push(el("div", "xgrab"));
    if (!card) {
      const t = el("h2", "xtitle", c && c.error ? "Could not read " + id : "Loading " + id + "…");
      t.id = "xcard-title"; out.push(t);
      if (c && c.error) out.push(el("p", "xtext", c.error));
      box.replaceChildren(...out);
      return;
    }
    const meta = el("p", "xmeta");
    meta.append(el("b", "", id));
    if (!card.found) {
      const t = el("h2", "xtitle", "Not found on this crew"); t.id = "xcard-title";
      out.push(meta, t);
      box.replaceChildren(...out);
      return;
    }
    const at = card.kind === "ask" ? null : card.chain[card.chain.findIndex((s) => s.id === id)] || card.chain[card.at];
    const head = card.kind === "ask" ? null : card.chain[card.chain.length - 1];
    const bits = [kindWord[id.startsWith("A-") ? "ask" : id.startsWith("L-") ? "later" : "task"]];
    if (card.kind === "ask") { if (card.ask.role) bits.push("from " + card.ask.role); }
    else if (at.kind === "task" && at.project) bits.push(at.project);
    const created = card.kind === "ask" ? card.ask.created : at.created, w = when(created);
    if (w) bits.push(/:/.test(w) ? "asked " + w : w);
    meta.append(" · " + bits.join(" · "));
    const title = el("h2", "xtitle", rootTitle(card)); title.id = "xcard-title";
    // c4: the state in a word, whom it is with and since when.
    const st = el("p", "xstate " + (card.state === "on" ? "on" : card.state));
    const who = [];
    if (card.state === "parked" && card.who) who.push("last with " + card.who);
    else if (card.state !== "on" && card.state !== "with" && card.who) who.push(card.who);
    if (card.overdue) who.push("overdue");
    else { const s = when(card.since); if (s) who.push((card.idle ? "idle " : "") + (/:/.test(s) ? "since " + s : s)); }
    const word = card.state === "on" ? "Waiting on " + card.who : card.state === "with" ? "With " + card.who : stateWord[card.state] || card.state;
    st.append(el("i"), el("b", "", word));
    if (who.length) st.append(el("span", "", " · " + who.join(" · ")));
    out.push(meta, title, st);
    // c2: Left, then Latest (an ask's Details).
    if (card.kind === "ask") {
      if (card.ask.title && card.ask.text) out.push(row("Details", card.ask.text));
      else if (card.ask.text && card.ask.text !== rootTitle(card)) out.push(row("Details", card.ask.text));
    } else {
      const source = head.kind === "later" ? head.title : head.note || head.result || "";
      const {latest, left} = split(source);
      if (left) out.push(row("Left", left, card.state === "need" ? "need" : ""));
      if (latest) { const t = when(head.updated); out.push(row(t ? ["Latest", " · " + t] : "Latest", latest)); }
    }
    // c3: the trail, when the work has more than one step.
    if (card.kind !== "ask" && card.chain.length > 1) {
      const trail = el("div", "xrow");
      trail.append(el("p", "xlab", "Path · " + card.chain.length + " steps"));
      const ids = el("p", "xpath");
      card.chain.forEach((s, i) => {
        if (i) ids.append(el("span", "xarrow", " → "));
        const b = el("button", "xstep" + (s.id === id ? " at" : "") + (i === card.chain.length - 1 ? " head" : ""), s.id);
        b.type = "button";
        b.addEventListener("click", () => { cache.set(keyOf(dest, s.id), {card: {...card, at: i}, at: cache.get(keyOf(dest, id))?.at || Date.now()}); open.id = s.id; fill(); });
        ids.append(b);
        if (i === card.chain.length - 1) ids.append(el("span", "xnow", " now"));
      });
      trail.append(ids);
      out.push(trail);
    }
    out.push(actions(card, dest));
    box.replaceChildren(...out);
  }

  function actions(card, dest) {
    const bar = el("div", "xacts");
    const talkRole = card.talk;
    let main = null;
    const sentLine = (text) => { const p = el("p", "xsent"); p.append(el("i"), text); return p; };
    const act = (button, body) => {
      button.addEventListener("click", async () => {
        if (button.disabled) return;
        button.disabled = true;
        bar.querySelector(".xfail")?.remove();
        try {
          await hooks.post("/api/todo", {crew: dest.node ? dest.node + "/" + dest.crew : dest.crew, ...body});
          const words = "Sent · " + clock.format(new Date());
          // The line stays while this card is open, through the redraw the ledger's new state brings.
          if (open) open.sent = words;
          const focused = button === document.activeElement;
          button.replaceWith(sentLine(words));
          if (focused && open) open.box.focus({preventScroll: true});
          // What the ledger says now.
          for (const k of [...cache.keys()]) if (k.startsWith((dest.node || "") + "|" + dest.crew + "|")) cache.get(k).at = 0;
          want(dest, open?.id || card.id);
        } catch (e) {
          button.disabled = false;
          const f = el("p", "xfail", "Not sent · retry");
          f.title = e.message;
          button.after(f);
        }
      });
    };
    if (open.sent) main = sentLine(open.sent);
    else if (card.state === "need" && card.decision) {
      // D-15 amendment b: the word starts with a capital.
      const d = card.decision, cap = d.word.charAt(0).toUpperCase() + d.word.slice(1), word = cap.length > 28 ? cap.slice(0, 27).trimEnd() + "…" : cap;
      main = el("button", "xdo", word);
      main.type = "button";
      main.title = d.word;
      act(main, {id: d.id, action: d.action, text: d.text || ""});
    } else if (card.state === "parked" && card.back) {
      main = el("button", "xback", "Bring back");
      main.type = "button";
      act(main, {id: card.back, action: "back", text: ""});
    }
    const talk = talkRole ? el("button", "xtalk", "Talk to " + talkRole) : null;
    if (talk) {
      talk.type = "button";
      talk.addEventListener("click", () => {
        const item = {id: card.kind === "ask" ? card.id : open.id, title: rootTitle(card)};
        close(false);
        hooks.talk({node: dest.node || "", crew: dest.crew, role: talkRole}, item);
      });
    }
    // Phone: the answer first, Talk last; the popover: Talk on the left, the answer on the right.
    if (open.sheet) bar.append(...[main, talk].filter(Boolean));
    else { if (talk) bar.append(talk); bar.append(el("span", "sp")); if (main) bar.append(main); }
    return bar;
  }

  // place puts the popover under its id, above it when it does not fit, inside the window.
  function place() {
    if (!open || open.sheet) return;
    const r = open.b.getBoundingClientRect(), box = open.box, w = Math.min(360, innerWidth - 16);
    box.style.width = w + "px";
    const left = Math.max(8, Math.min(r.left, innerWidth - w - 8));
    const h = box.offsetHeight;
    let top = r.bottom + 8;
    if (top + h > innerHeight - 8 && r.top - 8 - h >= 8) top = r.top - 8 - h;
    box.style.left = left + "px";
    box.style.top = Math.max(8, top) + "px";
  }

  function show(b, pinned) {
    clearTimeout(showTimer); clearTimeout(hideTimer);
    const dest = destOf(b), id = b.dataset.id;
    // A card open from hover or focus that gets pinned (Enter, a click) takes the focus, so its buttons are next.
    if (open && open.b === b) { if (pinned && !open.pinned) { open.pinned = true; open.box.focus({preventScroll: true}); } return; }
    close(false);
    const sheet = phone();
    const box = el("section", sheet ? "xsheet" : "xpop");
    box.setAttribute("role", "dialog");
    box.setAttribute("aria-labelledby", "xcard-title");
    box.tabIndex = -1;
    let shade = null;
    if (sheet) {
      shade = el("div", "xshade");
      shade.addEventListener("click", () => close(true));
      document.body.append(shade);
      swipe(box);
    } else {
      box.addEventListener("pointerenter", () => clearTimeout(hideTimer));
      box.addEventListener("pointerleave", () => { if (!open?.pinned) hideSoon(); });
    }
    document.body.append(box);
    open = {b, dest, id, pinned: pinned || sheet, box, shade, sheet};
    b.classList.add("on");
    b.setAttribute("aria-expanded", "true");
    want(dest, id);
    fill();
    if (sheet || pinned) box.focus({preventScroll: true});
  }
  function hideSoon() { clearTimeout(hideTimer); hideTimer = setTimeout(() => close(false), 200); }
  function close(back) {
    clearTimeout(showTimer); clearTimeout(hideTimer);
    if (!open) return;
    const {b, box, shade} = open;
    open = null;
    box.remove(); shade?.remove();
    b.classList.remove("on");
    b.setAttribute("aria-expanded", "false");
    // The focus goes back to the id without opening its card again.
    if (back && b.isConnected) { returning = true; b.focus({preventScroll: true}); returning = false; }
  }
  // c1: the sheet goes down with a swipe from its top.
  function swipe(box) {
    let y0 = null, dy = 0;
    box.addEventListener("touchstart", (e) => { if (box.scrollTop > 0) return; y0 = e.touches[0].clientY; dy = 0; box.style.transition = "none"; }, {passive: true});
    box.addEventListener("touchmove", (e) => { if (y0 == null) return; dy = Math.max(0, e.touches[0].clientY - y0); box.style.transform = dy ? "translateY(" + dy + "px)" : ""; }, {passive: true});
    box.addEventListener("touchend", () => { if (y0 == null) return; y0 = null; box.style.transition = ""; if (dy > 80) close(true); else box.style.transform = ""; });
  }

  function init(h, list, scroller) {
    hooks = h;
    // Desktop: hover 350 ms or focus opens; a click pins. Phone: a tap opens the sheet.
    list.addEventListener("click", (e) => {
      const b = e.target.closest?.(".xid");
      if (!b) return;
      e.preventDefault(); e.stopPropagation();
      if (open && open.b === b && open.pinned && !open.sheet) { close(true); return; }
      show(b, true);
    });
    list.addEventListener("pointerover", (e) => {
      if (e.pointerType !== "mouse" || phone()) return;
      const b = e.target.closest?.(".xid");
      if (!b) return;
      clearTimeout(hideTimer);
      if (open && open.b === b) return;
      clearTimeout(showTimer);
      showTimer = setTimeout(() => show(b, false), 350);
    });
    list.addEventListener("pointerout", (e) => {
      if (e.pointerType !== "mouse") return;
      const b = e.target.closest?.(".xid");
      if (!b || b.contains(e.relatedTarget)) return;
      clearTimeout(showTimer);
      if (open && open.b === b && !open.pinned) hideSoon();
    });
    list.addEventListener("focusin", (e) => {
      const b = e.target.closest?.(".xid");
      if (b && !returning && !phone() && b.matches(":focus-visible")) show(b, false);
    });
    document.addEventListener("keydown", (e) => {
      if (e.key === "Escape" && open) { e.preventDefault(); e.stopPropagation(); close(true); }
    }, true);
    document.addEventListener("pointerdown", (e) => {
      if (open && !open.sheet && !open.box.contains(e.target) && e.target !== open.b) close(false);
    });
    scroller.addEventListener("scroll", () => { if (open && !open.sheet) { if (open.b.isConnected) place(); else close(false); } }, {passive: true});
    addEventListener("resize", () => place());
  }

  // reply reads an owner's message that went out about an item: «Re <id> (<title>): <text>».
  function reply(text) {
    const m = /^Re ([TLA]-\d{1,7}) \((.*?)\):[ \n]/s.exec(text || "");
    return m ? {id: m[1], title: m[2], text: text.slice(m[0].length)} : null;
  }

  window.zminaIds = Object.freeze({init, decorate, linkify, short, reply});
})();
