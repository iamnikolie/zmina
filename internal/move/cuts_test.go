package move

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

// A step that the beat stops CutLimit times in a row is told once for its phase: the count starts again when a beat gets a piece further
// (another step) or stops none, and what was told stays told for the phase.
func TestCutsTellTheThirdBeatInARowOncePerPhase(t *testing.T) {
	var c Cuts

	for i := 1; i < CutLimit; i++ {
		assert.False(t, c.StepCut("released:0", PhaseReleased), "beat %d", i)
	}

	assert.True(t, c.StepCut("released:0", PhaseReleased), "the third in a row")
	assert.False(t, c.StepCut("released:0", PhaseReleased), "once: not every beat")
	assert.False(t, c.StepCut("released:0", PhaseReleased))

	// A beat that got a piece further starts the count again, and the same phase is not told twice.
	c = Cuts{Cut: 2, CutAt: "released:0", CutTold: PhaseReleased}
	assert.False(t, c.StepCut("released:1", PhaseReleased))
	assert.Equal(t, 1, c.Cut)

	// A beat that stopped no step starts it again too: two, one that fit, and two more tell nothing.
	c = Cuts{}
	c.StepCut("released:0", PhaseReleased)
	c.StepCut("released:0", PhaseReleased)
	c.StepDone()
	assert.False(t, c.StepCut("released:0", PhaseReleased))
	assert.False(t, c.StepCut("released:0", PhaseReleased))
	assert.True(t, c.StepCut("released:0", PhaseReleased))

	// Another phase is told once more.
	c = Cuts{Cut: CutLimit, CutAt: "poll", CutTold: "poll"}
	for i := 1; i < CutLimit; i++ {
		assert.False(t, c.StepCut("take", "take"))
	}

	assert.True(t, c.StepCut("take", "take"))
}

// How far a move is in its phase is the phase and the pieces of the handing over that are done.
func TestAJournalsStepCountsWhatIsDone(t *testing.T) {
	j := Journal{Phase: PhaseReleased, Tasks: []Task{{ID: "T-1"}, {ID: "T-2"}}}
	assert.Equal(t, "released:0", j.Step())

	j.Tasks[0].WIP = "abc"
	assert.Equal(t, "released:1", j.Step())

	j.Tasks[0].Pushed, j.Tasks[0].WIPPushed, j.Tasks[1].Pushed = true, true, true
	assert.Equal(t, "released:4", j.Step())

	j.Phase = PhaseReady
	assert.Equal(t, "ready:4", j.Step())
}

// A journal or a drive written before the count reads as one that was never cut, and one that was not cut writes none of its keys: the
// files the live crews have written stay readable, and an older zmina reads one of these.
func TestJournalAndDriveReadAndWriteCutsOptionally(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}

	old := `{"id":"mv-0123456789ab","role":"dev","from":"alpha","to":"beta","phase":"released","started":"2026-11-01T10:00:00Z","base":"x","after":"y"}`

	var j Journal

	require.NoError(t, json.Unmarshal([]byte(old), &j))
	assert.Zero(t, j.Cuts)
	assert.Equal(t, PhaseReleased, j.Phase)

	require.NoError(t, Save(l, j))

	raw, err := json.Marshal(j)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "cut")

	var d Drive

	require.NoError(t, json.Unmarshal([]byte(`{"id":"mv-0123456789ab","role":"dev","from":"alpha","to":"beta","base":"x","started":"2026-11-01T10:00:00Z"}`), &d))
	assert.Zero(t, d.Cuts)

	raw, err = json.Marshal(d)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "cut")

	// Once counted, they are keys of the record itself, and survive a save and a read.
	j.Cuts = Cuts{Cut: 3, CutAt: "released:1", CutTold: PhaseReleased}
	require.NoError(t, Save(l, j))

	got, ok, err := Open(l, "dev")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, j.Cuts, got.Cuts)

	raw, err = json.Marshal(got)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"cut":3,"cut_at":"released:1","cut_told":"released"`)

	d.Cuts = Cuts{Cut: 3, CutAt: "take", CutTold: "take"}
	require.NoError(t, SaveDrive(l, d))

	gotDrive, ok, err := OpenDrive(l, "dev")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, d.Cuts, gotDrive.Cuts)
}
