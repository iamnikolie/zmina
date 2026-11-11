package cmd

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// nowRoles is a crew whose roles are up and, per its status, idle or busy, in a fake tmux, registry and inbox socket:
// no process, no socket and no clock but the state's own.
type nowRoles struct {
	status string
	posts  []string
	// shown is whether the conversation shows what was posted.
	shown bool
}

type nowTmux struct{ shift.Tmux }

func (nowTmux) Probe(string) (tmux.Pane, error)     { return tmux.Pane{State: tmux.Up, PID: 4242}, nil }
func (nowTmux) KeepScreens(string) error            { return nil }
func (nowTmux) Attached(string) (bool, error)       { return false, nil }
func (nowTmux) Prompt(string) (string, bool, error) { return "", true, nil }

type nowReg struct{ n *nowRoles }

func (r nowReg) ByPID(pid int) (*registry.Session, error) {
	return &registry.Session{PID: pid, SessionID: "sid-1", Status: r.n.status, StatusUpdatedAt: time.Now().UnixMilli()}, nil
}
func (nowReg) LiveByName(string) *registry.Session { return nil }
func (r nowReg) Last(string) (registry.Turn, error) {
	return registry.Turn{Ended: r.n.status == "idle", At: time.Now()}, nil
}

type nowPeer struct{ n *nowRoles }

func (nowPeer) Usable(*registry.Session) bool { return true }
func (p nowPeer) Post(_ string, _ *registry.Session, text string) (string, error) {
	p.n.posts = append(p.n.posts, text)

	return fmt.Sprintf("m-%d", len(p.n.posts)), nil
}
func (p nowPeer) Delivered(string, string, string) (bool, error) { return p.n.shown, nil }

// app is the command tree over those fakes.
func (n *nowRoles) app(out, errb *bytes.Buffer) *app {
	return &app{out: out, err: errb, shiftSeams: func(s *shift.Shift) {
		s.Tmux, s.Reg, s.Peer = nowTmux{}, nowReg{n}, nowPeer{n}
	}}
}

// zm runs a command of the crew in file.
func (n *nowRoles) zm(t *testing.T, file string, args ...string) (string, error) {
	t.Helper()

	var out, errb bytes.Buffer

	root := newRoot(n.app(&out, &errb))
	root.SetArgs(append([]string{"--crew", file}, args...))
	err := root.Execute()

	return out.String(), err
}

// up writes the state of a crew whose dev and dispatcher are up on their own channel, booted long ago, the last beat
// a marked time.
func (n *nowRoles) up(t *testing.T, file string, beat time.Time) {
	t.Helper()

	var out, errb bytes.Buffer

	a := n.app(&out, &errb)
	a.crew = file

	require.NoError(t, a.withState(func(s *shift.Shift) error {
		for _, name := range []string{"dev", "dispatcher"} {
			rs := s.State.Role(name)
			rs.Up, rs.Inbound, rs.Tag, rs.Started = true, true, "abcdefghijkl", time.Now().Add(-time.Hour)
		}

		s.State.LastTick = beat

		return nil
	}))
}

func stateOf(t *testing.T, file string) *shift.State {
	t.Helper()

	st, err := shift.Read(filepath.Dir(file))
	require.NoError(t, err)

	return st
}

func TestSendWhenIdleDeliversWithinTheCommand(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	n := &nowRoles{status: "idle"}
	beat := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
	n.up(t, file, beat)

	out, err := n.zm(t, file, "send", "--when-idle", "dev", "[dispatcher] look at T-1")
	require.NoError(t, err)
	assert.Regexp(t, `^delivered to dev as S-1: said again until it acks\n$`, out)
	require.Len(t, n.posts, 1)
	assert.Contains(t, n.posts[0], "[dispatcher] look at T-1 (zmina S-1)")

	st := stateOf(t, file)
	assert.Empty(t, st.Role("dev").Inbox)
	assert.Len(t, st.Role("dev").Posted, 1)
	assert.True(t, st.Role("dev").Up, "the pass does not write Up")
	assert.Equal(t, beat, st.LastTick.UTC(), "the pass is no beat")

	// The first line has not shown in the conversation yet: the second waits for the beat, and the command says so.
	out, err = n.zm(t, file, "send", "--when-idle", "dev", "second line")
	require.NoError(t, err)
	assert.Equal(t, "queued for dev as S-2: delivered once it is up and between turns; said again until it acks\n", out)
	assert.Len(t, n.posts, 1)
	assert.Len(t, stateOf(t, file).Role("dev").Inbox, 1)
}

func TestSendWhenIdleLeavesABusyRoleToTheBeat(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	n := &nowRoles{status: "busy"}
	n.up(t, file, time.Time{})

	out, err := n.zm(t, file, "send", "--when-idle", "dev", "[dispatcher] look at T-1")
	require.NoError(t, err)
	assert.Equal(t, "queued for dev as S-1: delivered once it is up and between turns; said again until it acks\n", out)
	assert.Empty(t, n.posts)
	assert.Len(t, stateOf(t, file).Role("dev").Inbox, 1, "queued for the beat")

	// The role is a down one: nothing is started, the line waits.
	n2 := &nowRoles{status: "idle"}
	file2 := testCrew(t, "  dev:\n")
	out, err = n2.zm(t, file2, "send", "--when-idle", "dev", "hi")
	require.NoError(t, err)
	assert.Contains(t, out, "queued for dev")
	assert.Empty(t, n2.posts)
}

func TestTaskHandoffGoesOutWithTheCommandAndOnlyOnce(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	n := &nowRoles{status: "idle"}
	beat := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
	n.up(t, file, beat)

	_, err := n.zm(t, file, "task", "new", "dev", "Review the change")
	require.NoError(t, err)
	require.Len(t, n.posts, 1, "posted as the command ends")
	assert.Contains(t, n.posts[0], "[zmina T-1] Read handoff packet ")
	assert.Empty(t, stateOf(t, file).Role("dev").Inbox)

	_, err = n.zm(t, file, "task", "handoff", "1", "--queue")
	require.NoError(t, err)
	assert.Len(t, n.posts, 1, "the same packet is not posted again while the first is on its way")
	assert.Empty(t, stateOf(t, file).Role("dev").Inbox)
	assert.Equal(t, beat, stateOf(t, file).LastTick.UTC())
	assert.True(t, stateOf(t, file).Role("dev").Up)
}

func TestTaskSetRoleHandoffGoesOutWithTheCommand(t *testing.T) {
	file := testCrew(t, "  dev:\n  qa:\n")
	n := &nowRoles{status: "idle"}
	n.up(t, file, time.Time{})

	var out, errb bytes.Buffer

	a := n.app(&out, &errb)
	a.crew = file

	require.NoError(t, a.withState(func(s *shift.Shift) error {
		rs := s.State.Role("qa")
		rs.Up, rs.Inbound, rs.Tag, rs.Started = true, true, "mnopqrstuvwx", time.Now().Add(-time.Hour)

		return nil
	}))

	_, err := n.zm(t, file, "task", "new", "dev", "Review the change")
	require.NoError(t, err)
	n.posts = nil

	_, err = n.zm(t, file, "task", "set", "T-1", "--role", "qa")
	require.NoError(t, err)
	require.Len(t, n.posts, 1)
	assert.Contains(t, n.posts[0], "--role 'qa'")
}

func TestReportAndTodoAnswerReachTheDispatcherWithTheCommand(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	n := &nowRoles{status: "idle"}
	beat := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
	n.up(t, file, beat)

	out, err := n.zm(t, file, "report", "Ready", "--key", "T-1/review")
	require.NoError(t, err)
	assert.Equal(t, "report T-1/review · queued: true\n", out)
	require.Len(t, n.posts, 1)
	assert.Contains(t, n.posts[0], "report T-1/review: Ready")
	assert.Empty(t, stateOf(t, file).Signals, "told: nothing waits")

	// The same report again is a count, not news.
	_, err = n.zm(t, file, "report", "Ready", "--key", "T-1/review")
	require.NoError(t, err)
	assert.Len(t, n.posts, 1)

	n.shown = true // the report showed in its conversation

	_, err = n.zm(t, file, "todo", "add", "--rec", "keep the name", "rename the flag?")
	require.NoError(t, err)
	out, err = n.zm(t, file, "todo", "yes", "1")
	require.NoError(t, err)
	assert.Equal(t, "A-1 · yes · queued for the dispatcher\n", out)
	require.Len(t, n.posts, 2, "the answer is posted as the command ends")
	assert.True(t, strings.Contains(n.posts[1], "A-1 · yes: keep the name"), n.posts[1])
	assert.Equal(t, beat, stateOf(t, file).LastTick.UTC())
}

func TestReportToADispatcherWithADialogWaitsForTheBeat(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	n := &nowRoles{status: "waiting"}
	n.up(t, file, time.Time{})

	_, err := n.zm(t, file, "report", "Ready", "--key", "T-1/review")
	require.NoError(t, err)
	assert.Empty(t, n.posts, "never into a dialog")
	assert.Len(t, stateOf(t, file).Signals, 1, "queued for the beat")
}

func TestLaterTakeHandoffGoesOutWithTheCommand(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	n := &nowRoles{status: "idle"}
	n.up(t, file, time.Time{})

	_, err := n.zm(t, file, "later", "add", "rotate the deploy key")
	require.NoError(t, err)
	out, err := n.zm(t, file, "later", "take", "L-1", "dev")
	require.NoError(t, err)
	assert.Contains(t, out, "L-1 taken")
	require.Len(t, n.posts, 1)
	assert.Contains(t, n.posts[0], "[zmina T-1] Read handoff packet ")
}
