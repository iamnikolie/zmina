package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/message"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

// retentionCrew is a crew whose directory holds stores that have grown: tasks closed long ago, open ones, a task
// that carries another's checkpoint, packets, results, requests, a read meter and a big tick.log. The files are old
// by their own times (old) or from now.
type retentionCrew struct {
	file string
	c    *crew.Crew
	old  time.Time
	now  time.Time
}

func (rc *retentionCrew) path(parts ...string) string {
	return filepath.Join(append([]string{rc.c.StateDir()}, parts...)...)
}

func (rc *retentionCrew) put(t *testing.T, rel, body string, mtime time.Time) string {
	t.Helper()

	p := rc.path(rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	require.NoError(t, os.Chtimes(p, mtime, mtime))

	return p
}

func (rc *retentionCrew) exists(rel string) bool {
	_, err := os.Stat(rc.path(rel))

	return err == nil
}

func newRetentionCrew(t *testing.T, key string) *retentionCrew {
	t.Helper()

	file := testCrew(t, "  dev:\n")
	raw := mustRead(t, file)
	require.NoError(t, os.WriteFile(file, []byte(strings.Replace(string(raw), "roles:\n", key+"roles:\n", 1)), 0o600))

	c, err := crew.Load(file)
	require.NoError(t, err)

	now := time.Now().UTC()
	rc := &retentionCrew{file: file, c: c, now: now, old: now.Add(-90 * 24 * time.Hour)}
	store := taskStore(c)

	put := func(id string, state task.State, updated time.Time, mod func(*task.Task)) {
		tk := task.Task{ID: id, Role: "dev", Asked: "work " + id, State: state, Created: updated, Updated: updated}
		if state == task.Done || state == task.Dropped {
			tk.Closed = &task.Closure{By: "dev", At: updated, State: state}
		}

		if mod != nil {
			mod(&tk)
		}

		require.NoError(t, store.Put(tk))
	}

	recent := now.Add(-5 * 24 * time.Hour)

	put("T-1", task.Done, rc.old, nil)    // closed long ago: everything of it goes
	put("T-2", task.Open, rc.old, nil)    // open, however old: nothing of it goes
	put("T-3", task.Done, recent, nil)    // closed lately: stays
	put("T-4", task.Parked, rc.old, nil)  // parked comes back: stays
	put("T-5", task.Dropped, rc.old, nil) // closed long ago, but T-6 carries its checkpoint
	put("T-6", task.Open, recent, func(tk *task.Task) {
		tk.Checkpoint = &record.Ref{Task: "T-5", Revision: strings.Repeat("c", 64), At: rc.old}
	})

	for _, id := range []string{"T-1", "T-2", "T-3", "T-4", "T-5"} {
		rc.put(t, "handoffs/"+id+"/"+strings.Repeat("a", 64)+".json", "{}", rc.old)
		rc.put(t, "results/"+id+"/"+strings.Repeat("b", 64)+".json", "{}", rc.old)
		rc.put(t, "checkpoints/"+id+"/"+strings.Repeat("c", 64)+".json", "{}", rc.old)
	}

	// No task of these ids: old files go, fresh ones stay.
	rc.put(t, "handoffs/T-7/"+strings.Repeat("a", 64)+".json", "{}", rc.old)
	rc.put(t, "handoffs/T-8/"+strings.Repeat("a", 64)+".json", "{}", now)
	rc.put(t, "handoffs/notes/readme.json", "{}", rc.old)

	// Requests: one old and unnamed, one old but named by the state, one fresh.
	rc.put(t, "releases/"+strings.Repeat("1", 64)+".json", "{}", rc.old)
	rc.put(t, "releases/"+strings.Repeat("2", 64)+".json", "{}", rc.old)
	rc.put(t, "restarts/"+strings.Repeat("3", 64)+".json", "{}", now)
	rc.put(t, "restarts/"+strings.Repeat("4", 64)+".json", "{}", rc.old)
	require.NoError(t, os.WriteFile(rc.path("state.json"), []byte(`{"version":2,"roles":{"dev":{"inbox":["[zmina release] Read checkpoint request \"`+
		rc.path("releases", strings.Repeat("2", 64)+".json")+`\""]}}}`), 0o600))

	// The read meter: two old lines, two recent; tick.log past its cap.
	for _, at := range []time.Time{rc.old, rc.old.Add(time.Hour), recent, now} {
		raw := fmt.Sprintf(`{"at":%q,"kind":"read","out":1}`+"\n", at.Format(time.RFC3339))
		f, err := os.OpenFile(rc.path("reads.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, err = f.WriteString(raw)
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}

	var log strings.Builder
	for i := 0; log.Len() < tickLogMax+(1<<20); i++ {
		fmt.Fprintf(&log, "2026-01-01 00:00:00 line %06d %s\n", i, strings.Repeat("x", 80))
	}

	rc.put(t, "tick.log", log.String(), now)
	rc.put(t, "courier.log", log.String(), now)

	// Messages: one finished long ago, one still waiting.
	ms := message.Store{Dir: rc.path("messages")}
	for key, stage := range map[string]string{"done": "read", "waiting": "received"} {
		r := message.Report{Version: 1, Key: key, From: "local/default", To: "local/ops", Body: key, Sequence: 1, At: rc.old}
		r.ID = message.Identity(r.From, r.To, r.Key)
		r.Revision = message.Content(r)

		m, _, err := ms.Add(r)
		require.NoError(t, err)

		v := m.Revisions[r.Revision]
		require.NoError(t, message.Advance(v, stage, rc.old))
		v.Intents["report"] = message.Intent{Queued: true}
		require.NoError(t, ms.Save(m))
	}

	return rc
}

func (rc *retentionCrew) messageID(key string) string {
	return message.Identity("local/default", "local/ops", key)
}

func (rc *retentionCrew) tickLogSize(t *testing.T) int64 {
	t.Helper()

	fi, err := os.Stat(rc.path("tick.log"))
	require.NoError(t, err)

	return fi.Size()
}

func (rc *retentionCrew) courierLogSize(t *testing.T) int64 {
	t.Helper()

	fi, err := os.Stat(rc.path("courier.log"))
	require.NoError(t, err)

	return fi.Size()
}

func (rc *retentionCrew) events(t *testing.T, name string) []events.Event {
	t.Helper()

	evs, err := eventLog(rc.c).Read(events.Filter{})
	require.NoError(t, err)

	var out []events.Event

	for _, e := range evs {
		if e.Event == name {
			out = append(out, e)
		}
	}

	return out
}

// --dry-run lists what a pass would move and moves nothing, writes no stamp and logs nothing.
func TestRetentionDryRunListsAndMovesNothing(t *testing.T) {
	rc := newRetentionCrew(t, "")
	sizeBefore, courierBefore := rc.tickLogSize(t), rc.courierLogSize(t)

	out, _, err := zm(t, rc.file, "retention", "--dry-run")
	require.NoError(t, err)

	for _, want := range []string{
		"handoffs/T-1 (1 files, task done", "handoffs/T-5 ", "handoffs/T-7 (1 files, no such task",
		"results/T-1 ", "results/T-5 ", "checkpoints/T-1 ",
		"releases/" + strings.Repeat("1", 64) + ".json", "restarts/" + strings.Repeat("4", 64) + ".json",
		"messages/" + rc.messageID("done"), "reads.jsonl (2 of 4 lines", "tick.log (", "courier.log (",
		"retention: would move",
	} {
		assert.Contains(t, out, want)
	}

	for _, not := range []string{"T-2", "T-3", "T-4", "T-6", "T-8", "checkpoints/T-5", strings.Repeat("2", 64), strings.Repeat("3", 64),
		rc.messageID("waiting"), "notes"} {
		assert.NotContains(t, out, not, "this stays and is not listed")
	}

	assert.True(t, rc.exists("handoffs/T-1/"+strings.Repeat("a", 64)+".json"))
	assert.True(t, rc.exists("messages/"+rc.messageID("done")+".json"))
	assert.Equal(t, sizeBefore, rc.tickLogSize(t))
	assert.Equal(t, courierBefore, rc.courierLogSize(t))
	assert.NoFileExists(t, rc.path("retention.json"), "a dry run stamps nothing")
	assert.NoDirExists(t, rc.path("archive"))
	assert.Empty(t, rc.events(t, "retention"))
}

// A pass moves finished work into archive/retention/<yyyy-mm>/ and nothing an open task points at; the stores shrink
// and the stubs say where the records went.
func TestRetentionPassMovesFinishedWorkAndKeepsWhatIsPointedAt(t *testing.T) {
	rc := newRetentionCrew(t, "")
	arch := "archive/retention/" + rc.now.Format("2006-01") + "/"
	hash := func(c string) string { return strings.Repeat(c, 64) + ".json" }

	out, _, err := zm(t, rc.file, "retention")
	require.NoError(t, err)
	assert.Contains(t, out, "retention: moved ")

	// Gone, archived whole.
	for _, kind := range []string{"handoffs", "results", "checkpoints"} {
		assert.False(t, rc.exists(kind+"/T-1/"+hash(map[string]string{"handoffs": "a", "results": "b", "checkpoints": "c"}[kind])), kind)
		assert.True(t, rc.exists(arch+kind+"/T-1/"+hash(map[string]string{"handoffs": "a", "results": "b", "checkpoints": "c"}[kind])), kind)
	}

	assert.True(t, rc.exists(arch+"handoffs/T-5/"+hash("a")), "T-5 is dropped long ago")
	assert.True(t, rc.exists(arch+"results/T-5/"+hash("b")))
	assert.True(t, rc.exists(arch+"handoffs/T-7/"+hash("a")), "no such task, old files")
	assert.True(t, rc.exists(arch+"releases/"+hash("1")))
	assert.True(t, rc.exists(arch+"restarts/"+hash("4")))
	assert.False(t, rc.exists("releases/"+hash("1")))

	// Kept: open, recently closed, parked, pointed at, named by the state, fresh, not a record at all.
	for _, keep := range []string{
		"handoffs/T-2/" + hash("a"), "results/T-2/" + hash("b"), "checkpoints/T-2/" + hash("c"),
		"handoffs/T-3/" + hash("a"), "results/T-3/" + hash("b"),
		"handoffs/T-4/" + hash("a"), "checkpoints/T-4/" + hash("c"),
		"checkpoints/T-5/" + hash("c"), // the open T-6 carries it
		"handoffs/T-8/" + hash("a"),
		"handoffs/notes/readme.json",
		"releases/" + hash("2"), "restarts/" + hash("3"),
	} {
		assert.True(t, rc.exists(keep), "%s stays", keep)
	}

	// A task that is asked for its archived result finds a note saying where it went.
	stub, err := os.ReadFile(rc.path("results/T-1/archived.txt"))
	require.NoError(t, err)
	assert.Contains(t, string(stub), arch+"results/T-1")
	assert.NoFileExists(t, rc.path("handoffs/T-1/archived.txt"), "packets of a closed task need no note")

	// Messages: the finished one is archived and still found; the waiting one stays.
	ms := message.Store{Dir: rc.path("messages")}
	assert.False(t, rc.exists("messages/"+rc.messageID("done")+".json"))
	assert.True(t, rc.exists("messages/archive/"+rc.messageID("done")+".json"))
	_, err = ms.Load(rc.messageID("done"))
	require.NoError(t, err)
	assert.True(t, rc.exists("messages/"+rc.messageID("waiting")+".json"))

	// The meter keeps its recent lines; the old ones are in the archive.
	kept, err := os.ReadFile(rc.path("reads.jsonl"))
	require.NoError(t, err)
	assert.Len(t, strings.Fields(string(kept)), 2)

	moved, err := os.ReadFile(rc.path(arch + "reads.jsonl"))
	require.NoError(t, err)
	assert.Len(t, strings.Fields(string(moved)), 2)

	// tick.log is cut to its tail, keeping the newest line and saying that it was cut.
	assert.LessOrEqual(t, rc.tickLogSize(t), int64(tickLogKeep+200))

	log, err := os.ReadFile(rc.path("tick.log"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(log), "[zmina retention "), string(log[:60]))
	assert.Contains(t, string(log), " line ")
	assert.True(t, strings.HasSuffix(string(log), "\n"))
	assert.NotContains(t, string(log[:200]), " line 000000 ")

	// courier.log, which a service that restarts often fills, is cut the same way.
	assert.LessOrEqual(t, rc.courierLogSize(t), int64(tickLogKeep+200))

	courier, err := os.ReadFile(rc.path("courier.log"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(courier), "[zmina retention "), string(courier[:60]))
	assert.NotContains(t, string(courier[:200]), " line 000000 ")
	assert.True(t, strings.HasSuffix(string(courier), "\n"))

	// One event with the counts, a stamp with the same, and the doctor line says so.
	evs := rc.events(t, "retention")
	require.Len(t, evs, 1)
	assert.Contains(t, evs[0].Detail, "handoffs 3")
	assert.Contains(t, evs[0].Detail, "messages 1")
	assert.Contains(t, evs[0].Detail, "tick.log cut")

	st := readRetentionStamp(rc.c.StateDir())
	assert.False(t, st.At.IsZero())
	assert.Equal(t, 3, st.Moved["handoffs"])

	// A second pass finds nothing more.
	out, _, err = zm(t, rc.file, "retention")
	require.NoError(t, err)
	assert.Contains(t, out, "retention: nothing to move")
}

// With a longer span in the crew file, what is younger than it stays.
func TestRetentionSpanComesFromTheCrewFile(t *testing.T) {
	rc := newRetentionCrew(t, "retention: 120d\n")

	out, _, err := zm(t, rc.file, "retention", "--dry-run")
	require.NoError(t, err)

	for _, what := range []string{"handoffs/", "results/", "checkpoints/", "releases/", "restarts/", "messages/", "reads.jsonl"} {
		assert.NotContains(t, out, what, "everything is 90 days old: younger than 120")
	}

	assert.Contains(t, out, "tick.log (", "a log past its size is cut whatever its age")
}

// The ticker runs the pass once a day, from its own stamp; off keeps it from running; a pass takes no state lock.
func TestTickerRunsTheRetentionPassOnceADay(t *testing.T) {
	rc := newRetentionCrew(t, "")
	a := &app{crew: rc.file, out: io.Discard, err: io.Discard}

	// A beat in the middle of a locked command: the pass needs no state lock.
	lock, err := shift.Lock(rc.c.StateDir())
	require.NoError(t, err)

	var out bytes.Buffer

	a.out = &out
	done := make(chan struct{})

	go func() {
		a.retentionBeat(rc.now)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the pass waited for the state lock")
	}

	lock.Release()

	assert.Contains(t, out.String(), "retention: moved ", "the ticker says what it moved")
	require.Len(t, rc.events(t, "retention"), 1)
	assert.Equal(t, "ticker", rc.events(t, "retention")[0].By)

	// The same day: nothing. A day later: a pass again (it finds nothing and, quiet, says nothing).
	out.Reset()
	a.retentionBeat(rc.now.Add(time.Hour))
	a.retentionBeat(rc.now.Add(23 * time.Hour))
	assert.Len(t, rc.events(t, "retention"), 1)
	assert.Empty(t, out.String())

	a.retentionBeat(rc.now.Add(25 * time.Hour))
	assert.Len(t, rc.events(t, "retention"), 2)
	assert.Empty(t, out.String(), "a pass with nothing to move is silent in the ticker's output")
}

func TestTickerDoesNotRunTheRetentionPassWhenOff(t *testing.T) {
	rc := newRetentionCrew(t, "retention: off\n")
	a := &app{crew: rc.file, out: io.Discard, err: io.Discard}

	a.retentionBeat(rc.now)

	assert.Empty(t, rc.events(t, "retention"))
	assert.NoFileExists(t, rc.path("retention.json"))
	assert.True(t, rc.exists("handoffs/T-1/"+strings.Repeat("a", 64)+".json"))

	// By hand it still runs: off only stops the ticker.
	_, _, err := zm(t, rc.file, "retention")
	require.NoError(t, err)
	assert.False(t, rc.exists("handoffs/T-1/"+strings.Repeat("a", 64)+".json"))
}

// The real beat calls the pass: `zmina tick` does it, once.
func TestTickCommandRunsTheRetentionPass(t *testing.T) {
	rc := newRetentionCrew(t, "")

	_, _, err := zm(t, rc.file, "tick")
	require.NoError(t, err)
	assert.False(t, rc.exists("handoffs/T-1/"+strings.Repeat("a", 64)+".json"))
	assert.Len(t, rc.events(t, "retention"), 1)

	_, _, err = zm(t, rc.file, "tick")
	require.NoError(t, err)
	assert.Len(t, rc.events(t, "retention"), 1, "once a day")
}

// A pass past its deadline stops, says so and fails nothing; what it did not reach stays for the next pass.
func TestRetentionPassStopsAtItsDeadline(t *testing.T) {
	rc := newRetentionCrew(t, "")

	was := retentionTimeout
	retentionTimeout = time.Nanosecond

	t.Cleanup(func() { retentionTimeout = was })

	out, _, err := zm(t, rc.file, "retention")
	require.NoError(t, err)
	assert.Contains(t, out, "stopped at its 1ns deadline")
	assert.True(t, rc.exists("handoffs/T-1/"+strings.Repeat("a", 64)+".json"))
}

// Task files that do not read: nothing is archived that one of them may point at, and the pass says why.
func TestRetentionLeavesRecordsAloneWhileATaskFileDoesNotRead(t *testing.T) {
	rc := newRetentionCrew(t, "")
	require.NoError(t, os.WriteFile(rc.path("tasks", "T-9.json"), []byte("{broken"), 0o600))

	out, _, err := zm(t, rc.file, "retention")
	require.NoError(t, err)
	assert.Contains(t, out, "results left alone: 1 task files do not read")
	assert.True(t, rc.exists("results/T-1/"+strings.Repeat("b", 64)+".json"))
	assert.True(t, rc.exists("checkpoints/T-1/"+strings.Repeat("c", 64)+".json"))
	assert.False(t, rc.exists("handoffs/T-1/"+strings.Repeat("a", 64)+".json"), "a packet points nowhere else: it goes")

	// A pass in a crew with no task ledger at all moves nothing that is tied to a task.
	bare := newRetentionCrew(t, "")
	require.NoError(t, os.RemoveAll(bare.path("tasks")))

	out, _, err = zm(t, bare.file, "retention")
	require.NoError(t, err)
	assert.True(t, bare.exists("handoffs/T-1/"+strings.Repeat("a", 64)+".json"), out)
}

// Two passes do not run at once: one that finds the other's lock leaves.
func TestRetentionPassesDoNotOverlap(t *testing.T) {
	rc := newRetentionCrew(t, "")

	unlock, err := lockRetention(rc.c.StateDir())
	require.NoError(t, err)

	defer unlock()

	_, _, err = zm(t, rc.file, "retention")
	require.ErrorIs(t, err, errRetentionBusy)

	a := &app{crew: rc.file, out: io.Discard, err: io.Discard}
	a.retentionBeat(rc.now)
	assert.True(t, rc.exists("handoffs/T-1/"+strings.Repeat("a", 64)+".json"))
	assert.Empty(t, rc.events(t, "retention"))
}

// doctor names the retention pass, and a report record that does not read, by its file.
func TestDoctorShowsRetentionAndUnreadableMessages(t *testing.T) {
	rc := newRetentionCrew(t, "")

	var lines []string

	collect := func(name, level, format string, args ...any) {
		lines = append(lines, level+" "+name+" "+fmt.Sprintf(format, args...))
	}

	checkStores(rc.c, rc.now, collect)
	assert.Contains(t, strings.Join(lines, "\n"), "ok retention keeps 30 days · no pass yet")
	assert.Contains(t, strings.Join(lines, "\n"), "ok messages 2 readable")

	_, _, err := zm(t, rc.file, "retention")
	require.NoError(t, err)

	bad := strings.Repeat("9", 64) + ".json"
	require.NoError(t, os.WriteFile(rc.path("messages", bad), []byte("{no"), 0o600))

	lines = nil
	checkStores(rc.c, time.Now(), collect)

	all := strings.Join(lines, "\n")
	assert.Contains(t, all, "warn messages "+bad)
	assert.Contains(t, all, "ok retention keeps 30 days · last pass ")
	assert.Contains(t, all, "handoffs 3")

	lines = nil
	checkStores(rc.c, time.Now().Add(5*24*time.Hour), collect)
	assert.Contains(t, strings.Join(lines, "\n"), "warn retention")
	assert.Contains(t, strings.Join(lines, "\n"), "is the ticker running?")

	off := newRetentionCrew(t, "retention: off\n")
	lines = nil
	checkStores(off.c, off.now, collect)
	assert.Contains(t, strings.Join(lines, "\n"), "ok retention off")
}
