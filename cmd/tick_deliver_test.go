package cmd

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// passCalls is what a run of zmina tick --deliver asked of tmux and the session registry.
type passCalls struct{ list []string }

type countedTmux struct {
	nowTmux
	c *passCalls
}

func (t countedTmux) Probe(n string) (tmux.Pane, error) {
	t.c.list = append(t.c.list, "tmux probe")

	return t.nowTmux.Probe(n)
}

func (t countedTmux) KeepScreens(n string) error {
	t.c.list = append(t.c.list, "tmux keep-screens")

	return t.nowTmux.KeepScreens(n)
}

func (t countedTmux) Attached(n string) (bool, error) {
	t.c.list = append(t.c.list, "tmux attached")

	return t.nowTmux.Attached(n)
}

func (t countedTmux) Prompt(n string) (string, bool, error) {
	t.c.list = append(t.c.list, "tmux prompt")

	return t.nowTmux.Prompt(n)
}

type countedReg struct {
	nowReg
	c *passCalls
}

func (r countedReg) ByPID(pid int) (*registry.Session, error) {
	r.c.list = append(r.c.list, "registry session")

	return r.nowReg.ByPID(pid)
}

func (r countedReg) Last(id string) (registry.Turn, error) {
	r.c.list = append(r.c.list, "registry transcript")

	return r.nowReg.Last(id)
}

// pass runs zmina tick --deliver over the fakes, counting what it asks of tmux and the registry.
func (n *nowRoles) pass(t *testing.T, file string) (out, errs string, calls *passCalls, err error) {
	t.Helper()

	var o, e bytes.Buffer

	calls = &passCalls{}
	a := &app{out: &o, err: &e, shiftSeams: func(s *shift.Shift) {
		s.Tmux, s.Reg, s.Peer = countedTmux{c: calls}, countedReg{nowReg{n}, calls}, nowPeer{n}
	}}

	root := newRoot(a)
	root.SetArgs([]string{"--crew", file, "tick", "--deliver"})
	err = root.Execute()

	return o.String(), e.String(), calls, err
}

// kept is every file under the crew's directory with its size and time, but the lock file, which every taker of the
// lock writes its pid into.
func kept(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := map[string]string{}

	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == "lock" {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(dir, path)
		out[rel] = fmt.Sprintf("%s %d", info.ModTime(), info.Size())

		return nil
	}))

	return out
}

// queueFor leaves one line waiting for a busy dev; the role is idle again when it returns.
func (n *nowRoles) queueFor(t *testing.T, file, line string) {
	t.Helper()

	n.status = "busy"
	out, err := n.zm(t, file, "send", "--when-idle", "dev", line)
	require.NoError(t, err)
	require.Contains(t, out, "queued for dev")
	require.Empty(t, n.posts)

	n.status = "idle"
}

// A pass gives what waits to a role that can take it, saves, and is no beat.
func TestTickDeliverGivesAWaitingLine(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	n := &nowRoles{status: "idle"}
	beat := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
	n.up(t, file, beat)
	n.queueFor(t, file, "[dispatcher] look at T-1")

	out, errs, _, err := n.pass(t, file)
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.Empty(t, errs)
	require.Len(t, n.posts, 1)
	assert.Contains(t, n.posts[0], "[dispatcher] look at T-1")

	st := stateOf(t, file)
	assert.Empty(t, st.Role("dev").Inbox)
	assert.Len(t, st.Role("dev").Posted, 1)
	assert.True(t, st.Role("dev").Up)
	assert.Equal(t, beat, st.LastTick.UTC(), "the pass is no beat")

	// Again: what went is not given twice.
	_, _, _, err = n.pass(t, file)
	require.NoError(t, err)
	assert.Len(t, n.posts, 1)
}

// Nothing waiting: not a tmux or registry call, not a byte written, no event, nothing said.
func TestTickDeliverWithNothingWaitingTouchesNothing(t *testing.T) {
	file := testCrew(t, "  dev:\n  qa:\n")
	n := &nowRoles{status: "idle"}
	n.up(t, file, time.Now().Add(-30*time.Second).UTC().Truncate(time.Second))

	// A line for a role that is down waits for the beat, which starts roles; the pass never does.
	_, err := n.zm(t, file, "send", "--when-idle", "qa", "for the beat")
	require.NoError(t, err)

	dir := filepath.Dir(file)
	before := kept(t, dir)
	require.Contains(t, before, "state.json")
	require.Contains(t, before, "events.jsonl", "the event log is among what is compared")

	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	require.NoError(t, err)

	out, errs, calls, err := n.pass(t, file)
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.Empty(t, errs)
	assert.Empty(t, calls.list, "no tmux or registry call")
	assert.Empty(t, n.posts)
	assert.Equal(t, before, kept(t, dir), "no file written: not the state, not the event log")

	again, err := os.ReadFile(filepath.Join(dir, "state.json"))
	require.NoError(t, err)
	assert.Equal(t, raw, again)
}

// A line that cannot go yet (the role is busy) is asked about, and the state, unchanged, is not saved.
func TestTickDeliverThatGivesNothingSavesNothing(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	n := &nowRoles{status: "busy"}
	n.up(t, file, time.Time{})

	_, err := n.zm(t, file, "send", "--when-idle", "dev", "[dispatcher] hi")
	require.NoError(t, err)

	dir := filepath.Dir(file)
	before := kept(t, dir)

	_, _, calls, err := n.pass(t, file)
	require.NoError(t, err)
	assert.NotEmpty(t, calls.list, "it looked at the busy role")
	assert.Empty(t, n.posts)
	assert.Equal(t, before, kept(t, dir), "nothing changed, nothing saved")
	assert.Len(t, stateOf(t, file).Role("dev").Inbox, 1)
}

// The lock is held (the beat, a command): the pass ends at once with nothing done, nothing read, nothing written.
func TestTickDeliverWhileTheLockIsHeldDoesNothing(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	n := &nowRoles{status: "idle"}
	n.up(t, file, time.Time{})
	n.queueFor(t, file, "[dispatcher] hi")

	dir := filepath.Dir(file)
	before := kept(t, dir)

	// A pass that waited would give up with an error after this, and the test would say so.
	old := shift.LockWait
	shift.LockWait = 300 * time.Millisecond

	t.Cleanup(func() { shift.LockWait = old })

	holder, err := shift.LockAs(dir, shift.LockOpts{Verb: "tick"})
	require.NoError(t, err)

	out, errs, calls, err := n.pass(t, file)
	require.NoError(t, err, "a held lock is exit 0")
	assert.Empty(t, out)
	assert.Empty(t, errs)
	assert.Empty(t, calls.list)
	assert.Empty(t, n.posts)

	holder.Release()
	assert.Equal(t, before, kept(t, dir), "the state is as it was")
	assert.Len(t, stateOf(t, file).Role("dev").Inbox, 1)

	// Free again: the same pass gives the line.
	_, _, _, err = n.pass(t, file)
	require.NoError(t, err)
	assert.Len(t, n.posts, 1)
}

// A state that does not read is an error with exit 1, as for every command, and is left as it was.
func TestTickDeliverFailsOnAStateThatDoesNotRead(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	n := &nowRoles{status: "idle"}
	n.up(t, file, time.Time{})

	path := filepath.Join(filepath.Dir(file), "state.json")
	require.NoError(t, os.WriteFile(path, []byte("{broken"), 0o600))

	_, _, _, err := n.pass(t, file)
	require.Error(t, err)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "{broken", string(raw))
}

// The flag is for the delivery process, not for a person: it is not in the help.
func TestTickDeliverIsHiddenFromHelp(t *testing.T) {
	var out bytes.Buffer

	root := NewRoot(&out, &out)
	root.SetArgs([]string{"tick", "--help"})
	require.NoError(t, root.Execute())
	assert.NotContains(t, out.String(), "--deliver")

	tick, _, err := NewRoot(&out, &out).Find([]string{"tick"})
	require.NoError(t, err)

	flag := tick.Flags().Lookup("deliver")
	require.NotNil(t, flag)
	assert.True(t, flag.Hidden)
}
