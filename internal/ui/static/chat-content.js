/* One content renderer for both runtimes. Unknown formats remain readable text. */
(() => {
  "use strict";
  const escape = text => String(text).replace(/[&<>"']/g, c => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"})[c]);
  const parser = new marked.Marked({gfm: true, breaks: false, renderer: {
    // HTML/XML from an agent is content, not a second application inside the chat.
    html(token) { return token.block ? `<pre><code>${escape(token.text)}</code></pre>` : `<code>${escape(token.text)}</code>`; },
    image({href, text}) { return `<a href="${escape(href)}">Image: ${escape(text || "open image")}</a>`; },
    checkbox({checked}) { return `<span class="task-check" aria-label="${checked ? "Completed" : "Not completed"}">${checked ? "[x]" : "[ ]"}</span> `; }
  }});
  function region(element, label) {
    element.tabIndex = 0;
    element.setAttribute("role", "region");
    element.setAttribute("aria-label", label);
  }
  function render(text, body) {
    try {
      body.innerHTML = DOMPurify.sanitize(parser.parse(text), {
        USE_PROFILES: {html: true},
        FORBID_TAGS: ["img", "audio", "video", "iframe", "object", "embed", "form", "input", "button", "textarea", "style"],
        FORBID_ATTR: ["id", "name", "style"]
      });
    } catch (_) { body.textContent = text; return; }
    body.querySelectorAll("a").forEach(link => {
      try {
        const url = new URL(link.getAttribute("href") || "", location.href);
        if (!["http:", "https:"].includes(url.protocol)) link.removeAttribute("href");
        else { link.target = "_blank"; link.rel = "noopener noreferrer"; }
      } catch (_) { link.removeAttribute("href"); }
    });
    body.querySelectorAll("table").forEach(table => {
      const frame = document.createElement("div"), hint = document.createElement("p"), scroll = document.createElement("div");
      frame.className = "message-table"; hint.className = "table-hint"; hint.textContent = "Table · scroll sideways";
      scroll.className = "table-scroll"; region(scroll, "Table, scroll horizontally to read more columns");
      table.before(frame); frame.append(hint, scroll); scroll.append(table);
    });
    body.querySelectorAll("pre").forEach(pre => {
      const frame = document.createElement("div"), bar = document.createElement("div"), label = document.createElement("span"), copy = document.createElement("button");
      const code = pre.querySelector("code"), language = [...(code?.classList || [])].find(name => name.startsWith("language-"))?.slice(9);
      frame.className = "message-code"; bar.className = "code-bar"; label.textContent = language || "Text";
      copy.type = "button"; copy.textContent = "Copy"; copy.setAttribute("aria-label", "Copy code or text block"); copy.setAttribute("aria-live", "polite");
      copy.addEventListener("click", async () => {
        try { await navigator.clipboard.writeText(pre.textContent); copy.textContent = "Copied"; }
        catch (_) { const range = document.createRange(); range.selectNodeContents(pre); const selection = window.getSelection(); selection.removeAllRanges(); selection.addRange(range); copy.textContent = "Select and copy"; }
      });
      region(pre, language ? language + " code, scroll horizontally" : "Text block, scroll horizontally");
      pre.before(frame); frame.append(bar, pre); bar.append(label, copy);
    });
  }
  window.zminaChatContent = Object.freeze({render});
})();
