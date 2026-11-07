# Chat content smoke test

From repository root:

```sh
python3 -m http.server 7530 --bind 127.0.0.1 --directory internal/ui
```

Open `http://127.0.0.1:7530/testdata/chat-content.html` in a browser. The fixture page uses
production assets without a running crew, API, model call or message delivery. Inspect
`window.chatContentResults`: `passed` must be true. Reload at 320, 390 and 1280 px.

Cases cover comparison/wide tables, long JSON/diff lines, exact code copying, long URLs and
paths, Unicode, nested lists, checklists, headings, quotes, malformed Markdown, raw HTML/XML,
unsafe links, image references and unknown fenced languages/math. Unknown syntax remains
text; diagrams and formulas are not executed. Inspect screenshots and sideways scrolling
as well as assertions. Tests do not replace Safari keyboard checks on a real iPhone.

# Chat outbox persistence test

Using the same static server on a dedicated test origin, open
`http://127.0.0.1:7530/testdata/chat-outbox.html`. The page reloads itself once;
`window.chatOutboxResults.passed` must become true. Cases cover actual reload
persistence (including attachment and draft Blob bytes), concurrent same-ID writes, changed-payload refusal, a late failure
following acknowledgement, bounded queue refusal, and bounded history retention.
No crew or network message delivery is involved. Do not serve this fixture on a
production chat origin: it exercises that origin's IndexedDB queue capacity.

# Cockpit patching test

With the same static server, open `http://127.0.0.1:7530/testdata/patch.html`;
`window.patchResults.passed` must be true. It runs `static/patch.js` in a real DOM: a
same-title check left after the other goes opens its own item, a group's control acts on the
group it is drawn for after groups trade places, an unchanged row stays the same element, a
kept row's panel buttons carry the current handlers, and focus follows a swapped button.

# Chat question test

With the same static server, open `http://127.0.0.1:7530/testdata/chat-question.html`;
`window.chatQuestionResults.passed` must be true. It runs the real `static/chat.html` in a frame
whose `fetch` answers for a role with a one-part single-select question (D-13): a tap marks the
choice and moves on to the review without sending; Send answers posts the answer once, the card
goes while the screen still shows the question, and the line under the field says what went (a
question still on the screen 10 s later points to Terminal again); a
refused answer keeps the card on the review with the choice and the reason in it. Headless:
`"Google Chrome" --headless=new --virtual-time-budget=30000 --dump-dom <url>` prints the result
in `#result`.

# Chat ids test

With the same static server, open `http://127.0.0.1:7530/testdata/chat-ids.html`;
`window.chatIdsResults.passed` must be true (headless as above, with `--virtual-time-budget=60000`:
the outbox write is real time, which virtual time runs ahead of). It runs the real `static/chat.html`
at 390 and 1280 px with a `fetch` that answers `/api/items` (D-15): ids become links (not in code,
not inside a longer word), the first mention of a piece of work in a run carries its short title and
a dot; a tap opens the sheet (title, state, Left, Latest, Path; a step swaps the card); an ask's
answer goes once as a todo answer and turns into Sent; an unknown id says so; «Talk to» puts the
item on the field and the message goes with it; on the desktop a click pins a 360 px popover and Esc
closes it, the focus back on the id.

# Chat send state test

Open `http://127.0.0.1:7530/testdata/chat-send.html` in a real browser (e.g. `oko open …`, then read
`#result` after a few seconds); `window.chatSendResults.passed` must be true. Headless
`--virtual-time-budget` does not work here: draft files are Blobs in IndexedDB, which take real time
that virtual time runs ahead of. The page attaches an image and sends it through a `fetch` that is
slow to take the upload and the message: the owner's bubble says «Uploading shot.png…», then
«Sending…», never «waiting to send» while the network is there, and no line under the field says
delivery waits for a connection.

# Chat attachments test

In a real browser as above, open `http://127.0.0.1:7530/testdata/chat-files.html`;
`window.chatFilesResults.passed` must be true. At 390 and 1280 px, with a `fetch` that answers
`/api/chat/paths` (D-16): a list of a role's image paths becomes a gallery in its place, each tile
named; a path in a sentence becomes its file name with its preview (or a PDF chip) under the
paragraph; a path the node does not show stays text; the owner's image and PDF sit above his bubble
(sizes in KB, never bytes) and an image-only message has no bubble; a tap opens the viewer over the
message's images (its name, «1 of 4», the path), the arrow key goes on, Esc gives the focus back. The
images themselves load from `/api/chat/...`, which the static server does not have: they draw broken
here; the layout checks do not depend on them. `chat-send.html` also checks the tile's upload bar.

# Chat command lines test

In a real browser as above, open `http://127.0.0.1:7530/testdata/chat-commands.html`;
`window.chatCommandsResults.passed` must be true. At 390 and 1280 px, with a `fetch` that answers a
conversation in which the role's prompt ran /compact, /context and /model (D-17): each is one quiet
centred 12 px line with its time, no card, no head, never the owner's; a command's answer joins its line;
a line with more output is a button (44 px to hit, a 12 px chevron) that opens it under the line in a mono
box and closes it again, open through the next read; every line sits one distance under the text above
(18 px phone, 24 px desktop; the phone frame has `embed=1`, so it is a phone when the page is 860 px or
narrower, e.g. inside a 390 px frame); no terminal escape and no `<command-*>` / `<local-command-*>` tag shows
anywhere, also in a reply an older node sends with escapes. The transcript side (folding the typed line, the
recorded command and its output, dropping hook lines) is `TestChatMessagesFoldsLocalCommands`.
