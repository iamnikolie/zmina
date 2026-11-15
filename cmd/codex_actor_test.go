package cmd

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/shift"
)

// In a crew that has a seat on the shared Codex daemon, a command with no $ZMINA_ROLE and a Codex thread no seat runs acts as codex-thread,
// not as the owner's hand: what it writes is signed so (note, todo, lesson, inbox fetch), a seat's own thread still acts as the seat, and
// the plain shell, which carries no thread, is the hand. A crew with no such seat is unchanged.
func TestACodexThreadOfNoSeatSignsAsCodexThreadWhereSeatsAreShared(t *testing.T) {
	file := testCrew(t, codexSeats("dev")+"  dev2: {}\n")
	seats := newCodexSeatCrew(t, file, map[string]string{"dev": "thread-1"})

	for who, c := range map[string]struct{ role, thread, want string }{
		"a seat's own thread": {"", "thread-1", "dev"},
		"a thread of no seat": {"", "thread-child", "codex-thread"},
		"the plain shell":     {"", "", "hand"},
		"a role":              {"dev2", "thread-child", "dev2"},
	} {
		_, errOut, err := seats.zm(t, c.role, c.thread, "note", "--if-up=false", "from "+who)
		require.NoError(t, err, errOut)

		assert.Contains(t, dispatcherLines(t, file), c.want+": from "+who, who)

		_, errOut, err = seats.zm(t, c.role, c.thread, "todo", "add", "a question from "+who, "--title", "q")
		require.NoError(t, err, errOut)

		_, errOut, err = seats.zm(t, c.role, c.thread, "lesson", "add", "a rule from "+who, "--why", "it holds")
		require.NoError(t, err, errOut)
	}

	asks, _, err := askStore(mustCrew(t, file)).Scan()
	require.NoError(t, err)

	by := map[string]string{}
	for _, ask := range asks {
		by[strings.TrimPrefix(ask.Text, "a question from ")] = ask.By
	}

	assert.Equal(t, map[string]string{"a seat's own thread": "dev", "a thread of no seat": "codex-thread", "the plain shell": "hand", "a role": "dev2"}, by)

	lessons, err := lessonStore(mustCrew(t, file)).All()
	require.NoError(t, err)

	got := map[string]string{}
	for _, l := range lessons {
		got[strings.TrimPrefix(l.Rule, "a rule from ")] = l.Role
	}

	assert.Equal(t, map[string]string{"a seat's own thread": "dev", "a thread of no seat": "codex-thread", "the plain shell": "hand", "a role": "dev2"}, got)
}

func TestACodexThreadIsTheHandWhereNoSeatIsShared(t *testing.T) {
	file := testCrew(t, "  dev: {}\n  dev2:\n    runtime: codex\n")
	t.Setenv("CODEX_THREAD_ID", "thread-owner")

	_, errOut, err := zm(t, file, "note", "--if-up=false", "from the owner's Codex")
	require.NoError(t, err, errOut)
	assert.Contains(t, dispatcherLines(t, file), "hand: from the owner's Codex")

	_, _, err = zm(t, file, "limits", "release", "claude")
	assert.NotContains(t, errString(err), "only the owner or the dispatcher", "the owner's own Codex acts as the owner")
}

// What only the owner or the dispatcher may do is refused to codex-thread, and the refusal says how the owner acts. The ack of a line is
// the role's own: codex-thread acks none of a seat's, a seat's own thread acks its own.
func TestCodexThreadHoldsNoAuthorityOfTheOwnersAndSaysHowTheOwnerActs(t *testing.T) {
	file := testCrew(t, codexSeats("dev")+"  dev2: {}\n")
	seats := newCodexSeatCrew(t, file, map[string]string{"dev": "thread-1"})

	_, _, err := seats.zm(t, "", "thread-child", "limits", "release", "claude")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only the owner or the dispatcher can lift a hold")
	assert.Contains(t, err.Error(), "acts as codex-thread and not as the owner")
	assert.Contains(t, err.Error(), "run from a plain terminal")

	// The plain shell, and a seat's own thread (which is a role: the dispatcher alone otherwise), are as they were.
	_, _, err = seats.zm(t, "", "", "limits", "release", "claude")
	assert.NotContains(t, errString(err), "only the owner or the dispatcher")

	_, _, err = seats.zm(t, "", "thread-1", "limits", "release", "claude")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "run from a plain terminal", "a seat is a role: no word of the owner's shell")

	// The ack of a line is its reader's own: codex-thread acks none of a seat's, the seat's own thread acks its own.
	a := &app{out: io.Discard, err: io.Discard, crew: file}
	require.NoError(t, a.withState(func(s *shift.Shift) error {
		s.State.Acks = map[string]*shift.Ack{"S-1": {ID: "S-1", Role: "dev", Text: "look"}, "S-2": {ID: "S-2", Role: "dev", Text: "and this"}}

		return nil
	}))

	out, errOut, err := seats.zm(t, "", "thread-child", "ack", "S-1")
	require.NoError(t, err)
	assert.Equal(t, "nothing acked\n", out)
	assert.Contains(t, errOut, "S-1 is dev's line, not codex-thread's: left for dev to ack")

	out, _, err = seats.zm(t, "", "thread-1", "ack", "S-1")
	require.NoError(t, err)
	assert.Equal(t, "acked S-1\n", out)

	out, _, err = seats.zm(t, "", "thread-1", "ack", "--all")
	require.NoError(t, err)
	assert.Equal(t, "acked S-2\n", out)

	// plugin deliver is the ticker's and the owner's: a thread of no seat is not the owner.
	_, _, err = seats.zm(t, "", "thread-child", "plugin", "deliver")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a role (codex-thread) does not run it")
	assert.Contains(t, err.Error(), "run it from a plain terminal")

	_, _, err = seats.zm(t, "", "thread-1", "plugin", "deliver")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a role (dev) does not run it", "a seat's tool is the seat's")
}

func errString(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}
