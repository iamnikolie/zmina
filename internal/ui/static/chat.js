/* Structured chat uses zmina's owner queue. Terminal bytes and native UI redraws stay in Terminal. */
(() => {
  "use strict";
  const $ = id => document.getElementById(id);
  const node = $("node"), target = $("target"), settings = $("session-settings"), input = $("message");
  const conversation = $("conversation"), messages = $("messages"), send = $("send-message"), mic = $("dictate");
  const params = new URLSearchParams(location.search), language = $("speech-language");
  const Recognition = window.SpeechRecognition || window.webkitSpeechRecognition;
  let targets = [], destination = null, snapshot = null, generation = 0, pollTimer = null, readRequest = null;
  let loadingTargets = false, posting = false, draftID = "", cleared = null, recognition = null, listening = false, firstPaint = true;
  const articles = new Map();
  const fileInput=$("file-input"), attach=$("attach-file"), draftFiles=$("draft-files");
  let selectedFiles=[], filesRevision=0;
  const fileSignature=files=>JSON.stringify((files||[]).map(file=>[file.id,file.name,file.size]));
  const matches=(entry,row)=>row && row.text===entry.text && fileSignature(entry.files)===fileSignature(row.attachments);
  function renderSelected() {
    draftFiles.replaceChildren();
    selectedFiles.forEach(file=>{
      const row=document.createElement("div"), name=document.createElement("span"), remove=document.createElement("button");
      row.className="draft-file";name.textContent=file.name+" · "+new Intl.NumberFormat(undefined,{style:"unit",unit:"byte",unitDisplay:"short"}).format(file.size);
      remove.type="button";remove.textContent="Remove";remove.setAttribute("aria-label","Remove "+file.name);
      remove.addEventListener("click",async()=>{try{const version=generation,dest={...destination},next=selectedFiles.filter(item=>item.id!==file.id);await store.saveFiles(dest,next);if(version!==generation)return;selectedFiles=next;filesRevision++;cleared=null;draftID="";renderSelected();controls();}catch(e){error("Could not save the file selection. "+e.message);}});
      row.append(name,remove);draftFiles.append(row);
    });
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
  const sameDestination = dest => destination && store.destinationKey(dest) === store.destinationKey(destination);
  const deadline = 6 * 24 * 60 * 60 * 1000;
  async function refreshOutbox() { outbox = await store.list(); render(); }
  function applySnapshot(data) {
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
    for (const entry of outbox) if (sameDestination(entry.dest) && matches(entry,evidence.get("web:" + entry.id))) {
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
          if ([...(current.messages || []), ...(current.pending || [])].some(row => row.id === "web:" + entry.id && matches(entry,row))) {
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
          for(const file of entry.files||[]) {
            if(entry.uploaded.some(meta=>meta.id===file.id))continue;
            const result=await post("/api/chat/upload",{...entry.dest,upload:{id:file.id,name:file.name,data:await fileData(file.blob)}});
            if(!result.uploaded || result.uploaded.id!==file.id)throw new Error("Attachment upload was not confirmed.");
            entry.uploaded.push(result.uploaded);await store.update(entry);
          }
          await post("/api/chat/send", {...entry.dest, id: entry.id, text: entry.text, files:(entry.files||[]).map(file=>file.id)});
          await store.remove(entry.key);
          if (sameDestination(entry.dest)) read();
        } catch (e) {
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
  function saveDraft(dest = destination, text = input.value, id = draftID) { if (!dest) return; try { localStorage.setItem(storageKey(dest), JSON.stringify({text, id})); } catch (_) { /* optional browser persistence */ } }
  function restoreDraft() { selectedFiles=[];renderSelected();const revision=++filesRevision, version=generation, dest={...destination};store.loadFiles(dest).then(files=>{if(version!==generation||revision!==filesRevision)return;selectedFiles=files;renderSelected();controls();}).catch(e=>error("Could not restore draft files. "+e.message)); let saved = {}; try { saved = JSON.parse(localStorage.getItem(storageKey(destination)) || sessionStorage.getItem(storageKey(destination)) || "{}"); } catch (_) { /* optional */ } input.value = saved.text || ""; draftID = saved.id || ""; cleared = null; controls(); }
  try { language.value = localStorage.getItem("zmina-speech-language") || "ru-RU"; } catch (_) { /* optional */ }
  language.addEventListener("change", () => { try { localStorage.setItem("zmina-speech-language", language.value); } catch (_) { /* optional */ } });
  function controls() {
    send.disabled = !destination || loadingTargets || posting || listening || (!input.value.trim() && !selectedFiles.length);
    attach.disabled = !destination || loadingTargets || posting || listening;
    mic.disabled = !destination || loadingTargets || posting;
    input.readOnly = listening;
    $("clear-message").disabled = posting || (!input.value && !selectedFiles.length && !cleared);
    $("clear-message").setAttribute("aria-label", cleared ? "Undo clear" : "Clear draft");
    $("clear-message").title = cleared ? "Undo clear" : "Clear draft";
    mic.setAttribute("aria-pressed", String(listening)); mic.setAttribute("aria-label", listening ? "Stop dictation" : "Dictate");
    mic.title = listening ? "Stop dictation" : "Dictate";
    $("start-role").hidden = !snapshot || snapshot.state === "up";
    $("start-role").disabled = posting || !live;
    $("stop-turn").hidden = !snapshot?.active;
    $("stop-turn").disabled = posting || !live;
  }
  function render() {
    if (!destination) return;
    snapshot ||= {state: "unknown", messages: [], pending: []};
    const follow = firstPaint || atBottom(), oldScroll = conversation.scrollTop;
    const rows = [...(snapshot.messages || []), ...(snapshot.pending || [])], seen = new Set(rows.map(row => row.id));
    for (const entry of outbox) if (sameDestination(entry.dest) && !seen.has("web:" + entry.id)) rows.push({id: "web:" + entry.id, role: "user", text: entry.text, at: entry.at, state: entry.state, failure: entry.failure, outboxKey: entry.key, attachments:entry.files||[]});
    const keep = new Set();
    rows.forEach((row, index) => {
      keep.add(row.id);
      let article = articles.get(row.id);
      if (!article) { article = document.createElement("article"); article.className = "chat-message " + (row.event ? (row.event.kind==="session"?"event session":"event") : row.role === "user" ? "user" : "assistant"); const head = document.createElement("div"); head.className = "message-head"; const who = document.createElement("span"), time = document.createElement("time"); head.append(who, time); const body = document.createElement("div"); body.className = "message-body"; const state = document.createElement("div"); state.className = "message-state"; const files=document.createElement("div");files.className="message-files";article.append(head, body, files, state); articles.set(row.id, article); }
      const presentation=JSON.stringify({role:row.role,event:row.event||null});
      article.className="chat-message "+(row.event?(row.event.kind==="session"?"event session":"event"):row.role==="user"?"user":"assistant");
      article.querySelector(".message-head span").textContent = row.event ? (row.event.kind==="session"?"Session message":"Background task") : row.role === "user" ? "You" : destination.role;
      const time = article.querySelector("time"); const date = new Date(row.at); time.textContent = Number.isNaN(date.getTime()) ? "" : new Intl.DateTimeFormat(undefined, {hour: "2-digit", minute: "2-digit"}).format(date);
      if (article._text !== row.text || article._presentation!==presentation) { const body = article.querySelector(".message-body"); if (row.event) {
        const title = document.createElement("p"), summary = document.createElement("p");
        title.className = "event-status";
        title.textContent = ({completed: "Completed", failed: "Failed", stopped: "Stopped", running: "Running", received:"Received"})[row.event.status] || "Update";
        summary.textContent = row.event.summary;
        body.replaceChildren(title, summary);
        if (row.event.result) { const result = document.createElement("p"); result.className = "event-result"; result.textContent = row.event.result; body.append(result); }
        if (row.event.detail) { const details = document.createElement("details"), label = document.createElement("summary"), text = document.createElement("pre"); label.textContent = row.event.kind==="session"?"Full message":"Technical details"; text.textContent = row.event.detail; details.append(label, text); body.append(details); }
      } else if (row.role === "assistant") window.zminaChatContent.render(row.text, body); else body.textContent = row.text; article._text = row.text; article._presentation=presentation; }
      const fileRows=article.querySelector(".message-files"), signature=fileSignature(row.attachments)+Boolean(row.outboxKey);
      if(article._files!==signature){fileRows.replaceChildren();for(const file of row.attachments||[]){const item=document.createElement(row.outboxKey?"span":"a");item.textContent=file.name+" · "+new Intl.NumberFormat().format(file.size)+" bytes";if(!row.outboxKey){item.href="/api/chat/file?"+new URLSearchParams({...destination,id:file.id});item.download=file.name;}fileRows.append(item);}article._files=signature;}
      const state = article.querySelector(".message-state"); state.textContent = ({local: "Saved on this device · waiting to send", queued: "Queued · delivered when the role is idle", sent: "Sent to the role · awaiting agent confirmation", accepted: "Accepted by agent", unconfirmed: "Delivery unconfirmed · the conversation changed", expired: "Delivery unconfirmed · check Terminal before sending again", failed: "Delivery failed", sending: "Saving…", uncertain: "Delivery unconfirmed · retry with the same message ID"})[row.state] || ""; if (row.failure) state.textContent += " · " + row.failure;
      if (row.outboxKey && row.state === "failed") { const retry = document.createElement("button"); retry.type = "button"; retry.textContent = "Retry delivery"; retry.addEventListener("click", () => retryEntry(row.outboxKey).catch(e => error(e.message))); state.append(" ", retry); }
      if (row.outboxKey && ["failed", "expired"].includes(row.state)) {
        const remove = document.createElement("button"); remove.type = "button"; remove.textContent = "Remove saved message";
        remove.addEventListener("click", () => { if (confirm("Remove this message from this device’s delivery queue? It may already have reached the role.")) store.remove(row.outboxKey).then(refreshOutbox).then(drainOutbox).catch(e => error(e.message)); });
        state.append(" ", remove);
      }
      state.hidden = !state.textContent; state.classList.toggle("failed", ["failed", "uncertain"].includes(row.state));
      if (messages.children[index] !== article) messages.insertBefore(article, messages.children[index] || null);
    });
    for (const [id, article] of articles) if (!keep.has(id)) { article.remove(); articles.delete(id); }
    $("empty-chat").hidden = rows.length > 0;
    $("empty-chat").textContent = snapshot.session ? "No saved messages yet. Send a message to this role." : "No conversation yet. Start the role, then send a message.";
    $("runtime-info").textContent = [snapshot.runtime, snapshot.profile, snapshot.context ? new Intl.NumberFormat().format(snapshot.context) + " context tokens" : ""].filter(Boolean).join(" · ");
    $("agent-state").textContent = !live ? "Connecting… Showing saved conversation" : snapshot.state !== "up" ? "Role stopped · queued messages wait until it starts" : snapshot.needs_input ? "Needs input · open Terminal to respond" : snapshot.active ? "Working…" : snapshot.status === "unknown" || !snapshot.session ? "Status unavailable · open Terminal" : "Idle";
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
  async function post(path, body) { const controller = new AbortController(); const timeout = setTimeout(() => controller.abort(), path==="/api/chat/upload"?65000:35000); try { const response = await fetch(path, {signal: controller.signal, method: "POST", headers: {"Content-Type": "application/json", "X-Zmina": "1"}, body: JSON.stringify(body)}); if (!response.ok) { const error = new Error(await response.text()); error.status = response.status; throw error; } return await response.json(); } finally { clearTimeout(timeout); } }
  $("composer").addEventListener("submit", async event => {
    event.preventDefault(); if (send.disabled || !destination) return;
    const dest = {...destination}, text = input.value, version = generation, files=[...selectedFiles], revision=filesRevision;
    const id = draftID || uid(); draftID = id; saveDraft(); posting = true; cleared = null; error(""); note(""); controls();
    let accepted=false;
    try {
      await store.enqueue(dest, id, text, files);accepted=true;
      // Storage failure keeps the draft intact and sends nothing over the network.
      if (version === generation && input.value === text && revision===filesRevision) { input.value = ""; draftID = ""; selectedFiles=[];filesRevision++;renderSelected();await store.saveFiles(dest,[]);saveDraft(); }
      await refreshOutbox(); note("Message saved. Delivery resumes when connected."); drainOutbox();
    } catch (e) { error((accepted ? "Message saved. Could not finish updating this draft. " : "Message was not saved. Keep this draft and retry. ") + e.message); if(accepted){refreshOutbox().catch(()=>{});drainOutbox();} }
    finally { posting = false; controls(); }

  });
  async function action(name) { if (!destination || posting) return; const version = generation; posting = true; controls(); try { const data = await post("/api/chat/action", {...destination, action: name}); if (version === generation) { applySnapshot(data); error(""); } } catch (e) { if (version === generation) error(e.message); } finally { posting = false; controls(); } }
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
  if (params.get("crew") && params.get("role")) showDestination({node: params.get("node") || "", crew: params.get("crew"), role: params.get("role")});
  drainOutbox();
  (async () => { try { const response = await fetch("/api/terminal/nodes"); if (!response.ok) throw new Error("Nodes unavailable. Reload this page."); const nodes = await response.json(); node.replaceChildren(); nodes.forEach(row => node.add(new Option(row.local ? `${row.name || "This node"} · local` : row.name, row.local ? "" : row.name))); if ([...node.options].some(option => option.value === params.get("node"))) node.value = params.get("node"); await loadTargets(); } catch (e) { error(e.message); } })();
})();
