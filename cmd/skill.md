# zmina — agent reference

zmina runs role sessions — a designer, a reviewer, developer seats, whatever the crew file names —
as long-lived Claude Code sessions, one tmux session each on zmina's own tmux server
(`tmux -L zmina`). One role is the **dispatcher** (`zm-dispatcher` by default): it never sleeps,
routes tasks to roles, starts and stops them, and hears the ticker's health notes. The work
itself lives in a **task ledger** outside any session, and everything that happens goes to an
**event log**.

## The crew file

`~/.zmina/crew.yaml`; another crew is `--config <name>` (`~/.zmina/<name>/crew.yaml`; such a
crew is named `<name>` unless it says otherwise, so it gets its own tmux
server, ticker and session prefix), or any file with `--crew` / `$ZMINA_CREW`. Relative paths in
the file are relative to the file. Keys zmina does not know are warnings with their line (a typo
shows), not errors. `zmina init` writes a starter with
every role that has `<agents>/<role>/AGENT.md` (`--agents`, default `agents/` next to the crew
file). Change it with `zmina seat` (below), not by hand. A full example crew with boot documents
is in the repository's `examples/`.

```yaml
namespace: ~/.claude                     # CLAUDE_CONFIG_DIR of every role (default ~/.claude; init writes yours)
dir: ~/work                              # default working directory (default ~)
# claude: /opt/bin/claude                # the claude binary (default: claude from PATH)
agents: ~/.zmina/agents                  # <role>/AGENT.md boot documents (default: agents/ next to this file)
dispatcher: dispatcher                   # this role is always kept up
stuck_minutes: 90                        # busy longer -> the dispatcher hears of it
idle_minutes: 30                         # idle longer with no active task -> the dispatcher hears of it
boot_grace: 2m                           # a started role is left this long before lines are typed into it
remote_control: true                     # open roles to Remote Control (per role too); default true
drop_env: [MY_TERMINAL_*]                # variables kept from the roles, on top of the built-in list
drop_tools: [Artifact, Workflow]         # Claude Code tools every role starts without (--disallowedTools)
context_max: 400k                        # every role's, unless it sets its own (default: none)
context_action: compact                  # every role's, unless it sets its own (default compact)
# name: ops                              # a second crew: tmux -L zmina-ops, its own ticker
# prefix: ops-                           # default session names (default zm-)
# projects:                              # the ui page's colour per task project (default: one of five, by name)
#   site: {tint: p3}                     # p1 … p5: pin one when two projects collide
roles:
  designer:
    keep: manual                         # manual | always
  dev-api:
    session: zm-dev-api                  # default <prefix><role>; other sessions message it by this
    dir: ~/work/api
    boot: read AGENTS.md, then wait for a task   # default: read <agents>/<role>/AGENT.md and boot
    about: the api service, backend      # the dispatcher routes by this line
    model: opus                          # --model
    args: [--chrome]                     # extra claude flags
    drop_tools: [Agent]                  # this role also starts without these
    keep_tools: [Workflow]               # ... and keeps these of the crew's list
    resume: always                       # start on the role's last conversation (claude --resume)
    expires: 2026-11-01                  # a temporary seat: past it, reported for removal once down
    context_max: 200k                    # conversation fuller than this -> trimmed once idle (150k, 1m; 50k at least; off)
    context_action: clear                # compact (default: /compact) | clear (/clear, then the boot line again)
  scribe:
    schedule: "0 19 * * 1-5"             # cron: minute hour day-of-month month day-of-week
    prompt: write today's summary        # one line typed at each run
```

A role runs `claude [--model m] [args] [--disallowedTools=<tools>] [--resume id] --name <session>
"<boot>" [--remote-control <session>]` in its dir, with `CLAUDE_CONFIG_DIR`, `ZMINA_ROLE`, `ZMINA_CREW`, and the caller's PATH
less directories under the temp dir (a terminal's per-window shims, such as cmux's, vanish with the
window and would wrap claude in that window's hooks),
on top of the tmux server's environment minus what ties a process to another session or terminal
(`CLAUDECODE`, `CLAUDE_CODE_SESSION_ID`, `CLAUDE_CODE_MESSAGING_*`, `TMUX`, `CMUX_*`, … and
`drop_env`); provider settings such as `CLAUDE_CODE_USE_BEDROCK` pass. With Remote Control every
role is reachable from claude.ai/code and the phone under its session name.

Every tool's description sits in the context of every turn, used or not: a fresh session of a
setup with many skills and plugins fills ~50k tokens before the first word, and `Artifact` alone
~12k. `drop_tools` takes them out for the roles that never use them (tool names as
`--disallowedTools` takes them, `mcp__<server>` for a whole MCP server; never `ToolSearch` — without it every
deferred tool loads in full). A change applies at the role's next start; a role with
`resume: always` goes on with its conversation. A role whose `args` pass `--disallowedTools`
themselves keeps only that flag, with a warning: move the list to `drop_tools`, so the crew's and
the role's add up.

## Roles

| Command | Does |
|---|---|
| `zmina ls [--json]` | first line: when the ticker last beat (and `STOPPED` while the shift is stopped). Then every role: `up` / `down` / `dead` (its process ended, the ticker collects it within a minute) / `outside` (a claude with that name runs, not under zmina) / `unknown` (tmux did not answer), `idle`/`busy` (`unknown` when its registry entry does not read), for how long, how many tokens its conversation fills (`362k/150k` with its `context_max`), the active tasks it holds, keep (+ next scheduled run, expiry), about. `--json` also has `prompt: true` while an up role's screen shows a dialog or a question instead of its input line, and `now` / `now_at`: the role's last status line in words (its screen's last `⏺ …` line that is not a tool call) and since when it says so. `*` marks the dispatcher; `(broken)` a role that dies on boot; `(bad dir)` a role whose dir is gone — it does not start, the rest of the crew runs on; `(N failed lines)` see `zmina queue`. Last line `later: N parked` while the parked list is not empty. |
| `zmina up <role...>` / `--all` | start roles. Refuses a role already up, and a name a claude outside zmina already holds. Clears a hold and a broken mark. |
| `zmina up <role> --as N` | a copy of the role, `<role>#N` (2–9) under `<session>-N`, same dir/boot/model/args; never kept up, gone once it is down. `zmina down dev#2`. |
| `zmina up <role> [--as N] --task T-14` | a seat for a recorded task: started in the task's worktree when it has one, the task names this role, and its first line is `[zmina T-14] <asked> · you are in its worktree <path> on branch <b>`. Claude Code's "trust this folder" is answered for a worktree zmina made itself (`<crew>/worktrees/`), nowhere else. Refuses a task that is not active. |
| `zmina up <role> --resume` / `--resume=<id>` | start on the role's last conversation, or on the session id given — with `=`: `--resume <id>` reads the id as a second role. zmina keeps each role's last session id. The boot message is sent again into the resumed conversation. |
| `zmina down <role...>` / `--all` | types `/exit` (session-end hooks run), kills after 15 s; the wait happens outside the lock, so other commands and the ticker are not held up. `/exit` goes only into an empty input line: a draft there (it would be sent with `/exit`) or a dialog (its Enter would pick the highlighted answer) refuses the down, exit 1, nothing pressed. `--clear` drops the draft (C-c; BSpace and End do not clear it) or closes the dialog (Escape) first, then `/exit`; a screen Escape leaves without an input line gets C-c C-c in one call. Claude Code's "Background work is running" question on exit is answered "Exit and stop tasks". The dispatcher needs `--force`. |
| `zmina restart <role>` | down + up: a new session under the same name. A draft or a dialog refuses it as with `down`; `--clear` too. |
| `zmina send <role> <text>` | type one line into a role's prompt and press Enter. Text that starts with `-` goes after `--`: `zmina send dev -- '- item'`. `--when-idle` queues it instead: the ticker types it the first time the role is up, booted and idle (one queued line per minute). For a message between sessions prefer `SendMessage({to: "<prefix><role>"})`. |
| `zmina peek <role> [-n 40]` | the last lines of a role's screen, as text — works from an agent, no terminal; also on a `dead` role. |
| `zmina keys <role> <key...>` | press named keys: `Enter Escape Up Down Left Right Tab BTab Space BSpace PageUp PageDown Home End C-c y n 1`–`9`. Text goes with `send`. The keys of one call go in one tmux call: `zmina keys <role> C-c C-c` ends a claude, two separate calls are too slow for its double C-c. |
| `zmina attach <role>` | attach a terminal (detach: your tmux prefix, then `d`). Needs a TTY; from an agent it prints the command instead. |

## The shift: stop, start, doctor, queues, state

| Command | Does |
|---|---|
| `zmina stop` | the ticker starts nothing any more (no keep: always role, no scheduled run), then every role goes down, the dispatcher too; a role with a draft or a dialog stays up and is named (`--clear` as with `down`). `ls` shows `STOPPED`. |
| `zmina start` | lifts the stop and starts the dispatcher. |
| `zmina doctor [--json]` | checks crew file, tmux (3.2 or newer), claude as roles find it (their PATH; a binary in a temp dir fails) and the flags zmina passes, Claude Code's session registry (zmina reads it to tell idle from busy; it is not a public interface — doctor says when its shape changed), ticker beat, state, lock holder, task files, crews sharing a tmux server. Exit 1 on a fail. |
| `zmina queue ls [--json]` | what waits to be typed: the dispatcher's signals and health notes, each role's inbox, and its failed lines (a line tmux refused three beats in a row). |
| `zmina queue retry <role>` / `queue drop <role> [--all]` | put failed lines back at the head of the inbox / drop them (`--all`: the inbox too; for the dispatcher its signals and notes). |
| `zmina state show` | the state as zmina reads it (JSON). |
| `zmina state recover` | an unreadable `state.json` (or one from a newer zmina) is never written over: every writing command refuses until this moves it aside as `state.json.broken-<time>`. |
| `zmina version` / `--version` | `zmina version <v>` on stdout. `version`, `skill` and `help` need no crew file. |

## Notes — signals for the dispatcher

For a job outside the crew (a launchd or systemd watcher, a script) that has found work: it signals, the
dispatcher routes the work to a seat. No headless `claude` of its own.

`zmina [--config <crew>] note [--from <source>] [--if-up] <text>` queues `<source>: <text>` for the
dispatcher (`--from` defaults to `$ZMINA_ROLE`, else `hand`; whitespace and newlines fold to one
line). Notes from `zmina note` and the watches are **signals**: a queue of their own, refused at
intake when 20 wait and never dropped once accepted. The ticker's health notes are a second queue,
deduplicated and capped at 20 on their own. Once the dispatcher is idle and has been up
`boot_grace`, everything waiting goes as one line, signals first —
`[zmina] mr-watch: !35 — 4 new threads (reviewer X) · dev: ended`. A note still waiting with the
same source and text is not queued twice; once told, the same note is news again. Event `noted`
in the log.

Exit codes: `0` queued (or already waiting), `1` error (no crew file, 20 signals already wait),
`3` with `--if-up`: the dispatcher is not up, nothing queued — the caller falls back to its own way.
To look first: `zmina --config <crew> ls --json`, the dispatcher's `state` is `up`.

Safe from launchd and systemd: no TTY needed, the state lock is the one every command takes, and tmux is found
in `/opt/homebrew/bin` or `/usr/local/bin` when PATH is `/usr/bin:/bin`. Pass the crew explicitly
(`--config ops`): such a job has no `$ZMINA_CREW`.

## Nodes — crews on other machines

A **node** is one `~/.zmina` (one machine, or one user on a shared server) with every crew under it. Nodes reach
each other without a hub: each keeps its own address book (`~/.zmina/mesh/`), and a message waits on the node that
has it until the next node takes it. A node is known by its key, never by its address: the owner's key (on one
node, `owner.key`) signs every node's certificate once, and a node talks only to nodes this owner signed. The
network under it is a transport and an address: today ssh, or any command line that reaches the peer's zmina.

| Command | What it does |
|---|---|
| `zmina node init --name <n> --owner` | the first node: its key, the owner key, its certificate. Keep `~/.zmina/mesh/owner.key` on this machine only. |
| `zmina node enroll <n> --cmd '<command>'` / `--ssh [user@]host` | on the owner's node: runs `zmina node init --name <n>` and `zmina node accept` there through the command (e.g. `'ssh box sudo -n -iu ops zmina'`), signs it, adds it as a peer. The far side needs this zmina. |
| `zmina node init [--name <n>] [--json]` / `node sign` / `node accept` | the same by hand: `init` prints the request, `sign` (on the owner's node) turns it into a certificate on stdout, `accept` stores it on the new node. Default name `<host>-<user>`. |
| `zmina node peer add <n> --ssh … \| --cmd …` / `peer rm <n>` | the address book by hand. |
| `zmina node ls [--json]` | this node and its crews; each peer: how known (`hand`, `called` — it called in, `told` — another node told of it), its addresses or the peers that reach it (`through`), last good session, last failure; what waits to leave. |
| `zmina node sync [--peer <n>]` | one round now. Exit 1 when a peer failed. |
| `zmina tell <node>[/<crew>] <text>` | one line for that crew's dispatcher (no crew: the node's default crew, or its only one). On this node it is queued at once; for another it is signed, queued in `outbox/`, and handed over now if that peer answers, else by the next round. |

A round (the ticker runs one every minute, whichever crew's ticker comes first; `zmina node sync` by hand): this
node meets every peer it has an address for — both prove their keys, tell each other the nodes they know (with
ssh addresses only: a command line is never told, so a peer cannot make this node run anything), hand over what
is for the other, and take what the other has. So a laptop that sleeps still gets its lines: it takes them from
the server once it wakes. A node with no way to a destination hands the line to a peer that told it reaches
that node; the destination checks the writer's signature, not the carrier (up to 4 carriers). A line arrives
as a signal from `<node>/<crew>` (`box-ops/ops: the dump is ready`), queued like `zmina note`: kept on the
sender until the crew takes it (20 already waiting: tried again next round), never twice (ids are remembered
30 days). `tick.log` gets a `mesh:` line when lines move, and when a peer starts or stops failing.

## Watches — detectors the ticker runs

A watch finds work and queues notes for the dispatcher; it never does the work (no agent, no budget
of its own). It is level-triggered: each run its source reports what is true now, as events
`{key, fingerprint, text, at?, url?}`, and zmina decides what is news. Note:
`<watch>: <text> <url>` — `[zmina] mrs: group/repo!12 — 4 new review threads (reviewer-x) https://…/12`.

```yaml
watch:
  mrs:                           # the name is the note's source
    kind: gitlab-mr              # gitlab-mr | exec
    every: 10m                   # at least 1m
    quiet: 10m                   # hold an event until it has not moved for this long (default 0)
    timeout: 2m                  # one run (default 2m)
    forget_after: 14d            # a key gone this long is forgotten (default 14d)
    enabled: true                # false: skipped, state kept
    baseline: true               # the first run records everything, notes nothing (default)
    gl_config: work              # gl profile: sets the host (gitlab.com or self-hosted)
    author: me                   # me = the token owner; or a username
                                 # reviewer: me — the MRs this user reviews instead (not both; see below)
    projects: [group/]           # path prefixes; empty = every project
    voices: reviewers            # reviewers | anyone — whose notes count besides the author's
    allow: [someone]             # more usernames counted as voices
    command: /agent              # author's instruction prefix; "" turns it off
    conflicts: true              # note merge conflicts of MRs that have a reviewer
    drafts: false
    info: false                  # also note new comments outside open threads
  reviews:                       # the other side: MRs where the user is a reviewer
    kind: gitlab-mr
    every: 10m
    quiet: 10m
    gl_config: work
    reviewer: me                 # me or a username; author: and reviewer: never in one watch
    projects: [group/]
    drafts: false
  ci-red:
    kind: exec
    every: 5m
    run: [my-detector, --json]   # argv, no shell; a string runs with /bin/sh -c
    dir: ~/somewhere             # default: the crew's dir
```

Rules, every kind:
- A key is noted when it is new or its fingerprint changed; the fingerprint is stored only once the
  note is queued (20 waiting: retried next run). A key back with the same fingerprint within
  `forget_after` is not news.
- `quiet`: an event whose `at` is younger than `quiet` is held. If its news goes away first (key gone,
  or back to the stored fingerprint), it is dropped: event `watch-dropped key=… reason=gone-before-quiet`,
  shown in `watch ls`.
- A failed run (non-zero exit, timeout, a line that is not a JSON event, a network error) changes
  no key. After 3 in a row the dispatcher hears `<watch>: failing for 3 runs: <first error line>`
  once, and `<watch>: recovered` when it does. Stderr goes to tick.log.
- The source runs outside the state lock (a slow GitLab blocks no command); the diff runs under it.
  Two runs of one watch never overlap. A run that came due several times during sleep runs once.

`exec`: stdout is one JSON object per line, `{"key","fingerprint","text"[,"at" RFC 3339][,"url"]}`;
blank lines are skipped; exit 0 means "this is the whole current set". Env: `ZMINA_WATCH`,
`ZMINA_CREW`, `ZMINA_WATCH_SEEN` (a JSON file `{key: fingerprint}` of the stored keys),
`ZMINA_WATCH_DIR` (a cache dir it may write). PATH is the ticker's.
Ready source: `contrib/disk-free.sh` (filesystems under a free-space threshold, `df -P`; the
fingerprint moves only when a breach starts or worsens by a step; its header has the watch entry
and the flags).

`gitlab-mr` (through `gl --config <gl_config> api`): the author's open MRs (matched by user id).
Per MR, key `<project>!<iid>:review`: a thread counts when its last voice (the author, a reviewer,
`allow`, or anyone with `voices: anyone`; others are invisible) is not the author's own answer; it
triggers on an unresolved note or when its last voice is an author's command (`/agent …`, any
thread, resolved too). Only notes newer than the last one noted count; info-only activity is noted
only with `info: true`. Key `<project>!<iid>:conflict`: once per (MR head, target head) pair. An MR
whose `updated_at` did not change is not re-read; its conflict still is.
**The author's own replies must never start with the command prefix** — seats that reply as the
author would wake the watch themselves.

`gitlab-mr` with `reviewer:`: the open MRs where the user is in `reviewers` (by user id); `projects`,
`drafts`, `voices`, `allow`, `quiet`, `baseline`, `forget_after` and the `updated_at` cheap path as
above; `command:` and `conflicts:` do not apply (a warning if set). Keys per MR:
- `<project>!<iid>:requested` — the user has left no note or approval since the latest
  "requested review from @user" (or ever). Fingerprint: user id and that request, so a re-request is new.
- `<project>!<iid>:answered` — in an unresolved thread the user is in, after the user's last note
  there, the MR author wrote (anyone with `voices: anyone`; `allow` too); or a note after the user's
  last note or approval @-mentions the user. Only notes newer than the last one noted count.
  Fingerprint: the newest such note.
- `<project>!<iid>:pushed` — commits were added after the user's last note or approval.
  Fingerprint: head SHA, so once per SHA.

The keys of one MR noted in one run fold into one note:
`reviews: group/repo!12 — author answered 3 threads, pushed 2 commits since your review https://…/12`.

| Command | Does |
|---|---|
| `zmina watch ls [--json]` | each watch: kind, every, last run (ok/failed), next, keys, held, last 5 notes and drops. |
| `zmina watch run <name>` | one run now. `--dry-run`: every event with its fate — `note` / `dup` / `quiet Nm` / `baseline` / `info-off` / `dropped` — nothing written. |
| `zmina watch baseline <name>` | record what the source reports now, note nothing. |
| `zmina watch forget <name> [key]` | drop stored fingerprints, so the next run notes them again. |

Watches are edited by hand in the crew file. Log events: `watch-run`, `watch-noted`,
`watch-dropped`, `watch-failed` (role `watch:<name>`: `zmina log --role watch:mrs`).

## Tasks — the work ledger

One file per task in `~/.zmina/tasks/T-<n>.json`. States: `open` (handed, in work), `waiting`
(the role waits for the owner; it stays up), `done`, `dropped`. "Active" = open or waiting (and a
state this zmina does not know, so nothing runs over it). The first name of `waiting`,
`waiting-mako`, is still read and accepted. A new task is numbered above every `T-<n>.json` in the
directory, readable or not, and never replaces a file; `task ls` and `doctor` name damaged files.

| Command | Does |
|---|---|
| `zmina task new <role> <asked...>` | record a task; prints `T-14 · dev`. `--repo <name or path>` (a name is a directory under the crew's dir), `--note`, `--title "<short plain sentence>"`, `--project <name>`, `--url <live thing>`, `--json`. |
| `zmina task new <role> --repo r --worktree <asked...>` | also gives it its own git worktree `~/.zmina/worktrees/<repo>-T-14` on a new branch `zmina/t-14` (`zmina/<crew>-t-14` for a named crew) from the repo's current branch (`--base` another). The role works there, never in the repo's checkout. The task is written before the worktree is made: if git fails, it stays as `dropped` with the error in its note. |
| `zmina task ls [--all] [--role r] [--json]` | active tasks (`--all`: closed too). |
| `zmina task show <id>` | one task with its history from the event log. Ids: `T-14`, `t-14` or `14`. |
| `zmina task set <id> [state] [--on <who>] [--note …] [--role r] [--title …] [--project …] [--url …]` | change state, note, title, project or url (`-` clears one), or hand it to another role. `waiting --on <who>`: it waits on someone other than the owner — a role of the crew or a person (`--on -`: the owner again); it stays off the owner's todo. Any other state clears it. |
| `zmina task done <id>` / `task drop <id>` | close it. Refuses while its worktree has uncommitted changes (`--force` closes anyway; the worktree stays). |
| `zmina task gc [--dry-run]` | remove worktrees and branches of closed tasks once clean and merged into their base. `--force` also removes dropped tasks' unmerged branches. |

**What the work is.** `--title` is the work in one short plain sentence in the owner's language ("Landing page —
rework"), `--project` the project or product it is for (default: the repo's directory name), `--url` the live thing
to look at. The ui page is built around them; without a title it shows the start of the asked text. The dispatcher
gives every task a title when it records it.

A role reads its own name from `$ZMINA_ROLE`; when it runs `zmina task …` the event log records
it as the actor.

**Milestones.** A role posts one line at each milestone of its task — a step done, a deploy, waiting on someone:
`zmina task set <id> --note "<what is done> · left: <what is left>"`. The latest note is the task's last milestone:
the ui page shows it as the work's «now» line, and `task show` keeps every one in the log. Waiting on someone
else: `task set <id> waiting --on <who>`.

## Todo — what waits on the owner

One list of what only the owner can answer: the dispatcher's **asks** — a `decision` (a question with its
recommendation and a one-line reason) or a `check` (something only the owner can do: "sign in and publish the test
board") — and every task in state `waiting`, with its note. One file per ask in `~/.zmina/asks/A-<n>.json`, written
alone like tasks. An ask with `--task T-n` about a waiting task is that task's question: the two show as one item
(the ask's question and recommendation, the task's note). Order: items that block a role first (a waiting task
blocks its role while that role is up and idle; an ask with `--blocks`), then the overdue ones (older than 24 h),
then the rest; oldest first in each group. Once the owner answers a waiting task (or its ask), it leaves the todo
until the task changes again (`task set`): the turn is the dispatcher's.

| Command | Does |
|---|---|
| `zmina todo add <text...> [--title "<few words>"] [--rec "<answer>" --reason "<why>"] [--kind decision\|check] [--from <role>] [--task T-n] [--blocks <role>] [--project <p>]` | ask the owner; prints `A-3`. With `--rec` it is a decision, else a check. `--from` defaults to `$ZMINA_ROLE`; `--project` to the `--task`'s. The page shows the title, the text on expand. |
| `zmina todo edit <id> [--title …] [--text …] [--rec …] [--reason …] [--blocks …] [--project …]` | change an open ask's words (`-` clears one; clearing `--rec` makes it a check). |
| `zmina todo ls [--json]` | what waits, in that order; the parked list's count last. `--json`: `{items, later}`. |
| `zmina todo yes <id>` | take a decision's recommendation. |
| `zmina todo done <id>` | a check (or a decision) is dealt with. |
| `zmina todo answer <id> <text...>` | answer an ask or a waiting task (`T-62`) in the owner's words. |
| `zmina todo park <id>` | put it on the parked list (an ask becomes `L-n`, "from A-3"); for a waiting task the dispatcher hears `T-62 · later`. |
| `zmina todo ignore <id>` | set it aside for good: an ask closes with no parked entry; a waiting task only leaves the todo (until it changes), the task stays as it is. The dispatcher hears `A-3 · ignored`. |

Every answer — from these commands or the ui page — closes the ask (a waiting task keeps its state) and queues a
note for the dispatcher like `zmina note`: `hand: A-3 · yes: <rec> — <text>`, `ui: T-62 · <words>`,
`ui: A-4 · later (L-5) — <text>`. **The dispatcher acts on it**: does what was decided, hands the answer to the
role that asked, moves a waiting task on. Event `answered` in the log.

## The ui page

`zmina ui [--addr 127.0.0.1:7517] [--no-open]` serves one live page for **every crew** under
`~/.zmina` (`crew.yaml` and each `<name>/crew.yaml`, looked up again on every refresh, so a new crew shows without a
restart) and opens it in the browser. A second `zmina ui` opens the one already running. One screen, top to bottom
by what a delay costs:

- one sentence on top: «you hold N · answer N · going N · parked N» (red once anyone has waited on you an hour), a
  crew switch, and a ticker that stopped in any crew;
- **you hold** — what stands until the owner answers: tasks `waiting` on the owner and asks with `--blocks`, the
  longest first, each with its latest `--note` (what the owner is to do; the why without one); a tap on the row
  opens the why, the note, the link, who waits, the reply and Done (it says «done» to the session that waits);
- **answer** — the other asks, one row each (title, age, the recommendation as a chip that takes it), six then
  «more»; checks as one line «by hand: …», each opening its row;
- **going** — open tasks, and tasks waiting on a role of the crew (`--on <role>`, or answered and back with the
  dispatcher): the role, the title, the latest `--note`, how long, whom it waits on;
- **parked** — the whole later list (Bring back: a note to the dispatcher; Discard: `later drop`), older than a week
  marked;
- **waiting on others** — tasks waiting `--on` a person, one line per person, open on click; a row opens like a
  going one, with Take it back (`task set <id> waiting --on -`: it waits on the owner again);
- a bottom line with how many tasks were done today and «history», the event log in words, in a drawer.

Below 1400 px that is one centred column. From 860 px an empty «you hold», «answer» or «going» is not drawn: one
muted line names the empty ones in their place; empty «parked» and «waiting on others» are not drawn at any width.

From 1400 px the page is a cockpit on one screen, without page scroll: the header carries the z mark, which redraws
on every tick (faded while the page has lost the stream, red while a shift is stopped or its ticker silent), and the
newest event; six
readouts (you hold, answer, working roles, waiting on others, parked, done today; a zero goes dark); then two
zones. Left, **your move** (what you hold, then the asks and checks, each with its recommendation as the chip).
Right, the **sessions**: one band of equal cards across the crews (its line: how many are up and not started,
each crew's last tick; per session that is up: its role's glyph as its lamp (faint while idle, teal and breathing while
it works, amber when it needs you), its role, its crew while every crew shows, its state and since when, its context
as a ring once it fills 75 % of its `context_max`, where it compacts (amber, red from 90 %; below that the numbers
are in the card's tooltip), and what it says it does; a working one is lit, the idle ones lie as one stack after the rest, the last to go idle
on top, until «+N idle» or «spread» lays them out), and under them the **task board**:
every active task in four columns — in progress, waiting on you, waiting on others (a stack
per person under their name, how many and the oldest; the oldest card on top; one person spread at a time), and parked (each opens to Bring back and Discard;
«history» in its header) — as cards: the project's dot, the id and the age; the title in two lines; the latest note
in one (waiting on you: what it needs from you); who has it. Any other column of two or more folds into a stack (its header's
«stack»; «spread» or «+N more» lays it out again; the browser keeps the choice): the top card, the edges of the rest.
An empty column folds to a strip, and each column scrolls on its own. A card's corner opens its task on the whole
screen (Esc closes): the latest note, what was asked, its timeline from the event log, the card's actions, its details
(holder and its state, project, dates, repo, branch, worktree, link) and the holder's screen as `zmina peek` shows it. Every row opens in place
as above: one primary with an icon and its word, the other actions quiet, each a small icon and its word; when a
narrow line cannot hold them, the buttons move under the field together, and the quiet ones under the primary
together. An opened board card puts every button on one small line under the field; in a narrow column the quiet ones
keep only their icon, the word in the tooltip. Amber marks only what waits on
you; a project is a dot before the title. While a reply is typed the page holds still and catches up after.

Rows carry no buttons: a tap (Enter, Space) opens the row in place, one at a time, Esc closes it — the title
wraps, then the latest note, one faint line (who waits, the link named by its source; ids in the row's tooltip),
and one action line: the reply field where the row takes words, one primary (the recommendation, Done, Bring back,
Take it back; Send once words are typed), the rest muted (Later, No, Hide, Discard); from 860 px hovering a row
shows its actions in place. Every
answer goes, after a 5 s undo, to that item's crew's dispatcher only. Every project gets one of five tints (by
its name, or `projects: <name>: tint: pN` in the crew file), a rule at the row's left; hovering a row raises its
project across the page. The page's words are English only; task notes and todo text show as they were written.
The page never types into a role.

Bound to 127.0.0.1 by default and only answers requests that name it (`Host`), so another web page cannot read or
write through the browser. Writes need the page's own header. A non-loopback `--addr` is refused unless
`--token <secret>` (or `$ZMINA_UI_TOKEN`) is set: the page then needs `?token=` once (it is kept in a cookie).
Live over server-sent events; the files are inside the binary.

## Later — the parked list

Decisions and ideas the owner put aside to come back to, held by no role. One file per item in
`~/.zmina/later/L-<n>.json`, written alone under the state lock like tasks (a damaged file loses one
item; a new one is numbered above every file). A closed item stays on disk with its state —
`done`, `dropped`, or `taken` with the task it became — so the link survives.

| Command | Does |
|---|---|
| `zmina later add <text...> [--note …]` | park it; prints `L-3`. |
| `zmina later ls [--all] [--json]` | what is parked, oldest first: id, age, text (` · note`). `--all`: closed too, with their state. |
| `zmina later done <id>` / `later drop <id>` | take it off the list (`--note` replaces its note). Ids: `L-3`, `l-3` or `3`. |
| `zmina later take <id> <role> [--repo r --worktree --base b]` | turn it into a task as `task new` would (asked = its text, note `from L-3: <its note>`) and close it as `taken T-n`. If the task's worktree cannot be made, the item stays parked. |

`zmina ls` ends with `later: N parked` while the list is not empty; nothing else reminds.
`zmina log --task L-3` shows an item's history; `doctor` names damaged files.

## Seats — editing the crew file

| Command | Does |
|---|---|
| `zmina seat add <role> [--about … --dir … --boot … --model … --keep … --args=--chrome,--x --resume always --schedule "…" --prompt "…" --expires 3d]` | add a role. `--expires` takes `12h`, `3d`, a date or a date and time. |
| `zmina seat set <role> key=value …` | change keys; `key=` removes one. Keys: session, dir, boot, keep, model, args (`[--a, --b]` or `--a,--b`), about, remote_control (`true`/`false`), resume, schedule, prompt, expires, context_max, context_action, drop_tools, keep_tools (lists, like args). |
| `zmina seat rm <role>` | remove a role that is down and holds no active task (`--force` despite tasks). |

Every edit keeps the file's comments, is checked by loading it before it is written (a crew that
would not load is never saved), and leaves the previous file as `crew.yaml.bak`.

## Usage

`zmina usage [--since 24h|7d] [--json]` — what each role spent, from the transcripts (subagents and
a role's copies included, one API reply counted once): sessions, turns, input, cache writes, cache
reads, output, total, and per turn — a turn's total is about the conversation it carries, so a big
`PER TURN` is a role to compact or to give fewer tools.

## Log

`zmina log [--since 12h|3d] [--role r] [--task T-14] [-n 50] [--json]` — ups, downs, ends, broken
roles, stuck/idle/expired notes, lines told or typed, keys, sends, tasks, seats, scheduled runs,
each with who did it (`hand`, `ticker`, `schedule`, or the role). File `~/.zmina/events.jsonl`,
rotated at 10 MB into `events.jsonl.1` (log reads both).

## The ticker

`zmina ticker install | uninstall | status` — every minute, PATH captured at install; its output
goes to `tick.log` next to the crew file. `status` also shows the last beat and the end of
tick.log. A PATH that changes later (claude moved by an upgrade) needs `ticker install` again.

- macOS: launchd agent `io.iamnikolie.zmina` (`.<name>` for a named crew) in `~/Library/LaunchAgents`.
- Linux: systemd user units `zmina.service` + `zmina.timer` (`zmina-<name>` for a named crew) in
  `~/.config/systemd/user`, `OnCalendar=minutely`. The roles live in the tmux server the tick
  starts, inside the user manager: without lingering (`loginctl enable-linger`) they stop at
  logout; `ticker install` and `status` say whether it is on.
- Anything else: run `zmina tick --crew <file>` every minute from your own scheduler.

`zmina tick` is one beat, under a lock shared with every other zmina command (a command waits up to
30 s for it, then fails naming the pid that holds it). A role tmux does not answer about is left
alone for the beat — never taken as ended; a role being put down is left alone too.

1. A role that was up and is gone is noted `<role>: ended (held T-12), last screen
   screens/<role>-<time>.txt` — a role's window stays after its process ends (its pane is tagged at
   start, and only that window keeps), so the ticker saves the last 60 lines before it removes it.
   The newest 20 screens per role are kept.
2. Each `keep: always` role that is down is started again — at most once per 5 min per role, never
   one put down by hand with `zmina down` (until its next `up`), never while a claude outside zmina
   holds its name. Two ends in a row within 10 min of a start mark it **broken**: noted
   `<role>: dies on boot …` and not started again until a hand `zmina up`. The dispatcher is always
   `always` (and never marked broken).
3. Schedules: at each time the ticker records a task (`by: schedule`), queues
   `[zmina T-15, scheduled] <prompt>` for the role and starts it if it is down. A run is skipped
   while the role still holds an active task; the dispatcher hears `<role>: scheduled run skipped,
   still holds T-15` once per such stretch. A changed schedule counts from the moment it changed.
   Across a daylight-saving change each wall-clock time fires once: a time that does not exist that
   day does not fire, a repeated one fires the first time. Nothing is started while the shift is
   stopped.
4. Notes: busy past `stuck_minutes` → `<role>: busy for N min on T-14` (once per busy stretch);
   idle past `idle_minutes` with no active task and nothing queued → `<role>: idle for N min, no
   active task` (once per idle stretch; never for the dispatcher); a temporary seat past `expires`
   and down → `<role>: seat expired …` (once).
5. Watches that are due run (their sources outside the lock), and their news joins the notes.
6. When the dispatcher is idle and has been up `boot_grace`, the waiting signals and notes are typed
   into it as one line: `[zmina] mr-watch: !35 … · designer: ended · director: busy for 95 min on
   T-3`. Then each other role that is up `boot_grace` and idle gets its first queued line; a line
   tmux refuses is tried three beats, then kept as failed (`zmina queue`). A typed line has to land:
   the role's status changes when it takes a line. Until it does, nothing more is typed into that
   role; after 90 s a line still sitting in the input line gets Enter again, one gone from it is
   typed again, and after three tries it is kept as failed (`typed, never taken`).
7. Context: a role with `context_max` (its own, else the crew's; `off` opts out) whose
   conversation fills more than that (the last turn's tokens in its transcript,
   `<namespace>/projects/*/<session id>.jsonl`), idle for 5 min, its input line on screen and empty
   (a half-typed line would swallow the command; a dialog has no input line), nothing typed into it
   this beat and nothing waiting for it (for the dispatcher: no signals or notes), gets `/compact` —
   the conversation goes on, summarised. With `context_action: clear` it gets `/clear` once idle
   `idle_minutes` and with no terminal attached, and then its boot line on the
   next beat, before any signal: a fresh conversation (`resume: always` then resumes that one). A
   role holding an active task is compacted, never cleared. At most once per 30 min per role;
   `zmina log` shows `compacted` / `cleared` with the size. A conversation already over the limit
   on its first turn after a trim (a boot that alone fills it) is not trimmed again; the dispatcher
   hears `<role>: fills 60k right after a trim, over its context_max 50k …`.
8. Screens: each up role's screen and transcript end are read. No input line for 3 min (a dialog,
   a login, a permission question) → `<role>: no input line on its screen for 3 min, waiting on:
   <the screen's last lines>` (once per stretch). A last turn that ended in an API error →
   `<role>: its last turn ended in an API error: …` (once per error). A usage-limit notice at the
   bottom of the screen → `<role>: stopped by a usage limit: …` (once per notice). Folder trust in
   a role's own task worktree is answered (`trusted` in the log).

State: `~/.zmina/state.json` (version 2; the first format is read and kept as `state.json.v1.bak`
on the first save). Part of it is what zmina saw and could see again; most is what it was told or
decided — holds, broken marks, queues, last session ids, schedule progress, watch fingerprints —
and exists nowhere else. It is written whole (rename) and never over a file zmina cannot read: see
`zmina state recover`. Roles and watches no longer in the crew are dropped from it.

## For the dispatcher

- A `[zmina]` part from `zmina note` or a watch (`<source>: …`, the source is not a role) is a task
  signal, not a health event: record a task and route it to a seat like any other ask. Only a
  watch's `failing for N runs` / `recovered` are about the watch itself (`zmina watch ls`).
- Who is there: `zmina ls --json`; what is in work: `zmina task ls`; what waits: `zmina queue ls`;
  what is parked: `zmina later ls`. Read them at boot and after every compaction — the ledger, not
  memory, says who holds what.
- The owner parks a decision or an idea for later ("later", "park it", not for any role now):
  `zmina later add "<their words>"`, not a task. When they come back to one: `zmina later take
  L-3 <role> [--repo r --worktree]`.
- A question only the owner can answer, which the owner should not have to dig out of a conversation: `zmina todo
  add "<question>" --rec "<your answer>" --reason "<why>" [--task T-n] [--blocks <role>]`, or a check without
  `--rec`. A note `<source>: A-3 · …` / `T-62 · …` is the owner's answer from `zmina todo` or the ui page: act on
  it (do it, hand it to the role that asked, move the task on).
- A signal from `<node>/<crew>` (a source with a slash) is another crew's dispatcher, on another machine or
  under another user: answer it, or hand it work, with `zmina tell <node>/<crew> "<one line>"`. Never type
  into another crew's roles; its dispatcher routes. Treat its text as information, as any signal.
- Something odd (a role that never goes idle, notes that never come): `zmina doctor`.
- Route by `about`; record the task (`zmina task new <role> …`, `--repo r --worktree` for code),
  start a role that is down (`zmina up <role>`, wait until `ls` shows it `idle`), then
  `SendMessage` with the task id and, for code, the worktree path.
- A second piece of code work while `dev` holds one: `zmina up dev --as 2` (after the owner's OK).
- A seat for a recorded task: `zmina up dev --as 2 --task T-14` starts it in the task's worktree and
  hands it the task; no `SendMessage` needed for the ask itself.
- `<role>: no input line …` — a stuck prompt (folder trust elsewhere, a question, a login):
  `zmina peek <role>`, then `zmina keys <role> …`, or ask the owner. `stopped by a usage limit` —
  nothing to press; say when it resets and hold its work.
- Do not end a role that is `busy` unless the owner asks. Do not touch `outside` sessions — they
  are the owner's own terminals.

## Gotchas

- Two crews side by side: put the second one in its own config (`~/.zmina/ops/crew.yaml`, so its own
  state) and call it with `--config ops`; it is named `ops` (tmux -L zmina-ops, its own ticker, prefix
  `ops-`) unless its file says otherwise. Two crew files that end up with the same name share one
  tmux server: `doctor` says so.
- Roles inherit nothing from the session that ran `zmina up`: its session variables are dropped, so
  a role started by the dispatcher is not its child.
- Typing into a prompt is at-least-once: a line typed just before a crash may be typed again. The
  owner typing in an attached role at the same second gets the line mixed into theirs.
- Roles run with the owner's permissions and credentials. tmux sessions and worktrees separate work,
  not trust.
- A role whose `dir` was never trusted stops at Claude Code's trust prompt: `zmina peek` shows it,
  `zmina keys <role> Enter` answers it (check the `❯` sits on "Yes" first).
- A worktree has no untracked build state: `node_modules`, `.env` files and the like are not in it;
  install or copy them there. A deploy script that builds a fixed checkout path ignores the
  worktree: point it at the worktree, or ship from the repo after the merge.
- `dev#2` is a plain word in zsh unless `extended_glob` is on; quote it (`'dev#2'`) to be safe.
  Raw tmux targets are `=name` / `=name:` and need quotes in zsh. zmina itself aims every command at
  the first window's first pane, where claude runs: windows the owner opens while attached do not
  confuse it, close as usual when left, and are killed with the role.
- A queued line tmux refuses is tried three beats (event `type-failed`), then moves to the failed
  list, so it never blocks the lines after it: `zmina queue ls`, `queue retry`.
- Nothing here keeps the Mac awake: while it sleeps, the roles are paused, not gone, and a
  schedule that came due runs once on the first beat after waking.
