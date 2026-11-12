package cmd

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

	"github.com/iamnikolie/zmina/internal/shift"
)

// rotationRecord writes a role's rotation record, line by line, next to the crew file.
func rotationRecord(t *testing.T, crewFile, role string, lines ...string) string {
	t.Helper()

	path := filepath.Join(filepath.Dir(crewFile), "rotations", role+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))

	return path
}

const (
	rotRebuilt = `{"id":"aaaa1111","at":"2026-11-01T10:00:00Z","phase":"prepared","role":"dev","kind":"restart_at","why":"context 262k over restart_at 250k","from":{"runtime":"claude","session":"sid-old","dir":"/work/example","context":262000},"artifacts":[{"task":"T-1","checkpoint":"7b2b6944f4c7"}],"asked_at":"2026-11-01T09:50:00Z"}
{"id":"aaaa1111","at":"2026-11-01T10:00:01Z","phase":"switching","claim":"2026-11-01T10:00:01Z"}
{"id":"aaaa1111","at":"2026-11-01T10:01:00Z","phase":"started","lines":["[zmina] restarted fresh: context 262k over restart_at 250k. Your note: T-1 7b2b6944f4c7, in each task's packet that follows."]}
{"id":"aaaa1111","at":"2026-11-01T10:01:30Z","phase":"evidence","what":"session","session":"sid-new"}
{"id":"aaaa1111","at":"2026-11-01T10:02:00Z","phase":"evidence","what":"boot","by":"dev","session":"sid-new"}
{"id":"aaaa1111","at":"2026-11-01T10:03:00Z","phase":"evidence","what":"accepted","task":"T-1","by":"dev","session":"sid-new"}
{"id":"aaaa1111","at":"2026-11-01T10:03:30Z","phase":"closed","outcome":"rebuilt","confirmed":["boot","T-1 packet accepted"]}`

	rotLost = `{"id":"bbbb2222","at":"2026-11-02T10:00:00Z","phase":"prepared","role":"dev","kind":"restart_idle","why":"idle 1h at 120k, near restart_idle 1h","from":{"runtime":"claude","session":"sid-new"},"artifacts":[{"task":"T-2","checkpoint":"aabbccddeeff"}]}
{"id":"bbbb2222","at":"2026-11-02T10:00:01Z","phase":"switching","claim":"2026-11-02T10:00:01Z"}
{"id":"bbbb2222","at":"2026-11-02T10:01:00Z","phase":"started"}
{"id":"bbbb2222","at":"2026-11-02T10:12:00Z","phase":"closed","outcome":"lost","detail":"it did not accept T-2 within 10m0s idle after its context was shown","back":"zmina --crew 'x' down 'dev' --clear && zmina --crew 'x' up 'dev' --resume='sid-new'"}
{"id":"bbbb2222","at":"2026-11-02T10:20:00Z","phase":"evidence","what":"accepted","task":"T-2","by":"dev","session":"sid-new"}`

	rotOpen = `{"id":"cccc3333","at":"2026-11-03T10:00:00Z","phase":"prepared","role":"lead","kind":"restart_done","why":"task done: T-3, context 180k over restart_done 150k","from":{"runtime":"claude","session":"sid-lead"},"artifacts":[{"task":"T-3"}]}
{"id":"cccc3333","at":"2026-11-03T10:00:01Z","phase":"switching","claim":"2026-11-03T10:00:01Z"}
{"id":"cccc3333","at":"2026-11-03T10:01:00Z","phase":"started"}
{"id":"cccc3333","at":"2026-11-03T10:02:00Z","phase":"evidence","what":"boot","by":"lead"}`
)

func rotationCrew(t *testing.T) string {
	t.Helper()

	file := testCrew(t, "  dev: {}\n  lead: {}\n")
	rotationRecord(t, file, "dev", rotRebuilt, rotLost)
	rotationRecord(t, file, "lead", rotOpen)

	return file
}

// ls is the newest first, across the crew or for one role, with where each stands and what confirmed it or why not.
func TestRotationLsListsTheNewestFirst(t *testing.T) {
	file := rotationCrew(t)

	out, _, err := zm(t, file, "rotation", "ls")
	require.NoError(t, err)

	lead, lost, rebuilt := strings.Index(out, "cccc3333"), strings.Index(out, "bbbb2222"), strings.Index(out, "aaaa1111")
	require.True(t, lead >= 0 && lost >= 0 && rebuilt >= 0, out)
	assert.Less(t, lead, lost, "newest first: %s", out)
	assert.Less(t, lost, rebuilt, out)

	assert.Contains(t, out, "restart_done")
	assert.Contains(t, out, "restart_idle")
	assert.Contains(t, out, "rebuilt")
	assert.Contains(t, out, "lost → rebuilt (late)", "the lost one whose conversation then accepted its task")
	assert.Regexp(t, `started \(open \d+[mhd]\)`, out, "an open one says how long")
	assert.Contains(t, out, "context 262k over restart_at 250k; boot, T-1 packet accepted")
	assert.Contains(t, out, "it did not accept T-2 within 10m0s idle after its context was shown")
	assert.Contains(t, out, "so far: boot", "an open one says what it has")

	out, _, err = zm(t, file, "rotation", "ls", "dev")
	require.NoError(t, err)
	assert.NotContains(t, out, "cccc3333")
	assert.Contains(t, out, "aaaa1111")

	out, _, err = zm(t, file, "rotation", "ls", "-n", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "cccc3333")
	assert.NotContains(t, out, "bbbb2222")

	out, _, err = zm(t, file, "rotation", "ls", "nobody")
	require.NoError(t, err)
	assert.Equal(t, "no rotations\n", out)
}

func TestRotationLsJSON(t *testing.T) {
	file := rotationCrew(t)

	out, _, err := zm(t, file, "rotation", "ls", "--json")
	require.NoError(t, err)

	var rots []shift.Rotation
	require.NoError(t, json.Unmarshal([]byte(out), &rots))
	require.Len(t, rots, 3)
	assert.Equal(t, "cccc3333", rots[0].ID)
	assert.Equal(t, "open", map[bool]string{true: "open", false: "closed"}[rots[0].Open()])
	assert.Equal(t, shift.OutcomeLost, rots[1].Closed.Outcome)
	assert.True(t, rots[1].LateRebuilt())
	assert.Equal(t, "sid-new", rots[2].To.Session)

	out, _, err = zm(t, file, "rotation", "ls", "nobody", "--json")
	require.NoError(t, err)
	assert.Equal(t, "[]", strings.TrimSpace(out), "none is an empty list, not null")
}

// show prints one whole: a role's newest, or the one with the id.
func TestRotationShow(t *testing.T) {
	file := rotationCrew(t)

	out, _, err := zm(t, file, "rotation", "show", "aaaa1111")
	require.NoError(t, err)

	for _, want := range []string{"aaaa1111 · dev · restart_at · rebuilt", "why: context 262k over restart_at 250k",
		"from: claude conversation sid-old, 262k in it, in /work/example", "to: sid-new", "handed: T-1, note 7b2b6944f4c7",
		"asked      ", "prepared   ", "switching  ", "started    ", "line: [zmina] restarted fresh: context 262k",
		"evidence   ", "boot", "T-1 accepted", "closed     ", "confirmed: boot, T-1 packet accepted"} {
		assert.Contains(t, out, want)
	}

	out, _, err = zm(t, file, "rotation", "show", "dev")
	require.NoError(t, err)
	assert.Contains(t, out, "bbbb2222 · dev · restart_idle · lost → rebuilt (late)", "the role's newest")
	assert.Contains(t, out, "back to its old conversation: zmina --crew 'x' down 'dev' --clear")
	assert.Contains(t, out, "late       ", "its late accept")
	assert.Contains(t, out, "detail: it did not accept T-2")

	out, _, err = zm(t, file, "rotation", "show", "lead")
	require.NoError(t, err)
	assert.Contains(t, out, "cccc3333 · lead · restart_done · started (open ")
	assert.Contains(t, out, "followed: T-3")

	out, _, err = zm(t, file, "rotation", "show", "bbbb2222", "--json")
	require.NoError(t, err)

	var r shift.Rotation
	require.NoError(t, json.Unmarshal([]byte(out), &r))
	assert.Equal(t, "bbbb2222", r.ID)

	_, _, err = zm(t, file, "rotation", "show", "nobody")
	require.ErrorContains(t, err, `no role "nobody" in the crew and no rotation with that id`)

	// A role of the crew that was never rotated has none: said so, and no error.
	out, _, err = zm(t, file, "rotation", "show", "dispatcher")
	require.NoError(t, err)
	assert.Equal(t, "no rotations for dispatcher\n", out)

	out, _, err = zm(t, file, "rotation", "show", "dispatcher", "--json")
	require.NoError(t, err)
	assert.Equal(t, "null", strings.TrimSpace(out))
}

// A crew with no record says so, and a record that does not read is no failure of a look.
func TestRotationLsWithNoRecordOrABrokenOne(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	out, _, err := zm(t, file, "rotation", "ls")
	require.NoError(t, err)
	assert.Equal(t, "no rotations\n", out)

	rotationRecord(t, file, "dev", "not json", `{"id":"x","at":"nonsense"}`, rotRebuilt)

	out, _, err = zm(t, file, "rotation", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "aaaa1111")
}

// evidenceLines are the evidence a role's record holds of a rotation.
func evidenceLines(t *testing.T, path, id string) []string {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var out []string

	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var l struct {
			ID, Phase, What, Task, By, Session string
		}

		if json.Unmarshal([]byte(line), &l) != nil || !strings.EqualFold(l.ID, id) || l.Phase != "evidence" {
			continue
		}

		out = append(out, l.What+" "+l.Task+" "+l.By+" "+l.Session)
	}

	return out
}

// An open rotation of the role is given a hook-start by the role's own SessionStart hook, from the session the hook names,
// once. A command run by hand, for another role, or with no session in its input is not that hook.
func TestPendingBriefNotesTheHookStartOfARotation(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	path := rotationRecord(t, file, "dev", strings.Join(strings.Split(rotRebuilt, "\n")[:3], "\n"))
	hook := `{"session_id":"sid-new","source":"startup"}`

	// By hand, or as another role: nothing.
	_, err := zmIn(t, file, hook, "pending", "--role", "dev", "--brief")
	require.NoError(t, err)
	t.Setenv("ZMINA_ROLE", "lead")
	_, err = zmIn(t, file, hook, "pending", "--role", "dev", "--brief")
	require.NoError(t, err)

	// The role's own hook, with no session in its input: nothing.
	t.Setenv("ZMINA_ROLE", "dev")
	_, err = zmIn(t, file, "", "pending", "--role", "dev", "--brief")
	require.NoError(t, err)
	assert.Empty(t, evidenceLines(t, path, "aaaa1111"))

	// The old conversation's own hook is not the fresh session's start.
	_, err = zmIn(t, file, `{"session_id":"sid-old","source":"resume"}`, "pending", "--role", "dev", "--brief")
	require.NoError(t, err)
	assert.Empty(t, evidenceLines(t, path, "aaaa1111"))

	for range 2 {
		_, err = zmIn(t, file, hook, "pending", "--role", "dev", "--brief")
		require.NoError(t, err)
	}

	assert.Equal(t, []string{"hook-start  dev sid-new"}, evidenceLines(t, path, "aaaa1111"), "once")
}

// The commands that read a task's note note it in the record of an open rotation that handed the task: run by the role, not by
// its owner's shell or another role, and not by the old conversation.
func TestReadingATasksNoteIsEvidenceOfARotation(t *testing.T) {
	file := testCrew(t, "  support:\n    handoff:\n      sections:\n        - name: draft\n          guidance: Proposed response\n")

	_, _, err := zm(t, file, "task", "new", "support", "Answer the question")
	require.NoError(t, err)

	body := filepath.Join(t.TempDir(), "checkpoint.json")
	require.NoError(t, os.WriteFile(body, []byte(`{"draft":"half done"}`), 0o600))
	_, _, err = zm(t, file, "task", "checkpoint", "1", "--file", body)
	require.NoError(t, err)

	started := strings.ReplaceAll(strings.Join(strings.Split(rotRebuilt, "\n")[:3], "\n"), `"role":"dev"`, `"role":"support"`)
	path := rotationRecord(t, file, "support", started)

	read := func(args ...string) {
		t.Helper()

		_, _, err := zm(t, file, args...)
		require.NoError(t, err)
	}

	// The owner's shell, and a tool of an agent's session that does not say whose it is: neither is the role reading it.
	read("task", "checkpoint", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sid-new")
	read("task", "checkpoint", "1")
	read("task", "show", "1")
	assert.Empty(t, evidenceLines(t, path, "aaaa1111"), "a caller that does not name its role is not evidence")

	// Another role.
	t.Setenv("ZMINA_ROLE", "dispatcher")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sid-new")
	read("task", "checkpoint", "1")
	read("task", "show", "1")
	assert.Empty(t, evidenceLines(t, path, "aaaa1111"))

	// Saving a note is not reading one; the old conversation's reading is its own.
	t.Setenv("ZMINA_ROLE", "support")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sid-old")
	read("task", "checkpoint", "1")
	read("task", "show", "1")
	assert.Empty(t, evidenceLines(t, path, "aaaa1111"))

	// The role, in the fresh session; once however many times.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sid-new")
	read("task", "checkpoint", "1")
	read("task", "show", "1")
	read("task", "checkpoint", "1", "--json")

	assert.Equal(t, []string{"note-fetched T-1 support sid-new"}, evidenceLines(t, path, "aaaa1111"))
}

// The daily pass removes the rotation records nothing was written to for the span, and keeps the others.
func TestRetentionRemovesStaleRotationRecords(t *testing.T) {
	rc := newRetentionCrew(t, "")

	line := `{"id":"aaaa1111","at":"2026-01-01T10:00:00Z","phase":"prepared","role":"dev"}` + "\n" +
		`{"id":"aaaa1111","at":"2026-01-01T10:00:05Z","phase":"closed","outcome":"resumed"}` + "\n"
	rc.put(t, "rotations/dev.jsonl", line, rc.old)
	rc.put(t, "rotations/lead.jsonl", line, rc.now)

	out, _, err := zm(t, rc.file, "retention", "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "rotations/dev.jsonl (1 rotations, nothing written to it since ")
	assert.NotContains(t, out, "rotations/lead.jsonl")
	assert.True(t, rc.exists("rotations/dev.jsonl"), "a dry run moves nothing")

	_, _, err = zm(t, rc.file, "retention")
	require.NoError(t, err)
	assert.False(t, rc.exists("rotations/dev.jsonl"))
	assert.True(t, rc.exists("rotations/lead.jsonl"))

	evs := rc.events(t, "retention")
	require.Len(t, evs, 1)
	assert.Contains(t, evs[0].Detail, "rotation records 1")

	_ = time.Now
}

// A record past what a look reads is set aside, and the pass says what it replaces: the set-aside file of an earlier pass that
// was kept is gone with it, and the dry run tells so before it happens.
func TestRetentionSaysWhatASetAsideRotationRecordReplaces(t *testing.T) {
	rc := newRetentionCrew(t, "")

	var big strings.Builder

	for i := 0; big.Len() < 1<<20+4096; i++ {
		fmt.Fprintf(&big, `{"id":"r%05d","at":"2026-11-01T10:00:00Z","phase":"prepared","role":"dev","why":%q}`+"\n", i, strings.Repeat("w", 300))
		fmt.Fprintf(&big, `{"id":"r%05d","at":"2026-11-01T10:00:05Z","phase":"closed","outcome":"resumed"}`+"\n", i)
	}

	rc.put(t, "rotations/dev.jsonl", big.String(), rc.now.Add(-time.Hour))
	rc.put(t, "rotations/dev.jsonl.old", `{"id":"e1","at":"2026-10-20T10:00:00Z","phase":"prepared","role":"dev"}`+"\n", rc.now.Add(-48*time.Hour))

	out, _, err := zm(t, rc.file, "retention", "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "over 1 MiB: set aside as dev.jsonl.old, replacing the one set aside before it, which held 1 rotations and is gone")
	assert.True(t, rc.exists("rotations/dev.jsonl"), "a dry run moves nothing")

	out, _, err = zm(t, rc.file, "retention")
	require.NoError(t, err)
	assert.Contains(t, out, "retention: moved ")
	assert.False(t, rc.exists("rotations/dev.jsonl"))
	assert.True(t, rc.exists("rotations/dev.jsonl.old"))

	raw, err := os.ReadFile(rc.path("rotations/dev.jsonl.old"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"id":"r00000"`, "the big one is what is kept")
}
