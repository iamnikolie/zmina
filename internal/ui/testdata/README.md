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
