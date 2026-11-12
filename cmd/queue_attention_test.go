package cmd

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

// failedLines writes a role's failed lines, and the line typed into it last, to the crew's state.
func failedLines(t *testing.T, file, role string, sent *shift.Sent, failed ...shift.Failed) {
	t.Helper()

	c, err := crew.Load(file)
	require.NoError(t, err)

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)

	rs := l.Role(role)
	rs.Failed, rs.Sent = failed, sent
	require.NoError(t, l.Save())
}

// queue ls puts what may have reached the role apart from what did not, each under its own words, and lists a line
// typed into the role that has not landed.
func TestQueueLsSeparatesAttentionRequiredFromFailed(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	now := time.Now()

	failedLines(t, file, "dev", &shift.Sent{Line: "[dispatcher] T-3 look at this", At: now.Add(-2 * time.Minute)},
		shift.Failed{Line: "[dispatcher] refused line", Error: "tmux refused", At: now.Add(-time.Hour)},
		shift.Failed{Line: "[dispatcher] maybe there", Error: shift.UnclearTyping + " (tmux: timed out)", At: now.Add(-time.Hour)})

	out, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)

	assert.Regexp(t, `typed +\[dispatcher\] T-3 look at this \(typed into its prompt \d\d-\d\d \d\d:\d\d, 2m ago, not landed yet`, out)
	assert.Regexp(t, `attention required \(may have reached dev; look before retrying.*--unclear.*\):\n +\[dispatcher\] maybe there \(typing broke and may have reached the pane`, out)
	assert.Regexp(t, `failed \(nothing reached dev; .*\):\n +\[dispatcher\] refused line \(tmux refused`, out)
	assert.Less(t, strings.Index(out, "attention required"), strings.Index(out, "failed ("), "the line to look at comes first")
}

// --json carries the kind of each failed line and the line in flight.
func TestQueueLsJSONCarriesTheKindOfEachFailedLine(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	now := time.Now()

	failedLines(t, file, "dev", &shift.Sent{Line: "typed a moment ago", At: now},
		shift.Failed{Line: "refused", Error: "tmux refused", At: now},
		shift.Failed{Line: "maybe there", Error: shift.UnclearEnded, At: now})

	out, _, err := zm(t, file, "queue", "ls", "--json")
	require.NoError(t, err)

	var qs []struct {
		Role   string `json:"role"`
		Sent   *struct{ Line string }
		Failed []struct {
			Line    string `json:"line"`
			Error   string `json:"error"`
			Unclear bool   `json:"unclear"`
		} `json:"failed"`
	}

	require.NoError(t, json.Unmarshal([]byte(out), &qs))
	require.Len(t, qs, 1)
	require.NotNil(t, qs[0].Sent)
	assert.Equal(t, "typed a moment ago", qs[0].Sent.Line)
	require.Len(t, qs[0].Failed, 2)
	assert.False(t, qs[0].Failed[0].Unclear)
	assert.Equal(t, "refused", qs[0].Failed[0].Line)
	assert.True(t, qs[0].Failed[1].Unclear)
	assert.Equal(t, shift.UnclearEnded, qs[0].Failed[1].Error)
}

// queue retry puts back the lines nothing reached and leaves the others, saying how many and why; --unclear puts back
// those too.
func TestQueueRetrySkipsLinesThatNeedAttentionUnlessAsked(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	now := time.Now()

	failedLines(t, file, "dev", nil,
		shift.Failed{Line: "refused", Error: "tmux refused", At: now},
		shift.Failed{Line: "maybe there", Error: shift.UnclearTyping, At: now})

	out, _, err := zm(t, file, "queue", "retry", "dev")
	require.NoError(t, err)
	assert.Contains(t, out, "dev: 1 lines back in the inbox")
	assert.Contains(t, out, "dev: 1 lines left failed: they may have reached the role")
	assert.Contains(t, out, "zmina --crew "+quoteCommandArg(file)+" queue retry 'dev' --unclear")

	out, _, err = zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "inbox    refused")
	assert.Contains(t, out, "attention required")
	assert.NotContains(t, out, "failed (nothing reached")

	out, _, err = zm(t, file, "queue", "retry", "dev", "--unclear")
	require.NoError(t, err)
	assert.Contains(t, out, "dev: 1 lines back in the inbox")
	assert.NotContains(t, out, "left failed")

	out, _, err = zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "inbox    maybe there")
	assert.NotContains(t, out, "attention required")
}

// ls says how many lines of a role need attention, apart from the ones that only failed; --json counts them.
func TestLsSaysHowManyLinesNeedAttention(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	now := time.Now()

	failedLines(t, file, "dev", nil,
		shift.Failed{Line: "refused", Error: "tmux refused", At: now},
		shift.Failed{Line: "maybe there", Error: shift.UnclearTyping, At: now},
		shift.Failed{Line: "never shown", Error: shift.UnclearUnconfirmed, At: now})

	out, _, err := zm(t, file, "ls", "--all")
	require.NoError(t, err)
	assert.Contains(t, out, "2 lines need attention")
	assert.Contains(t, out, "1 failed line")

	out, _, err = zm(t, file, "ls", "--json")
	require.NoError(t, err)

	var views []struct {
		Role      string `json:"role"`
		Failed    int    `json:"failed"`
		Attention int    `json:"attention"`
	}

	require.NoError(t, json.Unmarshal([]byte(out), &views))

	for _, v := range views {
		if v.Role == "dev" {
			assert.Equal(t, 3, v.Failed)
			assert.Equal(t, 2, v.Attention)
		}
	}

	// Only lines that failed: no attention.
	failedLines(t, file, "dev", nil, shift.Failed{Line: "refused", Error: "tmux refused", At: now})

	out, _, err = zm(t, file, "ls", "--all")
	require.NoError(t, err)
	assert.NotContains(t, out, "need attention")
	assert.Contains(t, out, "1 failed line")
}

// doctor counts the two kinds apart.
func TestDoctorSaysHowManyQueuedLinesNeedAttention(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	now := time.Now()

	failedLines(t, file, "dev", nil,
		shift.Failed{Line: "refused", Error: "tmux refused", At: now},
		shift.Failed{Line: "maybe there", Error: shift.UnclearEnded, At: now})

	out, _, _ := zm(t, file, "doctor")
	assert.Regexp(t, `warn +state +1 queued lines need attention, they may have reached their role`, out)
	assert.Regexp(t, `warn +state +1 queued lines could not be typed`, out)
}

// The cockpit does not call a line that may have reached the role failed: it is unconfirmed, which the owner can look
// at and set aside; a line nothing reached is failed.
func TestChatSnapshotCallsAnUnclearLineUnconfirmedNotFailed(t *testing.T) {
	uiHome(t)

	a := &app{out: io.Discard, err: io.Discard}
	unclear, plain := "aaaaaaaa-1234-1234-1234-123456789abc", "bbbbbbbb-1234-1234-1234-123456789abc"

	_, err := a.localChatSend("default", "dev", unclear, "maybe delivered")
	require.NoError(t, err)
	_, err = a.localChatSend("default", "dev", plain, "refused")
	require.NoError(t, err)

	c, err := a.crewApp("default")
	require.NoError(t, err)
	require.NoError(t, c.withState(func(s *shift.Shift) error {
		rs := s.State.Role("dev")
		rs.LastSession, rs.LastRuntime = "session", "claude"
		rs.ChatReceipts[0].Session, rs.ChatReceipts[1].Session = "session", "session"
		rs.Failed = []shift.Failed{
			{Line: rs.Inbox[0], Error: shift.UnclearTyping, At: s.Now()},
			{Line: rs.Inbox[1], Error: "tmux refused", At: s.Now()},
		}
		rs.Inbox = nil

		return nil
	}))

	snapshot, err := a.localChatRead("default", "dev")
	require.NoError(t, err)
	require.Len(t, snapshot.Pending, 2)
	assert.Equal(t, "unconfirmed", snapshot.Pending[0].State)
	assert.Equal(t, "failed", snapshot.Pending[1].State)

	_, err = a.localChatAction("default", "dev", "dismiss:"+unclear)
	require.NoError(t, err, "the owner can set a line aside that may have reached the role")

	// Looked at and set aside: it no longer counts as a line that needs attention; the one that only failed stays.
	require.NoError(t, c.readState(func(s *shift.Shift) error {
		failed := s.State.Role("dev").Failed
		require.Len(t, failed, 1)
		require.Equal(t, "tmux refused", failed[0].Error)
		return nil
	}))
}
