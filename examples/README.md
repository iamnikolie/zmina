# zmina examples

A crew to read and to start from: five roles and the boot documents that make them work together
through zmina's task ledger. Nothing here is a live crew; copy it and change it.

| File | What it shows |
|---|---|
| `crew.yaml` | a named crew (own tmux server and ticker), per-role model and flags, a scheduled role, a temporary seat |
| `agents/dispatcher/AGENT.md` | the session that never sleeps: routing, recording tasks, health notes from the ticker |
| `agents/dev/AGENT.md` | a developer seat: works in the task's worktree, closes its own task |
| `agents/designer/AGENT.md` | a judgment seat: answers as a decision, asks the owner only with a proposed answer |
| `agents/reviewer/AGENT.md` | a read-only seat that reviews another task's worktree |
| `agents/scribe/AGENT.md` | a scheduled seat: the ticker starts it and types its prompt; it closes its run's task |

## Try it

```
mkdir -p ~/.zmina/example && cp -R examples/. ~/.zmina/example/
$EDITOR ~/.zmina/example/crew.yaml          # namespace, dir, models
zmina --config example ls
zmina --config example up dispatcher
zmina --config example ticker install       # its own ticker (launchd on macOS, systemd on Linux)
```

Then talk to `ex-dispatcher` from a terminal (`zmina --config example attach dispatcher`) or from
claude.ai/code. Take it down with `zmina --config example down --all` and
`zmina --config example ticker uninstall`.

## How the pieces meet

```
owner ──▶ ex-dispatcher ── zmina task new dev --repo api --worktree "…" ──▶ T-7 + worktree
                │
                └─ SendMessage(ex-dev, "T-7 · worktree … · done when …")
                                   │
ex-dev ── works in the worktree ── zmina task done T-7 --note "…" ──▶ ledger
   │
   └─ SendMessage(ex-dispatcher, "T-7 done: …") ──▶ dispatcher relays to the owner

ticker (every minute) ── health notes, scheduled runs, queued lines ──▶ ex-dispatcher / roles
```

Every boot document follows one shape — role, boot checklist, how a task is worked, boundaries —
and points at `zmina skill` for the commands instead of repeating them.
