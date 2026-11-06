// The sign-in page: ask for a one-time code (sent to the owner's Telegram by zmina ui's --code-command), then enter
// it. A right code sets the same device cookie as the ?token= link, and the page it stood for opens.
"use strict";
(() => {
  const $ = (id) => document.getElementById(id);
  const say = (text, bad) => { $("status").textContent = text; $("status").classList.toggle("bad", !!bad); };
  const post = (path, body) => fetch(path, { method: "POST", headers: { "Content-Type": "application/json", "X-Zmina": "1" }, body: JSON.stringify(body || {}), cache: "no-store" });
  let waitTimer = 0;

  // wait keeps «Send code» off for the seconds the server asked.
  function wait(button, s) {
    clearInterval(waitTimer);
    const label = button.textContent;
    let left = s;
    button.disabled = true;
    const tick = () => {
      if (left <= 0) { clearInterval(waitTimer); button.disabled = false; button.textContent = label; return; }
      button.textContent = label + " · " + left + " s";
      left--;
    };
    tick();
    waitTimer = setInterval(tick, 1000);
  }

  async function ask(button) {
    button.disabled = true;
    say("Sending…");
    try {
      const r = await post("/api/signin/code");
      const data = await r.json().catch(() => ({}));
      if (r.status === 429) { say("A code was sent a moment ago. Use it, or wait for a new one.", true); button.disabled = false; wait(button, data.wait || 60); return; }
      if (!r.ok) { say("The code could not be sent. Try again in a minute.", true); button.disabled = false; return; }
      $("ask").hidden = true;
      $("enter").hidden = false;
      $("lead").textContent = "Enter the 6-digit code from Telegram. It works once, for 5 minutes.";
      say("");
      $("code").value = "";
      $("code").focus();
      wait($("again"), 60);
    } catch (_) {
      say("No connection. Try again.", true);
      button.disabled = false;
    }
  }

  $("ask").addEventListener("submit", (e) => { e.preventDefault(); ask($("send")); });
  $("again").addEventListener("click", () => ask($("again")));
  $("code").addEventListener("input", () => {
    $("code").value = $("code").value.replace(/\D/g, "").slice(0, 6);
    if ($("code").value.length === 6) $("enter").requestSubmit();
  });
  $("enter").addEventListener("submit", async (e) => {
    e.preventDefault();
    const code = $("code").value;
    if (!/^\d{6}$/.test(code)) { say("The code is 6 digits.", true); return; }
    $("go").disabled = true;
    say("Checking…");
    try {
      const r = await post("/api/signin/verify", { code });
      if (r.ok) { say("Signed in."); location.replace(location.pathname + location.search + location.hash); return; }
      say("Wrong or expired code. Check it, or send a new one.", true);
      $("code").select();
    } catch (_) {
      say("No connection. Try again.", true);
    }
    $("go").disabled = false;
  });
})();
