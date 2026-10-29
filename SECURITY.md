# Security Policy

## Supported versions

The latest release is the supported one. Fixes land on `main` and go out in the
next tag.

## Reporting a vulnerability

Please do **not** open a public issue for a security problem.

Use GitHub's private vulnerability reporting instead:
[Security → Report a vulnerability](https://github.com/iamnikolie/zmina/security/advisories/new).
That opens a private advisory visible only to the maintainers.

Include what you did, what happened, and the impact you think it has. Expect a
first response within a week — this is a spare-time project, not a product with
an on-call rotation.

## Scope notes

Some things are known and by design rather than vulnerabilities:

- **Roles run with your permissions and credentials.** Every role is a Claude
  Code session started as you, in a directory you chose, with the flags in your
  crew file. tmux sessions and worktrees separate work, not trust.
- **The tmux server is yours.** Anything running as you can attach to
  `tmux -L zmina`, read a role's screen and type into it — as it can with any
  of your terminals.
- **Watch sources are commands you configure.** An `exec` watch runs its argv
  (or a `/bin/sh -c` string) every few minutes with the ticker's PATH; its
  output is typed into the dispatcher's prompt. Treat a source's text, and
  GitLab comments read by `gitlab-mr`, as untrusted input to an agent (prompt
  injection).
- **Claude Code folder trust** is answered automatically only inside worktrees
  zmina created itself under the crew's `worktrees/` directory.
