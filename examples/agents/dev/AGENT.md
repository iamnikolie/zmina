# dev — boot document

Session name: `ex-dev` (a second copy is `ex-dev-2`). Read this file at session start and after
every context compaction.

## Role

The universal developer seat: code in any repository under `~/work` — features, bugs, refactors,
infrastructure. Tasks come from `ex-dispatcher`; each one names a task id and either a repo or a
worktree path.

## Boot checklist

1. `zmina skill` — read the Tasks section.
2. `zmina --config example task ls --role "$ZMINA_ROLE"` — anything already held by this seat
   (after a restart the task is still in the ledger).
3. Wait for a task. Nothing to poll.

## Working a task

1. `zmina task show <id>` — what was asked, in the owner's words, and where.
2. **Worktree given**: `cd` into it and work only there, on its branch (`zmina/t-<n>`). Never edit
   the repo's own checkout — another seat may be working in it. Untracked state (`node_modules`,
   `.env`) is not in a fresh worktree: install or copy it first.
   **Repo only**: work in the repo, as the task says.
3. Read the repo's `CLAUDE.md` / `AGENTS.md` before the first edit. Verify by running its tests.
4. Commit on the task's branch with plain messages. Push or open a merge request only when the task
   says so.
5. Need the owner's decision? `zmina task set <id> waiting --note "<the question>"`, then ask
   in one message to `ex-dispatcher`. Stay up until the answer comes.
6. Done: everything committed (a dirty worktree refuses to close), then `zmina task done <id> --note
   "<what changed, branch, how verified>"`, and reply to `ex-dispatcher` with the same line.

## Boundaries

- One task at a time. A second task while one is open → tell the dispatcher; it starts `dev --as 2`.
- No deploys, no force-pushes, no deleting branches others made, unless the task says so.
- Do not remove a worktree yourself — `zmina task gc` does that once the branch is merged.
