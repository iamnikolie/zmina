package shift

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rotViewOf is designer's view.
func rotViewOf(t *testing.T, f fix) View {
	t.Helper()

	for _, v := range f.s.Views() {
		if v.Role == "designer" {
			return v
		}
	}

	require.Fail(t, "no view of designer")

	return View{}
}

// A role's view carries its newest rotation: in flight with where it stands and since when, else how it ended, and what became of
// a lost one. A compact or a clear is no rotation to look at. A role with no record has none, and a look writes nothing.
func TestViewShowsTheRotation(t *testing.T) {
	f := settled(t)
	assert.Nil(t, rotViewOf(t, f).Rotation, "never rotated")

	// In flight: the restart's down is begun.
	f2, _ := restartBegun(t, 0)
	rot := rotViewOf(t, f2).Rotation
	require.NotNil(t, rot)
	assert.True(t, rot.Open)
	assert.Equal(t, PhaseSwitching, rot.Phase)
	assert.Equal(t, KindRestartAt, rot.Kind)
	assert.Equal(t, lastRotation(t, f2, "designer").ID, rot.ID)
	assert.True(t, rot.At.Equal(lastRotation(t, f2, "designer").Prepared), "since it was prepared, not started yet")

	f2.finish(t)
	rot = rotViewOf(t, f2).Rotation
	require.NotNil(t, rot)
	assert.Equal(t, PhaseStarted, rot.Phase)
	assert.True(t, rot.At.Equal(lastRotation(t, f2, "designer").Started))
	assert.Empty(t, rot.Outcome)

	// A compact typed since is not what a look shows.
	f2.s.rotationTrimOpen("designer", KindCompact, "context 300k over 250k")
	assert.Equal(t, lastRotationNotTrim(t, f2).ID, rotViewOf(t, f2).Rotation.ID)

	// Closed: how it ended; a lost one that accepted late, or was put back.
	freshSession(t, f2, true)
	f2.typedAfter("designer", "sid-new", 2)
	f2.s.cardSeen("designer", lastRotationNotTrim(t, f2).Lines[0], SeenTranscript, *f2.now)
	*f2.now = f2.now.Add(rotationAcceptWait + time.Minute)
	f2.idleSince(f2.now.Add(-rotationAcceptWait - 30*time.Second))
	f2.s.Tick()

	rot = rotViewOf(t, f2).Rotation
	require.NotNil(t, rot)
	assert.False(t, rot.Open)
	assert.Equal(t, OutcomeLost, rot.Outcome)
	assert.Empty(t, rot.Then)
	assert.Empty(t, rot.Phase)

	f2.s.By = "designer"
	require.NoError(t, f2.s.AcceptTask("T-1", deliveryOf(t, f2).ID))
	f2.s.By = "hand"
	f2.s.Tick()
	assert.Equal(t, "rebuilt", rotViewOf(t, f2).Rotation.Then)

	// Put back on its old conversation instead.
	f3, _ := freshByHand(t)
	bootedFresh(f3)
	rotID := lastRotation(t, f3, "designer").ID
	*f3.now = f3.now.Add(rotationAcceptWait + 20*time.Minute)
	f3.s.rotationEnd("designer", rotID, OutcomeLost, "it did not accept T-1", nil, true)
	require.NoError(t, abortWith(t, f3, true))
	assert.Equal(t, "aborted", rotViewOf(t, f3).Rotation.Then)
}

// lastRotationNotTrim is the role's newest rotation that is not a compact or a clear.
func lastRotationNotTrim(t *testing.T, f fix) Rotation {
	t.Helper()

	rots := rotationsOn(t, f, "designer")
	for i := len(rots) - 1; i >= 0; i-- {
		if !rots[i].Trim() {
			return rots[i]
		}
	}

	require.Fail(t, "no rotation that is not a trim")

	return Rotation{}
}

// A look reads the end of a record once and writes nothing to it.
func TestViewReadsTheRecordAndWritesNothing(t *testing.T) {
	f, _ := restartBegun(t, 0)
	path := f.s.Crew.Layout().Rotations("designer")

	before, err := os.Stat(path)
	require.NoError(t, err)

	for range 3 {
		require.NotNil(t, rotViewOf(t, f).Rotation)
	}

	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, before.Size(), after.Size())
	assert.True(t, before.ModTime().Equal(after.ModTime()), "nothing was written")

	// A role with no record costs a failed open and nothing more.
	assert.NoFileExists(t, f.s.Crew.Layout().Rotations("harvester"))
	assert.Nil(t, func() *RotationView {
		for _, v := range f.s.Views() {
			if v.Role == "harvester" {
				return v.Rotation
			}
		}

		return nil
	}())
	assert.NoFileExists(t, f.s.Crew.Layout().Rotations("harvester"))
}
