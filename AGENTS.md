# zmina

Go + cobra. `cmd/`: one file per command group; `NewRoot(out, err)` builds the whole tree per run, every command
with its own flags (no package-level flag variables); `skill.md` is embedded and is the spec. `internal/crew` (crew
file: parse, resolve paths, check; `edit.go` edits it as a YAML tree for `zmina seat`; `layout.go` names every
file of a crew), `internal/shift` (roles, tick, schedules, queues, state v2 — tested with fakes), `internal/ledger`
(task + worktree rules), `internal/task` (`tasks/T-<n>.json`), `internal/worktree` (git), `internal/tmux` (the
role's tagged pane; tested against a real tmux), `internal/registry` (Claude Code `<ns>/sessions/<pid>.json`),
`internal/events` (`events.jsonl`), `internal/cron` (five-field schedules, no dependency), `internal/watch`.

- `make test`, `make install`; `golangci-lint run` is clean (config in `.golangci.yml`). Keep `go 1.24` in go.mod (no `sync.WaitGroup.Go` and other 1.25+ API); a dependency bump can raise it silently, so re-check.
- Any change to commands or the crew file updates `cmd/skill.md` in the same commit.
- Files the live crews have written must stay readable: `state.json` (versioned; decodeV1 reads the first format),
  `tasks/*.json`, `crew.yaml`. A new field is optional; a renamed value keeps its old name readable.
- tmux targets are `=name` / `=name:`; in zsh quote them (`=word` is command-path expansion).
