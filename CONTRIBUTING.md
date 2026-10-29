# Contributing

Thanks for taking a look. This is a small, focused CLI — bug reports and pull
requests are welcome, and so is a plain question in an issue.

## Reporting a bug

Include the output of `zmina version`, `zmina doctor`, your OS, `tmux -V` and
`claude --version`, the exact command you ran, and what you expected instead.
**Redact anything private** — paths, prompts, screens — before pasting.

## Pull requests

Before opening one:

```bash
make fmt           # gofmt -w .
make vet           # go vet ./...
make test          # go test ./... (needs tmux 3.2+ on PATH)
golangci-lint run  # config in .golangci.yml
```

CI runs the same on Linux and macOS, so a green local run usually means a green
PR.

House rules:

- **One concern per PR.** A bug fix and a refactor in the same diff take three
  times as long to review.
- **A test for every behaviour change.** `internal/shift` is tested with fakes;
  `internal/tmux` and `cmd` against a real tmux server.
- **Files a running crew wrote stay readable.** `state.json`, `tasks/*.json`
  and `crew.yaml`: a new field is optional, a renamed value keeps its old name
  readable.
- **Update the docs in the same commit.** Any change to the commands or the
  crew file must also update `cmd/skill.md` (embedded in the binary, printed by
  `zmina skill`, and the single source of truth for command UX) and, where it
  shows, `README.md`.
- **Conventional commit subjects** — `feat:`, `fix:`, `docs:`, `refactor:`,
  `test:`, `chore:`. Release notes are generated from them.

## Releases

Maintainer-only. Tag and push:

```bash
git tag -a v1.2.3 -m "v1.2.3"
git push origin v1.2.3
```

GoReleaser builds archives for linux/darwin on amd64 and arm64, publishes the
GitHub release with a generated changelog and updates the Homebrew formula.
