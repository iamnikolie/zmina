package shift

import (
	"fmt"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Shift) runtimeScope(r *crew.Role) (string, string) {
	runtime, home := r.Runtime, s.Crew.Namespace
	if runtime == "" {
		runtime = "claude"
	}
	if runtime == "codex" {
		home = r.CodexHome
		if home == "" {
			home = crew.ExpandHome("~/.codex")
		}
	}
	return runtime, scopePath(home)
}

func (s *Shift) rememberTaskConversation(name string, sess *registry.Session, r *crew.Role) {
	if s.Tasks == nil {
		return
	}
	turn, err := s.registryFor(name).Last(sess.SessionID)
	if err != nil || (turn.Context == 0 && !turn.Ended) {
		return
	}
	runtime, home := s.runtimeScope(r)
	cwd := sess.Cwd
	if cwd == "" {
		cwd = s.State.Role(name).Dir
		if cwd == "" {
			cwd = r.Dir
		}
	}
	at := sess.StatusSince().UTC()
	if at.IsZero() || at.Equal(time.UnixMilli(0)) {
		at = s.Now().UTC()
	}
	ref := task.Conversation{Node: s.Node, Crew: s.Crew.Name, CrewPath: s.Crew.Path, Role: name, Runtime: runtime, Home: home, Session: sess.SessionID, Cwd: scopePath(cwd), LastObservedWork: at, Context: turn.Context}
	for _, t := range s.Tasks.Active() {
		if t.Role != name {
			continue
		}
		found := -1
		for i, c := range t.Conversations {
			if c.Session == ref.Session && c.Runtime == ref.Runtime && c.Home == ref.Home && c.Node == ref.Node && c.CrewPath == ref.CrewPath {
				found = i
				break
			}
		}
		if found >= 0 {
			old := t.Conversations[found]
			if old == ref {
				continue
			}
			t.Conversations[found] = ref
		} else {
			t.Conversations = append(t.Conversations, ref)
		}
		if err := s.Tasks.Put(t); err != nil && s.Warn != nil {
			s.Warn(err)
		}
	}
}

// TaskStart selects only a compatible conversation from this task, not the role's last unrelated one.
func (s *Shift) TaskStart(name string, t task.Task, fresh, requireContinue bool) (UpOptions, string, error) {
	if fresh && requireContinue {
		return UpOptions{}, "", fmt.Errorf("fresh and continue are mutually exclusive")
	}
	r, err := s.Role(name)
	if err != nil {
		return UpOptions{}, "", err
	}
	runtime, home := s.runtimeScope(r)
	cwd := r.Dir
	if t.Worktree != "" {
		cwd = t.Worktree
	}
	opts := UpOptions{Dir: t.Worktree, Fresh: true}
	if fresh {
		return opts, "fresh (requested)", nil
	}
	base := func(v string) string { v, _, _ = strings.Cut(v, "#"); return v }
	var best *task.Conversation
	for i := range t.Conversations {
		c := &t.Conversations[i]
		if c.Session == "" || c.Node != s.Node || c.CrewPath != s.Crew.Path || c.Crew != s.Crew.Name || base(c.Role) != base(name) || c.Runtime != runtime || c.Home != home || scopePath(c.Cwd) != scopePath(cwd) {
			continue
		}
		if best == nil || c.LastObservedWork.After(best.LastObservedWork) {
			best = c
		}
	}
	if best != nil {
		opts.Fresh = false
		opts.Resume = best.Session
		return opts, "continue " + best.Session, nil
	}
	if requireContinue {
		return UpOptions{}, "", fmt.Errorf("%s: no compatible task conversation for %s; use --fresh to read its saved handoff", t.ID, name)
	}
	return opts, "fresh (no compatible task conversation)", nil
}

func scopePath(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(real)
	}
	return filepath.Clean(path)
}

// RestartOptions freezes the conversation before ending a copy removes its RoleState.
func (s *Shift) RestartOptions(name string, fresh, requireContinue bool) (UpOptions, error) {
	r, err := s.Role(name)
	if err != nil {
		return UpOptions{}, err
	}
	p, err := s.probe(name)
	if err != nil {
		return UpOptions{}, err
	}
	rs := s.State.Role(name)
	opts := UpOptions{Fresh: true, Dir: rs.Dir}
	policy := r.Resume
	if base, _, copy := SplitInstance(name); copy {
		baseRole, _ := s.Crew.Role(base)
		policy = baseRole.Resume // New copies start fresh; restarting one follows its base policy.
	}
	if fresh || (!requireContinue && policy != crew.ResumeAlways && s.Asleep(name).IsZero()) {
		return opts, nil
	}
	runtime, home := s.runtimeScope(r)
	// Do not overwrite an already known scope using a changed role configuration.
	if rs.LastSession != "" && rs.LastHome != "" && rs.LastHome != home {
		if requireContinue {
			return UpOptions{}, fmt.Errorf("%s: last session belongs to another runtime/home scope; start fresh", name)
		}
		return opts, nil
	}
	s.remember(name, p)
	if rs.LastSession == "" {
		if requireContinue {
			return UpOptions{}, fmt.Errorf("%s: no last session known to resume; start fresh", name)
		}
		return opts, nil
	}
	lastRuntime := rs.LastRuntime
	if lastRuntime == "" {
		lastRuntime = "claude"
	}
	if lastRuntime != runtime || (rs.LastHome != "" && rs.LastHome != home) {
		if requireContinue {
			return UpOptions{}, fmt.Errorf("%s: last session belongs to %s in another runtime/home scope; start fresh", name, lastRuntime)
		}
		return opts, nil
	}
	dir := rs.LastDir
	if dir == "" {
		dir = rs.Dir
	}
	if dir == "" {
		dir = r.Dir
	}
	info, err := os.Stat(dir)
	if err != nil {
		return UpOptions{}, fmt.Errorf("%s: continuation directory: %w", name, err)
	}
	if !info.IsDir() {
		return UpOptions{}, fmt.Errorf("%s: continuation directory is not a directory", name)
	}
	opts = UpOptions{Resume: rs.LastSession, Dir: dir, Continuation: &task.Conversation{Runtime: runtime, Home: home, Session: rs.LastSession, Cwd: scopePath(dir)}}
	return opts, nil
}
