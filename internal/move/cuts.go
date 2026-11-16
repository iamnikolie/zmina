package move

import "strconv"

// CutLimit is how many beats in a row may stop the same step of a move for want of time before it is told: a step that takes longer
// than the beat gives it is started again from the beginning at each beat, so it never ends by the beat, and nothing else would say so.
const CutLimit = 3

// Cuts is what a move's record keeps of the beats of the ticker that stopped a step of it for want of time (the beat gives all the moves
// of a crew one budget). It is embedded in the journal and in the owner's record of a drive, as their own keys, which are left out
// while they are empty, so a record from before them reads as one that was never cut.
type Cuts struct {
	// Cut is how many beats in a row stopped the same step.
	Cut int `json:"cut,omitempty"`
	// CutAt names that step: the phase and how much of it was done, so that a beat that got further than the one before starts the count again.
	CutAt string `json:"cut_at,omitempty"`
	// CutTold is the phase the owner was told of it in: once for each phase, not every beat.
	CutTold string `json:"cut_told,omitempty"`
}

// StepCut notes a beat that stopped the step named at (a phase, and how much of it was done) in the phase, and reports whether it is
// time to say so: the CutLimit-th beat in a row at the same step, and not said in this phase already.
func (c *Cuts) StepCut(at, phase string) bool {
	if c.CutAt != at {
		c.Cut, c.CutAt = 0, at
	}

	c.Cut++

	if c.Cut < CutLimit || c.CutTold == phase {
		return false
	}

	c.CutTold = phase

	return true
}

// StepDone notes a beat that stopped no step: the count starts again. What was said stays said for its phase.
func (c *Cuts) StepDone() { c.Cut, c.CutAt = 0, "" }

// Step is how far the move is in its phase: the phase, and how many pieces of the handing over are done in it (a task's uncommitted
// work committed, its branch pushed, its work pushed). A beat that got a piece further stopped another step than the beat before it.
func (j Journal) Step() string {
	done := 0

	for _, t := range j.Tasks {
		if t.WIP != "" {
			done++
		}

		if t.Pushed {
			done++
		}

		if t.WIPPushed {
			done++
		}
	}

	return j.Phase + ":" + strconv.Itoa(done)
}
