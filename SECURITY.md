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
- **A plugin is trusted with the owner's voice.** A program the crew file names under `plugins:` is told what reaches the
  owner and can answer in his name (`--by`, with `ZMINA_PLUGIN` set to its name, which guards against a mistake and proves
  nothing: any process of yours can set it). It must check for itself who is talking to it. zmina runs it only from `run`, PATH
  or the crew's `plugins/` directory, and only a regular executable file that group and others cannot write; it hands it
  roles' text as cleaned and bounded data, never as an instruction, and none of its own secrets in the environment.
- **The commands waiting for approval are kept in a file.** The `PermissionRequest` hook adds each tool call of a role to
  `hooks/<role>.permission.json` (mode 0600, the last 8 calls, each cut to 1000 characters), so that a plugin and the cockpit can
  show what a permission prompt is about. They stay until later calls push them out, a day passes or the role starts again, and
  a command may carry a secret. The file is the crew's own and is read by nothing that leaves the machine except a plugin the owner
  configured, which is shown the one call a prompt is certainly of.
