package shift

import (
	"fmt"

	"github.com/iamnikolie/zmina/internal/donecheck"
	"github.com/iamnikolie/zmina/internal/task"
)

// RecordCheckEnv keeps a done check that could not run (donecheck.KindEnvironment) on task t, which stays as it was: the
// result is listed in the task's file as CheckEnv (its state, result, delivery and Updated are not touched, so a check run
// again is still of this task), and the dispatcher is told once for each task and cause, in the note a late report is told
// in, however often the check meets the same fault. The check's run is an event already (ledger.RunCheck). t is the task
// as read under the lock; by is who ran the command. It returns whether the dispatcher was told now: false for a fault the
// task has told of already, and for a task that is closed since the check began.
func (s *Shift) RecordCheckEnv(t task.Task, by string, res donecheck.Result) (bool, error) {
	if !res.Environment() || !t.State.Active() || s.Tasks == nil {
		return false, nil
	}

	told := t.CheckEnv != nil && t.CheckEnv.Fault() == res.Fault()

	t.CheckEnv = &res

	if err := s.Tasks.Put(t); err != nil {
		return false, err
	}

	if told {
		return false, nil
	}

	if _, _, err := s.note(by, checkEnvText(t, res), false, true); err != nil {
		if s.Warn != nil {
			s.Warn(err)
		}

		return false, nil
	}

	return true, nil
}

// checkEnvText is the dispatcher's note: one line, once for a task and a cause. It asks for nothing but a seat that can
// run the check; the worker cannot mend it.
func checkEnvText(t task.Task, res donecheck.Result) string {
	return fmt.Sprintf("%s (%s): its done check could not run, and the work was not judged: %s (%s); the environment of the "+
		"check is broken, not the task, which stays open: fix it, then the role runs zmina task done %s again (zmina task show %s)",
		t.ID, t.Role, donecheck.Line(res.Fault()), donecheck.Line(res.Run), t.ID, t.ID)
}
