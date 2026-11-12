package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/shift"
)

// courierRig is a courier loop over a fake clock, fake stats and a fake pass: no real time, tmux, files or home.
type courierRig struct {
	t     *testing.T
	start time.Time
	now   time.Time
	tick  int

	// What the stats say now; a test changes them, or bump does.
	state, crewFile, exe fileSig
	stateStats           int

	reloads   int
	reloadErr error

	// passes are the times (since the start) a pass ran, asked the times each role was asked.
	passes []time.Duration
	asked  map[string][]time.Duration
	pass   func(try func(role string) bool) (courierPass, error)

	events, warns []string
	// onTick runs after each sleep, the clock already moved.
	onTick func(r *courierRig)
	cancel context.CancelFunc
}

func newCourierRig(t *testing.T) *courierRig {
	t.Helper()

	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	return &courierRig{
		t: t, start: now, now: now, asked: map[string][]time.Duration{},
		state:    fileSig{present: true, mtime: 1, size: 100, inode: 7},
		crewFile: fileSig{present: true, mtime: 1, size: 50, inode: 8},
		exe:      fileSig{present: true, mtime: 1, size: 9000, inode: 9},
	}
}

func (r *courierRig) since() time.Duration { return r.now.Sub(r.start) }

// bump is a save of state.json by someone: a new time and inode.
func (r *courierRig) bump() { r.state.mtime++; r.state.inode++ }

// over makes the pass over the roles waiting names now: each one try allows is asked, and ask says whether asking it
// changed the state (which, as a real pass's save does, changes state.json's stat).
func (r *courierRig) over(waiting func() []string, ask func(role string) bool) {
	r.pass = func(try func(role string) bool) (courierPass, error) {
		res := courierPass{Waiting: waiting()}

		for _, name := range res.Waiting {
			if !try(name) {
				continue
			}

			r.asked[name] = append(r.asked[name], r.since())

			changed := ask != nil && ask(name)
			if changed {
				r.bump()
			}

			res.Tried = append(res.Tried, shift.RoleDelivery{Role: name, Changed: changed})
		}

		return res, nil
	}
}

func (r *courierRig) named(names ...string) func() []string { return func() []string { return names } }

// loop is the loop over the rig; life is its max life.
func (r *courierRig) loop(life time.Duration) *courierLoop {
	return &courierLoop{
		Poll: 250 * time.Millisecond, MaxLife: life,
		Now: func() time.Time { return r.now },
		Sleep: func(ctx context.Context, d time.Duration) error {
			r.tick++
			r.now = r.now.Add(d)

			if r.onTick != nil {
				r.onTick(r)
			}

			return ctx.Err()
		},
		State: func() (fileSig, error) { r.stateStats++; return r.state, nil },
		Crew:  func() (fileSig, error) { return r.crewFile, nil },
		Exe:   func() (fileSig, error) { return r.exe, nil },
		Reload: func() error {
			r.reloads++

			return r.reloadErr
		},
		Pass: func(try func(role string) bool) (courierPass, error) {
			r.passes = append(r.passes, r.since())

			if r.pass == nil {
				return courierPass{}, nil
			}

			return r.pass(try)
		},
		Event: func(event, detail string) { r.events = append(r.events, event+" "+detail) },
		Warn:  func(msg string) { r.warns = append(r.warns, msg) },
	}
}

// run runs the loop to its end, which a max life of life makes the test's own clock.
func (r *courierRig) run(life time.Duration) error {
	r.t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel

	defer cancel()

	return r.loop(life).Run(ctx)
}

func secs(f ...float64) []time.Duration {
	out := make([]time.Duration, len(f))
	for i, s := range f {
		out[i] = time.Duration(s * float64(time.Second))
	}

	return out
}

// While state.json stays as it was and no role waits on a timer, the courier looks at it and does nothing: one pass at
// the start, then not a pass, not a crew file read, not an event, however many looks.
func TestCourierDoesNothingWhileNothingChanges(t *testing.T) {
	r := newCourierRig(t)
	r.over(r.named(), nil)

	require.NoError(t, r.run(30*time.Second))

	assert.Equal(t, secs(0), r.passes, "the look at the start, and no other")
	assert.Equal(t, 1, r.reloads, "the crew file is read at the start only")
	assert.Equal(t, 120, r.tick)
	assert.Equal(t, r.tick, r.stateStats, "one stat of the state at each look")
	require.Len(t, r.events, 2, "start and stop; nothing per look or per empty pass")
	assert.Equal(t, "courier-start poll 250ms, max life 30s", r.events[0])
	assert.Equal(t, "courier-stop max life 30s reached", r.events[1])
	assert.Empty(t, r.warns)
}

// While a pass says a queue is held by a time gate, the courier looks again every gatedLook though state.json stays as
// it was; once a pass says none is, it stops.
func TestCourierLooksAgainWhileAQueueIsGated(t *testing.T) {
	r := newCourierRig(t)
	r.pass = func(func(string) bool) (courierPass, error) {
		return courierPass{Gated: r.since() < 12*time.Second}, nil
	}

	require.NoError(t, r.run(40*time.Second))

	assert.Equal(t, secs(0, 5, 10, 15), r.passes)
}

// A change of state.json (its time, its inode, its size) is a pass at the next look; an own save is one too.
func TestCourierPassesWhenTheStateChanges(t *testing.T) {
	r := newCourierRig(t)
	r.over(r.named(), nil)
	r.onTick = func(r *courierRig) {
		switch r.tick {
		case 10:
			r.state.mtime++
		case 20:
			r.state.size++
		case 30:
			r.state.inode++
		}
	}

	require.NoError(t, r.run(20*time.Second))
	assert.Equal(t, secs(0, 2.5, 5, 7.5), r.passes, "a pass at the look after each change, none between")

	// What a pass saves is a change as well: the courier looks at once whether the line it gave freed the next.
	r = newCourierRig(t)
	waiting := true
	r.over(func() []string {
		if waiting {
			return []string{"dev"}
		}

		return nil
	}, func(string) bool { waiting = false; return true })

	require.NoError(t, r.run(10*time.Second))
	assert.Equal(t, secs(0, 0.25), r.passes, "its own save is seen at the next look, which finds nothing more")
	assert.Len(t, r.events, 2)
}

// A role whose try was slow and changed nothing waits ten times what the try took, not the second its backoff says: a
// tmux that takes its whole timeout must not have the courier hold the state lock most of the time.
func TestCourierLeavesARoleWhoseTryWasSlowForLonger(t *testing.T) {
	r := newCourierRig(t)
	r.pass = func(try func(string) bool) (courierPass, error) {
		res := courierPass{Waiting: []string{"dev"}}
		if try("dev") {
			r.asked["dev"] = append(r.asked["dev"], r.since())
			res.Tried = []shift.RoleDelivery{{Role: "dev", Took: 20 * time.Second}}
		}

		return res, nil
	}

	require.NoError(t, r.run(700*time.Second))
	assert.Equal(t, secs(0, 200, 400, 600), r.asked["dev"])
}

// A role that is waiting and cannot take its line is asked again after 1 s, 2 s, 4 s, 8 s and then every 15 s, however
// often state.json changes meanwhile: a pass that runs for the change leaves the role alone until its wait is over.
func TestCourierAsksABlockedRoleAfterABackoffWhateverElseChanges(t *testing.T) {
	r := newCourierRig(t)
	r.over(r.named("dev", "qa"), func(role string) bool { return false })
	r.onTick = func(r *courierRig) { r.bump() } // other activity changes the file at every look

	require.NoError(t, r.run(60*time.Second))

	assert.Len(t, r.passes, 240, "a pass at every look: the file changed at every look")
	assert.Equal(t, secs(0, 1, 3, 7, 15, 30, 45), r.asked["dev"])
	assert.Equal(t, r.asked["dev"], r.asked["qa"], "each role has its own wait, the same here")
}

// A wait starts over when the role is no longer named by Waiting (its line went some other way, or it was dropped) and
// is named again.
func TestCourierForgetsTheWaitOfARoleThatIsNotWaiting(t *testing.T) {
	r := newCourierRig(t)
	r.over(func() []string {
		if s := r.since(); s >= 20*time.Second && s < 21*time.Second {
			return nil
		}

		return []string{"dev"}
	}, func(string) bool { return false })
	r.onTick = func(r *courierRig) { r.bump() }

	require.NoError(t, r.run(60*time.Second))

	assert.Equal(t, secs(0, 1, 3, 7, 15, 21, 22, 24, 28, 36, 51), r.asked["dev"],
		"asked at once when named again, then from one second up")
}

// A role whose line went is asked again at the next look, and its wait starts over at one second.
func TestCourierResetsTheWaitWhenTheRoleChangedTheState(t *testing.T) {
	r := newCourierRig(t)
	r.over(r.named("dev"), func(string) bool { return r.since() == 7*time.Second })

	require.NoError(t, r.run(60*time.Second))

	assert.Equal(t, secs(0, 1, 3, 7, 7.25, 8.25, 10.25, 14.25, 22.25, 37.25, 52.25), r.asked["dev"],
		"the pass at 7 s gave its line: asked again at the next look, then 1 s, 2 s, 4 s, 8 s, 15 s")
}

// A role that changes the state at every pass without end is a bug the courier must not turn into a rewrite of
// state.json at every look: past spinRun passes in a row it waits like a role that changed nothing.
func TestCourierDoesNotSpinOnARoleThatKeepsChangingTheState(t *testing.T) {
	r := newCourierRig(t)
	r.over(r.named("dev"), func(string) bool { return true })

	require.NoError(t, r.run(60*time.Second))

	require.Len(t, r.asked["dev"], 14, "the 8 passes in a row, then the wait doubles: not 240")
	assert.Equal(t, secs(0, 0.25, 0.5, 0.75, 1, 1.25, 1.5, 1.75, 2.75, 4.75, 8.75, 16.75, 31.75, 46.75), r.asked["dev"])
}

// A lock another zmina holds ends the pass without a word, and the next look tries again whether or not the file
// changed since; the roles' waits are kept.
func TestCourierSkipsAHeldLockAndTriesAtTheNextLook(t *testing.T) {
	held := func(r *courierRig) bool { return r.since() >= time.Second && r.since() < 2*time.Second }

	t.Run("a change called for the pass", func(t *testing.T) {
		r := newCourierRig(t)
		r.onTick = func(r *courierRig) {
			if r.tick == 4 {
				r.bump()
			}
		}
		r.pass = func(func(string) bool) (courierPass, error) { return courierPass{LockHeld: held(r)}, nil }

		require.NoError(t, r.run(10*time.Second))
		assert.Equal(t, secs(0, 1, 1.25, 1.5, 1.75, 2), r.passes, "every look while it is held, once after, then quiet")
		assert.Empty(t, r.warns, "nothing said")
		assert.Len(t, r.events, 2, "nothing logged")
	})

	t.Run("a wait that is over", func(t *testing.T) {
		r := newCourierRig(t)
		r.over(r.named("dev"), func(string) bool { return false })

		inner := r.pass
		r.pass = func(try func(string) bool) (courierPass, error) {
			if held(r) {
				return courierPass{LockHeld: true}, nil
			}

			return inner(try)
		}

		require.NoError(t, r.run(10*time.Second))
		assert.Equal(t, secs(0, 2, 4, 8), r.asked["dev"], "due at 1 s, asked when the lock let go, and the wait went on from there")
		assert.Empty(t, r.warns)
		assert.Len(t, r.events, 2)
	})
}

// A pass that fails backs the whole courier off, 5 s doubling to a minute, whatever the file does; a pass that does not
// fail ends it.
func TestCourierBacksOffAfterAFailedPassAndResets(t *testing.T) {
	r := newCourierRig(t)
	r.onTick = func(r *courierRig) { r.bump() }
	r.pass = func(func(string) bool) (courierPass, error) { return courierPass{}, errors.New("not saved: disk full") }

	require.NoError(t, r.run(1300*time.Second))
	assert.Equal(t, secs(0, 60, 180, 420, 720, 1020), r.passes, "a minute, 2, 4, then 5 minutes: never under the beat's")
	require.Len(t, r.warns, 6)
	assert.Equal(t, "not saved: disk full (next try in 1m0s)", r.warns[0])
	assert.Equal(t, "not saved: disk full (next try in 2m0s)", r.warns[1])
	assert.Equal(t, "not saved: disk full (next try in 5m0s)", r.warns[5])

	// The pass that works resets it: the next failure waits a minute again, not four.
	r = newCourierRig(t)
	r.onTick = func(r *courierRig) { r.bump() }

	calls := 0
	r.pass = func(func(string) bool) (courierPass, error) {
		calls++
		if calls == 3 {
			return courierPass{}, nil
		}

		return courierPass{}, errors.New("not saved: disk full")
	}

	require.NoError(t, r.run(250*time.Second))
	assert.Equal(t, secs(0, 60, 180, 180.25, 240.25), r.passes)
}

// A state that does not read is a pass that failed like any other: the courier waits, and looks again after the wait
// even though the file did not change, for it may have been put right in a way that leaves its stat as it was.
func TestCourierKeepsGoingOnAStateThatDoesNotRead(t *testing.T) {
	r := newCourierRig(t)
	r.pass = func(func(string) bool) (courierPass, error) {
		return courierPass{}, fmt.Errorf("x: %w (bad); nothing was changed", shift.ErrStateBroken)
	}

	require.NoError(t, r.run(200*time.Second))
	assert.Equal(t, secs(0, 60, 180), r.passes, "tried again after the wait even though the file did not change")
	assert.Len(t, r.warns, 3)
}

// A state a newer zmina wrote is not read, and not written over: the courier ends, once, with the error.
func TestCourierEndsOnAStateFromANewerZmina(t *testing.T) {
	r := newCourierRig(t)
	r.onTick = func(r *courierRig) { r.bump() }

	calls := 0
	r.pass = func(func(string) bool) (courierPass, error) {
		if calls++; calls == 3 {
			return courierPass{}, fmt.Errorf("state.json: %w (version 9); upgrade zmina", shift.ErrStateFuture)
		}

		return courierPass{}, nil
	}

	err := r.run(time.Hour)
	require.ErrorIs(t, err, shift.ErrStateFuture)
	assert.Equal(t, secs(0, 0.25, 0.5), r.passes, "no pass after it")
	require.Len(t, r.events, 2)
	assert.Equal(t, "courier-stop state written by a newer zmina", r.events[1])
	assert.Empty(t, r.warns, "the error is the exit's, not a warning too")
}

// The zmina on disk is not the one the courier started from: it ends cleanly, so the next one runs the new code.
func TestCourierEndsWhenTheBinaryChanges(t *testing.T) {
	for name, change := range map[string]func(r *courierRig){
		"a new time":  func(r *courierRig) { r.exe.mtime++ },
		"a new size":  func(r *courierRig) { r.exe.size++ },
		"a new inode": func(r *courierRig) { r.exe.inode++ },
		"gone":        func(r *courierRig) { r.exe = fileSig{} },
	} {
		t.Run(name, func(t *testing.T) {
			r := newCourierRig(t)
			r.over(r.named(), nil)
			r.onTick = func(r *courierRig) {
				if r.tick == 8 {
					change(r)
				}
			}

			require.NoError(t, r.run(time.Hour), "a clean exit")
			assert.Equal(t, 8*250*time.Millisecond, r.since())
			require.Len(t, r.events, 2)
			assert.Equal(t, "courier-stop the zmina executable changed on disk", r.events[1])
		})
	}

	t.Run("not known: not watched", func(t *testing.T) {
		r := newCourierRig(t)
		loop := r.loop(10 * time.Second)
		loop.Exe = nil
		r.onTick = func(r *courierRig) { r.exe.mtime++ }

		require.NoError(t, loop.Run(context.Background()))
		assert.Equal(t, 10*time.Second, r.since())
	})

	t.Run("a stat that fails says nothing", func(t *testing.T) {
		r := newCourierRig(t)
		loop := r.loop(10 * time.Second)
		loop.Exe = func() (fileSig, error) { return fileSig{}, errors.New("permission denied") }

		require.NoError(t, loop.Run(context.Background()))
		assert.Equal(t, 10*time.Second, r.since())
	})
}

// The life is spent: a clean end at that time, logged once. No life given (0) is none spent.
func TestCourierEndsWhenItsLifeIsSpent(t *testing.T) {
	r := newCourierRig(t)
	r.over(r.named(), nil)

	require.NoError(t, r.run(90*time.Second))
	assert.Equal(t, 90*time.Second, r.since())
	assert.Equal(t, "courier-stop max life 1m30s reached", r.events[len(r.events)-1])

	r = newCourierRig(t)
	r.over(r.named(), nil)
	r.onTick = func(r *courierRig) {
		if r.tick == 40000 { // 2 h 46 min of looks
			r.cancel()
		}
	}

	require.NoError(t, r.run(0))
	assert.Equal(t, 40000*250*time.Millisecond, r.since(), "no limit: it lived until it was told")
	assert.Equal(t, "courier-stop interrupted", r.events[len(r.events)-1])
	assert.Equal(t, "courier-start poll 250ms, max life none", r.events[0])
}

// A signal (the command's context ending) is a clean end, logged once; so is one that comes before the first look.
func TestCourierEndsWhenTold(t *testing.T) {
	r := newCourierRig(t)
	r.over(r.named(), nil)
	r.onTick = func(r *courierRig) {
		if r.tick == 6 {
			r.cancel()
		}
	}

	require.NoError(t, r.run(time.Hour))
	assert.Equal(t, 6*250*time.Millisecond, r.since())
	require.Len(t, r.events, 2)
	assert.Equal(t, "courier-stop interrupted", r.events[1])

	r = newCourierRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, r.loop(time.Hour).Run(ctx))
	assert.Empty(t, r.passes, "told before it began: not a pass")
	require.Len(t, r.events, 2)
	assert.Equal(t, "courier-stop interrupted", r.events[1])
}

// The crew file is read when its stat changed, not before: not by the passes, not by the looks. One that does not
// read is said once, kept to the last good one and left until it changes again; a read that works is a pass owed.
func TestCourierReadsTheCrewFileWhenItChanges(t *testing.T) {
	r := newCourierRig(t)
	r.over(r.named(), nil)
	r.onTick = func(r *courierRig) {
		switch r.tick {
		case 5:
			r.bump()
		case 20:
			r.crewFile.mtime++
		case 60:
			r.crewFile.size++
			r.reloadErr = errors.New("crew: roles: not a map")
		case 100:
			r.crewFile.size++
			r.reloadErr = nil
		}
	}

	var reloadsAt []int

	loop := r.loop(40 * time.Second)
	inner := loop.Reload
	loop.Reload = func() error { reloadsAt = append(reloadsAt, r.tick); return inner() }

	require.NoError(t, loop.Run(context.Background()))
	assert.Equal(t, []int{0, 20, 60, 100}, reloadsAt, "at the start and at each change of the file, never between")
	assert.Equal(t, secs(0, 1.25, 5, 25), r.passes, "a state change, a crew file read that worked (twice); not the one that failed")
	require.Len(t, r.warns, 1, "said once")
	assert.Contains(t, r.warns[0], "not a map")
	assert.Contains(t, r.warns[0], "keeps the one before")
}

// A crew file that does not read at the start is the command's error, with nothing logged: there is no courier yet.
func TestCourierDoesNotStartOnACrewFileThatDoesNotRead(t *testing.T) {
	r := newCourierRig(t)
	r.reloadErr = errors.New("crew: no such role")

	require.ErrorContains(t, r.run(time.Hour), "no such role")
	assert.Empty(t, r.events)
	assert.Empty(t, r.passes)
}

// courierOver is the real courier loop over a crew file and the fakes of nowRoles (tmux, registry, inbox socket), on a
// fake clock: the stats, the lock, the state and the crew file are the real ones.
type courierOver struct {
	loop *courierLoop
	out  *bytes.Buffer
	errb *bytes.Buffer
	// calls is what the passes asked of tmux and the registry.
	calls *passCalls
	now   time.Time
	tick  int
	// onTick runs after each sleep, the clock moved.
	onTick func(tick int)
}

func (n *nowRoles) courier(t *testing.T, file string, life time.Duration) *courierOver {
	t.Helper()

	c := &courierOver{out: &bytes.Buffer{}, errb: &bytes.Buffer{}, calls: &passCalls{}, now: time.Now()}
	a := &app{out: c.out, err: c.errb, crew: file, verb: "_courier", shiftSeams: func(s *shift.Shift) {
		s.Tmux, s.Reg, s.Peer = countedTmux{c: c.calls}, countedReg{nowReg{n}, c.calls}, nowPeer{n}
	}}

	path, err := filepath.Abs(file)
	require.NoError(t, err)

	c.loop = a.newCourierLoop(path, 250*time.Millisecond, life)
	c.loop.Now = func() time.Time { return c.now }
	c.loop.Sleep = func(ctx context.Context, d time.Duration) error {
		c.tick++
		c.now = c.now.Add(d)

		if c.onTick != nil {
			c.onTick(c.tick)
		}

		return ctx.Err()
	}

	return c
}

func (c *courierOver) run(t *testing.T) error {
	t.Helper()

	return c.loop.Run(context.Background())
}

// eventsOf is the names of the events in the crew's log, in order.
func eventsOf(t *testing.T, file string) []string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "events.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	require.NoError(t, err)

	var out []string

	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e struct{ Event string }
		if line != "" {
			require.NoError(t, json.Unmarshal([]byte(line), &e))
			out = append(out, e.Event)
		}
	}

	return out
}

// The real pass: a role that cannot take its line is asked after a wait that grows, the line goes the first time the
// role can take it, and once nothing waits the courier asks tmux and the registry nothing, however long it looks. It is
// no beat: nothing is started, no role is marked up, the beat's stamp stays as it was.
func TestCourierGivesALineWhenTheRoleCanTakeItAndAsksNothingAfter(t *testing.T) {
	file := testCrew(t, "  dev:\n  qa:\n")
	n := &nowRoles{status: "idle"}
	beat := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
	n.up(t, file, beat)
	n.queueFor(t, file, "[dispatcher] look at T-1")

	// A line for a role that is down waits for the beat, which starts roles; the courier never does.
	_, err := n.zm(t, file, "send", "--when-idle", "qa", "for the beat")
	require.NoError(t, err)

	n.status = "busy"

	c := n.courier(t, file, 30*time.Second)
	lens := []int{0}
	c.onTick = func(tick int) {
		lens = append(lens, len(c.calls.list))

		if tick == 6 {
			n.status = "idle" // at 1.5 s, between its first two asks
		}
	}

	require.NoError(t, c.run(t))

	// Looks at which the pass asked tmux or the registry: at the start, when the 1 s wait was over, and when the 3 s
	// wait was over and the role took its line; never between, never after.
	var asked []int

	for j := 0; j+1 < len(lens); j++ {
		if lens[j+1] != lens[j] {
			asked = append(asked, j)
		}
	}

	assert.Equal(t, []int{0, 4, 12}, asked)

	require.Len(t, n.posts, 1)
	assert.Contains(t, n.posts[0], "[dispatcher] look at T-1")

	st := stateOf(t, file)
	assert.Empty(t, st.Role("dev").Inbox)
	assert.Len(t, st.Role("dev").Posted, 1)
	assert.True(t, st.Role("dev").Up)
	assert.Equal(t, beat, st.LastTick.UTC(), "no beat")
	assert.False(t, st.Role("qa").Up, "no role started")
	assert.Equal(t, []string{"for the beat"}, st.Role("qa").Inbox[:1], "a line for a role that is down stays for the beat")

	evs := eventsOf(t, file)
	assert.Equal(t, 1, countOf(evs, "courier-start"))
	assert.Equal(t, 1, countOf(evs, "courier-stop"))
	assert.Empty(t, c.errb.String())

	// What the pass logged between the courier's start and stop is by the courier, not by the ticker: a line given
	// between beats reads as such in zmina log.
	var passEvents []eventBy

	inside := false

	for _, e := range eventsBy(t, file) {
		switch {
		case e.name == "courier-start":
			inside = true
		case e.name == "courier-stop":
			inside = false
		case inside:
			passEvents = append(passEvents, e)
		}
	}

	require.NotEmpty(t, passEvents, "the line that went is one of them")

	for _, e := range passEvents {
		assert.Equal(t, "courier", e.by, e.name)
	}

	for _, e := range eventsBy(t, file) {
		if strings.HasPrefix(e.name, "courier-") {
			assert.Equal(t, "courier", e.by, e.name)
		}
	}
}

// eventBy is an event's name and who it is by, with when it was logged.
type eventBy struct {
	name, by string
	at       time.Time
}

// eventsBy reads the crew's event log for who each event is by.
func eventsBy(t *testing.T, file string) []eventBy {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "events.jsonl"))
	require.NoError(t, err)

	var out []eventBy

	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e struct {
			Event, By string
			TS        time.Time
		}

		require.NoError(t, json.Unmarshal([]byte(line), &e))
		out = append(out, eventBy{name: e.Event, by: e.By, at: e.TS})
	}

	return out
}

func countOf(list []string, s string) int {
	n := 0

	for _, x := range list {
		if x == s {
			n++
		}
	}

	return n
}

// A lock another zmina holds (the beat, a command) is no wait and no word: the courier looks at the next poll, and the
// line goes when the lock is free again, with nothing logged for the time it was held.
func TestCourierLeavesAHeldLockAlone(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	n := &nowRoles{status: "idle"}
	n.up(t, file, time.Time{})
	n.queueFor(t, file, "[dispatcher] hi")

	dir := filepath.Dir(file)

	// A courier that waited for the lock would give up with an error after this, and the test would say so.
	old := shift.LockWait
	shift.LockWait = 300 * time.Millisecond

	t.Cleanup(func() { shift.LockWait = old })

	holder, err := shift.LockAs(dir, shift.LockOpts{Verb: "tick"})
	require.NoError(t, err)

	c := n.courier(t, file, 5*time.Second)

	var atFirstLook map[string]string

	c.onTick = func(tick int) {
		switch tick {
		case 1:
			atFirstLook = kept(t, dir)
		case 3:
			assert.Empty(t, n.posts, "held until now")
			assert.Empty(t, c.calls.list, "nothing asked while it was held")
			assert.Equal(t, atFirstLook, kept(t, dir), "and nothing written, not an event either")

			holder.Release()
		}
	}

	require.NoError(t, c.run(t))

	require.Len(t, n.posts, 1, "given at the look after the lock let go")
	assert.NotContains(t, eventsOf(t, file), "lock-wait")
	assert.NotContains(t, eventsOf(t, file), "lock-held")
	assert.Empty(t, c.errb.String())
}

// A state that cannot be saved after a line went fails the pass; the courier says so and waits 5 s, not one poll, before
// it tries again, however busy the file is.
func TestCourierBacksOffWhenTheStateCannotBeSaved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a directory cannot be made unwritable for root")
	}

	file := testCrew(t, "  dev:\n")
	n := &nowRoles{status: "idle"}
	n.up(t, file, time.Time{})
	n.queueFor(t, file, "[dispatcher] hi")

	dir := filepath.Dir(file)
	require.NoError(t, os.Chmod(dir, 0o500))

	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	c := n.courier(t, file, 4*time.Second)
	require.NoError(t, c.run(t))

	assert.Len(t, n.posts, 1, "tried once in the first four seconds: the line is not given at every look")
	assert.Equal(t, 1, strings.Count(c.errb.String(), "next try in 1m0s"), c.errb.String())
}

// The crew file is read at the start and again only when it changed; what it warns of is said then and not at every pass.
func TestCourierSaysTheCrewFilesWarningsOncePerRead(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	n := &nowRoles{status: "busy"}
	n.up(t, file, time.Time{})

	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, append(raw, []byte("no_such_key: 1\n")...), 0o600))

	c := n.courier(t, file, 20*time.Second)
	c.onTick = func(tick int) {
		switch {
		case tick%8 == 0 && tick <= 24:
			// Another command's save: a pass for the courier, which finds a role it cannot give to.
			_, err := n.zm(t, file, "send", "--when-idle", "dev", fmt.Sprintf("line %d", tick))
			require.NoError(t, err)
		case tick == 60:
			// The file is edited: it is read again.
			require.NoError(t, os.WriteFile(file, append(raw, []byte("no_such_key: 2\n")...), 0o600))
		}
	}

	require.NoError(t, c.run(t))

	assert.Equal(t, 2, strings.Count(c.errb.String(), "no_such_key"), "at the start and after the edit, not at the passes between: %s",
		c.errb.String())
}

// The second courier for a crew ends at once with a plain message and exit 0, logs nothing and leaves the first one's
// lock to it; with the first one gone the next runs, and a life spent is a clean end with start and stop logged.
func TestCourierSecondInstanceIsRefused(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	n := &nowRoles{status: "idle"}
	n.up(t, file, time.Time{})

	dir := filepath.Dir(file)

	release, err := lockCourier(dir)
	require.NoError(t, err)

	out, err := n.zm(t, file, "_courier", "--max-life", "5s")
	require.NoError(t, err, "exit 0: that a courier runs already is what was asked for")
	assert.Equal(t, fmt.Sprintf("courier: already running for this crew (pid %d); nothing to do\n", os.Getpid()), out)
	assert.NotContains(t, eventsOf(t, file), "courier-start")

	_, err = lockCourier(dir)
	require.Error(t, err, "and the first one's lock is still its own")

	release()

	out, err = n.zm(t, file, "_courier", "--max-life", "1ns")
	require.NoError(t, err)
	assert.Contains(t, out, "courier-start poll 250ms, max life 1ns")
	assert.Contains(t, out, "courier-stop max life 1ns reached")
	assert.Equal(t, []string{"courier-start", "courier-stop"}, courierEvents(eventsOf(t, file)))

	again, err := lockCourier(dir)
	require.NoError(t, err, "the lock is let go when the courier ends")
	again()
}

func courierEvents(all []string) []string {
	return slices.DeleteFunc(slices.Clone(all), func(e string) bool { return !strings.HasPrefix(e, "courier-") })
}

// The command is for a person who knows it: not in the help, its flags either; a flag that makes no sense is refused.
func TestCourierIsHidden(t *testing.T) {
	var out bytes.Buffer

	root := NewRoot(&out, &out)
	root.SetArgs([]string{"--help"})
	require.NoError(t, root.Execute())
	assert.NotContains(t, out.String(), "_courier")

	c, _, err := NewRoot(&out, &out).Find([]string{"_courier"})
	require.NoError(t, err)
	require.Equal(t, "_courier", c.Name())
	assert.True(t, c.Hidden)

	for _, flag := range []string{"poll", "max-life"} {
		f := c.Flags().Lookup(flag)
		require.NotNil(t, f, flag)
		assert.True(t, f.Hidden, flag)
	}

	assert.Equal(t, "250ms", c.Flags().Lookup("poll").DefValue)
	assert.Equal(t, "6h0m0s", c.Flags().Lookup("max-life").DefValue)

	file := testCrew(t, "  dev:\n")

	for _, args := range [][]string{{"--poll", "0s"}, {"--max-life", "-1s"}} {
		root := NewRoot(&out, &out)
		root.SetArgs(append([]string{"--crew", file, "_courier"}, args...))
		require.Error(t, root.Execute(), args)
	}
}

// Every save of state.json is a different stat, though the state saved is the same bytes: the file is replaced by a
// rename, so it is a new inode and a new time. A file that is not there is a stat of its own, not an error.
func TestStatSigSeesEverySaveOfTheState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	none, err := statSig(path)
	require.NoError(t, err)
	assert.False(t, none.present)

	save := func() fileSig {
		l, err := shift.Lock(dir)
		require.NoError(t, err)
		require.NoError(t, l.Save())

		sig, err := statSig(path)
		require.NoError(t, err)
		assert.True(t, sig.present)

		return sig
	}

	first, second, third := save(), save(), save()
	assert.NotEqual(t, none, first)
	assert.NotEqual(t, first, second)
	assert.NotEqual(t, second, third)
	assert.Equal(t, first.size, second.size, "the same state, the same size: the time and the inode tell")
}
