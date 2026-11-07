/* Structured chat uses zmina's owner queue. Terminal bytes and native UI redraws stay in Terminal. */
(() => {
  "use strict";
  const $ = id => document.getElementById(id);
  const node = $("node"), target = $("target"), settings = $("session-settings"), input = $("message");
  const conversation = $("conversation"), messages = $("messages"), send = $("send-message"), mic = $("dictate");
  const params = new URLSearchParams(location.search), language = $("speech-language");
  // Inside the cockpit's talk panel: the panel is the page's way back, not the header's link.
  document.body.classList.toggle("embed", params.get("embed") === "1");
  // Embedded in the cockpit's panel, Esc closes the panel (the parent page), unless a picker or dictation has it.
  if (params.get("embed") === "1" && window.parent !== window) {
    document.addEventListener("keydown", (e) => {
      if (e.key === "Escape" && !e.defaultPrevented) window.parent.postMessage("zmina:close", location.origin);
    });
    // D-11: on the phone the Talk tab reads like the Claude iOS app; the cockpit's width says which it is (this frame
    // may be narrow on the desktop too).
    // D-14 a: on the phone the working line leaves the conversation for the state line, beside Stop.
    const phone = () => {
      let on = false;
      try { on = window.parent.matchMedia("(max-width: 860px)").matches; } catch (_) { /* a frame of another page */ }
      // The phone's field has its own font and padding: a switch sizes it again.
      if (document.body.classList.contains("phone") !== on) { document.body.classList.toggle("phone", on); fitField(); }
      const w = $("chat-working");
      if (on && w.parentElement !== $("stop-turn").parentElement) $("start-role").before(w);
      else if (!on && w.parentElement !== $("conversation")) $("empty-chat").before(w);
    };
    phone();
    try { window.parent.addEventListener("resize", phone); } catch (_) { /* as above */ }
    // A field here with the keyboard up: the cockpit's tab bar steps aside (D-09 k2). The parent cannot see a focus
    // inside this frame on iOS, so it is told.
    const typing = (on) => { document.body.classList.toggle("typing", on); window.parent.postMessage(on ? "zmina:typing" : "zmina:typed", location.origin); };
    document.addEventListener("focusin", (e) => { if (e.target.matches?.("textarea, input:not([type=file])")) typing(true); });
    document.addEventListener("focusout", (e) => { if (e.target.matches?.("textarea, input:not([type=file])")) typing(false); });
    // D-11 m1: on the phone an older reply's Copy is folded away; a tap on the reply (not on a link, a button or a
    // selection) unfolds it, a second tap folds it again.
    messages.addEventListener("click", (e) => {
      const a = e.target.closest?.(".chat-message.assistant");
      if (!a || !document.body.classList.contains("phone") || e.target.closest("a, button, summary, pre") || String(getSelection()).trim()) return;
      a._metaOn = !a._metaOn;
      a.classList.toggle("meta-on", a._metaOn);
    });
  }
  // Embedded, the conversation wears the cockpit's own tokens (they change with its width and scheme), so the panel's
  // head and the conversation are one surface.
  if (params.get("embed") === "1" && window.parent !== window) {
    const names = ["--bg", "--panel", "--panel2", "--line", "--line2", "--ink", "--ink2", "--muted", "--faint", "--off", "--busy", "--need", "--late", "--ok"];
    const wear = () => { try { const from = window.parent.getComputedStyle(window.parent.document.documentElement); for (const n of names) { const v = from.getPropertyValue(n).trim(); if (v) document.documentElement.style.setProperty(n, v); } } catch (_) { /* not the cockpit */ } };
    wear(); try { window.parent.addEventListener("resize", wear); window.parent.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", wear); } catch (_) { /* not the cockpit */ }
  }
  const Recognition = window.SpeechRecognition || window.webkitSpeechRecognition;
  let targets = [], destination = null, snapshot = null, generation = 0, pollTimer = null, readRequest = null;
  let loadingTargets = false, posting = false, draftID = "", cleared = null, recognition = null, listening = false, firstPaint = true;
  const articles = new Map();
  const fileInput=$("file-input"), attach=$("attach-file"), draftFiles=$("draft-files");
  let selectedFiles=[], filesRevision=0;
  // D-15 h1: the item a message is about, shown as a chip above the field; it goes as «Re <id> (<title>):».
  let replyTo = null;
  // The mesh's nodes by name (this node's is ""): an id's node/crew prefix names one of them, else it is text (D-15 i1).
  let knownNodes = new Map();
  const fileSignature=files=>JSON.stringify((files||[]).map(file=>[file.id,file.name,file.size]));
  // D-18 f1–f3: the picked files as tiles in one row above the field, each with its ✕.
  function renderSelected() {
    window.zminaFiles.draft(draftFiles, selectedFiles, async(file)=>{try{const version=generation,dest={...destination},next=selectedFiles.filter(item=>item.id!==file.id);await store.saveFiles(dest,next);if(version!==generation)return;selectedFiles=next;filesRevision++;cleared=null;draftID="";renderSelected();controls();}catch(e){error("Could not save the file selection. "+e.message);}});
  }
  attach.addEventListener("click",()=>fileInput.click());
  fileInput.addEventListener("change",async()=>{
    const chosen=[...fileInput.files];fileInput.value="";
    if(!destination||!chosen.length)return;
    const dest={...destination}, version=generation;
    if(selectedFiles.length+chosen.length>4||chosen.some(file=>file.size>8*1024*1024)){error("Attach up to four files, each at most 8 MiB.");return;}
    const next=[...selectedFiles,...chosen.map(file=>({id:uid(),name:file.name,size:file.size,type:file.type,blob:file}))];
    try{await store.saveFiles(dest,next);if(version!==generation)return;selectedFiles=next;filesRevision++;cleared=null;draftID="";renderSelected();controls();error("");}catch(e){error("Could not save these files. Choose them again. "+e.message);}
  });
  function fileData(blob) {return new Promise((resolve,reject)=>{const reader=new FileReader();reader.onload=()=>resolve(reader.result.split(",")[1]);reader.onerror=()=>reject(reader.error);reader.onabort=()=>reject(new Error("File reading stopped."));reader.readAsDataURL(blob);});}
  const store = window.zminaChatStore;
  let outbox = [], draining = false, retryTimer = null, live = false;
  // The outbox entry this tab is uploading or sending now (name: the file going up), so its bubble says it goes.
  let inflight = null;
  const OFFLINE_NOTE = "Saved · sends when you’re back online";
  // outboxState is how a saved message stands for its bubble: going up or out now; a fresh one goes at once when
  // the network is there and nothing before it to the same role is stuck (else it waits, and says so).
  function outboxState(entry) {
    if (inflight?.key === entry.key) return inflight.name ? "uploading" : "transmitting";
    if (entry.state !== "local" || entry.attempts || navigator.onLine === false) return entry.state;
    const scope = store.destinationKey(entry.dest);
    const stuck = outbox.some((other) => other !== entry && store.destinationKey(other.dest) === scope && other.at <= entry.at && (other.state !== "local" || other.attempts));
    return stuck ? entry.state : "transmitting";
  }
  const sameDestination = dest => destination && store.destinationKey(dest) === store.destinationKey(destination);
  const deadline = 6 * 24 * 60 * 60 * 1000;
  async function refreshOutbox() { outbox = await store.list(); render(); }
  // a terminal escape never reaches the page as boxes and brackets, whichever node sent the text.
  const ansi = /\x1b(?:\[[0-?]*[ -\/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)?|[@-Z\\-_])|\x1b/g;
  const plain = (text) => typeof text === "string" && text.includes("\x1b") ? text.replace(ansi, "") : text;
  function applySnapshot(data) {
    for (const row of data.messages || []) {
      row.text = plain(row.text);
      if (row.event) for (const k of ["summary", "result", "detail"]) row.event[k] = plain(row.event[k]);
    }
    const previous = snapshot;
    if (data.cursor && !data.reset && previous?.cursor && previous.session === data.session) {
      const rows = new Map(previous.messages.map(row => [row.id, row]));
      for (const row of data.messages || []) rows.set(row.id, row);
      data.messages = [...rows.values()];
    }
    // This is a recent browser cache, not an unbounded transcript archive.
    let size = 0;
    data.messages = (data.messages || []).slice(-1000).reverse().filter(row => { size += row.text.length; return size <= 4 * 1024 * 1024; }).reverse();
    const evidence = new Map([...(data.messages || []), ...(data.pending || [])].map(row => [row.id, row]));
    for (const entry of outbox) if (sameDestination(entry.dest) && evidence.has("web:" + entry.id)) {
      store.remove(entry.key).then(refreshOutbox).catch(e => error(e.message));
    }
    snapshot = data; live = true;
    if (!previous || previous.cursor !== data.cursor || JSON.stringify(previous.pending) !== JSON.stringify(data.pending)) store.saveHistory({...destination}, data).catch(() => note("History could not be saved on this device."));
    if (data.history_gap) note("Earlier history changed or expired. Showing the available recent conversation.");
    render();
  }
  async function drainOutbox() {
    if (draining || navigator.onLine === false || document.hidden) return;
    draining = true; clearTimeout(retryTimer);
    try {
      outbox = await store.list();
      const blocked = new Set();
      for (const entry of outbox) {
        const scope = store.destinationKey(entry.dest);
        if (blocked.has(scope)) continue;
        if (entry.state === "failed" || entry.state === "expired") { blocked.add(scope); continue; }
        if (entry.firstAttemptAt && Date.now() - entry.firstAttemptAt >= deadline) {
          entry.state = "expired"; entry.failure = "Delivery unconfirmed. Check Terminal before sending again.";
          await store.update(entry); blocked.add(scope); continue;
        }
        if (entry.next > Date.now()) { blocked.add(scope); continue; }
        try {
          const status = await fetch("/api/chat?" + new URLSearchParams(entry.dest), {cache: "no-store", signal: AbortSignal.timeout(35000)});
          if (!status.ok) throw new Error("Could not check the destination before delivery.");
          const current = await status.json();
          if ([...(current.messages || []), ...(current.pending || [])].some(row => row.id === "web:" + entry.id)) {
            await store.remove(entry.key); continue;
          }
          // Old nodes evict recent identities by count. They must never receive
          // automatic uncertain retries under the new time-based guarantee.
          if (entry.firstAttemptAt && !(current.retry_window_seconds * 1000 > deadline)) {
            entry.state = "failed"; entry.failure = "Delivery unconfirmed. Update this node or inspect Terminal before retrying.";
            await store.update(entry); blocked.add(scope); continue;
          }
          if((entry.files||[]).length && !current.attachments_supported){entry.state="failed";entry.failure="Update this node to send attachments.";await store.update(entry);blocked.add(scope);continue;}
          entry.firstAttemptAt ||= Date.now();
          await store.update(entry);
          // Multiple tabs may retry concurrently; the server deduplicates this same ID.
          entry.uploaded ||= [];
          inflight = {key: entry.key, name: ""};
          for(const file of entry.files||[]) {
            if(entry.uploaded.some(meta=>meta.id===file.id))continue;
            inflight.name = file.name; inflight.file = file.id; inflight.progress = 0; render();
            const result=await upload({...entry.dest,upload:{id:file.id,name:file.name,data:await fileData(file.blob)}}, (fraction) => { if (inflight?.file !== file.id) return; inflight.progress = fraction; paintSoon(); });
            if(!result.uploaded || result.uploaded.id!==file.id)throw new Error("Attachment upload was not confirmed.");
            entry.uploaded.push(result.uploaded);await store.update(entry);
          }
          inflight.name = ""; inflight.file = ""; render();
          await post("/api/chat/send", {...entry.dest, id: entry.id, text: entry.text, files:(entry.files||[]).map(file=>file.id)});
          await store.remove(entry.key); inflight = null;
          // Sent: a note that said it waits for the network no longer holds.
          if ($("composer-status").textContent === OFFLINE_NOTE) note("");
          if (sameDestination(entry.dest)) read();
        } catch (e) {
          inflight = null;
          entry.attempts++;
          entry.state = e.status >= 400 && e.status < 500 && ![408, 429].includes(e.status) ? "failed" : "local";
          entry.failure = e.message;
          entry.next = Date.now() + Math.min(30000, 1000 * 2 ** Math.min(entry.attempts, 5));
          await store.update(entry); blocked.add(scope);
        }
      }
      await refreshOutbox();
    } catch (e) { error("Could not access the saved message queue. " + e.message); }
    finally { draining = false; retryTimer = setTimeout(drainOutbox, 3000); }
  }
  async function retryEntry(key) {
    const entry = (await store.list()).find(row => row.key === key);
    if (!entry || entry.state === "expired") return;
    entry.state = "local"; entry.next = 0; await store.update(entry); await refreshOutbox(); drainOutbox();
  }
  const storageKey = dest => "zmina.chat.draft." + JSON.stringify(dest);
  const uid = () => crypto.randomUUID ? crypto.randomUUID() : Array.from(crypto.getRandomValues(new Uint8Array(16)), x => x.toString(16).padStart(2, "0")).join("");
  const error = text => { $("chat-error").textContent = text; $("chat-error").hidden = !text; };
  const note = text => { $("composer-status").textContent = text; $("composer-status").hidden = !text; };
  const atBottom = () => conversation.scrollHeight - conversation.scrollTop - conversation.clientHeight < 80;
  function saveDraft(dest = destination, text = input.value, id = draftID, re = replyTo) { if (!dest) return; try { localStorage.setItem(storageKey(dest), JSON.stringify({text, id, ...(re ? {re} : {})})); } catch (_) { /* optional browser persistence */ } }
  function renderReply() {
    const box = $("reply-to"); box.hidden = !replyTo;
    if (!replyTo) { box.replaceChildren(); return; }
    const id = document.createElement("b"), title = document.createElement("span"), x = document.createElement("button");
    id.textContent = replyTo.id; title.textContent = replyTo.title; box.title = replyTo.id + " · " + replyTo.title;
    x.type = "button"; x.className = "reply-x"; x.setAttribute("aria-label", "Remove " + replyTo.id); x.title = "Remove";
    x.innerHTML = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 7l10 10M17 7L7 17"/></svg>';
    x.addEventListener("click", () => { replyTo = null; saveDraft(); renderReply(); input.focus(); });
    box.replaceChildren(id, title, x);
  }
  function restoreDraft() { selectedFiles=[];renderSelected();const revision=++filesRevision, version=generation, dest={...destination};store.loadFiles(dest).then(files=>{if(version!==generation||revision!==filesRevision)return;selectedFiles=files;renderSelected();controls();}).catch(e=>error("Could not restore draft files. "+e.message)); let saved = {}; try { saved = JSON.parse(localStorage.getItem(storageKey(destination)) || sessionStorage.getItem(storageKey(destination)) || "{}"); } catch (_) { /* optional */ } input.value = saved.text || ""; draftID = saved.id || ""; replyTo = saved.re && saved.re.id ? {id: String(saved.re.id), title: String(saved.re.title || "")} : null; renderReply(); cleared = null; controls();
    // Talk to a role from a card (D-15 h1): its field takes the focus once.
    if (saved.re?.focus) { saveDraft(); setTimeout(() => input.focus(), 0); } }
  try { language.value = localStorage.getItem("zmina-speech-language") || "ru-RU"; } catch (_) { /* optional */ }
  language.addEventListener("change", () => { try { localStorage.setItem("zmina-speech-language", language.value); } catch (_) { /* optional */ } });
  function controls() {
    send.disabled = !destination || loadingTargets || posting || listening || (!input.value.trim() && !selectedFiles.length);
    attach.disabled = !destination || loadingTargets || posting || listening;
    mic.disabled = !destination || loadingTargets || posting;
    input.readOnly = listening;
    $("clear-message").disabled = posting || (!input.value && !selectedFiles.length && !cleared);
    // D-08 k3: the ✕ sits in the field only while there is a draft (or a clear to undo).
    $("clear-message").hidden = !input.value && !selectedFiles.length && !cleared;
    $("clear-message").classList.toggle("undo", Boolean(cleared));
    $("clear-message").setAttribute("aria-label", cleared ? "Undo clear" : "Clear draft");
    $("clear-message").title = cleared ? "Undo clear" : "Clear draft";
    input.parentElement.classList.toggle("has-draft", !$("clear-message").hidden);
    fitField();
    mic.setAttribute("aria-pressed", String(listening)); mic.setAttribute("aria-label", listening ? "Stop dictation" : "Dictate");
    mic.title = listening ? "Stop dictation" : "Dictate";
    $("start-role").hidden = !snapshot || snapshot.state === "up";
    $("start-role").disabled = posting || !live;
    $("stop-turn").hidden = !snapshot?.active;
    $("stop-turn").disabled = posting || !live;
  }
  // fitField grows the field with its text, one line at rest (D-08 k2); its CSS max-height caps it.
  function setLabel(el, text) { if (el.textContent !== text) el.textContent = text; }
  // capped: the field reached its max-height and scrolls from there (D-14 b: on the phone it scrolls only then).
  function fitField() { input.style.height = "auto"; input.style.height = input.scrollHeight + "px"; input.classList.toggle("capped", input.scrollHeight > input.clientHeight + 1); }
  const clock = new Intl.DateTimeFormat(undefined, {hour: "2-digit", minute: "2-digit"}), dated = new Intl.DateTimeFormat("en-GB", {day: "numeric", month: "short", hour: "2-digit", minute: "2-digit"});
  const dayName = new Intl.DateTimeFormat("en-GB", {weekday: "short", day: "numeric", month: "short"});
  const dayOf = (at) => { const d = new Date(at); return Number.isNaN(d.getTime()) ? "" : d.toDateString(); };
  function dayLabel(at) {
    const d = new Date(at), today = new Date(), yesterday = new Date(today.getFullYear(), today.getMonth(), today.getDate() - 1);
    return d.toDateString() === today.toDateString() ? "Today" : d.toDateString() === yesterday.toDateString() ? "Yesterday" : dayName.format(d);
  }
  const deliveryText = {uploading: "Uploading…", transmitting: "Sending…", local: "Saved on this device · waiting to send", queued: "Queued · delivered when the role is idle", sent: "Sent to the role · awaiting agent confirmation", accepted: "", unconfirmed: "Delivery unconfirmed · the conversation changed", expired: "Delivery unconfirmed · check Terminal before sending again", failed: "Delivery failed", sending: "Saving…", uncertain: "Delivery unconfirmed · retry with the same message ID"};
  const deliveryKey = (row) => [row.state, row.upload || "", row.failure || "", Boolean(row.outboxKey)].join("|");
  // fillDelivery writes the delivery state of a group of owner messages (one, or a run in the same state) under its
  // last one; its buttons act on every message of the group, one after another.
  function fillDelivery(state, group) {
    const row = group[group.length - 1], many = group.length > 1;
    state.replaceChildren(row.state === "uploading" && row.upload ? "Uploading " + row.upload + "…" : deliveryText[row.state] || "");
    if (row.failure && row.state !== "uploading" && row.state !== "transmitting") state.append(" · " + row.failure);
    const add = (label, run) => { const b = document.createElement("button"); b.type = "button"; b.textContent = label; if (many) b.setAttribute("aria-label", label + " · all " + group.length + " messages"); b.addEventListener("click", () => run().catch(e => error(e.message))); state.append(" ", b); };
    if (row.outboxKey && row.state === "failed") add("Retry delivery", async () => { for (const r of group) await retryEntry(r.outboxKey); });
    // Set aside for every browser: the role's own record keeps it, unshown.
    // The group belongs to the conversation it was drawn for: another one opened meanwhile stops it.
    if (!row.outboxKey && row.state === "unconfirmed") add("Dismiss", async () => { const version = generation; for (const r of group) { if (version !== generation) return; await action("dismiss:" + r.id.slice(4), version); } });
    if (row.outboxKey && ["failed", "expired"].includes(row.state)) add(many ? "Remove saved messages" : "Remove saved message", async () => {
      if (!confirm(many ? "Remove these " + group.length + " messages from this device’s delivery queue? They may already have reached the role." : "Remove this message from this device’s delivery queue? It may already have reached the role.")) return;
      for (const r of group) await store.remove(r.outboxKey);
      await refreshOutbox(); await drainOutbox();
    });
    state.hidden = !state.textContent; state.classList.toggle("failed", ["failed", "uncertain"].includes(row.state));
  }
  // D-13: the question on the role's screen takes the composer's place in the dock — one item at a time,
  // drawn progress, flat rows (radio or box), the recommendation a neutral tag, Other as a field in its row, a review
  // to check and send. The answer goes as POST /api/chat/action answer:<json>, which presses the dialog's own keys.
  let ask = null;
  // A lone question with ticks is left to Terminal: its key path in the dialog is not known.
  const answerable = (q) => (q && !(q.items.length === 1 && q.items[0].multi_select) ? q : null);
  // D-13 q8: once its answer is sent the card goes, though the role's screen may show the question a moment longer.
  const sentAnswer = (q) => Boolean(q && ask && ask.id === q.id && ask.sent);
  // Keys that went but did not close the dialog leave the question on the screen: past this the state line says it
  // needs input again (Terminal answers it; Chat answers a question once).
  const ANSWER_SETTLES = 10000;
  const answerSettling = (q) => sentAnswer(q) && Date.now() - ask.sent < ANSWER_SETTLES;
  const openQuestion = (q) => (answerable(q) && !sentAnswer(q) ? q : null);
  const REC = /\s*\(Recommended\)\s*$/;
  function renderQuestion() {
    const box = $("chat-question"), all = live && answerable(snapshot?.question), q = openQuestion(all), composerBox = document.querySelector(".composer-box");
    if (!all) ask = null;
    if (!q) { box.hidden = true; box.replaceChildren(); if (composerBox) composerBox.hidden = false; return; }
    if (composerBox) composerBox.hidden = true;
    if (!ask || ask.id !== q.id) ask = {id: q.id, step: 0, items: q.items.map(() => ({choices: [], other: null})), sig: ""};
    const sig = JSON.stringify([ask.step, ask.items, posting, ask.failed || ""]);
    box.hidden = false;
    if (ask.sig === sig && box.childElementCount) return;
    ask.sig = sig;
    const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text) e.textContent = text; return e; };
    const btn = (cls, text, fn) => { const b = el("button", cls, text); b.type = "button"; b.addEventListener("click", fn); return b; };
    const n = q.items.length, review = ask.step >= n, go = (s) => { ask.step = s; renderQuestion(); };
    const answered = (a) => (a.other !== null ? a.other.trim() !== "" : a.choices.length > 0);
    const words = (it, a) => (a.other !== null ? a.other.trim() : a.choices.map(c => it.options[c - 1].label.replace(REC, "")).join(", "));
    // Nothing goes with a part unanswered: the first such part opens instead. Sent, the card goes and the line under
    // the field says what went; refused, the card stays with the answers chosen and says why in it (D-13 q8).
    const send = async () => {
      const gap = ask.items.findIndex(a => !answered(a));
      if (gap >= 0) { go(gap); return; }
      if (!destination || posting) return;
      const mine = ask, version = generation;
      const summary = n === 1 ? words(q.items[0], mine.items[0]) : q.items.map((it, i) => (it.header || "Question " + (i + 1)) + ": " + words(it, mine.items[i])).join(" · ");
      posting = true; mine.failed = ""; controls(); renderQuestion();
      try {
        const data = await post("/api/chat/action", {...destination, action: "answer:" + JSON.stringify({id: q.id, items: mine.items.map(a => ({choices: a.other !== null ? [] : a.choices, other: a.other || ""}))})});
        if (version !== generation) return;
        mine.sent = Date.now(); error(""); note("Answered · " + summary);
        applySnapshot(data);
      } catch (e) { if (version === generation) mine.failed = e.message || "Could not answer."; }
      finally { posting = false; controls(); if (version === generation) renderQuestion(); }
    };
    // D-13 q6: a single-select tap moves on — to the next part, and from the last to the review that sends.
    const after = () => go(ask.step + 1);
    const top = el("div", "q-top");
    top.append(el("span", "q-hdr", review ? "Check and send" : (q.items[ask.step].header || "Question") + (q.items[ask.step].multi_select ? " · choose any" : "")));
    if (n > 1) {
      const bar = el("span", "q-prog");
      q.items.forEach((_, i) => {
        const seg = btn("q-seg" + (i === ask.step ? " now" : answered(ask.items[i]) ? " done" : ""), "", () => answered(ask.items[i]) && go(i));
        seg.setAttribute("aria-label", "Item " + (i + 1) + " of " + n);
        bar.append(seg);
      });
      top.append(bar);
    }
    const out = [top];
    const foot = el("div", "q-foot");
    if (review) {
      const list = el("div", "q-review");
      q.items.forEach((it, i) => { const r = btn("q-rrow", "", () => go(i)); r.append(el("span", "q-rh", it.header || "Question " + (i + 1)), el("span", "q-ra", words(it, ask.items[i])), el("span", "q-rc", "›")); list.append(r); });
      out.push(list);
      foot.append(btn("q-back", "Back", () => go(n - 1)), btn("q-primary", "Send answers", send));
      foot.lastChild.disabled = posting;
    } else {
      const it = q.items[ask.step], a = ask.items[ask.step];
      out.push(el("p", "q-text", it.question));
      const list = el("div", "q-opts");
      list.setAttribute("role", it.multi_select ? "group" : "radiogroup");
      it.options.forEach((o, i) => {
        const on = a.other === null && a.choices.includes(i + 1);
        const row = btn("q-opt" + (on ? " on" : ""), "", () => {
          if (it.multi_select) { a.other = null; a.choices = on ? a.choices.filter(c => c !== i + 1) : [...a.choices, i + 1].sort((x, y) => x - y); renderQuestion(); return; }
          if (ask.moving) return; // a second tap within the 200 ms must not skip the next part
          const step = ask.step;
          ask.moving = true; a.other = null; a.choices = [i + 1]; renderQuestion();
          // The move goes only if the part still holds this choice: Other or another row meanwhile cancels it.
          ask.timer = setTimeout(() => { if (!ask || ask.id !== q.id) return; ask.moving = false; if (ask.step === step && a.other === null && a.choices[0] === i + 1) after(); }, matchMedia("(prefers-reduced-motion: reduce)").matches ? 0 : 200);
        });
        row.setAttribute("role", it.multi_select ? "checkbox" : "radio");
        row.setAttribute("aria-checked", on ? "true" : "false");
        row.disabled = posting;
        const t = el("span", "q-body"), l = el("span", "q-label", o.label.replace(REC, ""));
        if (REC.test(o.label)) l.append(el("span", "q-tag", "Recommended"));
        t.append(l);
        if (o.description) t.append(el("span", "q-desc", o.description));
        row.append(el("span", "q-ctl"), t);
        list.append(row);
      });
      // Words of one's own: the dialog takes them on a one-choice item (its «Type something»).
      if (!it.multi_select) {
        const on = a.other !== null, row = el("div", "q-opt q-other" + (on ? " on" : ""));
        const pick = btn("q-otherpick", "", () => { clearTimeout(ask.timer); ask.moving = false; a.other = a.other ?? ""; a.choices = []; renderQuestion(); box.querySelector(".q-otherfield")?.focus(); });
        pick.setAttribute("role", "radio"); pick.setAttribute("aria-checked", on ? "true" : "false");
        pick.append(el("span", "q-ctl"), el("span", "q-label", "Other…"));
        row.append(pick);
        if (on) {
          const f = el("textarea", "q-otherfield"); f.rows = 1; f.value = a.other; f.setAttribute("aria-label", "Your own answer");
          f.addEventListener("input", () => { a.other = f.value; ask.sig = JSON.stringify([ask.step, ask.items, posting, ask.failed || ""]); foot.querySelector(".q-primary").disabled = !f.value.trim(); f.style.height = "auto"; f.style.height = Math.min(f.scrollHeight, 120) + "px"; });
          f.addEventListener("keydown", (e) => { if (e.key === "Enter" && !e.shiftKey && f.value.trim()) { e.preventDefault(); after(); } });
          row.append(f);
        }
        list.append(row);
      }
      out.push(list);
      if (ask.step > 0) foot.append(btn("q-back", "Back", () => go(ask.step - 1)));
      if (it.multi_select || a.other !== null) {
        const label = it.multi_select ? "Continue · " + a.choices.length : ask.step === n - 1 ? "Review" : "Next";
        const p = btn("q-primary", label, after); p.disabled = posting || !answered(a); foot.append(p);
      }
    }
    out.push(foot);
    if (ask.failed && review) { const fail = el("p", "q-fail", "Not answered · " + ask.failed); fail.setAttribute("role", "alert"); out.push(fail); }
    // A redraw keeps the focus on the same row (or the first row of a new part when the card had it).
    const had = box.contains(document.activeElement), at = [...box.querySelectorAll(".q-opts > button, .q-rrow")].indexOf(document.activeElement);
    box.replaceChildren(...out);
    if (had) (box.querySelectorAll(".q-opts > button, .q-rrow")[Math.max(at, 0)] || box.querySelector("button"))?.focus();
  }
  // D-13 q9: keys while the card is open and the focus is in it — digits choose, Esc goes back.
  document.addEventListener("keydown", (e) => {
    const box = $("chat-question");
    if (box.hidden || !box.contains(document.activeElement) || document.activeElement.tagName === "TEXTAREA") return;
    if (/^[1-9]$/.test(e.key)) { const rows = box.querySelectorAll(".q-opts > button.q-opt"); rows[+e.key - 1]?.click(); e.preventDefault(); }
    else if (e.key === "Escape") { box.querySelector(".q-back")?.click(); e.preventDefault(); }
    else if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      const rows = [...box.querySelectorAll(".q-opts > button, .q-rrow")], i = rows.indexOf(document.activeElement);
      rows[Math.min(rows.length - 1, Math.max(0, i + (e.key === "ArrowDown" ? 1 : -1)))]?.focus();
      e.preventDefault();
    }
  }, true);
  const eventWord = {completed: "Completed", failed: "Failed", stopped: "Stopped", running: "Running", received: "Received"};
  const rowClass = (row) => row.event ? (row.event.kind === "session" ? "event session" : row.event.kind === "command" ? "event command" : "event") : row.role === "user" ? "user" : "assistant";
  const kindOf = (row) => row.event ? "event" : row.role === "user" ? "user" : "assistant";
  function render() {
    if (!destination) return;
    snapshot ||= {state: "unknown", messages: [], pending: []};
    const follow = firstPaint || atBottom(), oldScroll = conversation.scrollTop;
    const rows = [...(snapshot.messages || []), ...(snapshot.pending || [])], seen = new Set(rows.map(row => row.id));
    for (const entry of outbox) if (sameDestination(entry.dest) && !seen.has("web:" + entry.id)) rows.push({id: "web:" + entry.id, role: "user", text: entry.text, at: entry.at, state: outboxState(entry), upload: inflight?.key === entry.key ? inflight.name : "", uploadFile: inflight?.key === entry.key ? inflight.file : "", uploadProgress: inflight?.key === entry.key ? inflight.progress : 0, uploaded: (entry.uploaded || []).map(meta => meta.id), failure: entry.failure, outboxKey: entry.key, attachments:entry.files||[]});
    // A message not yet seen in the conversation stands where it was sent, not under every newer reply; one with no
    // time stays last. The sort is stable, so the conversation's own order holds.
    const when = (row) => { const t = Date.parse(row.at); return Number.isNaN(t) ? Infinity : t; };
    rows.sort((a, b) => when(a) - when(b));
    const keep = new Set();
    rows.forEach((row, index) => {
      keep.add(row.id);
      let article = articles.get(row.id);
      if (!article) { article = document.createElement("article"); article.className = "chat-message " + rowClass(row); const head = document.createElement("div"); head.className = "message-head"; const who = document.createElement("span"), time = document.createElement("time"); head.append(who, time); const body = document.createElement("div"); body.className = "message-body"; const state = document.createElement("div"); state.className = "message-state"; const files=document.createElement("div");files.className="message-files";const meta=document.createElement("div");meta.className="message-meta";const metaTime=document.createElement("time");const copy=document.createElement("button");copy.type="button";copy.className="ib copy";copy.setAttribute("aria-label","Copy");copy.title="Copy";copy.innerHTML='<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h2"/></svg>';copy.addEventListener("click",()=>{navigator.clipboard?.writeText(article._text||"").then(()=>{copy.title="Copied";setTimeout(()=>{copy.title="Copy";},1500);},()=>error("Could not copy this message."));});meta.append(metaTime,copy);article.append(head, body, files, state, meta); articles.set(row.id, article); }
      const date = new Date(row.at), shown = Number.isNaN(date.getTime()) ? "" : (date.toDateString() === new Date().toDateString() ? clock : dated).format(date);
      // A command's line carries its time, so a new day's format redraws it.
      const presentation=JSON.stringify({role:row.role,event:row.event||null,shown:row.event?.kind==="command"?shown:""});
      article.className="chat-message "+rowClass(row);
      // A reply unfolded by a tap stays unfolded through the next render (D-11 m1).
      if (article._metaOn) article.classList.add("meta-on");
      // D-08 c5: a card's label says what it is and how it stands on one line.
      article.querySelector(".message-head span").textContent = row.event ? (row.event.kind==="session"?"Session message":"Background task") + (eventWord[row.event.status] ? " · " + eventWord[row.event.status] : "") : row.role === "user" ? "You" : destination.role;
      for (const time of article.querySelectorAll("time")) time.textContent = shown;
      // D-08 c3: every message keeps its full time in its title; the meta row shows it under a run's last one.
      article.title = (row.event?.kind === "session" ? "Session message · " : "") + (Number.isNaN(date.getTime()) ? "" : date.toLocaleString());
      if (article._text !== row.text || article._presentation!==presentation) { const body = article.querySelector(".message-body"); if (row.event?.kind === "command") {
        // a command run at the role's prompt (/compact, /model) is one quiet line with its time, not a bubble.
        const words = row.event.summary + (shown ? " · " + shown : "");
        if (!row.event.detail) { const line = document.createElement("p"); line.className = "command-line"; line.textContent = words; body.replaceChildren(line); }
        else {
          // D-17 a: a line with output is a button; a tap opens the output under it, a second tap closes it.
          const own = article, line = document.createElement("button"), out = document.createElement("pre");
          line.type = "button"; line.className = "command-line";
          line.innerHTML = '<svg viewBox="0 0 12 12" aria-hidden="true"><path d="M3 4.5 6 7.5 9 4.5"/></svg>';
          line.prepend(document.createTextNode(words));
          out.className = "command-output"; out.textContent = row.event.detail;
          const show = () => { out.hidden = !own._open; line.setAttribute("aria-expanded", String(Boolean(own._open))); };
          line.addEventListener("click", () => { own._open = !own._open; show(); });
          show();
          body.replaceChildren(line, out);
        }
      } else if (row.event) {
        const title = document.createElement("p"), summary = document.createElement("p");
        title.className = "event-status";
        title.textContent = eventWord[row.event.status] || "Update";
        // The owner's own message from a session is shown whole, line breaks kept: it is his, not a log line to fold.
        summary.textContent = row.event.kind === "session" && row.event.detail ? row.event.detail : row.event.summary;
        body.replaceChildren(title, summary);
        if (row.event.result) { const result = document.createElement("p"); result.className = "event-result"; result.textContent = row.event.result; body.append(result); }
        // D-11 amendment b: an owner's session message offers its full text only when the line shown was clipped.
        if (row.event.detail && row.event.kind !== "session") { const details = document.createElement("details"), label = document.createElement("summary"), text = document.createElement("pre"); label.textContent = row.event.kind==="session"?"Full message":"Technical details"; text.textContent = row.event.detail; details.append(label, text); body.append(details); }
      } else if (row.role === "assistant") window.zminaChatContent.render(row.text, body); else { const re = window.zminaIds.reply(row.text); if (re) { const line = document.createElement("p"); line.className = "xre"; line.textContent = "↳ " + re.id + " · " + window.zminaIds.short(re.title); body.replaceChildren(line, document.createTextNode(re.text)); } else body.textContent = row.text; } body._xids = null; body._xfilesFor = null; article._text = row.text; article._presentation=presentation; }
      const fileRows=article.querySelector(".message-files"), signature=fileSignature(row.attachments)+Boolean(row.outboxKey);
      if(article._files!==signature){window.zminaFiles.ownerBlock(fileRows,row,destination);article._files=signature;}
      if(row.role==="user")window.zminaFiles.progress(fileRows,row);
    });
    for (const [id, article] of articles) if (!keep.has(id)) { article._day?.remove(); article.remove(); articles.delete(id); }
    // D-08 c3/c5: consecutive messages of one speaker are a run; a day line where the day changes; the meta row
    // under a run's last message, always shown under the newest one.
    // A day line stands before its message, outside it (a card keeps its own frame); each is put in place only when
    // it is not there already, so nothing on screen moves.
    let cursor = messages.firstElementChild;
    const place = (el) => { if (cursor === el) cursor = cursor.nextElementSibling; else messages.insertBefore(el, cursor); };
    rows.forEach((row, i) => {
      const article = articles.get(row.id), prev = rows[i - 1], next = rows[i + 1], kind = kindOf(row), day = dayOf(row.at);
      const newDay = Boolean(day) && (prev ? dayOf(prev.at) !== day : day !== new Date().toDateString());
      const start = !prev || kindOf(prev) !== kind || newDay || kind === "event";
      const end = !next || kindOf(next) !== kind || (Boolean(day) && dayOf(next.at) !== day) || kind === "event";
      if (newDay) {
        if (!article._day) { article._day = document.createElement("p"); article._day.className = "day-line"; }
        setLabel(article._day, dayLabel(row.at)); place(article._day);
      } else if (article._day) {
        // The cursor may stand on the line being dropped (an earlier message joined its day): step past it first.
        if (cursor === article._day) cursor = cursor.nextElementSibling;
        article._day.remove(); article._day = null;
      }
      place(article);
      article.classList.toggle("run-start", start); article.classList.toggle("run-end", end); article.classList.toggle("newest", end && !next);
    });
    // D-08 amendment b: one delivery state per run — consecutive owner messages in the same state show it once,
    // under the last of them, and its Dismiss / Retry / Remove acts on all of them.
    let group = [];
    const flush = () => { group.forEach((r, j) => { const st = articles.get(r.id).querySelector(".message-state"); if (j < group.length - 1) { st.replaceChildren(); st.hidden = true; } else fillDelivery(st, group); }); group = []; };
    rows.forEach((row) => {
      const article = articles.get(row.id), k = row.role === "user" && !row.event && deliveryText[row.state] ? deliveryKey(row) : "";
      if (group.length && (!k || article.classList.contains("run-start") || k !== deliveryKey(group[0]))) flush();
      if (k) group.push(row); else fillDelivery(article.querySelector(".message-state"), [row]);
    });
    flush();
    window.zminaIds.decorate(messages, destination);
    window.zminaFiles.decorate(messages, destination);
    renderQuestion();
    // D-08 c6: working shows where the next reply will land.
    const working = $("chat-working"), busy = live && Boolean(snapshot.active);
    working.hidden = !busy;
    if (busy) { const since = Date.parse(snapshot.since || ""), min = Number.isNaN(since) ? 0 : Math.max(1, Math.floor((Date.now() - since) / 60000)); working.lastElementChild.textContent = "Working" + (min ? " · " + min + " min" : ""); }
    $("empty-chat").hidden = rows.length > 0;
    $("empty-chat").textContent = snapshot.session ? "No saved messages yet. Send a message to this role." : "No conversation yet. Start the role, then send a message.";
    $("runtime-info").textContent = [snapshot.runtime, snapshot.profile, snapshot.context ? new Intl.NumberFormat().format(snapshot.context) + " context tokens" : ""].filter(Boolean).join(" · ");
    $("agent-state").textContent = !live ? "Connecting… Showing saved conversation" : snapshot.state !== "up" ? "Role stopped · queued messages wait until it starts" : openQuestion(snapshot.question) ? "Asks you · answer below" : answerSettling(snapshot.question) ? "Answered · the role goes on" : snapshot.needs_input ? "Needs input · open Terminal to respond" : snapshot.active ? "Working…" : snapshot.status === "unknown" || !snapshot.session ? "Status unavailable · open Terminal" : "Idle";
    // D-08 c6: the state line speaks only when something is off; working and idle are said elsewhere (the working
    // line, the panel's head) and stay for screen readers.
    const quiet = live && snapshot.state === "up" && !snapshot.needs_input && !(snapshot.status === "unknown" || !snapshot.session);
    $("agent-state").classList.toggle("term-sr", quiet); $("agent-state").parentElement.classList.toggle("quiet", quiet);
    if (follow) conversation.scrollTop = conversation.scrollHeight; else conversation.scrollTop = oldScroll;
    firstPaint = false; $("chat-latest").hidden = atBottom(); controls();
  }
  async function read() {
    if (!destination) return;
    const version = generation, dest = {...destination};
    readRequest?.abort(); const request = new AbortController(); readRequest = request;
    try { const response = await fetch("/api/chat?" + new URLSearchParams({...dest, ...(snapshot?.cursor ? {cursor: snapshot.cursor} : {})}), {signal: request.signal, cache: "no-store"}); if (!response.ok) throw new Error(await response.text()); const data = await response.json(); if (version !== generation) return; applySnapshot(data); if ($("chat-error").textContent.startsWith("Could not refresh this role:")) error(""); }
    catch (e) { if (!request.signal.aborted && version === generation) { live = false; render(); error("Could not refresh this role: " + e.message); } }
    finally { if (version === generation && readRequest === request && !request.signal.aborted) { clearTimeout(pollTimer); pollTimer = setTimeout(() => { if (document.hidden) pollTimer = setTimeout(read, 3000); else read(); }, snapshot?.more ? 0 : 3000); } }
  }
  function openChat() {
    const chosen = targets[Number(target.value)]; if (target.value === "" || !chosen) return;
    showDestination({node: node.value, crew: chosen.crew, role: chosen.role});
  }
  function showDestination(dest) {
    endSpeech(true); saveDraft(); generation++; clearTimeout(pollTimer); readRequest?.abort();
    destination = dest; snapshot = null; live = false; firstPaint = true; articles.clear(); messages.replaceChildren(); error(""); note("");
    const query = new URLSearchParams(destination); history.replaceState(null, "", location.pathname + "?" + query);
    $("terminal-link").href = "terminals.html?" + new URLSearchParams({...destination, view: "terminal"});
    $("session-title").textContent = `${destination.crew} / ${destination.role}`; settings.open = false;
    // D-08 h2: where the words go is said once, in the head (the cockpit panel's, or this page's session title).
    $("session-title").title = destination.node ? destination.node + "/" + destination.crew + " · over the mesh, signed" : "";
    input.placeholder = "Reply to " + destination.role + "…"; $("chat-working").hidden = true;
    $("agent-state").textContent = "Loading conversation…"; $("empty-chat").hidden = false; $("empty-chat").textContent = "Loading messages…";
    restoreDraft();
    const version = generation, selected = {...destination};
    store.loadHistory(selected).then(saved => { if (version !== generation || live || snapshot?.cursor || !saved || saved.presentation_version!==1) return; snapshot = saved; render(); }).catch(() => {});
    refreshOutbox().catch(e => error(e.message)); read(); drainOutbox();
  }
  async function loadTargets() {
    loadingTargets = true; controls(); const chosenNode = node.value; target.replaceChildren(new Option("Loading roles…", "")); $("open-chat").disabled = true;
    try { const response = await fetch("/api/terminal/targets?" + new URLSearchParams({node: chosenNode})); if (!response.ok) throw new Error(await response.text()); const rows = await response.json(); if (chosenNode !== node.value) return; targets = rows; target.replaceChildren(new Option("Choose a role", "")); rows.forEach((row, index) => target.add(new Option(`${row.crew} / ${row.role} · ${row.state}`, String(index)))); let index = rows.findIndex(row => row.crew === params.get("crew") && row.role === params.get("role")); if (index < 0) index = rows.findIndex(row => row.state === "up"); if (index < 0 && rows.length) index = 0; if (index >= 0) { target.value = String(index); $("open-chat").disabled = false; openChat(); } }
    catch (e) { target.replaceChildren(new Option("Roles unavailable", "")); error(e.message); }
    finally { if (chosenNode === node.value) { loadingTargets = false; controls(); } }
  }
  // upload sends one file with its progress (fetch has none): fraction 0–1 as the bytes leave.
  function upload(body, onProgress) {
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open("POST", "/api/chat/upload");
      xhr.setRequestHeader("Content-Type", "application/json"); xhr.setRequestHeader("X-Zmina", "1");
      xhr.timeout = 65000;
      xhr.upload.onprogress = (e) => { if (e.lengthComputable) onProgress(e.loaded / e.total); };
      xhr.onload = () => { if (xhr.status >= 200 && xhr.status < 300) { try { resolve(JSON.parse(xhr.responseText)); } catch (e) { reject(e); } } else { const e = new Error(xhr.responseText || xhr.statusText); e.status = xhr.status; reject(e); } };
      xhr.onerror = () => reject(new Error("Network error while uploading."));
      xhr.ontimeout = () => reject(new Error("Upload timed out."));
      xhr.send(JSON.stringify(body));
    });
  }
  // paintSoon draws the upload's progress at most once a frame.
  let paintFrame = 0;
  function paintSoon() { if (!paintFrame) paintFrame = requestAnimationFrame(() => { paintFrame = 0; render(); }); }
  async function post(path, body) { const controller = new AbortController(); const timeout = setTimeout(() => controller.abort(), path==="/api/chat/upload"?65000:35000); try { const response = await fetch(path, {signal: controller.signal, method: "POST", headers: {"Content-Type": "application/json", "X-Zmina": "1"}, body: JSON.stringify(body)}); if (!response.ok) { const error = new Error(await response.text()); error.status = response.status; throw error; } return await response.json(); } finally { clearTimeout(timeout); } }
  $("composer").addEventListener("submit", async event => {
    event.preventDefault(); if (send.disabled || !destination) return;
    const dest = {...destination}, raw = input.value, re = replyTo, text = (re ? "Re " + re.id + " (" + re.title + "): " : "") + raw, version = generation, files=[...selectedFiles], revision=filesRevision;
    const id = draftID || uid(); draftID = id; saveDraft(); posting = true; cleared = null; error(""); note(""); controls();
    let accepted=false;
    try {
      await store.enqueue(dest, id, text, files);accepted=true;
      // Storage failure keeps the draft intact and sends nothing over the network.
      if (version === generation && input.value === raw && revision===filesRevision) { input.value = ""; draftID = ""; if (replyTo === re) { replyTo = null; renderReply(); } selectedFiles=[];filesRevision++;renderSelected();await store.saveFiles(dest,[]);saveDraft(); }
      // The bubble says how it goes; only without a network does the line under the field speak.
      await refreshOutbox(); note(navigator.onLine === false ? OFFLINE_NOTE : ""); drainOutbox();
    } catch (e) { error((accepted ? "Message saved. Could not finish updating this draft. " : "Message was not saved. Keep this draft and retry. ") + e.message); if(accepted){refreshOutbox().catch(()=>{});drainOutbox();} }
    finally { posting = false; controls(); }

  });
  async function action(name, bound) { if (!destination || posting || (bound !== undefined && bound !== generation)) return; const version = generation; posting = true; controls(); try { const data = await post("/api/chat/action", {...destination, action: name}); if (version === generation) { applySnapshot(data); error(""); } } catch (e) { if (version === generation) error(e.message); } finally { posting = false; controls(); } }
  $("start-role").addEventListener("click", () => action("start")); $("stop-turn").addEventListener("click", () => action("stop"));
  input.addEventListener("input", () => { draftID = ""; cleared = null; saveDraft(); controls(); });
  input.addEventListener("keydown", event => { if (event.key === "Enter" && (event.ctrlKey || event.metaKey)) { event.preventDefault(); $("composer").requestSubmit(); } });
  $("clear-message").addEventListener("click", async () => {
    endSpeech(true);const version=generation,dest={...destination};
    const next=cleared?cleared:{text:"",id:"",files:[]}, previous={text:input.value,id:draftID,files:[...selectedFiles]};
    try{await store.saveFiles(dest,next.files||[]);if(version!==generation)return;const undo=Boolean(cleared);input.value=next.text;draftID=next.id;selectedFiles=next.files||[];cleared=undo?null:previous;filesRevision++;renderSelected();saveDraft();controls();}catch(e){error("Could not save this draft. "+e.message);}
  });
  function endSpeech(abort = false) { if (!recognition) return; const old = recognition; if (abort) { recognition = null; listening = false; old.abort(); controls(); } else old.stop(); }
  mic.addEventListener("click", () => {
    if (recognition) { endSpeech(); return; }
    if (!isSecureContext) { note("Dictation needs the private HTTPS link."); return; }
    if (!Recognition) { note("Dictation unavailable. Open in Safari or type your message."); return; }
    input.blur(); const base = input.value.trim(), version = generation, active = new Recognition(); recognition = active; listening = true; draftID = ""; controls();
    active.lang = language.value; active.continuous = false; active.interimResults = true;
    active.onstart = () => { if (recognition === active) note("Listening… Tap the microphone to stop."); };
    active.onresult = event => { if (recognition !== active || version !== generation) return; input.value = [base, Array.from(event.results, result => result[0].transcript).join(" ")].filter(Boolean).join(" ").slice(0, 4096); saveDraft(); controls(); };
    active.onerror = event => { if (recognition !== active) return; if (event.error !== "aborted") note(({"not-allowed": "Allow microphone and speech recognition in Safari’s website settings.", "service-not-allowed": "Enable Siri or Dictation in iPhone Settings.", "no-speech": "No speech heard. Try Dictate again.", "network": "Speech connection lost. Your draft is kept."})[event.error] || "Dictation stopped. Your draft is kept; try again."); };
    active.onend = () => { if (recognition !== active) return; recognition = null; listening = false; if ($("composer-status").textContent.startsWith("Listening")) note(""); controls(); };
    try { active.start(); } catch (_) { recognition = null; listening = false; note("Could not start dictation. Check microphone permission, then try again."); controls(); }
  });
  conversation.addEventListener("scroll", () => { $("chat-latest").hidden = atBottom(); });
  // D-18 l1: the Latest arrow stands 8 px over whatever is on top of the composer — the state line («Working · Stop»)
  // when it shows, else the composer itself; it never lies on Stop.
  { const form = $("composer"), state = document.querySelector(".chat-state");
    const lift = () => { const r = state.getBoundingClientRect(); form.style.setProperty("--over", (r.height ? Math.max(0, form.getBoundingClientRect().top - r.top) : 0) + "px"); };
    if (state && window.ResizeObserver) new ResizeObserver(lift).observe(state);
    window.addEventListener("resize", lift); }
  $("chat-latest").addEventListener("click", () => { conversation.scrollTop = conversation.scrollHeight; $("chat-latest").hidden = true; });
  $("open-chat").addEventListener("click", openChat); $("refresh").addEventListener("click", loadTargets); node.addEventListener("change", loadTargets); target.addEventListener("change", () => { $("open-chat").disabled = target.value === ""; if (target.value !== "") openChat(); });
  document.addEventListener("pointerdown", event => { if (!settings.contains(event.target)) settings.open = false; });
  document.addEventListener("visibilitychange", () => { if (!document.hidden && destination) { clearTimeout(pollTimer); read(); drainOutbox(); } });
  // dvh does not track iOS keyboard panning. Read browser geometry only here, not
  // message layout, and coalesce resize/scroll events during keyboard animation.
  let viewportFrame = 0;
  function fitViewport() {
    viewportFrame = 0;
    const viewport = window.visualViewport;
    if (!viewport || Math.abs(viewport.scale - 1) > .01) return; // Preserve pinch zoom.
    const follow = atBottom();
    document.body.style.setProperty("--chat-top", viewport.offsetTop + "px");
    document.body.style.setProperty("--chat-height", viewport.height + "px");
    if (follow) conversation.scrollTop = conversation.scrollHeight;
  }
  function scheduleViewport() { if (!viewportFrame) viewportFrame = requestAnimationFrame(fitViewport); }
  window.visualViewport?.addEventListener("resize", scheduleViewport);
  window.visualViewport?.addEventListener("scroll", scheduleViewport);
  window.addEventListener("pageshow", () => { scheduleViewport(); if (destination) { read(); drainOutbox(); } });
  window.addEventListener("online", () => { if (destination) read(); drainOutbox(); });
  fitViewport();
  window.addEventListener("pagehide", () => { saveDraft(); endSpeech(true); clearTimeout(pollTimer); clearTimeout(retryTimer); readRequest?.abort(); });
  // D-15 h1: «Talk to <role>» on a card — here when it is this conversation, else the role's own, the item on its field
  // (embedded, the cockpit opens it, so its panel and tabs follow).
  window.zminaFiles.init({post, redraw: () => render()});
  window.zminaIds.init({post, node: (name) => knownNodes.get(name), redraw: () => { if (destination) window.zminaIds.decorate(messages, destination); }, talk(dest, item) {
    if (destination && dest.node === (destination.node || "") && dest.crew === destination.crew && dest.role === destination.role) { replyTo = {id: item.id, title: item.title}; saveDraft(); renderReply(); input.focus(); return; }
    const to = {node: dest.node, crew: dest.crew, role: dest.role};
    let saved = {}; try { saved = JSON.parse(localStorage.getItem(storageKey(to)) || "{}"); } catch (_) { /* optional */ }
    try { localStorage.setItem(storageKey(to), JSON.stringify({...saved, re: {id: item.id, title: item.title, focus: true}})); } catch (_) { /* optional */ }
    if (params.get("embed") === "1" && window.parent !== window) window.parent.postMessage({zmina: "talk", ...to}, location.origin);
    else showDestination(to);
  }}, messages, conversation);
  if (params.get("crew") && params.get("role")) showDestination({node: params.get("node") || "", crew: params.get("crew"), role: params.get("role")});
  drainOutbox();
  (async () => { try { const response = await fetch("/api/terminal/nodes"); if (!response.ok) throw new Error("Nodes unavailable. Reload this page."); const nodes = await response.json(); node.replaceChildren(); knownNodes = new Map(nodes.filter(row => row.name).map(row => [row.name, row.local ? "" : row.name])); for (const article of articles.values()) article._text = null; render(); nodes.forEach(row => node.add(new Option(row.local ? `${row.name || "This node"} · local` : row.name, row.local ? "" : row.name))); if ([...node.options].some(option => option.value === params.get("node"))) node.value = params.get("node"); await loadTargets(); } catch (e) { error(e.message); } })();
})();
