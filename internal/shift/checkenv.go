package shift

import (
	"fmt"
	"slices"
	"strings"

	"github.com/iamnikolie/zmina/internal/donecheck"
	"github.com/iamnikolie/zmina/internal/task"
)

// CheckEnvTell is what RecordCheckEnv did about telling the dispatcher.
type CheckEnvTell struct {
	// Queued says the dispatcher's note is queued: it is told when the state that holds the note is saved, and only then is the
	// cause marked told (MarkCheckEnvTold).
	Queued bool
	// Already says the dispatcher was told this cause on this task before: nothing is queued.
	Already bool
	// Err is why the note could not be queued, when it could not. The fault is kept all the same, the dispatcher is not told, and
	// the cause is not marked told, so that the next run of the check tries again.
	Err error
}

// RecordCheckEnv keeps a done check that could not run (donecheck.KindEnvironment) on task t, which stays as it was: the
// result is listed in the task's file as CheckEnv (its state, result, delivery and Updated are not touched, so a check run
// again is still of this task), and the dispatcher's note is queued, in the note a late report is told in, unless the task has
// told this cause already. The event of the run is the ledger's (ledger.RunCheck). t is the task as read under the lock; by is
// who ran the command.
//
// It does not say the cause was told: a note that could not be queued, or a state that could not be saved after it, would be
// a dispatcher that was never told. The caller saves the state and then calls MarkCheckEnvTold, and says "told" only when the
// note was queued and the save went. The error is the task file that could not be written.
func (s *Shift) RecordCheckEnv(t task.Task, by string, res donecheck.Result) (CheckEnvTell, error) {
	var out CheckEnvTell

	if !res.Environment() || !t.State.Active() || s.Tasks == nil {
		return out, nil
	}

	t.CheckEnv = &res

	if err := s.Tasks.Put(t); err != nil {
		return out, err
	}

	// Told before, or being told by a run that queued its note and has not marked it yet (two runs of the check at once): the
	// note waits for the dispatcher in the state, which this run reads under the same lock, so it is folded into that one.
	if slices.Contains(t.CheckEnvTold, res.Fault()) || s.checkEnvWaiting(t, res) {
		out.Already = true

		return out, nil
	}

	if _, _, err := s.note(by, checkEnvText(t, res), false, true); err != nil {
		out.Err = err

		return out, nil
	}

	out.Queued = true

	return out, nil
}

// checkEnvWaiting reports whether the note for this task and cause is waiting for the dispatcher already.
func (s *Shift) checkEnvWaiting(t task.Task, res donecheck.Result) bool {
	head := fmt.Sprintf("%s (%s): its done check could not run, so the work was not judged: %s", t.ID, t.Role, donecheck.Line(res.Fault()))

	return slices.ContainsFunc(s.State.Signals, func(sig string) bool { return strings.Contains(sig, head) })
}

// MarkCheckEnvTold records that the dispatcher was told of the fault res on task id: its cause is in the task's set of told
// causes, the last task.CheckEnvToldMax of them (a cause that comes back is told no more, a ninth distinct one makes the oldest
// forgotten). Called once the state with the note is saved, which is some time after RecordCheckEnv, so it marks only if the
// fault on the task still is this one (the same check, commit and cause): a task whose check was changed or cleared meanwhile,
// or whose check has met another fault since, has nothing of this one to mark. A task that is not active any more, or has told
// the cause already, is left as it is.
func (s *Shift) MarkCheckEnvTold(id string, res donecheck.Result) error {
	if s.Tasks == nil {
		return nil
	}

	t, err := s.Tasks.Get(id)
	if err != nil {
		return err
	}

	cause := res.Fault()

	if !t.State.Active() || slices.Contains(t.CheckEnvTold, cause) {
		return nil
	}

	if c := t.CheckEnv; c == nil || c.Run != res.Run || c.Commit != res.Commit || c.Fault() != cause {
		return nil
	}

	t.CheckEnvTold = append(t.CheckEnvTold, cause)
	if len(t.CheckEnvTold) > task.CheckEnvToldMax {
		t.CheckEnvTold = t.CheckEnvTold[len(t.CheckEnvTold)-task.CheckEnvToldMax:]
	}

	return s.Tasks.Put(t)
}

// checkEnvText is the dispatcher's note: one line, once for a task and a cause. It asks for nothing but a seat that can run
// the check; the worker cannot mend it.
func checkEnvText(t task.Task, res donecheck.Result) string {
	return fmt.Sprintf("%s (%s): its done check could not run, so the work was not judged: %s (%s); the environment of the "+
		"check is broken or a file it runs is missing in the worktree, not the task, which stays open: mend it, then the role runs "+
		"zmina task done %s again (zmina task show %s)", t.ID, t.Role, donecheck.Line(res.Fault()), donecheck.Line(res.Run), t.ID,
		t.ID)
}
