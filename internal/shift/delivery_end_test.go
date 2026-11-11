package shift

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
)

// restarted makes the session of a role that was started again a new conversation, booted and between turns.
func restarted(f fix, role, session string) {
	*f.now = f.now.Add(3 * time.Minute)
	f.rg.byPID[f.tm.up[role]] = &registry.Session{Status: "idle", StatusUpdatedAt: f.now.UnixMilli(), SessionID: session}
}

// A line posted to a role that is then put down does not stay posted: the beat that finds the role down gives it
// back to the head of its queue, once, with its id, for the role's next session; started again, the role gets it
// once, and nothing is posted or kept as failed meanwhile.
func TestPostedLineOfARoleThatWentDownWaitsInItsQueue(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	line, id := postedTo(t, f, "designer", "[dev] look at T-1")

	require.NoError(t, f.s.Down("designer", DownOptions{}))
	require.Len(t, f.s.State.Role("designer").Posted, 1, "nothing has looked at it yet")

	for range 3 {
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
	}

	rs := f.s.State.Role("designer")
	assert.Empty(t, rs.Posted, "no longer posted")
	assert.Empty(t, rs.Failed)
	assert.Equal(t, []string{line}, rs.Inbox, "once, at the head of its queue")
	assert.Equal(t, []string{id}, rs.QueuedIDs[line], "with its id")
	assert.NotNil(t, f.s.State.Acks[id], "still waiting for its ack")
	assert.Equal(t, 1, f.eventCount("requeued"))
	assert.Len(t, p.raw, 1, "nothing was posted again to a role that is down")

	for _, q := range f.s.Queues() {
		assert.Empty(t, q.Posted, "queue ls no longer shows it posted")
	}

	// Started again, it gets the line once.
	require.NoError(t, f.s.Up("designer"))
	restarted(f, "designer", "sid-designer-2")

	for range 3 {
		f.s.deliverInbox("designer", *f.now)
		p.showAll()
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
	}

	assert.Equal(t, []string{line}, p.posted["sid-designer-2"], "exactly once, to the new session")
	assert.Empty(t, f.s.State.Role("designer").Inbox)
	assert.Empty(t, f.s.State.Role("designer").Failed)
}

// A role started again before any beat saw it down settles the old session's posted line as well: the line is
// given back once, never lost and never twice.
func TestRoleStartedFreshGetsItsStillDueLineOnce(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	line, _ := postedTo(t, f, "designer", "[dev] look at T-1")

	f.tm.die("designer")
	require.NoError(t, f.s.Up("designer"))
	rs := f.s.State.Role("designer")
	assert.Empty(t, rs.Posted)
	assert.Equal(t, []string{line}, rs.Inbox)
	assert.Equal(t, 1, f.eventCount("requeued"))

	restarted(f, "designer", "sid-designer-2")

	for range 4 {
		f.s.Tick()
		f.s.deliverInbox("designer", *f.now)
		p.showAll()
		*f.now = f.now.Add(time.Minute)
	}

	assert.Equal(t, []string{line}, p.posted["sid-designer-2"], "exactly once")
	assert.Equal(t, []string{line}, p.posted["sid-designer"], "and once to the old session, which never showed it")
	assert.Empty(t, f.s.State.Role("designer").Posted)
	assert.Empty(t, f.s.State.Role("designer").Inbox)
}

// A posted line the old conversation did show is done: a role that ended afterwards is not sent it again.
func TestPostedLineTheEndedConversationShowedIsNotGivenBack(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	postedTo(t, f, "designer", "[dev] look at T-1")
	p.showAll()

	require.NoError(t, f.s.Down("designer", DownOptions{}))
	f.s.Tick()

	rs := f.s.State.Role("designer")
	assert.Empty(t, rs.Posted)
	assert.Empty(t, rs.Inbox, "it was read")
	assert.True(t, f.ev.has("designer", "seen"))
	assert.Zero(t, f.eventCount("requeued"))
}

// The release line posted to a role that goes down before its conversation shows it means nothing to the next
// session, which is not released: it is dropped with an event, never given back. The role's other lines are.
func TestReleaseLinePostedToARoleThatWentDownIsDropped(t *testing.T) {
	f := fixtureWith(t, "release_idle_minutes: 30\n"+crewYAML)
	p := newFakePeer()
	f.s.Peer = p
	f.s.Tick() // the dispatcher and harvester start
	require.NoError(t, f.s.Up("designer"))
	*f.now = f.now.Add(5 * time.Minute)
	f.status("designer", "idle", f.now.Add(-31*time.Minute))
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open, Title: "draw the logo"}}

	f.s.Tick()
	rs := f.s.State.Role("designer")
	require.Len(t, rs.Posted, 1, "the release line was posted: %v", p.raw)
	require.Contains(t, rs.Posted[0].Line, "[zmina release] Read checkpoint request")

	// Another line posted after it, which the next session still has to read.
	other := "[dev] look at T-2"
	rs.Posted = append(rs.Posted, Posted{Line: other, MsgID: "m-other", Session: "sid-designer", At: *f.now})

	*f.now = f.now.Add(releaseWait + time.Minute)
	f.s.Tick()
	require.Equal(t, []string{"designer"}, f.s.Released)
	require.NoError(t, f.s.FinishDown("designer"))
	f.s.Released = nil

	*f.now = f.now.Add(time.Minute)
	f.s.Tick()

	assert.Empty(t, rs.Posted)
	assert.Equal(t, 1, f.eventCount("dropped"), "the release line")
	assert.Equal(t, 1, f.eventCount("requeued"), "the other line")
	assert.NotContains(t, strings.Join(rs.Inbox, "|"), "[zmina release]")
	assert.Contains(t, rs.Inbox, other)
}

// The lines zmina writes to one conversation are the ones sessionBound names, and the lines for a role's work are not:
// the producers' own texts are tied to the list here, so a reworded line cannot slip out of it.
func TestSessionBoundLinesAreThoseZminaWrites(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)

	release, err := f.s.releaseCheckpointRequest("designer", nil)
	require.NoError(t, err)

	bound := []string{
		release,
		releaseLine(time.Hour, []string{"T-1"}),
		fmt.Sprintf(restartLine, "/tmp/request.json"),
		fmt.Sprintf(fmt.Sprintf(earlyLine, "300k", "400k"), "/tmp/request.json"),
		recoveryLine("/tmp/pack.md"),
		"/compact",
		"/rename dev",
		"the boot a clear queued",
	}

	f.s.State.Role("dispatcher").QueuedBoot = "the boot a clear queued"

	for _, line := range bound {
		assert.True(t, f.s.sessionBound("dispatcher", line), line)
	}

	for _, line := range []string{
		"[dev] look at T-1 (zmina S-3)",
		AlarmPrefix + "R-1: stand up",
		"[zmina T-4] Read handoff packet /tmp/p.json",
		"[zmina] restarted fresh: its context is full. Your note: T-4 abc, in each task's packet that follows.",
		"[zmina] S-5 dev: T-5 result ready (zmina S-5)",
		"[zmina] dev: released after 31m idle",
		"restart the deploy",
		"[owner] released the build, idle since noon",
		"read d and boot", // the words of the boot, no boot queued: a line like any other
	} {
		assert.False(t, f.s.sessionBound("dispatcher", line), line)
	}
}

// A line typed into a session that ended is dropped with an event, not given back: it may have been taken. Its tracked
// ack stays, and is said again.
func TestTypedLineOfAnEndedSessionIsDropped(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.s.State.Role("designer").Inbound = true // told of acks, typed all the same
	f.status("designer", "idle", *f.now)
	f.tm.onType = nil // the status never changes: the line never shows as landed
	*f.now = f.now.Add(3 * time.Minute)

	id, err := f.s.QueueTracked("designer", "[dev] look at T-1")
	require.NoError(t, err)
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	rs := f.s.State.Role("designer")
	require.NotNil(t, rs.Sent)

	f.tm.die("designer")
	f.s.Tick()
	assert.NotNil(t, rs.Sent, "one beat that does not see the role is not its end")

	f.s.Tick()

	assert.Nil(t, rs.Sent)
	assert.Empty(t, rs.Inbox, "not typed again")
	assert.Empty(t, rs.Failed)
	assert.True(t, f.ev.has("designer", "dropped"))
	require.NotNil(t, f.s.State.Acks[id], "the ack stays: listed in the next brief, said again")
	assert.False(t, f.s.State.Acks[id].First.IsZero())
}

// A Codex bridge's inbox holds a posted line as a file until the bridge takes it, and outlives the bridge: a bridge that
// died before it took the file leaves the line waiting there for the next one. The record stays posted then, and the
// line is not queued again: queued, it would be a second file, run twice.
func TestCodexLineWaitingInItsFileIsNotQueuedAgain(t *testing.T) {
	f, dir := codexFixture(t)
	bridge(t, f, dir, "thread-1")

	id, err := f.s.QueueTracked("dev", "[dispatcher] look at T-1")
	require.NoError(t, err)
	require.NotEmpty(t, f.s.deliverInbox("dev", *f.now))

	rs := f.s.State.Role("dev")
	require.Len(t, rs.Posted, 1)
	require.Len(t, waitingFiles(t, dir), 1)

	// The bridge dies before it takes the file; two beats see the role gone.
	delete(f.tm.up, "dev")

	for range 3 {
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
	}

	assert.Len(t, rs.Posted, 1, "still posted: its file waits for the next bridge")
	assert.Empty(t, rs.Inbox, "not queued a second time")
	assert.Zero(t, f.eventCount("requeued"))
	assert.NotNil(t, f.s.State.Acks[id])

	// Started again, the new bridge takes the file; the record closes as seen.
	require.NoError(t, f.s.Up("dev"))
	bridge(t, f, dir, "thread-2")
	assert.Empty(t, rs.Inbox)

	files := waitingFiles(t, dir)
	require.Len(t, files, 1, "one file, not two")
	take(t, dir, files[0])

	*f.now = f.now.Add(3 * time.Minute)
	f.s.deliverInbox("dev", *f.now)

	assert.Empty(t, rs.Posted)
	assert.Empty(t, rs.Inbox)
	assert.Empty(t, waitingFiles(t, dir), "the line ran once")
	assert.True(t, f.ev.has("dev", "seen"))
}

// A Codex line whose file is gone and was never taken cannot be run by any bridge: it is given back, once.
func TestCodexLineWhoseFileIsGoneIsQueuedOnce(t *testing.T) {
	f, dir := codexFixture(t)
	bridge(t, f, dir, "thread-1")

	_, err := f.s.QueueTracked("dev", "[dispatcher] look at T-1")
	require.NoError(t, err)
	require.NotEmpty(t, f.s.deliverInbox("dev", *f.now))

	for _, file := range waitingFiles(t, dir) {
		require.NoError(t, os.Remove(file))
	}

	delete(f.tm.up, "dev")

	for range 3 {
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
	}

	rs := f.s.State.Role("dev")
	assert.Empty(t, rs.Posted)
	assert.Len(t, rs.Inbox, 1)
	assert.Equal(t, 1, f.eventCount("requeued"))
}

// codexFixture is a crew with a Codex role dev, its bridge's inbox a real directory and its bridge taking real files.
func codexFixture(t *testing.T) (fix, string) {
	t.Helper()

	f, _ := nativeFixture(t, crewYAML+"  dev:\n    runtime: codex\n")
	f.s.CodexInbox = codex.Inbox{}

	r, err := f.s.Role("dev")
	require.NoError(t, err)

	dir := codex.InboxDir(filepath.Join(f.s.Crew.StateDir(), "codex"), r.Session)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "taken"), 0o700))
	require.NoError(t, f.s.Up("dev"))

	return f, dir
}

// bridge is the record the bridge of dev's current process publishes, idle, behind this inbox.
func bridge(t *testing.T, f fix, dir, thread string) {
	t.Helper()

	*f.now = f.now.Add(3 * time.Minute)

	reg := &codex.Registry{Dir: filepath.Join(f.s.Crew.StateDir(), "codex")}
	require.NoError(t, reg.Save(codex.Record{Session: registry.Session{PID: f.tm.up["dev"], SessionID: thread,
		Status: "idle", StatusUpdatedAt: f.now.UnixMilli(), Inbox: dir}}))
}

// waitingFiles are the lines in the inbox no bridge has taken.
func waitingFiles(t *testing.T, dir string) []string {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(dir, "*.line"))
	require.NoError(t, err)

	return files
}

// take does what the bridge does with a line it starts a turn with: the taken mark, then the file goes.
func take(t *testing.T, dir, file string) {
	t.Helper()

	id := strings.TrimSuffix(strings.SplitN(filepath.Base(file), "-", 2)[1], ".line")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "taken", id), nil, 0o600))
	require.NoError(t, os.Remove(file))
}

// One beat that does not see a role (tmux asked and answered "no server") is not the end of its session: what is posted
// to it stays posted, what was typed stays typed, and when the next beat sees it again nothing was touched. Only a
// second beat in a row, with no session of that name alive in the registry, settles them.
func TestOneAbsentBeatSettlesNothing(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	_, _ = postedTo(t, f, "designer", "[dev] look at T-1")

	rs := f.s.State.Role("designer")
	rs.Sent = &Sent{Line: "typed text with no id", At: *f.now}
	pid := f.tm.up["designer"]

	delete(f.tm.up, "designer")
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()

	f.tm.up["designer"] = pid
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()

	assert.Len(t, rs.Posted, 1, "still posted to the live session")
	assert.Empty(t, rs.Inbox, "not queued into the session it was posted to")
	require.NotNil(t, rs.Sent, "the typed line is still tracked")
	assert.Zero(t, f.eventCount("requeued"))
	assert.Zero(t, f.eventCount("dropped"))
	assert.Len(t, p.posted["sid-designer"], 1)

	// The session is seen again for good: the posted line shows, as it always did.
	rs.Sent = nil
	p.showAll()
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	f.s.deliverInbox("designer", *f.now)
	assert.Empty(t, rs.Posted)
}

// A session of that name alive in the registry (the probe is what failed) is not gone, however many beats miss it.
func TestAbsentRoleWithALiveSessionIsNotSettled(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	_, _ = postedTo(t, f, "designer", "[dev] look at T-1")

	r, err := f.s.Role("designer")
	require.NoError(t, err)

	delete(f.tm.up, "designer")
	f.rg.outside[r.Session] = &registry.Session{Name: r.Session, SessionID: "sid-designer"}

	for range 4 {
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
	}

	rs := f.s.State.Role("designer")
	assert.Len(t, rs.Posted, 1)
	assert.Empty(t, rs.Inbox)

	delete(f.rg.outside, r.Session)
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()

	assert.Empty(t, rs.Posted, "the registry has no session of that name now: it is gone")
	assert.Len(t, rs.Inbox, 1)
}

// A line sent to a role whose boot starts the same way is an ordinary line: only the boot a clear queued is the boot.
func TestLineThatOpensLikeTheBootSurvivesASessionEnd(t *testing.T) {
	f, _ := nativeFixture(t, "namespace: /ns\nclaude: claude\nroles:\n  dispatcher:\n    boot: read d and boot\n"+
		"  designer:\n    boot: read AGENTS.md\n")
	upNative(t, f, "designer")

	line, _ := postedTo(t, f, "designer", "read AGENTS.md section 3 again, it changed")

	require.NoError(t, f.s.Down("designer", DownOptions{}))
	f.s.Tick()

	rs := f.s.State.Role("designer")
	assert.Equal(t, []string{line}, rs.Inbox, "given back for its next session")
	assert.Zero(t, f.eventCount("dropped"))

	// The boot a clear queued is that session's, and it alone: known by equality.
	rs.QueuedBoot = "read AGENTS.md\n\n[zmina] the rules"
	assert.True(t, f.s.sessionBound("designer", rs.QueuedBoot))
	assert.False(t, f.s.sessionBound("designer", "read AGENTS.md section 3 again, it changed"))
}

// The dispatcher's deliver clears QueuedBoot as soon as the boot has left the queue, so a boot posted and not yet shown
// is known at the session's end by the record, which says it is the boot: dropped, not given to the next session, which
// starts with its own boot.
func TestPostedBootIsDroppedAtSessionEnd(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	rs := f.s.State.Role("dispatcher")
	boot := "read d and boot\n\n[zmina] the rules"
	rs.QueuedBoot, rs.Inbox = boot, []string{boot}

	require.NotEmpty(t, f.s.deliverInbox("dispatcher", *f.now))
	require.Len(t, rs.Posted, 1)

	f.s.deliver(*f.now) // clears QueuedBoot: the boot is not in the queue any more
	require.Empty(t, rs.QueuedBoot)

	f.s.State.Stopped = *f.now // nothing starts a new dispatcher
	delete(f.tm.up, "dispatcher")

	for range 3 {
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
	}

	assert.Empty(t, rs.Posted)
	assert.Empty(t, rs.Inbox, "the next session has its own boot")
	assert.Zero(t, f.eventCount("requeued"))
	assert.Equal(t, 1, f.eventCount("dropped"))
}

// A release line posted to a Codex bridge that dies before it takes the file is session-bound: it would be run by the
// next bridge in the new session, which is not released. It is withdrawn from the inbox and dropped with an event; a
// line meant for the role stays in its file for the next bridge.
func TestCodexSessionBoundLineIsWithdrawnWithItsSession(t *testing.T) {
	f, dir := codexFixture(t)
	bridge(t, f, dir, "thread-1")

	sess := f.s.session("dev")
	require.NotNil(t, sess)
	require.NoError(t, f.s.post("dev", sess, releaseLine(time.Hour, []string{"T-1"}), *f.now))
	require.NoError(t, f.s.post("dev", sess, "[dispatcher] look at T-1", *f.now))

	rs := f.s.State.Role("dev")
	require.Len(t, rs.Posted, 2)
	require.Len(t, waitingFiles(t, dir), 2)

	delete(f.tm.up, "dev") // the bridge dies before it takes either file

	for range 3 {
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
	}

	require.Len(t, rs.Posted, 1, "the line for the role is still posted")
	assert.Contains(t, rs.Posted[0].Line, "look at T-1")
	assert.Equal(t, 1, f.eventCount("dropped"))
	assert.Empty(t, rs.Inbox)

	files := waitingFiles(t, dir)
	require.Len(t, files, 1, "the release line's file is gone")

	raw, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Contains(t, string(raw), "look at T-1")
}

// racingInbox is a Codex inbox whose bridge takes the line right after zmina's first look at it: that look finds the
// line untaken, and by the time zmina asks whether its file still waits it is gone, taken.
type racingInbox struct {
	codex.Inbox
	armed bool
}

func (r *racingInbox) Delivered(dir, session, id string) (bool, error) {
	seen, err := r.Inbox.Delivered(dir, session, id)

	if r.armed && !seen {
		r.armed = false

		files, _ := filepath.Glob(filepath.Join(dir, "*-"+id+".line"))
		for _, file := range files {
			_ = os.WriteFile(filepath.Join(dir, "taken", id), nil, 0o600)
			_ = os.Remove(file)
		}
	}

	return seen, err
}

// A live new bridge may take the file between zmina's look at whether the line was delivered and its look at whether
// the file waits: the line is then delivered, and giving it back would run it twice. The look is taken once more
// before a line goes back.
func TestCodexLineTakenBetweenTheLooksIsNotGivenBack(t *testing.T) {
	f, dir := codexFixture(t)
	race := &racingInbox{}
	f.s.CodexInbox = race
	bridge(t, f, dir, "thread-1")

	_, err := f.s.QueueTracked("dev", "[dispatcher] look at T-1")
	require.NoError(t, err)
	require.NotEmpty(t, f.s.deliverInbox("dev", *f.now))

	// The bridge dies and a new one starts on the same session name, with a new thread.
	delete(f.tm.up, "dev")
	require.NoError(t, f.s.Up("dev"))
	bridge(t, f, dir, "thread-2")

	rs := f.s.State.Role("dev")
	require.Len(t, rs.Posted, 1, "its file waits for the new bridge")

	race.armed = true
	f.s.deliverInbox("dev", *f.now)

	assert.Empty(t, rs.Inbox, "delivered by the new bridge: not given back")
	assert.Empty(t, rs.Posted)
	assert.Zero(t, f.eventCount("requeued"))
	assert.True(t, f.ev.has("dev", "seen"))
}

// A line giveBack leaves out because its reader acked it is not logged as requeued: the log says what happened.
func TestRequeueLogsWhatActuallyWentBack(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	rs := f.s.State.Role("designer")
	rs.Posted = []Posted{{Line: "[dev] look at T-1 (zmina S-9)", MsgID: "m-9", Session: "sid-designer", At: *f.now, IDs: []string{"S-9"}}}
	// S-9 waits for no ack: its reader acked it (or the ack is long gone), and giveBack leaves the line out.

	require.NoError(t, f.s.Down("designer", DownOptions{}))
	f.s.Tick()

	assert.Empty(t, rs.Posted)
	assert.Empty(t, rs.Inbox, "the reader has it")
	assert.True(t, f.ev.has("designer", "acked-queued"))
	assert.Zero(t, f.eventCount("requeued"), "nothing went back")
}

// The two flags a record carries (Sent.Acked, Posted.Boot) are optional in state.json: a state written without them
// reads as before, one written with them is read by a zmina that does not know them (unknown fields are ignored), and
// they are not written when false.
func TestRecordFlagsAreOptionalInState(t *testing.T) {
	old := `{"version":2,"roles":{"dev":{"sent":{"line":"a","at":"2026-01-02T10:00:00Z"},` +
		`"posted":[{"line":"b","msg_id":"m-1","session":"s-1","at":"2026-01-02T10:00:00Z"}]}}}`

	st, _, err := decode([]byte(old))
	require.NoError(t, err)
	assert.False(t, st.Roles["dev"].Sent.Acked)
	assert.False(t, st.Roles["dev"].Posted[0].Boot)

	raw, err := json.Marshal(st)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"acked"`)
	assert.NotContains(t, string(raw), `"boot"`)

	st.Roles["dev"].Sent.Acked, st.Roles["dev"].Posted[0].Boot = true, true
	raw, err = json.Marshal(st)
	require.NoError(t, err)

	back, _, err := decode(raw)
	require.NoError(t, err)
	assert.True(t, back.Roles["dev"].Sent.Acked)
	assert.True(t, back.Roles["dev"].Posted[0].Boot)
}

// A /clear ends a conversation and starts another in the same process: the boot a clear queued is posted while the
// registry still shows the old conversation, and the id then changes under the live process. That is not the end of a
// session (the process lives): the boot goes back to the queue, still known as the boot, and reaches the new
// conversation, which has no other.
func TestClearedConversationGetsItsBoot(t *testing.T) {
	f, p := nativeFixture(t, trimCrew)
	upNative(t, f, "dev")

	f.rg.context["sid-dev"] = 120_000
	f.status("dev", "idle", f.now.Add(-time.Hour))
	f.s.Tick()
	require.Equal(t, []string{"/clear"}, f.tm.typed["dev"])

	rs := f.s.State.Role("dev")
	boot := rs.QueuedBoot
	require.NotEmpty(t, boot)

	if len(rs.Posted) == 0 {
		f.s.Tick()
	}

	require.Len(t, rs.Posted, 1, "the boot is posted to the old conversation")
	require.True(t, rs.Posted[0].Boot)

	// The clear started another conversation.
	f.rg.byPID[f.tm.up["dev"]].SessionID = "sid-dev-2"
	f.rg.context["sid-dev-2"] = 10_000

	for range 3 {
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
	}

	assert.Zero(t, f.eventCount("dropped"), "the process lives: nothing is dropped")
	require.Len(t, p.posted["sid-dev-2"], 1, "the boot reached the new conversation, once")
	assert.Equal(t, boot, p.posted["sid-dev-2"][0])
	assert.Equal(t, boot, rs.QueuedBoot, "and is still known as the boot")
	require.Len(t, rs.Posted, 1)
	assert.True(t, rs.Posted[0].Boot)
}

// The dispatcher's deliver forgets QueuedBoot once the boot has left the queue; a boot given back because only its
// conversation ended is known as the boot again.
func TestClearedDispatcherGetsItsBootAgain(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	rs := f.s.State.Role("dispatcher")
	boot := "read d and boot\n\n[zmina] the rules"
	rs.QueuedBoot, rs.Inbox = boot, []string{boot}

	require.NotEmpty(t, f.s.deliverInbox("dispatcher", *f.now))
	f.s.deliver(*f.now)
	require.Empty(t, rs.QueuedBoot)

	f.rg.byPID[f.tm.up["dispatcher"]].SessionID = "sid-dispatcher-2"
	f.s.followPosted("dispatcher", *f.now)

	assert.Empty(t, rs.Posted)
	assert.Equal(t, []string{boot}, rs.Inbox)
	assert.Equal(t, boot, rs.QueuedBoot, "known as the boot, so it still goes alone and first")
	assert.Zero(t, f.eventCount("dropped"))
}

// Likewise a release line: the conversation changed under a live process, which is not released by that. It goes back
// as it did before the session's end was settled; only a session that is gone drops it.
func TestReleaseLineGoesBackWhenOnlyTheConversationChanged(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	line := releaseLine(time.Hour, []string{"T-1"})
	require.NoError(t, f.s.post("designer", f.s.session("designer"), line, *f.now))

	f.rg.byPID[f.tm.up["designer"]].SessionID = "sid-designer-2"
	f.s.followPosted("designer", *f.now)

	rs := f.s.State.Role("designer")
	assert.Empty(t, rs.Posted)
	assert.Equal(t, []string{line}, rs.Inbox)
	assert.Zero(t, f.eventCount("dropped"))
	assert.Equal(t, 1, f.eventCount("requeued"))
}

// A session-bound line waiting in a Codex file is withdrawn only with a session that is gone: the bridge process
// lives on, its thread changed, and it takes the file in that thread.
func TestCodexSessionBoundLineStaysWhenOnlyTheThreadChanged(t *testing.T) {
	f, dir := codexFixture(t)
	bridge(t, f, dir, "thread-1")

	require.NoError(t, f.s.post("dev", f.s.session("dev"), releaseLine(time.Hour, []string{"T-1"}), *f.now))
	require.Len(t, waitingFiles(t, dir), 1)

	bridge(t, f, dir, "thread-2") // the same process, another thread

	f.s.followPosted("dev", *f.now)

	rs := f.s.State.Role("dev")
	assert.Len(t, rs.Posted, 1, "its file waits for the bridge")
	assert.Len(t, waitingFiles(t, dir), 1, "not withdrawn")
	assert.Zero(t, f.eventCount("dropped"))
}
