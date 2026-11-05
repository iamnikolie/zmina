package cmd

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/shift"
)

// beat is one beat of a crew's ticker, as far as what others read of it goes.
func beat(t *testing.T, file string) {
	t.Helper()

	a := &app{crew: file, out: io.Discard, err: io.Discard}
	require.NoError(t, a.withState(func(s *shift.Shift) error {
		s.State.LastTick = s.Now()

		return nil
	}))
}

func stopped(t *testing.T, file string, at time.Time) {
	t.Helper()

	a := &app{crew: file, out: io.Discard, err: io.Discard}
	require.NoError(t, a.withState(func(s *shift.Shift) error {
		s.State.LastTick, s.State.Stopped = time.Now(), at

		return nil
	}))
}

func TestRetireSaysWhereTheLiveCrewIs(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, file, "retire", "--moved-to", "nonsense")
	require.Error(t, err)

	out, _, err := zm(t, file, "retire", "--moved-to", "box/ops")
	require.NoError(t, err)
	assert.Contains(t, out, "retired: its live crew is box/ops")

	text := readString(t, file)
	assert.Contains(t, text, "moved_to: box/ops")
	assert.Contains(t, text, "retired:")

	out, _, err = zm(t, file, "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "RETIRED")
	assert.Contains(t, out, "its live crew is box/ops (zmina ls box/ops; zmina task ls box/ops)")

	// Whoever reads its ledger or log hears it too, apart from what the command prints.
	out, errOut, err := zm(t, file, "task", "ls")
	require.NoError(t, err)
	assert.Equal(t, "no tasks\n", out)
	assert.Contains(t, errOut, "its live crew is box/ops")

	_, errOut, err = zm(t, file, "log", "-n", "1")
	require.NoError(t, err)
	assert.Contains(t, errOut, "not current")

	_, _, err = zm(t, file, "start")
	require.ErrorContains(t, err, "its live crew is box/ops")
	require.ErrorContains(t, err, "zmina retire --off")

	out, _, err = zm(t, file, "retire", "--off")
	require.NoError(t, err)
	assert.Contains(t, out, "stays stopped")
	assert.NotContains(t, readString(t, file), "moved_to")

	out, _, err = zm(t, file, "ls")
	require.NoError(t, err)
	assert.NotContains(t, out, "RETIRED")
	assert.Contains(t, out, "STOPPED", "taking the mark off starts nothing")
}

func readString(t *testing.T, file string) string {
	t.Helper()

	raw, err := os.ReadFile(file)
	require.NoError(t, err)

	return string(raw)
}

// TestTellRefusesACrewThatIsNotRunning: stopped, silent or retired, the sender hears it before anything is queued,
// and queues only on purpose; what it reads then does not say the line reached anyone.
func TestTellRefusesACrewThatIsNotRunning(t *testing.T) {
	uiHome(t)
	from, ops := crewPathOf(t, "default"), crewPathOf(t, "ops")
	_, _, err := zm(t, from, "node", "init", "--name", "laptop", "--owner")
	require.NoError(t, err)

	cases := []struct {
		name string
		set  func()
		says string
	}{
		{"never beat", func() {}, "its ticker never beat"},
		{"stopped", func() { stopped(t, ops, time.Now().Add(-49*time.Hour)) }, "stopped 2d ago"},
		{"silent", func() {
			a := &app{crew: ops, out: io.Discard, err: io.Discard}
			require.NoError(t, a.withState(func(s *shift.Shift) error {
				s.State.Stopped, s.State.LastTick = time.Time{}, time.Now().Add(-3*time.Hour)

				return nil
			}))
		}, "no ticker beat for 3h"},
	}

	for _, c := range cases {
		name := c.name
		c.set()

		_, _, err := zm(t, from, "tell", "laptop/ops", "hi")
		require.ErrorContains(t, err, c.says, name)
		require.ErrorContains(t, err, "zmina tell --queue laptop/ops", name)

		st, err := shift.Read(filepath.Dir(ops))
		require.NoError(t, err)
		assert.Empty(t, st.Signals, name+": nothing queued")
	}

	out, _, err := zm(t, from, "tell", "--queue", "laptop/ops", "on purpose")
	require.NoError(t, err)
	assert.Contains(t, out, "queued for laptop/ops's dispatcher")
	assert.Contains(t, out, "laptop/ops is not running (no ticker beat for 3h): the line waits until someone starts it")

	st, err := shift.Read(filepath.Dir(ops))
	require.NoError(t, err)
	assert.Len(t, st.Signals, 1)

	// The receipt's stage says it too.
	out, _, err = zm(t, from, "message", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "queued for its dispatcher")
	assert.Contains(t, out, "(not running: no ticker beat for 3h)")

	// Live again: no refusal, nothing added.
	beat(t, ops)
	out, _, err = zm(t, from, "tell", "laptop/ops", "hello")
	require.NoError(t, err)
	assert.NotContains(t, out, "not running")

	// The owner's mark says more than the ticker does.
	_, _, err = zm(t, ops, "retire", "--moved-to", "box/ops")
	require.NoError(t, err)
	_, _, err = zm(t, from, "tell", "laptop/ops", "hi")
	require.ErrorContains(t, err, "retired, moved to box/ops")
	require.ErrorContains(t, err, "Its live crew: box/ops")
}

// farBox is a second node of this owner in its own home, reached through a helper process, with one crew named ops.
func farBox(t *testing.T, homeA, from string) string {
	t.Helper()

	homeB := t.TempDir()
	ops := filepath.Join(homeB, ".zmina", "ops", "crew.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(ops), 0o700))
	require.NoError(t, os.WriteFile(ops, []byte("name: acme\nnamespace: "+filepath.Join(homeB, "ns")+"\ndir: "+homeB+
		"\nroles:\n  dispatcher:\n  dev:\n"), 0o600))

	r, err := mesh.Init(filepath.Join(homeB, ".zmina", "mesh"), "box")
	require.NoError(t, err)
	g, err := mesh.SignRequest(filepath.Join(homeA, ".zmina", "mesh"), r, time.Now())
	require.NoError(t, err)
	require.NoError(t, mesh.Accept(filepath.Join(homeB, ".zmina", "mesh"), g))

	script := filepath.Join(t.TempDir(), "box-zmina")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nHOME='"+homeB+"' ZMINA_TELL_RECEIPTS_PROCESS=1 exec '"+
		os.Args[0]+"' -test.run='^TestTellReceiptsHelperProcess$' -- \"$@\"\n"), 0o700))
	_, _, err = zm(t, from, "node", "peer", "add", "box", "--cmd", script)
	require.NoError(t, err)

	return ops
}

// TestTellAsksTheFarNode: the far node tells how its crews stand in its signed hello; tell asks before it queues, and
// names a crew of that name it knows of, as that and no more.
func TestTellAsksTheFarNode(t *testing.T) {
	homeA := uiHome(t)
	from, here := crewPathOf(t, "default"), crewPathOf(t, "ops")
	_, _, err := zm(t, from, "node", "init", "--name", "laptop", "--owner")
	require.NoError(t, err)

	far := farBox(t, homeA, from)
	stopped(t, far, time.Now().Add(-49*time.Hour))
	beat(t, here)

	_, _, err = zm(t, from, "tell", "box/ops", "the dump is ready")
	require.ErrorContains(t, err, "box/ops is not running: stopped 2d ago")
	require.ErrorContains(t, err, "Other crews of that name: laptop/ops (running)")
	require.ErrorContains(t, err, "one name does not prove one crew")

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	out, err := me.Outbox()
	require.NoError(t, err)
	assert.Empty(t, out, "nothing queued")

	// What came in the hello is in the book, and in node ls.
	ls, _, err := zm(t, from, "node", "ls")
	require.NoError(t, err)
	assert.Contains(t, ls, "crews: ops [stopped 2d ago]")

	said, _, err := zm(t, from, "tell", "--queue", "box/ops", "for when it starts")
	require.NoError(t, err)
	assert.Contains(t, said, "handed over to box/ops: it waits for that dispatcher")
	assert.Contains(t, said, "box/ops is not running (stopped 2d ago): the line waits until someone starts it")
	assert.NotContains(t, said, "delivered")

	st, err := shift.Read(filepath.Dir(far))
	require.NoError(t, err)
	assert.Len(t, st.Signals, 1)
}

// TestStoppedCrewNamesOthersOfItsName: no mark, no claim: a crew that stood still says so and names the crew of its
// name that runs, and says that a name proves nothing. The default crew's name is nobody's identity.
func TestStoppedCrewNamesOthersOfItsName(t *testing.T) {
	homeA := uiHome(t)
	from, here := crewPathOf(t, "default"), crewPathOf(t, "ops")
	_, _, err := zm(t, from, "node", "init", "--name", "laptop", "--owner")
	require.NoError(t, err)

	far := farBox(t, homeA, from)
	beat(t, far)
	_, _, err = zm(t, from, "node", "sync")
	require.NoError(t, err)

	stopped(t, here, time.Now().Add(-5*time.Hour))

	out, _, err := zm(t, here, "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "shift was stopped")
	assert.Contains(t, out, "Other crews of this name: box/ops (running); one name does not prove one crew.")

	_, errOut, err := zm(t, here, "task", "ls")
	require.NoError(t, err)
	assert.Contains(t, errOut, "look before you report it as standing still")

	stopped(t, from, time.Now().Add(-5*time.Hour))
	out, _, err = zm(t, from, "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "shift was stopped")
	assert.NotContains(t, out, "Other crews")
}

func TestRemoteTaskCommands(t *testing.T) {
	file, _ := remoteCrew(t)

	out, _, err := zm(t, file, "task", "ls", "far/ops", "--all", "--role", "dev")
	require.NoError(t, err)
	assert.Equal(t, "far: --config ops task ls --all=true --role=dev\n", out)

	out, _, err = zm(t, file, "task", "show", "far/ops", "T-5", "--full")
	require.NoError(t, err)
	assert.Equal(t, "far: --config ops task show T-5 --full=true\n", out)

	_, _, err = zm(t, file, "task", "ls", "far/default")
	require.ErrorContains(t, err, "update zmina on far", "an old far zmina's default crew is never guessed")

	_, _, err = zm(t, file, "task", "show", "far/ops")
	require.ErrorContains(t, err, "takes a task after the address")

	_, _, err = zm(t, file, "task", "show", "T-5", "T-6")
	require.ErrorContains(t, err, "one task")

	_, _, err = zm(t, file, "task", "ls", "dev")
	require.ErrorContains(t, err, "unknown argument")

	_, _, err = zm(t, file, "task", "ls", "nowhere/ops")
	require.ErrorContains(t, err, `no node "nowhere"`)
}

// The node's own name is that crew here, whatever crew the command was run for.
func TestTaskAddressOfThisNode(t *testing.T) {
	uiHome(t)
	def, ops := crewPathOf(t, "default"), crewPathOf(t, "ops")
	_, _, err := zm(t, def, "node", "init", "--name", "here", "--owner")
	require.NoError(t, err)
	_, _, err = zm(t, ops, "task", "new", "dev", "--title", "the ops task", "do it")
	require.NoError(t, err)

	out, _, err := zm(t, def, "task", "ls", "here/ops")
	require.NoError(t, err)
	assert.Contains(t, out, "the ops task")

	out, _, err = zm(t, def, "task", "show", "here/ops", "T-1")
	require.NoError(t, err)
	assert.Contains(t, out, "title: the ops task")

	out, _, err = zm(t, def, "task", "ls")
	require.NoError(t, err)
	assert.NotContains(t, out, "the ops task")
}

func TestNoCrewFileNamesTheCrews(t *testing.T) {
	uiHome(t)
	require.NoError(t, os.Remove(crewPathOf(t, "default")))

	root := NewRoot(io.Discard, io.Discard)
	root.SetArgs([]string{"task", "ls"})
	err := root.Execute()
	require.ErrorContains(t, err, "does not exist; the crews here: ops (--config ops)")
}
