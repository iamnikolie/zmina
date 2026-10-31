package shift

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/task"
)

// releaseWait is how long a released role has to park its tasks itself before the ticker parks what is left with
// the task's title and the role's last status line.
const releaseWait = 3 * time.Minute

// releases reports whether the ticker releases the role once it sits idle: release_idle_minutes is set for it, it
// is keep: manual and it is not the dispatcher. Such a role gets no idle notice.
func (s *Shift) releases(r *crew.Role) bool {
	return r.ReleaseAfter() > 0 && r.Keep == crew.KeepManual && r.Name != s.Crew.Dispatcher
}

// release puts down a role that has sat idle its release_idle_minutes and moves the tasks it holds to the later
// list, so a role never sits for hours holding work. Only a role that releases (see releases) and is up, booted
// and idle, with nothing queued for it, its input line on screen and empty (a dim suggestion is no draft) and no
// terminal attached. One with no task goes down at once. One with tasks first gets the release line, so it writes
// what remains of each itself (zmina task park); what it has not parked releaseWait later is parked with the task's
// title and its last status line, and it goes down. A new task for the role or a draft in its input line calls the
// release off. It returns a line for the log and a note for the dispatcher.
func (s *Shift) release(name string, r *crew.Role, now time.Time) (did, note string) {
	rs := s.State.Role(name)

	if !s.releases(r) || !rs.Up {
		rs.Releasing, rs.ReleaseTasks, rs.ReleaseIdle = time.Time{}, nil, 0

		return "", ""
	}

	if len(rs.Inbox) > 0 || !s.ready(name, now) || !s.landed(name, now) {
		return "", ""
	}

	sess := s.session(name)
	if sess == nil {
		return "", ""
	}

	idle := now.Sub(sess.StatusSince())
	if rs.Releasing.IsZero() && idle < r.ReleaseAfter() {
		return "", ""
	}

	draft, _, ok, err := s.Tmux.Input(name)
	if err != nil {
		return "", ""
	}

	attached, err := s.Tmux.Attached(name)
	if err != nil {
		return "", ""
	}

	var mine []task.Task

	if s.Tasks != nil {
		for _, t := range s.Tasks.Active() {
			if t.Role == name {
				mine = append(mine, t)
			}
		}
	}

	ids := make([]string, 0, len(mine))
	for _, t := range mine {
		ids = append(ids, t.ID)
	}

	if rs.Releasing.IsZero() {
		if !ok || draft != "" || attached {
			return "", ""
		}

		rs.ReleaseIdle = idle
		if len(mine) == 0 {
			return s.putDown(name)
		}

		rs.Releasing, rs.ReleaseTasks = now, ids
		rs.Inbox = append(rs.Inbox, releaseLine(idle, ids))
		s.log(name, "releasing", strings.Join(ids, ","), "idle "+idleFor(idle))

		return fmt.Sprintf("%s: idle %s, released; asked to park %s", name, idleFor(idle), strings.Join(ids, ", ")), ""
	}

	for _, id := range ids {
		if !slices.Contains(rs.ReleaseTasks, id) {
			return s.callOff(name, "it got "+id)
		}
	}

	if ok && draft != "" {
		return s.callOff(name, "a draft in its input line")
	}

	// A dialog (perhaps the permission to run zmina task park) or an attached terminal: wait.
	if !ok || attached || (len(mine) > 0 && now.Sub(rs.Releasing) < releaseWait) {
		return "", ""
	}

	for _, t := range mine {
		text := t.Headline()
		if rs.Now != "" {
			text += "; last: " + rs.Now
		}

		if _, err := s.Park(t, text); err != nil {
			return name + ": release: " + err.Error(), ""
		}
	}

	return s.putDown(name)
}

// callOff ends a release the role is no longer free for; it is released again after another idle stretch.
func (s *Shift) callOff(name, why string) (did, note string) {
	rs := s.State.Role(name)
	tasks := strings.Join(rs.ReleaseTasks, ",")
	rs.Releasing, rs.ReleaseTasks, rs.ReleaseIdle = time.Time{}, nil, 0

	line := name + ": release called off, " + why
	s.log(name, "release-off", tasks, why)

	return line, ""
}

// putDown ends a released role (BeginDown: the caller finishes the down outside the lock) and names the later items
// its tasks became.
func (s *Shift) putDown(name string) (did, note string) {
	rs := s.State.Role(name)

	var items []string

	for _, id := range rs.ReleaseTasks {
		if t, err := s.Tasks.Get(id); err == nil && t.State == task.Parked && t.Later != "" {
			items = append(items, t.Later)
		}
	}

	idle := rs.ReleaseIdle
	if _, err := s.BeginDown(name, DownOptions{}); err != nil {
		return name + ": release: " + err.Error(), ""
	}

	tasks := strings.Join(rs.ReleaseTasks, ",")
	rs.Releasing, rs.ReleaseTasks, rs.ReleaseIdle = time.Time{}, nil, 0
	s.Released = append(s.Released, name)

	note = "released " + name + " after " + idleFor(idle) + " idle"
	if len(items) > 0 {
		note += " → " + strings.Join(items, ", ")
	}

	s.log(name, "released", tasks, note)

	return note, note
}

// releaseLine is what a released role is told: write what remains of each task it holds, then stop.
func releaseLine(idle time.Duration, ids []string) string {
	id := "<id>"
	if len(ids) == 1 {
		id = ids[0]
	}

	return fmt.Sprintf("[zmina] idle %s: you are released and go down in a few minutes. For each task you hold (%s) "+
		"write what remains in one or two lines: zmina task park %s --note \"<what remains>\". Then stop.",
		idleFor(idle), strings.Join(ids, ", "), id)
}

// idleFor writes an idle stretch short: 45m, 21h, 3d.
func idleFor(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// Park moves an active task to the later list: an item with what remains (text, else the task's title), from the
// task, its role and the role's conversation, so zmina later take --resume continues it. The task closes as parked
// with the item's id; a worktree stays as it is.
func (s *Shift) Park(t task.Task, text string) (later.Item, error) {
	if s.Later == nil || s.Tasks == nil {
		return later.Item{}, errors.New("no later list")
	}

	if !t.State.Active() {
		return later.Item{}, fmt.Errorf("%s is %s", t.ID, t.Status())
	}

	if text = oneLine(text); text == "" {
		text = t.Headline()
	}

	now := s.Now()

	it, err := s.Later.Add(later.Item{Text: text, Note: "from " + t.ID + " (" + t.Role + ")", From: t.ID,
		Role: t.Role, Session: s.sessionOf(t.Role), By: s.By}, now)
	if err != nil {
		return it, err
	}

	was := t.Status()

	t.State, t.Later, t.On, t.Updated = task.Parked, it.ID, "", now
	if err := s.Tasks.Put(t); err != nil {
		return it, fmt.Errorf("%s is on the later list, but %s is still %s: %w", it.ID, t.ID, was, err)
	}

	s.log(t.Role, "task", t.ID, "parked "+it.ID+", note: "+text)

	return it, nil
}

// sessionOf is the role's conversation: the running one, else the last one zmina saw.
func (s *Shift) sessionOf(name string) string {
	if p, err := s.probe(name); err == nil {
		s.remember(name, p)
	}

	return s.State.Role(name).LastSession
}
