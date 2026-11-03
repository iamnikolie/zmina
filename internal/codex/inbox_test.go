package codex

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/registry"
)

func TestInboxPostTakeDelivered(t *testing.T) {
	dir := InboxDir(t.TempDir(), "zm-dev")
	require.NoError(t, openInbox(dir))
	s := &registry.Session{Inbox: dir}

	assert.True(t, Inbox{}.Usable(s))
	assert.False(t, Inbox{}.Usable(&registry.Session{}))

	a, err := Inbox{}.Post("", s, "first")
	require.NoError(t, err)
	b, err := Inbox{}.Post("", s, "second")
	require.NoError(t, err)

	l, path, err := nextLine(dir)
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.Equal(t, "first", l.Text, "oldest first")

	ok, err := Inbox{}.Delivered(dir, "", a)
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, take(dir, path, l.ID))
	ok, err = Inbox{}.Delivered(dir, "", a)
	require.NoError(t, err)
	assert.True(t, ok)

	l, _, err = nextLine(dir)
	require.NoError(t, err)
	assert.Equal(t, b, l.ID)

	_, err = Inbox{}.Post("", &registry.Session{}, "x")
	require.Error(t, err)
}

func TestInboxSetsABrokenFileAside(t *testing.T) {
	dir := InboxDir(t.TempDir(), "zm-dev")
	require.NoError(t, openInbox(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "1-x.line"), []byte("not json"), 0o600))

	l, _, err := nextLine(dir)
	require.NoError(t, err)
	assert.Nil(t, l)
	assert.FileExists(t, filepath.Join(dir, "1-x.line.broken"))
}

func TestDrainInboxWaitsForIdle(t *testing.T) {
	r, wire, out := testRunner(t)
	r.rec.Inbox = InboxDir(t.TempDir(), "zm-dev")
	require.NoError(t, openInbox(r.rec.Inbox))
	r.rec.SessionID = "thread"

	id, err := Inbox{}.Post("", &registry.Session{Inbox: r.rec.Inbox}, "[zmina] T-5 result ready")
	require.NoError(t, err)

	r.rec.Status = "busy"
	require.NoError(t, r.drainInbox())
	assert.Empty(t, wire.String(), "busy: the line waits")

	r.rec.Status = "idle"
	require.NoError(t, r.drainInbox())
	assert.Contains(t, wire.String(), `"method":"turn/start"`)
	assert.Contains(t, wire.String(), `[zmina] T-5 result ready`)
	assert.Contains(t, out.String(), "› [zmina] T-5 result ready", "the console shows what zmina gave the thread")

	ok, err := Inbox{}.Delivered(r.rec.Inbox, "", id)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestRunSubprocessTakesItsInbox(t *testing.T) {
	t.Setenv("ZMINA_CODEX_TEST_CHILD", "1")
	bin, err := os.Executable()
	require.NoError(t, err)

	input, send := io.Pipe()
	defer func() { _ = input.Close() }()
	defer func() { _ = send.Close() }()

	reg := Registry{Dir: filepath.Join(t.TempDir(), "registry")}
	finished := make(chan error, 1)

	go func() {
		finished <- Run(Options{Bin: bin, Name: "zm-dev", Dir: t.TempDir(), Boot: "boot", Registry: reg}, input, io.Discard, io.Discard)
	}()

	var sess *registry.Session

	require.Eventually(t, func() bool {
		sess, _ = reg.ByPID(os.Getpid())
		return sess != nil && sess.Status == "idle" && sess.Inbox != ""
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, InboxDir(reg.Dir, "zm-dev"), sess.Inbox, "the bridge says where its inbox is")

	id, err := Inbox{}.Post("", sess, "a line from zmina")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		ok, _ := Inbox{}.Delivered(sess.Inbox, "", id)
		return ok
	}, 5*time.Second, 20*time.Millisecond)

	_, err = io.WriteString(send, "/exit\n")
	require.NoError(t, err)

	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not exit")
	}
}
