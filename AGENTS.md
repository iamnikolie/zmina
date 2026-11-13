# zmina

Go + cobra. `cmd/`: one file per command group; `NewRoot(out, err)` builds the whole tree per run, every command
with its own flags (no package-level flag variables); `skill.md` is embedded and is the spec. `internal/crew` (crew
file: parse, resolve paths, check; `edit.go` edits it as a YAML tree for `zmina seat`; `history.go` keeps its versions in `crew.d/history`; `layout.go` names every
file of a crew), `internal/shift` (roles, tick, schedules, queues, state — tested with fakes), `internal/ledger`
(task + worktree rules), `internal/task` (`tasks/T-<n>.json`), `internal/worktree` (git), `internal/tmux` (the
role's tagged pane; tested against a real tmux), `internal/registry` (Claude Code `<ns>/sessions/<pid>.json`),
`internal/screenrules` (what zmina reads on a role's screen as embedded rows, one table per engine, with a small matcher and the crew's optional `screen-rules.yaml` over them; no crew, no state), `internal/events` (`events.jsonl`), `internal/limits` (plan limits: the status-line tap's file for Claude, Codex rollouts, staleness, policy; no crew, no state), `internal/route` (the crew file's routing rules as a pure function: rules, seat state and limits in, a seat and why out; no crew file, no state, no clock), `internal/cron` (five-field schedules, no dependency), `internal/watch`, `internal/plugin` (plugins: the events a program is told of on its stdin, its lookup and run under a deadline, what each was told, the plan of what to send; no crew state, no clock), `internal/mesh` (nodes:
keys signed by the owner's key, signed envelopes, the stdio protocol peers speak over ssh; tested over pipes).

- `make test`, `make install`; `golangci-lint run` is clean (config in `.golangci.yml`). Keep `go 1.24` in go.mod (no `sync.WaitGroup.Go` and other 1.25+ API); a dependency bump can raise it silently, so re-check.
- Any change to commands or the crew file updates `cmd/skill.md` in the same commit.
- Files the live crews have written must stay readable: `state.json` (versioned; decodeV1 reads the first format),
  `tasks/*.json`, `crew.yaml`. A new field is optional; a renamed value keeps its old name readable. A field added
  to or removed from `state.json` moves `stateVersion` (an older build refuses a higher number, and so never
  saves the state back without a field it does not know); `TestStateFieldsGolden` fails until it has, and
  `go test ./internal/shift -run TestStateFieldsGolden -update` records the new set.
- tmux targets are `=name` / `=name:`; in zsh quote them (`=word` is command-path expansion).
- The repo is public: fixtures, examples and docs use neutral names (alex, acme, T-1). Never a real person, client,
  ticket id, host or path from a live crew.
