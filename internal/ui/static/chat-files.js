/* D-16: attachments in Chat, both ways. The owner's files sit above his bubble — images as thumbnails, other files as
   chips — with their upload's progress; a role's files are the paths it names that its node may serve (images and
   pdfs under its dir, its task worktrees or the temp dir, POST /api/chat/paths): a message part that is only paths
   becomes a gallery, a path in a sentence its file name. A tap on an image opens the viewer. */
(() => {
  "use strict";
  let hooks = null;
  const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; };
  const phone = () => document.body.classList.contains("phone") || matchMedia("(max-width: 860px)").matches;
  const isImage = (type) => /^image\/(png|jpeg|gif|webp)$/.test(type || "");
  const svg = (paths) => `<svg viewBox="0 0 24 24" aria-hidden="true">${paths}</svg>`;
  const ICON = {
    base: '<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z"/><path d="M14 2v4a2 2 0 0 0 2 2h4"/>',
    text: '<path d="M10 9H8"/><path d="M16 13H8"/><path d="M16 17H8"/>',
    archive: '<path d="M10 6h1"/><path d="M10 9h1"/><path d="M10 12h1"/><circle cx="10.5" cy="17" r="2"/>',
    video: '<path d="m10 11 5 3-5 3v-6Z"/>',
  };
  // o2: the chip's icon by the file's kind.
  function icon(name) {
    const ext = (name.split(".").pop() || "").toLowerCase();
    const extra = ["zip", "tar", "gz", "tgz", "7z", "rar"].includes(ext) ? ICON.archive : ["mp4", "mov", "webm", "m4v"].includes(ext) ? ICON.video
      : ["pdf", "txt", "md", "csv", "json", "doc", "docx", "rtf", "log"].includes(ext) ? ICON.text : "";
    return svg(ICON.base + extra);
  }
  // o2: sizes in KB under 1 MB, else MB with one decimal; never bytes.
  function size(n) {
    if (!(n >= 0)) return "";
    if (n < 1000 * 1000) return Math.max(1, Math.round(n / 1000)) + " KB";
    return (n / 1000 / 1000).toFixed(1) + " MB";
  }
  const kindWord = (name) => { const ext = name.includes(".") ? name.split(".").pop() : ""; return ext ? ext.toUpperCase() : "File"; };

  function chip(name, bytes, href, sub) {
    const c = el(href ? "a" : "div", "fchip");
    if (href) { c.href = href; c.target = "_blank"; c.rel = "noopener"; c.title = name; }
    const tile = el("span", "fchip-icon"); tile.innerHTML = icon(name);
    const text = el("span", "fchip-text");
    text.append(el("span", "fchip-name", name), el("span", "fchip-sub", sub || kindWord(name) + " · " + size(bytes)));
    c.append(tile, text);
    return c;
  }

  // ---- the owner's files (o1–o3) ----
  const blobs = new Map(); // file id → object URL of the bytes this device still holds
  const fileURL = (dest, id, inline) => "/api/chat/file?" + new URLSearchParams({node: dest.node || "", crew: dest.crew, role: dest.role, id, ...(inline ? {inline: "1"} : {})});
  function ownURL(dest, file) {
    if (file.blob) { if (!blobs.has(file.id)) blobs.set(file.id, URL.createObjectURL(file.blob)); return blobs.get(file.id); }
    return fileURL(dest, file.id, true);
  }

  // ownerBlock draws a message's files: images first (one natural, two to four as squares, the fourth «+N»), then
  // chips. Progress is applied apart (progress()), so a redraw while bytes go up keeps the same images.
  function ownerBlock(box, row, dest) {
    const files = row.attachments || [];
    box.replaceChildren();
    box.hidden = !files.length;
    if (!files.length) return;
    const images = files.filter((f) => isImage(f.type)), others = files.filter((f) => !isImage(f.type));
    const list = images.map((f) => ({name: f.name, src: ownURL(dest, f), open: f.blob ? ownURL(dest, f) : fileURL(dest, f.id, true)}));
    if (images.length) {
      const grid = el("div", "fimgs" + (images.length === 1 ? " one" : ""));
      images.slice(0, 4).forEach((f, i) => {
        const tile = el("button", "fimg");
        tile.type = "button";
        tile.dataset.file = f.id;
        tile.setAttribute("aria-label", "Open " + f.name);
        const img = el("img"); img.src = list[i].src; img.alt = f.name; img.loading = "lazy"; img.decoding = "async";
        tile.append(img, el("i", "fbar"));
        if (i === 3 && images.length > 4) tile.append(el("b", "fmore", "+" + (images.length - 4)));
        tile.addEventListener("click", () => view(list, i, tile));
        grid.append(tile);
      });
      box.append(grid);
    }
    for (const f of others) {
      const c = chip(f.name, f.size, f.blob ? "" : fileURL(dest, f.id, true));
      c.dataset.file = f.id;
      box.append(c);
    }
  }
  // progress marks what is going up (o3): a tile at 60 % with a bar, a chip «Uploading · 40 %»; a failed one «!».
  function progress(box, row) {
    const up = row.state === "uploading" || row.state === "transmitting" || row.state === "local", failed = row.state === "failed" || (row.state === "local" && row.failure);
    for (const t of box.querySelectorAll("[data-file]")) {
      const id = t.dataset.file, done = !row.outboxKey || (row.uploaded || []).includes(id);
      const frac = done ? 1 : row.uploadFile === id ? row.uploadProgress || 0 : 0;
      t.classList.toggle("going", Boolean(row.outboxKey) && up && !failed && (!done || row.state !== "transmitting"));
      t.classList.toggle("failed", Boolean(row.outboxKey) && failed && !done);
      t.style.setProperty("--p", String(frac));
      if (t.classList.contains("fchip")) {
        const sub = t.querySelector(".fchip-sub"), file = (row.attachments || []).find((f) => f.id === id);
        const going = t.classList.contains("going") && !done;
        const words = going ? "Uploading · " + Math.round(frac * 100) + " %" : kindWord(file.name) + " · " + size(file.size);
        if (sub.textContent !== words) sub.textContent = words;
      }
      let mark = t.querySelector(".fbad");
      if (t.classList.contains("failed") && !mark && !t.classList.contains("fchip")) { mark = el("b", "fbad", "!"); t.append(mark); }
      if (!t.classList.contains("failed") && mark) mark.remove();
    }
  }

  // ---- a role's files (r1–r3) ----
  const PATH = /(?<![\w./~-])(?:~\/|\/)(?:[^\s"'`<>()[\]{}|*?\\]+\/)*[^\s"'`<>()[\]{}|*?\\/]+\.(?:png|jpe?g|gif|webp|pdf)(?![\w/-]|\.\w)/gi; // a sentence's full stop after it is not part of it
  const known = new Map(); // dest|path → {info, at}; info null when the node does not show it
  const waiting = new Map(); // dest key → {dest, paths:Set}
  const TTL = 60000, MISS = 20000;
  let timer = 0;
  const destKey = (d) => (d.node || "") + "|" + d.crew + "|" + d.role;
  const roleURL = (dest, path, download) => "/api/chat/rolefile?" + new URLSearchParams({node: dest.node || "", crew: dest.crew, role: dest.role, path, ...(download ? {download: "1"} : {})});
  function lookup(dest, path) {
    const k = destKey(dest) + "|" + path, hit = known.get(k);
    if (hit && Date.now() - hit.at < (hit.info ? TTL : MISS)) return hit;
    if (!waiting.has(destKey(dest))) waiting.set(destKey(dest), {dest, paths: new Set()});
    waiting.get(destKey(dest)).paths.add(path);
    if (!timer) timer = setTimeout(load, 80);
    return hit;
  }
  async function load() {
    timer = 0;
    const groups = [...waiting.values()];
    waiting.clear();
    let changed = false;
    await Promise.all(groups.map(async ({dest, paths}) => {
      const list = [...paths].slice(0, 64), at = Date.now();
      try {
        const data = await hooks.post("/api/chat/paths", {...dest, paths: list});
        const found = new Map((data.paths || []).map((p) => [p.path, p]));
        for (const p of list) {
          const k = destKey(dest) + "|" + p, before = known.get(k)?.info;
          const info = found.get(p) || null;
          if (JSON.stringify(before || null) !== JSON.stringify(info)) changed = true;
          known.set(k, {info, at});
        }
      } catch (_) {
        // An older node, or none: the paths stay text, asked again later.
        for (const p of list) known.set(destKey(dest) + "|" + p, {info: null, at});
      }
    }));
    if (changed) hooks.redraw();
  }

  // pathNodes are the text nodes of a body that name paths, with each match.
  function pathNodes(body) {
    const out = [];
    const walk = document.createTreeWalker(body, NodeFilter.SHOW_TEXT, {acceptNode: (n) =>
      n.parentElement.closest("a, button, .fgal, .fchip") ? NodeFilter.FILTER_REJECT : (PATH.lastIndex = 0, PATH.test(n.data)) ? NodeFilter.FILTER_ACCEPT : NodeFilter.FILTER_REJECT});
    while (walk.nextNode()) {
      const n = walk.currentNode, found = [];
      PATH.lastIndex = 0;
      for (let m; (m = PATH.exec(n.data));) found.push({path: m[0], at: m.index});
      out.push({node: n, found});
    }
    return out;
  }
  // onlyPaths: the element's text, without the given paths, has no words left (bullets, commas, colons at most).
  function onlyPaths(e, paths) {
    let t = e.textContent;
    for (const p of paths) t = t.split(p).join("");
    return !/[\p{L}\p{N}]/u.test(t);
  }

  // decorate turns the paths of every role message into its files, once their lookup is back.
  function decorate(list, dest) {
    for (const article of list.querySelectorAll(".chat-message.assistant")) {
      const body = article.querySelector(".message-body");
      if (!body) continue;
      if (body._xfilesFor !== article._text) { body._xfiles = null; body._xfilesFor = article._text; body._paths = null; }
      if (!body._paths) body._paths = [...new Set(pathNodes(body).flatMap((x) => x.found.map((f) => f.path)))];
      if (!body._paths.length) continue;
      const infos = body._paths.map((p) => lookup(dest, p)), sig = JSON.stringify(infos.map((h) => h && h.info ? h.info.path + ":" + h.info.size : ""));
      if (body._xfiles === sig) continue;
      if (infos.some((h) => !h)) continue; // asked; drawn when the answer comes
      if (body._xfiles) continue; // drawn already: a file that changed shows next time the message is drawn
      body._xfiles = sig;
      const shown = new Map(infos.filter((h) => h.info).map((h) => [h.info.path, h.info]));
      if (shown.size) transform(body, shown, dest);
    }
  }

  function transform(body, shown, dest) {
    const images = []; // every image of the message, in order: the viewer's set
    const groups = new Map(); // top block → infos to show after it
    const top = (n) => { let e = n.nodeType === 1 ? n : n.parentElement; while (e.parentElement && e.parentElement !== body) e = e.parentElement; return e; };
    const later = (block, info) => { if (!groups.has(block)) groups.set(block, []); if (!groups.get(block).some((i) => i.path === info.path)) groups.get(block).push(info); };
    // r2: a list, a paragraph or a code block that is only paths becomes the gallery in its place.
    for (const block of [...body.children]) {
      const inner = block.matches(".message-code") ? block.querySelector("pre") : block;
      const paths = [...shown.keys()].filter((p) => inner && inner.textContent.includes(p));
      if (!paths.length) continue;
      if (onlyPaths(inner, paths)) {
        const gal = gallery(paths.map((p) => shown.get(p)), dest, images);
        block.replaceWith(gal);
      }
    }
    // A path in a sentence: its file name as a link; its preview under that part of the message.
    for (const {node, found} of pathNodes(body)) {
      const hits = found.filter((f) => shown.has(f.path));
      if (!hits.length) continue;
      const block = top(node);
      for (const f of hits) later(block, shown.get(f.path));
      if (node.parentElement.closest("pre")) continue; // a code block keeps its text; the preview goes under it
      const code = node.parentElement.closest("code");
      if (code && hits.length === 1 && code.textContent.trim() === hits[0].path) { code.replaceWith(link(shown.get(hits[0].path), dest, images)); continue; }
      if (code) continue;
      const frag = document.createDocumentFragment();
      let last = 0;
      for (const f of hits) { frag.append(node.data.slice(last, f.at), link(shown.get(f.path), dest, images)); last = f.at + f.path.length; }
      frag.append(node.data.slice(last));
      node.replaceWith(frag);
    }
    for (const [block, infos] of groups) if (block.isConnected) block.after(gallery(infos, dest, images));
  }

  function link(info, dest, images) {
    const a = el("a", "flink", info.name);
    a.href = roleURL(dest, info.path, false);
    a.title = info.path;
    a.target = "_blank"; a.rel = "noopener";
    if (isImage(info.type)) {
      const item = {name: info.name, src: a.href, path: info.path, open: a.href};
      if (!images.some((i) => i.path === info.path)) images.push(item);
      a.addEventListener("click", (e) => { e.preventDefault(); view(images, images.findIndex((i) => i.path === info.path), a); });
    }
    return a;
  }

  // gallery draws previews (one natural, more as square tiles 3 a row, six at most, the sixth «+N») and chips (r2, r3).
  function gallery(infos, dest, images) {
    const box = el("div", "fgal");
    const pics = infos.filter((i) => isImage(i.type)), docs = infos.filter((i) => !isImage(i.type));
    for (const info of pics) if (!images.some((i) => i.path === info.path)) images.push({name: info.name, src: roleURL(dest, info.path, false), path: info.path, open: roleURL(dest, info.path, false)});
    if (pics.length) {
      const grid = el("div", "fgrid" + (pics.length === 1 ? " one" : ""));
      pics.slice(0, 6).forEach((info, i) => {
        const fig = el("figure", "ftile");
        const b = el("button", "fshot");
        b.type = "button";
        b.setAttribute("aria-label", "Open " + info.name);
        const img = el("img"); img.src = roleURL(dest, info.path, false); img.alt = info.name; img.loading = "lazy"; img.decoding = "async";
        b.append(img);
        if (i === 5 && pics.length > 6) b.append(el("b", "fmore", "+" + (pics.length - 6)));
        b.addEventListener("click", () => view(images, images.findIndex((x) => x.path === info.path), b));
        const cap = el("figcaption", "", info.name); cap.title = info.path;
        fig.append(b, cap);
        grid.append(fig);
      });
      box.append(grid);
    }
    for (const info of docs) box.append(chip(info.name, info.size, roleURL(dest, info.path, false)));
    return box;
  }

  // ---- the images a role looked at (D-20) ----
  // One strip per turn: «Looked at N images» (a button: it opens the strip), then 56 px tiles in read order, five at
  // most closed — the fifth «+N» — all of them wrapped open. Tiles are thumbnails of the image as the role saw it; a
  // tile keeps its element while the strip grows, and one that comes after the first draw fades in.
  const EYE = '<path d="M2.062 12.348a1 1 0 0 1 0-.696 10.75 10.75 0 0 1 19.876 0 1 1 0 0 1 0 .696 10.75 10.75 0 0 1-19.876 0"/><circle cx="12" cy="12" r="3"/>';
  const lookedURL = (dest, key, thumb) => "/api/chat/looked?" + new URLSearchParams({node: dest.node || "", crew: dest.crew, role: dest.role, key, ...(thumb ? {thumb: "1"} : {})});
  function looked(body, row, dest, owner) {
    let s = body._looked;
    if (!s) {
      s = {label: el("button", "lk-label"), words: el("span", "lk-words"), tiles: el("div", "lk-tiles"), map: new Map(), drawn: false, list: []};
      s.label.type = "button";
      const eye = el("span", "lk-eye"), chev = el("span", "lk-chev");
      eye.innerHTML = svg(EYE);
      chev.innerHTML = '<svg viewBox="0 0 12 12" aria-hidden="true"><path d="M3 4.5 6 7.5 9 4.5"/></svg>';
      s.label.append(eye, s.words, chev);
      s.label.addEventListener("click", () => { owner._lookOpen = !owner._lookOpen; looked(body, owner._lookRow, dest, owner); });
      body.replaceChildren(s.label, s.tiles);
      body._looked = s;
    }
    owner._lookRow = row;
    const imgs = row.looked || [], n = imgs.length, open = Boolean(owner._lookOpen);
    s.label.setAttribute("aria-expanded", String(open));
    s.words.textContent = "Looked at " + (n === 1 ? "1 image" : n + " images");
    s.tiles.classList.toggle("open", open);
    s.list = imgs.map((im) => ({name: im.name, src: lookedURL(dest, im.key, false), open: lookedURL(dest, im.key, false), path: im.path, looked: true}));
    const shown = open ? imgs : imgs.slice(0, 5), order = [];
    shown.forEach((im, i) => {
      let t = s.map.get(im.key);
      if (!t) {
        t = el("button", "lk-tile" + (s.drawn ? " lk-new" : ""));
        t.type = "button";
        t.dataset.key = im.key;
        const img = el("img"); img.alt = ""; img.decoding = "async"; img.loading = "lazy"; img.src = lookedURL(dest, im.key, true);
        t.append(img);
        t.addEventListener("click", () => {
          if (t.querySelector(".fmore")) { owner._lookOpen = true; looked(body, owner._lookRow, dest, owner); return; }
          view(s.list, s.list.findIndex((x, j) => (owner._lookRow.looked || [])[j]?.key === t.dataset.key), t);
        });
        s.map.set(im.key, t);
      }
      t.setAttribute("aria-label", "Looked at " + im.name + ", " + (i + 1) + " of " + n);
      let more = t.querySelector(".fmore");
      if (!open && i === 4 && n > 5) { if (!more) { more = el("b", "fmore"); t.append(more); } more.textContent = "+" + (n - 4); t.setAttribute("aria-label", (n - 4) + " more images: show all"); }
      else if (more) more.remove();
      order.push(t);
    });
    for (const [key, t] of s.map) if (!order.includes(t)) { t.remove(); if (!imgs.some((im) => im.key === key)) s.map.delete(key); }
    order.forEach((t, i) => { if (s.tiles.children[i] !== t) s.tiles.insertBefore(t, s.tiles.children[i] || null); });
    s.drawn = true;
  }

  // ---- the composer's picked files (D-18 f1–f3) ----
  // An image is a 64 px square of the picked file itself (an object URL: nothing goes up to draw it), another file a
  // 176 × 64 tile with its kind's icon, name and «PDF · 412 KB»; a ✕ on the corner removes it, a tap previews it.
  const PLAIN = ["txt", "md", "log", "csv", "json"];
  const safeType = (name) => { const ext = (name.split(".").pop() || "").toLowerCase(); return ext === "pdf" ? "application/pdf" : PLAIN.includes(ext) ? "text/plain" : ""; };
  const typed = new Map(); // file id → object URL of its bytes under a type that runs nothing
  function typedURL(f, type) {
    if (!typed.has(f.id)) typed.set(f.id, URL.createObjectURL(new Blob([f.blob], {type})));
    return typed.get(f.id);
  }
  function draft(box, files, remove) {
    box.replaceChildren();
    const images = files.filter((f) => isImage(f.type) && f.blob);
    const list = images.map((f) => ({name: f.name, src: ownURL({}, f), open: ownURL({}, f)}));
    for (const f of files) {
      const image = isImage(f.type) && f.blob, tile = el("div", "dtile" + (image ? "" : " file"));
      const open = el("button", "dt-open");
      open.type = "button";
      open.setAttribute("aria-label", "Preview " + f.name);
      if (image) {
        const img = el("img"); img.src = ownURL({}, f); img.alt = ""; img.decoding = "async";
        open.append(img);
        open.addEventListener("click", () => view(list, images.indexOf(f), open));
      } else {
        const ic = el("span", "dt-icon"); ic.innerHTML = icon(f.name);
        const text = el("span", "dt-text");
        text.append(el("span", "dt-name", f.name), el("span", "dt-sub", kindWord(f.name) + " · " + size(f.size)));
        open.append(ic, text);
        // A picked file opens only as a pdf or plain text, its bytes re-typed: a blob runs at this page's origin, so a
        // picked .html or .svg opened with its own type would run as the cockpit. Any other file's tile does nothing.
        const as = safeType(f.name);
        if (f.blob && as) open.addEventListener("click", () => window.open(typedURL(f, as), "_blank", "noopener"));
        else { open.disabled = true; open.removeAttribute("aria-label"); }
      }
      const rm = el("button", "dt-rm");
      rm.type = "button";
      rm.setAttribute("aria-label", "Remove " + f.name);
      rm.title = "Remove";
      rm.innerHTML = '<svg viewBox="0 0 10 10" aria-hidden="true"><path d="M2 2l6 6M8 2 2 8"/></svg>';
      rm.addEventListener("click", () => remove(f));
      tile.append(open, rm);
      box.append(tile);
    }
  }

  // ---- the viewer (v1) ----
  let viewer = null;
  function view(list, index, from) {
    if (!list.length || index < 0) return;
    close();
    const small = phone();
    const box = el("div", "fview" + (small ? " small" : ""));
    box.setAttribute("role", "dialog");
    box.setAttribute("aria-modal", "true");
    box.setAttribute("aria-label", "Image viewer");
    box.tabIndex = -1;
    const bar = el("div", "fv-bar"), x = el("button", "fv-x"), title = el("div", "fv-title"), name = el("b"), count = el("span");
    x.type = "button"; x.setAttribute("aria-label", "Close"); x.innerHTML = svg('<path d="M6 6l12 12M18 6 6 18"/>');
    title.append(name, count);
    bar.append(x, title);
    let share = null;
    if (small && navigator.share) {
      share = el("button", "fv-share"); share.type = "button"; share.setAttribute("aria-label", "Share");
      share.innerHTML = svg('<path d="M4 12v8a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-8"/><path d="m16 6-4-4-4 4"/><path d="M12 2v13"/>');
      bar.append(share);
    }
    const stage = el("div", "fv-stage"), img = el("img", "fv-img");
    stage.append(img);
    const prev = el("button", "fv-prev"), next = el("button", "fv-next");
    prev.type = next.type = "button"; prev.setAttribute("aria-label", "Previous image"); next.setAttribute("aria-label", "Next image");
    prev.innerHTML = svg('<path d="m15 18-6-6 6-6"/>'); next.innerHTML = svg('<path d="m9 18 6-6-6-6"/>');
    const dots = el("div", "fv-dots"), foot = el("div", "fv-foot"), path = el("code", "fv-path"), copy = el("button", "fv-copy"), open = el("a", "fv-open", "Open original");
    copy.type = "button"; copy.setAttribute("aria-label", "Copy path"); copy.title = "Copy path";
    copy.innerHTML = svg('<rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h2"/>');
    open.target = "_blank"; open.rel = "noopener";
    const pathRow = el("div", "fv-pathrow"); pathRow.append(path, copy);
    foot.append(pathRow);
    if (!small) foot.append(open);
    box.append(bar, stage);
    if (list.length > 1) { if (small) box.append(dots); else box.append(prev, next); }
    box.append(foot);
    document.body.append(box);
    viewer = {box, list, index, from, img};
    const show = (i) => {
      viewer.index = (i + list.length) % list.length;
      const it = list[viewer.index];
      img.src = it.src; img.alt = it.name; img.classList.remove("zoom");
      name.textContent = it.name;
      // D-20 v1: an image a role looked at says so under its name.
      count.textContent = it.looked ? "Looked at · " + (viewer.index + 1) + " of " + list.length : list.length > 1 ? (viewer.index + 1) + " of " + list.length : "";
      path.textContent = it.path || ""; pathRow.hidden = !it.path;
      open.href = it.open;
      dots.replaceChildren(...list.map((_, j) => el("i", j === viewer.index ? "on" : "")));
    };
    viewer.show = show;
    show(index);
    x.addEventListener("click", () => close(true));
    prev.addEventListener("click", () => show(viewer.index - 1));
    next.addEventListener("click", () => show(viewer.index + 1));
    copy.addEventListener("click", async () => { try { await navigator.clipboard.writeText(path.textContent); copy.title = "Copied"; } catch (_) { copy.title = "Select and copy"; } });
    share?.addEventListener("click", async () => {
      const it = list[viewer.index];
      try {
        const blob = await (await fetch(it.open)).blob(), file = new File([blob], it.name, {type: blob.type});
        if (navigator.canShare?.({files: [file]})) await navigator.share({files: [file], title: it.name});
        else await navigator.share({url: new URL(it.open, location.href).href, title: it.name});
      } catch (_) { /* dismissed */ }
    });
    // The desktop: a click on the dark around the image closes it.
    box.addEventListener("click", (e) => { if (!small && (e.target === box || e.target === stage)) close(true); });
    // Double-tap (or double-click) zooms in and back.
    img.addEventListener("dblclick", () => img.classList.toggle("zoom"));
    // The phone: swipe between images, down to close.
    let start = null;
    stage.addEventListener("touchstart", (e) => { if (e.touches.length === 1 && !img.classList.contains("zoom")) start = {x: e.touches[0].clientX, y: e.touches[0].clientY}; else start = null; }, {passive: true});
    stage.addEventListener("touchend", (e) => {
      if (!start) return;
      const t = e.changedTouches[0], dx = t.clientX - start.x, dy = t.clientY - start.y;
      start = null;
      if (dy > 90 && Math.abs(dy) > Math.abs(dx)) close(true);
      else if (Math.abs(dx) > 50 && Math.abs(dx) > Math.abs(dy)) show(viewer.index + (dx < 0 ? 1 : -1));
    }, {passive: true});
    box.focus({preventScroll: true});
  }
  function close(back) {
    if (!viewer) return;
    const {box, from} = viewer;
    viewer = null;
    box.remove();
    if (back && from?.isConnected) from.focus({preventScroll: true});
  }
  document.addEventListener("keydown", (e) => {
    if (!viewer) return;
    if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); close(true); }
    else if (e.key === "ArrowLeft") { e.preventDefault(); viewer.show(viewer.index - 1); }
    else if (e.key === "ArrowRight") { e.preventDefault(); viewer.show(viewer.index + 1); }
    else if (e.key === "Tab") {
      // The focus stays in the viewer while it is open.
      const f = [...viewer.box.querySelectorAll("button, a[href]")].filter((x) => x.offsetParent !== null);
      if (!f.length) return;
      const i = f.indexOf(document.activeElement);
      if (e.shiftKey && i <= 0) { e.preventDefault(); f[f.length - 1].focus(); }
      else if (!e.shiftKey && i === f.length - 1) { e.preventDefault(); f[0].focus(); }
      else if (i < 0) { e.preventDefault(); f[0].focus(); }
    }
  }, true);

  window.zminaFiles = Object.freeze({init: (h) => { hooks = h; }, ownerBlock, progress, decorate, size, draft, looked});
})();
