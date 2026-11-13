package plugin

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func armed(t *testing.T) Armed {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "plugins")

	return Armed{Path: filepath.Join(dir, "pager.armed.json"), Lock: filepath.Join(dir, "pager.armed.lock")}
}

// A press made through a plugin goes once: the first press disarms its mark, a second is refused with its own words, and so is a
// press of a mark that was never told or is for another question.
func TestArmedPressIsOneShot(t *testing.T) {
	a := armed(t)
	at := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	nonce := NewNonce()

	assert.Len(t, nonce, 16)
	assert.NotEqual(t, nonce, NewNonce())

	require.ErrorContains(t, a.Consume(nonce, "dev:ab", at, at), "not armed", "never told")

	require.NoError(t, a.Arm(nonce, "dev:ab", at, at))
	require.ErrorContains(t, a.Consume(nonce, "dev:cd", at, at), "another question", "a card of another dialog")

	require.NoError(t, a.Consume(nonce, "dev:ab", at, at.Add(time.Second)), "the first press")

	err := a.Consume(nonce, "dev:ab", at, at.Add(2*time.Second))
	require.Error(t, err, "a second press, a double tap, a retry")
	assert.ErrorContains(t, err, "used already")
	assert.ErrorContains(t, err, "a press goes once")

	// A press that never happened is given back.
	require.NoError(t, a.Rearm(nonce, at))
	require.NoError(t, a.Consume(nonce, "dev:ab", at, at.Add(3*time.Second)))

	// A mark whose event was never taken is forgotten.
	other := NewNonce()
	require.NoError(t, a.Arm(other, "dev:ab", at, at))
	require.NoError(t, a.Disarm(other, at))
	require.ErrorContains(t, a.Consume(other, "dev:ab", at, at), "not armed")
}

// A card two days old is no card; the marks of a plugin are a small file that does not grow.
func TestArmedMarksExpire(t *testing.T) {
	a := armed(t)
	at := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	old := NewNonce()

	require.NoError(t, a.Arm(old, "dev:ab", at, at))
	require.NoError(t, a.Arm(NewNonce(), "dev:ab", at, at.Add(armedKeep+time.Hour)), "any change prunes")
	require.ErrorContains(t, a.Consume(old, "dev:ab", at, at.Add(armedKeep+2*time.Hour)), "not armed")

	// A file that does not read is no marks at all: nothing is armed, and nothing is pressed.
	require.NoError(t, osWrite(a.Path, "{not json"))
	require.ErrorContains(t, a.Consume(old, "dev:ab", at, at), "not armed")
	require.NoError(t, a.Arm(old, "dev:ab", at, at), "and it is written whole again")
}

// Two presses at once: exactly one goes.
func TestArmedTwoPressesAtOnceGoOnce(t *testing.T) {
	a := armed(t)
	at := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	nonce := NewNonce()

	require.NoError(t, a.Arm(nonce, "dev:ab", at, at))

	var (
		wg   sync.WaitGroup
		wins atomic.Int32
	)

	for range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if a.Consume(nonce, "dev:ab", at, at) == nil {
				wins.Add(1)
			}
		}()
	}

	wg.Wait()
	assert.EqualValues(t, 1, wins.Load())
}

// A card is of one wait: when the question is answered at the terminal and asked again, with the very same words, the old card
// presses nothing of the new wait, though it was never pressed.
func TestArmedMarkIsOfOneWait(t *testing.T) {
	a := armed(t)
	first := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	again := first.Add(5 * time.Minute)
	nonce := NewNonce()

	require.NoError(t, a.Arm(nonce, "dev:ab", first, first))

	err := a.Consume(nonce, "dev:ab", again, again)
	require.Error(t, err, "the same words, a later wait")
	assert.ErrorContains(t, err, "earlier wait")

	require.NoError(t, a.Consume(nonce, "dev:ab", first, first), "and it still serves its own wait: it was not spent by the refusal")
}

// A press spends every card of its wait, whoever pressed: a second card of the wait, another plugin's or the same plugin's, goes
// nowhere while the question is still on the screen; a card of another wait is left alone.
func TestArmedSpendDisarmsTheWait(t *testing.T) {
	a := armed(t)
	first := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	later := first.Add(time.Hour)
	one, two, other := NewNonce(), NewNonce(), NewNonce()

	require.NoError(t, a.Arm(one, "dev:ab", first, first))
	require.NoError(t, a.Arm(two, "dev:ab", first, first))
	require.NoError(t, a.Arm(other, "dev:ab", later, first))

	require.NoError(t, a.Spend("dev:ab", first, first.Add(time.Second)))

	require.ErrorContains(t, a.Consume(one, "dev:ab", first, first.Add(2*time.Second)), "used already")
	require.ErrorContains(t, a.Consume(two, "dev:ab", first, first.Add(2*time.Second)), "used already")
	require.NoError(t, a.Consume(other, "dev:ab", later, first.Add(2*time.Second)), "another wait's card is its own")
}
