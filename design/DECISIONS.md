# Design decisions — zmina

## D-2026-10-29-01 · `zmina ui` · the one-screen live status page
asked by: the dispatcher (the owner wants a micro UI to see what is really happening) ·
decided: 2026-10-29 (the designer) · brief: design/BRIEF.md.
### Reading order — "what is happening" in two seconds
The page answers three questions in this order and never reorders them: does anything need me → who is doing
what → what just happened.
1. Header, 40 px: «zmina» (13/18 600) · the live dot (see Motion) · ticker heartbeat right-aligned, mono 12
   («ticker 14:12:05»). If the ticker is older than 2 minutes the header becomes a danger bar: «Ticker stopped
   4 min ago».
2. «Needs you» — present only when non-empty, first thing on the page, one row each: a role at a prompt or dialog,
   a role stuck (busy past `stuck_minutes`), a task in state waiting. Row: state word · role · what (task id and the
   first line of its text, or «on a prompt») · age. Empty → nothing rendered; the crew list moves up. The tab title
   carries the count: «zmina · 2 need you», else «zmina»; the favicon dot turns amber with it.
3. Crew — one row per role that is up, sorted needs-you → busy → idle → outside, then by time in state. Row line 1:
   state mark + word · role name (14/20 600) · task ids (mono 12) · time in state (mono 12, tabular, right) ·
   context meter. Line 2 (muted, one line, ellipsis): the first held task's text, else the role's `about`.
   Roles that are down collapse into one muted line under the list: «down: dev, director, …»; a `keep: always`
   role that is down or dead is never collapsed — it gets a danger row of its own.
4. Tasks — open and waiting only, newest first: id (mono) · role · state word · age · text (one line, ellipsis).
   Done tasks are not listed; they are in the log.
5. Log — last 20 events, newest first: HH:MM (mono) · role · event · note (one line, ellipsis).
6. Footer, muted 12: «later: 5» (expands the parked list inline) · «zmina <version>».
≥ 960 px: two columns — left: needs-you + crew; right: tasks + log. Below that, one column in the order above,
16 px gutters; no horizontal scroll at 420.
### States — the encoding (mark + word + colour; never colour alone)
| state | mark | word | colour |
|---|---|---|---|
| needs you: on a prompt / dialog | ■ filled square | prompt | `--need` |
| needs you: stuck (busy > stuck_minutes) | ■ | stuck 41m | `--need` |
| needs you: task waiting | ■ | waiting | `--need` |
| busy | ● filled dot | busy 12m | `--busy` |
| idle | ○ ring | idle 3m | `--muted` |
| outside (started outside zmina) | ◌ dotted ring | outside | `--muted` |
| down (manual) | — | (collapsed line) | `--faint` |
| down or dead with keep: always, ticker stopped | ✕ | down / dead | `--danger` |
"on a prompt" needs one field zmina does not expose yet: `ls --json` gains `"prompt": true` when the role's screen
shows a dialog or permission question instead of the ❯ input line (the detection `clearPrompt`/`screen.go` already
reads). Until it exists the row shows busy.
context meter: 48×4 px, track `--line`, fill `--muted`; ≥ 75 % `--need`, ≥ 90 % `--danger`; `title`
«221k / 400k».
### Tokens (CSS custom properties in the one file; dark by `prefers-color-scheme`)
| token | dark | light |
|---|---|---|
| `--bg` | #121416 | #f6f7f8 |
| `--surface` (screen preview, expanded rows) | #1a1d20 | #ffffff |
| `--line` | #2a2e33 | #dde1e5 |
| `--ink` | #e6e8ea | #16191c |
| `--muted` | #9aa1a8 | #59616a |
| `--faint` | #7d858d | #6b737b |
| `--busy` | #7aa7ff | #2f5fd0 |
| `--need` (the accent: only "needs you") | #f0b45a | #9a5b08 |
| `--danger` | #ff7b72 | #c2332b |
Measured (contrast.py): every text token ≥ 4.5:1 on `--bg` and `--surface` in both modes (lowest: `--faint` on
`--bg` 4.93 dark, 4.81 light). Neutrals lean cool — a terminal neighbour.
type: system stacks only — `ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif` 13/18; mono
`ui-monospace, SFMono-Regular, Menlo, Consolas, monospace` 12/16 with `font-variant-numeric: tabular-nums` for ids,
times, counts. Section labels 12/16 600 `--muted`, sentence case: «Needs you», «Crew», «Tasks», «Log». Rows 36 px
min (40 px at ≥ 960), hairline `--line` between rows; no cards, no shadows, no radius beyond 4 px on the preview.
### Interaction
- Click a role row → it expands in place: the screen preview, last 30 lines of `peek`, mono 11/15 on `--surface`,
  max-height 50vh, scrolls, refreshed every 2 s only while open; under it the line «zmina attach <role>» with a
  copy button. One row open at a time; Esc or a second click closes. Nothing on the page types into a role — no
  send, no keys: read-only by design (an open-source page must not be a remote control).
- Click a task → expands its full text and its log lines (`log --task`).
- Keyboard: rows are buttons, `:focus-visible` 2 px `--busy` ring; j/k move, Enter opens.
### Motion
One moving element: the live dot in the header flashes `--busy` for 300 ms on each SSE event and sits `--faint`
when the stream drops (then «reconnecting…» beside it). Nothing else animates — no pulsing busy dots, no row
transitions. `prefers-reduced-motion`: the dot changes colour without the flash.
why: one reader, glancing — the only colour that pulls the eye is the one that asks for them (taste: colour means
meaning, one accent, one moving element; compact, rows over cards; numbers only from real data).
amendment 2026-10-29 (the owner: a good todo matters most) — the page opens with a
todo of what waits on the owner; it replaces «Needs you» (section 2) and stands above the crew.
### The todo — what is in it
Three kinds, one list: **decision** (a question the dispatcher keeps asking, with its recommendation and a one-line
reason), **check** (something only the owner can do: «sign in and publish the test board»), **waiting** (a task in
state waiting, with its note). The parked `later` items sit collapsed under the list: «Later · 5».
Data zmina does not have yet: decisions and checks need a small store the dispatcher writes — an "ask" with kind,
text, recommendation, reason, role, task id, created, blocks (the role it holds up, if any). Waiting tasks and
`later` come from what exists.
### Order
blocking first (an item a role is held up by), then overdue (older than 24 h), then the rest; oldest first inside
each group. Roles on a prompt and stuck roles stay in the crew with the `--need` mark and count in the tab title
(«zmina · 4 waiting on you»), but are not todo items — they are answered in the terminal.
### Item anatomy (a full-width row, no card)
- line 1: kind mark + the question, 15/22 500 `--ink`, wraps up to 3 lines then ellipsis (the only text on the
  page that wraps). Marks: decision ◆, check ☐, waiting ■ — all `--need`.
- line 2 (decisions): «Recommended: <answer>» 13/18 `--ink`, then the reason in `--muted` on the same line or the
  next. Waiting: the task's note in `--muted`.
- meta, mono 12 `--muted`: role · task id · age («dev2 · T-62 · 3h»).
- actions, one row of buttons: decision — «Yes» (primary: takes the recommendation) · «Answer…» · «Later»;
  check — «Done» (primary) · «Answer…» · «Later»; waiting — «Answer…» (primary) · «Later». «Answer…» opens a
  textarea in place (16 px, 3 lines, grows) with «Send» and «Cancel». Later items: «Bring back» · «Drop».
- every action sends a note to the dispatcher (`zmina note`, text «<id> · yes» / «<id> · <the words>» / «<id> ·
  later») and writes a log line; the row then shows «Sent to dispatcher · Undo» for 5 s, the note is held until the
  5 s pass, and the row leaves the list. Nothing is answered without a visible row-level confirmation.
### Standing out
- blocking: 3 px `--need` bar on the row's left edge and a meta tag «blocks dev2» in `--need`. Only blocking rows
  carry the bar.
- overdue: the age in `--danger` («2d»), nothing else changes. Colour never fills a whole row.
- primary button: `--ink` fill, `--bg` text; the others: 1 px `--line` outline, `--ink` text. 36 px tall at ≥ 600,
  44 px below.
### Empty state
One line, `--muted`: «Nothing waits on you.» and, mono 12, «last answer 14:02». No illustration; the crew moves up.
### Phone (< 600 px; the owner answers from the phone)
One column, todo first; item text 16/24; the actions become a full-width row — buttons share it equally, 44 px
tall, 8 px gaps; the textarea is full width. The crew collapses to one summary line under the todo — «Crew: 3 busy ·
1 idle · 4 down» — that expands on tap; tasks and log sit behind it the same way. Safe-area insets at the bottom.
### Write actions and reach
The read-only rule now has one exception: the todo writes notes to the dispatcher, and nothing else (still no
send or keys into a role). The server binds 127.0.0.1 by default. Reaching it from a phone is a separate, explicit
switch (LAN or tunnel) and must carry a secret — a token in the URL or a cookie set once; that is the developer's
and the owner's call, not a design default, because the page now writes.
