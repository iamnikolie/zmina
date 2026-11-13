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

	require.ErrorContains(t, a.Consume(nonce, "dev:ab", at), "not armed", "never told")

	require.NoError(t, a.Arm(nonce, "dev:ab", at))
	require.ErrorContains(t, a.Consume(nonce, "dev:cd", at), "another question", "a card of another dialog")

	require.NoError(t, a.Consume(nonce, "dev:ab", at.Add(time.Second)), "the first press")

	err := a.Consume(nonce, "dev:ab", at.Add(2*time.Second))
	require.Error(t, err, "a second press, a double tap, a retry")
	assert.ErrorContains(t, err, "used already")
	assert.ErrorContains(t, err, "a press goes once")

	// A press that never happened is given back.
	require.NoError(t, a.Rearm(nonce, at))
	require.NoError(t, a.Consume(nonce, "dev:ab", at.Add(3*time.Second)))

	// A mark whose event was never taken is forgotten.
	other := NewNonce()
	require.NoError(t, a.Arm(other, "dev:ab", at))
	require.NoError(t, a.Disarm(other, at))
	require.ErrorContains(t, a.Consume(other, "dev:ab", at), "not armed")
}

// A card two days old is no card; the marks of a plugin are a small file that does not grow.
func TestArmedMarksExpire(t *testing.T) {
	a := armed(t)
	at := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	old := NewNonce()

	require.NoError(t, a.Arm(old, "dev:ab", at))
	require.NoError(t, a.Arm(NewNonce(), "dev:ab", at.Add(armedKeep+time.Hour)), "any change prunes")
	require.ErrorContains(t, a.Consume(old, "dev:ab", at.Add(armedKeep+2*time.Hour)), "not armed")

	// A file that does not read is no marks at all: nothing is armed, and nothing is pressed.
	require.NoError(t, osWrite(a.Path, "{not json"))
	require.ErrorContains(t, a.Consume(old, "dev:ab", at), "not armed")
	require.NoError(t, a.Arm(old, "dev:ab", at), "and it is written whole again")
}

// Two presses at once: exactly one goes.
func TestArmedTwoPressesAtOnceGoOnce(t *testing.T) {
	a := armed(t)
	at := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	nonce := NewNonce()

	require.NoError(t, a.Arm(nonce, "dev:ab", at))

	var (
		wg   sync.WaitGroup
		wins atomic.Int32
	)

	for range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if a.Consume(nonce, "dev:ab", at) == nil {
				wins.Add(1)
			}
		}()
	}

	wg.Wait()
	assert.EqualValues(t, 1, wins.Load())
}
