# zmina ui · brief · 2026-10-29
offer: `zmina ui` serves one local page that shows, live, what the shift is doing right now.
reader: the person who runs the shift, on the same machine, in a browser tab or a ~420 px side panel next to the
  terminal, glanced at many times an hour · moment: "does anything need me? is anyone stuck? who is on what?"
action: none on the page beyond looking — it is read-only; acting stays in the terminal (`zmina attach`, `keys`).
proof / numbers: only zmina's own data (`ls --json`, `task ls`, `log`, `peek`, `later`); nothing invented.
voice: English UI (open source); task text is shown verbatim in whatever language it was written.
feel: a status board you read in two seconds · must not read as: a dashboard of cards and charts.
constraints: one embedded HTML/CSS/JS file served by the Go binary, live over SSE, no build step, no web fonts,
  no personal names; dark and light from the system setting; 420 px and full width.
decided by: the owner's wish relayed by the dispatcher, answers proposed by the designer · D-2026-10-29-01
