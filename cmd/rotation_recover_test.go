package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/shift"
)

// screenHas waits for a role's screen to show a text, the fake claude printing its arguments when it starts.
func screenHas(t *testing.T, file, role, text string) {
	t.Helper()

	var screen string

	require.Eventually(t, func() bool {
		screen, _, _ = zm(t, file, "peek", role)

		return strings.Contains(screen, text)
	}, 15*time.Second, 50*time.Millisecond, "its screen: %s", screen)
}

// abort puts a role whose rotation was lost back on the conversation the rotation took it from; the closed record gets a line of
// evidence, and ls says so.
func TestRotationAbortPutsTheRoleBackOnItsOldConversation(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	_, _, err := zm(t, file, "up", "dev")
	require.NoError(t, err)
	screenHas(t, file, "dev", "claude ")

	// lost, and not accepted since
	rotationRecord(t, file, "dev", strings.Join(strings.Split(rotLost, "\n")[:4], "\n"))

	out, _, err := zm(t, file, "rotation", "abort", "dev")
	require.NoError(t, err)
	assert.Contains(t, out, "dev is back on its conversation sid-new (rotation bbbb2222 aborted)")
	screenHas(t, file, "dev", "--resume sid-new")

	out, _, err = zm(t, file, "rotation", "ls", "dev")
	require.NoError(t, err)
	assert.Contains(t, out, "lost → back on its old conversation (aborted)")

	out, _, err = zm(t, file, "rotation", "show", "dev")
	require.NoError(t, err)
	assert.Contains(t, out, "late       ")
	assert.Contains(t, out, "aborted")
}

// abort refuses, naming zmina up --fresh, when the old conversation's transcript is gone, and with nothing to go back from.
func TestRotationAbortRefuses(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	_, _, err := zm(t, file, "up", "dev")
	require.NoError(t, err)
	screenHas(t, file, "dev", "claude ")

	_, _, err = zm(t, file, "rotation", "abort", "dev")
	require.ErrorContains(t, err, "no rotation of it to abort")

	path := rotationRecord(t, file, "dev", rotLost)
	keep := hasTranscript
	hasTranscript = func(string, string) bool { return false }

	t.Cleanup(func() { hasTranscript = keep })

	_, _, err = zm(t, file, "rotation", "abort", "dev")
	require.ErrorContains(t, err, "its old conversation sid-new is gone")
	assert.ErrorContains(t, err, "zmina up dev --fresh")

	out, _, perr := zm(t, file, "peek", "dev")
	require.NoError(t, perr)
	assert.NotContains(t, out, "--resume", "nothing was put down")

	hasTranscript = func(string, string) bool { return true }
	rotationRecord(t, file, "dev", rotRebuilt)

	_, _, err = zm(t, file, "rotation", "abort", "dev")
	require.ErrorContains(t, err, "closed rebuilt; only an open or a lost one is aborted")

	_ = path
}

// A fresh start of a role that holds a task, by zmina up and by zmina restart --fresh, hands the task and keeps a record each,
// of its own kind; retry then finds the lines in the queue and hands nothing; one for a role that is down has no session to
// hand to.
func TestUpAndRestartFreshHandTheHeldTaskAndRetryHandsNothingTwice(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, file, "task", "new", "dev", "Draw", "the", "logo")
	require.NoError(t, err)

	_, _, err = zm(t, file, "rotation", "retry", "dev")
	require.ErrorContains(t, err, "no rotation of it to retry")

	_, _, err = zm(t, file, "up", "dev")
	require.NoError(t, err)
	screenHas(t, file, "dev", "claude ")

	out, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "restarted fresh: started by hand (zmina up)")
	assert.Contains(t, out, "Read handoff packet")

	out, _, err = zm(t, file, "rotation", "show", "dev", "--json")
	require.NoError(t, err)

	var r shift.Rotation
	require.NoError(t, json.Unmarshal([]byte(out), &r))
	assert.Equal(t, shift.KindUp, r.Kind)
	assert.Equal(t, []shift.RotationArtifact{{Task: "T-1"}}, r.Artifacts)
	assert.Len(t, r.Lines, 2)

	out, _, err = zm(t, file, "rotation", "retry", "dev")
	require.NoError(t, err)
	assert.Contains(t, out, "dev: T-1: left, its packet is already in its queue")

	_, _, err = zm(t, file, "restart", "dev", "--fresh")
	require.NoError(t, err)
	screenHas(t, file, "dev", "claude ")

	out, _, err = zm(t, file, "rotation", "show", "dev", "--json")
	require.NoError(t, err)

	r = shift.Rotation{}
	require.NoError(t, json.Unmarshal([]byte(out), &r))
	assert.Equal(t, shift.KindManual, r.Kind, "the restart is a rotation of its own")
	assert.Equal(t, []shift.RotationArtifact{{Task: "T-1"}}, r.Artifacts)
	assert.NotEmpty(t, r.Lines)
	assert.True(t, r.Open())

	out, _, err = zm(t, file, "rotation", "ls", "dev")
	require.NoError(t, err)
	assert.Contains(t, out, "manual")
	assert.Contains(t, out, "up ")

	// On its conversation: a record that closed resumed, and nothing handed.
	a := &app{crew: file}
	require.NoError(t, a.withState(func(s *shift.Shift) error { s.State.Role("dev").LastSession = "previous-session"; return nil }))

	_, _, err = zm(t, file, "restart", "dev", "--continue")
	require.NoError(t, err)
	screenHas(t, file, "dev", "--resume previous-session")

	out, _, err = zm(t, file, "rotation", "show", "dev", "--json")
	require.NoError(t, err)

	r = shift.Rotation{}
	require.NoError(t, json.Unmarshal([]byte(out), &r))
	require.False(t, r.Open(), "%+v", r)
	assert.Equal(t, shift.OutcomeResumed, r.Closed.Outcome)

	_, _, err = zm(t, file, "down", "dev")
	require.NoError(t, err)

	_, _, err = zm(t, file, "rotation", "retry", "dev")
	require.ErrorContains(t, err, "closed resumed; only an open or a lost one is retried")
}

// abort of a rotation whose fresh conversation did work needs --force: the refusal names the conversation it would leave and the
// command that comes back to it, and the success says it again.
func TestRotationAbortOfAFreshConversationThatDidWorkNeedsForce(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	_, _, err := zm(t, file, "up", "dev")
	require.NoError(t, err)
	screenHas(t, file, "dev", "claude ")

	at := time.Now().Add(-2 * time.Hour)
	lines := rotationLinesAt("fresh001", "dev", "restart_at", "context 262k over restart_at 250k", at,
		`"phase":"evidence","what":"session","session":"sid-fresh"`)
	lines = append(lines,
		fmt.Sprintf(`{"id":"fresh001","at":%q,"phase":"closed","outcome":"lost","detail":"it did not accept T-1"}`, at.Add(time.Hour).UTC().Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"id":"fresh001","at":%q,"phase":"evidence","what":"accepted","task":"T-1","by":"dev","session":"sid-fresh"}`, at.Add(time.Hour+time.Minute).UTC().Format(time.RFC3339Nano)))
	rotationRecord(t, file, "dev", lines...)

	_, _, err = zm(t, file, "rotation", "abort", "dev")
	require.ErrorContains(t, err, "its fresh conversation sid-fresh did work (it accepted T-1)")
	assert.ErrorContains(t, err, "zmina rotation abort dev --force")
	assert.ErrorContains(t, err, "up 'dev' --resume='sid-fresh'")

	out, _, perr := zm(t, file, "peek", "dev")
	require.NoError(t, perr)
	assert.NotContains(t, out, "--resume", "nothing was put down")

	out, _, err = zm(t, file, "rotation", "abort", "dev", "--force")
	require.NoError(t, err)
	assert.Contains(t, out, "dev is back on its conversation sid-old (rotation fresh001 aborted)")
	screenHas(t, file, "dev", "--resume sid-old")

	_, _, err = zm(t, file, "rotation", "retry", "dev")
	require.ErrorContains(t, err, "rotation fresh001 was aborted")
}
