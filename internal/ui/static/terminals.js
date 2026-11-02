/* Terminal bytes stay bytes: xterm renders output; nothing from a role becomes HTML. */
(() => {
  "use strict";
  if (new URLSearchParams(location.search).get("view") !== "terminal") { location.replace("chat.html" + location.search); return; }
  const byId = id => document.getElementById(id);
  const node = byId("node"), target = byId("target"), status = byId("status");
  const connect = byId("connect"), disconnect = byId("disconnect");
  const params = new URLSearchParams(location.search);
  const historyView = byId("history-view"), historyText = byId("history-text"), historyToggle = byId("history-toggle");
  let historyRequest = null, historyReady = false, liveTarget = null, touchY = null, touchX = null;
  const settings = byId("session-settings"), title = byId("session-title");
  let connected = false, keyboard = false, currentSession = "", currentLabel = "", inputRuntime = "claude";
  const draft = byId("draft"), draftPanel = byId("draft-panel"), speechStatus = byId("speech-status"), mic = byId("dictate");
  const language = byId("speech-language"), sendDraft = byId("send-draft");
  const Recognition = window.SpeechRecognition || window.webkitSpeechRecognition;
  let recognition = null, listening = false, draftSession = "", draftLabel = "", baseDraft = "", clearedDraft = null;
  try { language.value = localStorage.getItem("zmina-speech-language") || "ru-RU"; } catch (_) { /* storage may be disabled */ }
  language.addEventListener("change", () => { try { localStorage.setItem("zmina-speech-language", language.value); } catch (_) { /* optional */ } });
  const theme = () => { const css = getComputedStyle(document.documentElement); return {background: css.getPropertyValue("--bg").trim(), foreground: css.getPropertyValue("--ink").trim(), cursor: css.getPropertyValue("--ink").trim()}; };
  const terminal = new Terminal({fontSize: 16, fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace", theme: theme(), scrollback: 5000, allowProposedApi: false});
  const fit = new FitAddon.FitAddon(); terminal.loadAddon(fit); terminal.open(byId("terminal"));
  let socket = null, targets = [], loading = 0;
  const encoder = new TextEncoder();
  function report(text, failed = false) { status.textContent = text; status.classList.toggle("failed", failed); status.hidden = connected && !failed; }
  function speechReport(text, failed = false) { speechStatus.hidden = !text; speechStatus.textContent = text; speechStatus.classList.toggle("failed", failed); }
  function draftState() {
    draftPanel.hidden = !listening && !draft.value;
    sendDraft.hidden = !draft.value.trim();
    byId("clear-draft").disabled = !draft.value && !clearedDraft;
    byId("clear-draft").textContent = clearedDraft ? "Undo clear" : "Clear message";
    sendDraft.disabled = !connected || listening || !draft.value.trim() || draftSession !== currentSession;
    byId("edit-draft").disabled = listening || !draft.value;
    byId("draft-destination").textContent = draft.value || listening ? `· ${draftLabel}` : "";
  }
  function setKeyboard(enabled) {
    keyboard = enabled;
    terminal.textarea.readOnly = !enabled; terminal.textarea.inputMode = enabled ? "text" : "none";
    byId("keyboard").setAttribute("aria-pressed", String(enabled));
    if (enabled) { showLive(); terminal.focus(); } else terminal.blur();
  }
  function micState(active) { mic.setAttribute("aria-label", active ? "Stop dictation" : "Dictate"); mic.setAttribute("title", active ? "Stop dictation" : "Dictate"); mic.setAttribute("aria-pressed", String(active)); }
  function finishSpeech(abort = false) {
    if (!recognition) return;
    if (abort) {
      const old = recognition; recognition = null; listening = false;
      micState(false); old.abort(); draftState();
    } else recognition.stop();
  }
  function keys(enabled) {
    connected = enabled;
    document.querySelectorAll(".term-keys button, [data-key], #keyboard").forEach(button => { button.disabled = !enabled; });
    mic.disabled = !enabled;
    historyToggle.disabled = !enabled;
    byId("restore-input").disabled = !enabled || inputRuntime === "codex";
    draftState();
  }
  setKeyboard(false);
  function stop() { showLive(); finishSpeech(true); setKeyboard(false); const old = socket; socket = null; if (old) old.close(); keys(false); disconnect.disabled = true; connect.disabled = !target.value; }
  function size() { fit.fit(); if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify({type: "resize", cols: Math.max(2, Math.min(terminal.cols, 500)), rows: Math.max(2, Math.min(terminal.rows, 300))})); }
  new ResizeObserver(size).observe(byId("terminal"));
  window.visualViewport?.addEventListener("resize", () => { document.body.style.height = window.visualViewport.height + "px"; size(); });
  matchMedia("(prefers-color-scheme: light)").addEventListener("change", () => { terminal.options.theme = theme(); });
  terminal.onData(data => { if (socket?.readyState === WebSocket.OPEN) socket.send(encoder.encode(data)); });
  terminal.onBinary(data => { if (socket?.readyState === WebSocket.OPEN) socket.send(Uint8Array.from(data, char => char.charCodeAt(0))); });
  function save() { const chosen = targets[Number(target.value)]; const state = new URLSearchParams({view: "terminal"}); if (node.value) state.set("node", node.value); if (chosen) { state.set("crew", chosen.crew); state.set("role", chosen.role); } history.replaceState(null, "", location.pathname + "?" + state); const chatState = new URLSearchParams(state); chatState.delete("view"); byId("chat-link").href = "chat.html?" + chatState; }
  async function loadTargets() {
    stop(); const generation = ++loading; target.replaceChildren(new Option("Loading roles…", "")); connect.disabled = true; report("Loading roles…");
    try {
      const response = await fetch("/api/terminal/targets?" + new URLSearchParams({node: node.value}));
      if (!response.ok) throw new Error(await response.text());
      const rows = await response.json(); if (generation !== loading) return;
      targets = rows; target.replaceChildren(new Option("Choose a role", ""));
      rows.forEach((row, index) => { const option = new Option(`${row.crew} / ${row.role} · ${row.state}`, String(index)); option.disabled = row.state !== "up"; target.add(option); });
      const wanted = rows.findIndex(row => row.crew === params.get("crew") && row.role === params.get("role") && row.state === "up");
      if (wanted >= 0) target.value = String(wanted);
      connect.disabled = !target.value; report(rows.some(row => row.state === "up") ? "Choose a role, then connect." : "No running roles. Start a role, then refresh.");
    } catch (error) { if (generation !== loading) return; target.replaceChildren(new Option("Roles unavailable", "")); report(error.message, true); }
  }
  connect.addEventListener("click", () => {
    const chosen = targets[Number(target.value)]; if (!chosen) return;
    stop(); save(); terminal.reset(); fit.fit(); report("Connecting…"); connect.disabled = true;
    currentSession = JSON.stringify([node.value, chosen.crew, chosen.role]);
    inputRuntime = chosen.runtime || "claude";
    liveTarget = {node: node.value, crew: chosen.crew, role: chosen.role};
    currentLabel = `${node.options[node.selectedIndex]?.textContent.replace(" · local", "") || "local"} / ${chosen.crew} / ${chosen.role}`;
    title.textContent = currentLabel;
    const query = new URLSearchParams({node: node.value, crew: chosen.crew, role: chosen.role, cols: Math.max(2, Math.min(terminal.cols, 500)), rows: Math.max(2, Math.min(terminal.rows, 300))});
    const ws = new WebSocket(`${location.protocol === "https:" ? "wss:" : "ws:"}//${location.host}/api/terminal/connect?${query}`);
    socket = ws; ws.binaryType = "arraybuffer";
    let lastError = "";
    ws.onmessage = event => {
      if (socket !== ws) return;
      if (typeof event.data !== "string") { terminal.write(new Uint8Array(event.data)); return; }
      const message = JSON.parse(event.data);
      if (message.error) { lastError = message.error; report(message.error, true); }
      if (message.status === "connected") { keys(true); disconnect.disabled = false; report(`Connected · ${chosen.crew} / ${chosen.role}`); size(); settings.open = false; if (keyboard) terminal.focus(); }
    };
    ws.onclose = () => { if (socket !== ws) return; stop(); report(lastError || "Disconnected. Reconnect to the same role; it keeps running.", true); };
    ws.onerror = () => { if (socket === ws) report("Connection failed. Check this node’s connection, then reconnect.", true); };
  });
  disconnect.addEventListener("click", () => { stop(); report("Disconnected. The role keeps running."); });
  byId("refresh").addEventListener("click", loadTargets);
  node.addEventListener("change", loadTargets);
  target.addEventListener("change", () => { stop(); save(); report("Ready to connect."); });
  const sequences = {"clear-input": "\x05\x15", "restore-input": "\x19", escape: "\x1b", tab: "\t", interrupt: "\x03", up: "\x1b[A", down: "\x1b[B", enter: "\r"};
  document.querySelectorAll("[data-key]").forEach(button => button.addEventListener("click", () => { if (socket?.readyState === WebSocket.OPEN) { showLive(); socket.send(encoder.encode(sequences[button.dataset.key])); } if (keyboard) terminal.focus(); }));
  byId("keyboard").addEventListener("click", () => { finishSpeech(); setKeyboard(!keyboard); });
  mic.addEventListener("click", () => {
    if (recognition) { finishSpeech(); return; }
    if (!window.isSecureContext) { speechReport("Dictation needs HTTPS. Open the private Tailscale HTTPS link.", true); return; }
    if (!Recognition) { speechReport("Dictation is unavailable in this browser. Open this page in Safari, or use Keyboard.", true); return; }
    if (draft.value && draftSession !== currentSession) { speechReport("A draft belongs to another role. Clear it before dictating here.", true); return; }
    setKeyboard(false); draft.readOnly = true; draft.blur();
    clearedDraft = null; draftSession = currentSession; draftLabel = currentLabel; baseDraft = draft.value.trim();
    const active = new Recognition(); recognition = active; listening = true; micState(true); draftState();
    active.lang = language.value; active.continuous = false; active.interimResults = true; active.maxAlternatives = 1;
    active.onstart = () => {
      if (recognition !== active) return;
      listening = true; micState(true); speechReport("Listening… Tap Stop dictation when finished."); draftState();
    };
    active.onresult = event => {
      if (recognition !== active) return;
      const words = Array.from(event.results, result => result[0].transcript).join(" ");
      draft.value = [baseDraft, words].filter(Boolean).join(" ").slice(0, 16000); draftState();
    };
    active.onerror = event => {
      if (recognition !== active) return;
      const errors = {"not-allowed": "Allow microphone and speech recognition in Safari’s website settings, then try again.", "service-not-allowed": "Enable Siri or Dictation in iPhone Settings, then try again.", "audio-capture": "Microphone unavailable. Check its permission, then try again.", "no-speech": "No speech heard. Tap Dictate to try again.", "network": "Speech recognition lost its connection. Your draft is kept; try again.", "language-not-supported": "This dictation language is unavailable. Choose another in Session."};
      if (event.error !== "aborted") speechReport(errors[event.error] || "Dictation stopped. Your draft is kept; try again.", true);
    };
    active.onend = () => {
      if (recognition !== active) return;
      recognition = null; listening = false; micState(false);
      if (!speechStatus.classList.contains("failed")) speechReport("");
      draftState();
    };
    try { active.start(); } catch (_) { recognition = null; listening = false; micState(false); speechReport("Could not start dictation. Check Safari’s microphone permission and try again.", true); draftState(); }
  });
  byId("edit-draft").addEventListener("click", () => { finishSpeech(); clearedDraft = null; draft.readOnly = false; draft.focus(); draftState(); });
  draft.addEventListener("input", () => { if (!draftSession) { draftSession = currentSession; draftLabel = currentLabel; } draftState(); });
  byId("clear-draft").addEventListener("click", () => {
    finishSpeech(true);
    if (clearedDraft) { draft.value = clearedDraft.text; draftSession = clearedDraft.session; draftLabel = clearedDraft.label; clearedDraft = null; }
    else { clearedDraft = draft.value ? {text: draft.value, session: draftSession, label: draftLabel} : null; draft.value = ""; baseDraft = ""; draftSession = ""; }
    draft.readOnly = true; draft.blur(); speechReport(""); draftState();
  });
  sendDraft.addEventListener("click", () => {
    if (!connected || listening || draftSession !== currentSession || socket?.readyState !== WebSocket.OPEN) return;
    const text = draft.value.replace(/[\x00-\x1f\x7f]/g, " ").replace(/\s+/g, " ").trim(); if (!text) return;
    showLive(); socket.send(encoder.encode(text + "\r")); clearedDraft = null; draft.value = ""; draftSession = ""; draft.readOnly = true; draft.blur(); speechReport(""); draftState();
  });
  document.addEventListener("keydown", event => { if (event.key === "Escape") { settings.open = false; byId("key-menu").open = false; } });
  document.addEventListener("pointerdown", event => { if (!settings.contains(event.target)) settings.open = false; if (!byId("key-menu").contains(event.target)) byId("key-menu").open = false; });
  window.addEventListener("beforeunload", event => { if (draft.value.trim()) { event.preventDefault(); event.returnValue = ""; } });
  function showLive() {
    historyRequest?.abort(); historyRequest = null; historyReady = false;
    historyView.hidden = true; historyToggle.setAttribute("aria-pressed", "false");
  }
  async function showHistory(distance = 120) {
    if (!connected || !liveTarget || !historyView.hidden) return;
    setKeyboard(false); historyView.hidden = false; historyToggle.setAttribute("aria-pressed", "true");
    historyReady = false; historyText.textContent = "Loading terminal history…";
    const request = new AbortController(); historyRequest = request;
    const forSession = currentSession;
    try {
      const response = await fetch("/api/terminal/history?" + new URLSearchParams(liveTarget), {signal: request.signal, cache: "no-store"});
      if (!response.ok) throw new Error(await response.text());
      const snapshot = await response.json();
      if (request.signal.aborted || forSession !== currentSession) return;
      historyText.textContent = snapshot.text || "No terminal history has been retained yet.";
      historyText.scrollTop = Math.max(0, historyText.scrollHeight - historyText.clientHeight - distance);
      requestAnimationFrame(() => { if (!historyView.hidden && forSession === currentSession) historyReady = true; });
    } catch (error) {
      if (request.signal.aborted) return;
      historyText.textContent = "History unavailable: " + error.message + "\n\nReturn to Latest, then try History again.";
    }
  }
  historyToggle.addEventListener("click", () => { historyView.hidden ? showHistory() : showLive(); });
  byId("history-return").addEventListener("click", showLive);
  historyText.addEventListener("scroll", () => { if (historyReady && historyText.scrollHeight - historyText.clientHeight - historyText.scrollTop < 2) showLive(); });
  // tmux redraws an alternate screen. Native xterm scrolling cannot recover the pane's older output.
  // Intercept an upward wheel/downward finger swipe before xterm can translate it into agent keys.
  const screen = byId("terminal");
  screen.addEventListener("wheel", event => {
    if (!connected || !historyView.hidden) return;
    event.preventDefault(); event.stopImmediatePropagation(); if (event.deltaY < 0) showHistory(Math.max(120, Math.abs(event.deltaY)));
  }, {capture: true, passive: false});
  screen.addEventListener("touchstart", event => {
    if (!historyView.hidden || event.touches.length !== 1) { touchY = null; return; }
    touchY = event.touches[0].clientY; touchX = event.touches[0].clientX;
  }, {capture: true, passive: true});
  screen.addEventListener("touchmove", event => {
    if (!historyView.hidden || touchY === null || event.touches.length !== 1) return;
    const dy = event.touches[0].clientY - touchY, dx = event.touches[0].clientX - touchX;
    if (Math.abs(dy) > 12 && Math.abs(dy) > Math.abs(dx)) {
      event.preventDefault(); event.stopImmediatePropagation(); touchY = null; if (dy > 0) showHistory(Math.max(120, dy));
    }
  }, {capture: true, passive: false});
  screen.addEventListener("touchend", () => { touchY = null; }, {capture: true, passive: true});
  window.addEventListener("pagehide", stop);
  (async () => {
    try {
      const response = await fetch("/api/terminal/nodes"); if (!response.ok) throw new Error("Nodes unavailable. Reload the page.");
      const nodes = await response.json(); node.replaceChildren();
      nodes.forEach(row => node.add(new Option(row.local ? `${row.name || "This node"} · local` : row.name, row.local ? "" : row.name)));
      if ([...node.options].some(option => option.value === params.get("node"))) node.value = params.get("node");
      await loadTargets();
    } catch (error) { report(error.message, true); }
  })();
})();
