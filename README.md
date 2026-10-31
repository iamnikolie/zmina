# zmina

[![CI](https://github.com/iamnikolie/zmina/actions/workflows/ci.yml/badge.svg)](https://github.com/iamnikolie/zmina/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/iamnikolie/zmina)](https://github.com/iamnikolie/zmina/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

The shift: long-lived Claude Code sessions, one per role (a designer, a reviewer, developer seats,
whatever you name), each in its own tmux session on a private tmux server (`tmux -L zmina`), named
and open to Remote Control. One role, the dispatcher, never sleeps: a ticker (launchd on macOS, a
systemd user timer on Linux) keeps it up, starts the other `keep: always` roles again, runs
scheduled roles and types health notes into it. Work handed to roles is kept in a task ledger
(with a git worktree per code task), and everything that happens goes to an event log.

```
zmina init                # ~/.zmina/crew.yaml
zmina up dispatcher
zmina ticker install      # the ticker, every minute
zmina doctor              # check the setup
zmina ls
zmina task new dev --repo api --worktree "fix the login"
zmina task park T-14 --note "tests left"   # what remains goes to the later list (an idle role is released so)
zmina later take L-3 dev --resume          # back to work, in the role's own conversation
zmina log --since 12h
zmina --config ops note --from mr-watch --if-up "!35 — 4 new threads"   # a job signals the dispatcher
zmina watch ls            # detectors the ticker runs (watch: in the crew file), e.g. GitLab MR reviews
zmina ui                  # one live page for every crew: what you hold, what to answer, what goes
zmina tell box-ops/ops "the dump is ready"   # a crew on another machine or user (zmina node), no hub
```

Reference: `zmina skill` (`cmd/skill.md`, embedded in the binary). An example crew with five roles
and their boot documents, ready to copy: `examples/`. A ready watch source: `contrib/disk-free.sh`.

## Install

**Homebrew:**

```bash
brew trust iamnikolie/tap   # Homebrew 6 refuses untrusted third-party taps
brew tap iamnikolie/tap
brew install iamnikolie/tap/zmina
```

**Prebuilt binary** from [Releases](https://github.com/iamnikolie/zmina/releases) (macOS, Linux;
amd64/arm64), or **from source**:

```bash
go install github.com/iamnikolie/zmina@latest
# or from a checkout:
make install        # builds ./zmina and symlinks ~/.local/bin/zmina (PREFIX=... to change)
```

## What it is, and is not

A small supervisor for persistent, named Claude Code roles, with the work kept outside their
conversations (task ledger, event log) and recovery built in (last screens, broken roles, queues
that never lose an accepted signal, a state file that is never written over blind). It is not a
TUI for running many agents on one task (claude-squad does that), not a manager hierarchy or merge
queue (Gas Town), and not in-process delegation (Claude Code agent teams).

## Runs on

| | |
|---|---|
| macOS | launchd ticker |
| Linux | systemd user session (`loginctl enable-linger` to keep roles after logout) |
| tmux | 3.2 or newer |
| Claude Code | the `claude` CLI, logged in |
| Go | 1.24 or newer, to build |

The `gitlab-mr` watch reads GitLab through [`gl`](https://github.com/iamnikolie/gitlab-cli) (a
profile per GitLab host); the other features need nothing beyond tmux and Claude Code.

zmina reads Claude Code's own session registry (`<CLAUDE_CONFIG_DIR>/sessions/<pid>.json`) to tell
idle from busy, each session's transcript (`projects/*/<session id>.jsonl`) for how full its
conversation is, what it spent and whether its last turn failed, and each role's screen for an
empty input line or a dialog; it passes `--name`, `--resume` and `--remote-control`. Neither is a
public interface: `zmina doctor` checks the registry's shape and says when a Claude Code update
changed it.

Two things to know: a line typed into a prompt is delivered at least once, not exactly once; and
roles run with your permissions and credentials — tmux sessions and worktrees separate work, not
trust.

## When something is off

`zmina doctor` (setup), `zmina ls` (first line: when the ticker last beat), `zmina queue ls`
(what waits, what failed), `zmina log --since 12h` (what happened), `zmina state recover` (a
state file zmina cannot read), `zmina stop` / `zmina start` (the whole shift).

## License

MIT — see [LICENSE](LICENSE). Not affiliated with Anthropic; Claude Code is theirs.
