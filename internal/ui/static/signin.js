// The sign-in page (D-10): ask for a one-time code (sent to the owner's Telegram by zmina ui's --code-command), then
// enter it. A right code sets the same device cookie as the ?token= link, and the page it stood for opens. The step
// survives a reload while the code lives: iOS may reload a home-screen app left for Telegram.
"use strict";
(() => {
  const $ = (id) => document.getElementById(id);
  const KEY = "zmina.signin", TTL = 5 * 60e3, GAP = 60;
  const keep = (v) => { try { v ? localStorage.setItem(KEY, JSON.stringify(v)) : localStorage.removeItem(KEY); } catch (_) { /* this visit only */ } };
  const kept = () => { try { return JSON.parse(localStorage.getItem(KEY) || "null"); } catch (_) { return null; } };
  const post = (path, body) => fetch(path, { method: "POST", headers: { "Content-Type": "application/json", "X-Zmina": "1" }, body: JSON.stringify(body || {}), cache: "no-store" });
  const field = $("code");
  let timer = 0;

  function say(id, text, bad) { $(id).textContent = text; $(id).classList.toggle("bad", !!bad); }
  function invalid(on) { field.setAttribute("aria-invalid", on ? "true" : "false"); }

  // countdown keeps «Send a new code» plain text, «New code in N s», for the seconds left.
  function countdown(s) {
    clearInterval(timer);
    const b = $("again");
    let left = Math.max(0, Math.ceil(s));
    const tick = () => {
      if (left <= 0) { clearInterval(timer); b.disabled = false; b.textContent = "Send a new code"; return; }
      b.disabled = true;
      b.textContent = "New code in " + left + " s";
      left--;
    };
    tick();
    timer = setInterval(tick, 1000);
  }

  // step2 is the code step: what happened, the field, the countdown to a new code.
  function step2(wait) {
    $("ask").hidden = true;
    $("enter").hidden = false;
    $("head").textContent = "Code sent to Telegram";
    $("lead").textContent = "Enter the 6 digits. The code works once, for 5 minutes.";
    field.value = "";
    invalid(false);
    field.focus();
    countdown(wait);
  }

  async function ask(status) {
    const button = status === "ask-status" ? $("send") : $("again");
    button.disabled = true;
    say(status, "Sending…");
    try {
      const r = await post("/api/signin/code");
      const data = await r.json().catch(() => ({}));
      if (r.status === 429) {
        // A code went out a moment ago (this page reloaded, or another one asked): its field is the way on.
        if (status === "ask-status") { button.disabled = false; step2(data.wait || GAP); }
        else countdown(data.wait || GAP);
        say("status", "A code was sent a moment ago. Use it, or wait for a new one.", true);
        return;
      }
      if (!r.ok) { say(status, "The code could not be sent. Try again in a minute.", true); button.disabled = false; return; }
      keep({ sentAt: Date.now() });
      say("ask-status", "");
      say("status", "");
      step2(GAP);
    } catch (_) {
      say(status, "No connection. Try again.", true);
      button.disabled = false;
    }
  }

  // open loads the page this one stood for. An old ?token= in its address would win over the new cookie and bring
  // this page back, so it goes; with the same address a reload, since a navigation to it with a #tab in it would
  // only scroll.
  function open() {
    const u = new URL(location.href);
    if (!u.searchParams.has("token")) { location.reload(); return; }
    u.searchParams.delete("token");
    location.replace(u.pathname + u.search + u.hash);
  }

  $("ask").addEventListener("submit", (e) => { e.preventDefault(); ask("ask-status"); });
  $("again").addEventListener("click", () => ask("status"));
  field.addEventListener("input", () => {
    field.value = field.value.replace(/\D/g, "").slice(0, 6);
    if (field.getAttribute("aria-invalid") === "true") { invalid(false); say("status", ""); }
    if (field.value.length === 6) $("enter").requestSubmit();
  });
  $("enter").addEventListener("submit", async (e) => {
    e.preventDefault();
    const code = field.value;
    if (!/^\d{6}$/.test(code)) { invalid(true); say("status", "The code is 6 digits.", true); return; }
    $("go").disabled = true;
    say("status", "Checking…");
    try {
      const r = await post("/api/signin/verify", { code });
      if (r.ok) { keep(null); say("status", "Signed in. Opening…"); open(); return; }
      invalid(true);
      say("status", "Wrong or expired code. Check it, or send a new one.", true);
      field.value = "";
      field.focus();
    } catch (_) {
      say("status", "No connection. Try again.", true);
    }
    $("go").disabled = false;
  });

  // A code asked for less than five minutes ago: open on its step, with what is left of the wait.
  const was = kept();
  if (was && Date.now() - was.sentAt < TTL) step2(GAP - (Date.now() - was.sentAt) / 1000);
  else keep(null);
})();
