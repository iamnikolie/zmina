package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

// Every state the courier line tells apart, with its level and the words that say what to do.
func TestCourierVerdict(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	held := courierLock{Held: true, PID: 4242, Since: now.Add(-3 * time.Hour)}
	base := courierFacts{Now: now, Log: "/c/courier.log", Install: "zmina ticker install"}

	with := func(edit func(f *courierFacts)) courierFacts {
		f := base
		edit(&f)

		return f
	}

	for _, tc := range []struct {
		name  string
		facts courierFacts
		level string
		says  []string
		not   []string
	}{
		{"off, nothing installed or running", with(func(f *courierFacts) { f.Off, f.Ticker = true, true }), "ok",
			[]string{"off in the crew file (courier: off)", "waits for the beat"}, []string{"install"}},
		{"off, its unit still there", with(func(f *courierFacts) { f.Off, f.Ticker, f.Unit = true, true, true }), "warn",
			[]string{"courier: off", "still installed", "zmina ticker install removes it"}, nil},
		{"off, one runs by hand", with(func(f *courierFacts) { f.Off, f.Lock = true, held }), "ok",
			[]string{"off in the crew file", "pid 4242, up 3h", "started by hand"}, nil},
		{"ticker installed, no courier unit", with(func(f *courierFacts) { f.Ticker = true }), "warn",
			[]string{"no courier unit beside the ticker", "up to a minute", "zmina ticker install installs it"}, nil},
		{"unit installed and a courier holds the lock", with(func(f *courierFacts) { f.Ticker, f.Unit, f.Lock = true, true, held }),
			"ok", []string{"pid 4242, up 3h", "kept running by its unit", "/c/courier.log"}, nil},
		{"unit installed, nobody holds the lock", with(func(f *courierFacts) { f.Ticker, f.Unit = true, true }), "warn",
			[]string{"unit is installed but no courier runs", "supervisor should have started one again",
				"its output is /c/courier.log", "zmina ticker status"}, nil},
		{"a courier by hand beside the ticker, no unit", with(func(f *courierFacts) { f.Ticker, f.Lock = true, held }), "warn",
			[]string{"pid 4242, up 3h", "started by hand", "no unit keeps it running", "zmina ticker install adds the unit"}, nil},
		{"a courier by hand, no ticker either", with(func(f *courierFacts) { f.Lock = held }), "ok",
			[]string{"pid 4242", "started by hand", "no ticker unit here"}, []string{"install"}},
		{"nothing at all", base, "ok", []string{"the ticker is not installed here either", "nobody runs a courier"}, nil},
		{"the lock file cannot be opened", with(func(f *courierFacts) { f.Ticker, f.Unit, f.LockErr = true, true, errors.New("permission denied") }),
			"warn", []string{"courier.lock could not be looked at: permission denied"}, nil},
		{"a holder whose pid is not in the file", with(func(f *courierFacts) {
			f.Ticker, f.Unit, f.Lock = true, true, courierLock{Held: true}
		}), "ok", []string{"pid unknown"}, []string{", up"}},
		{"a courier just started", with(func(f *courierFacts) {
			f.Ticker, f.Unit, f.Lock = true, true, courierLock{Held: true, PID: 7, Since: now.Add(-5 * time.Second)}
		}), "ok", []string{"pid 7, up under a minute"}, nil},
		{"off beats a lock that cannot be read", with(func(f *courierFacts) { f.Off, f.LockErr = true, errors.New("x") }), "ok",
			[]string{"off in the crew file"}, []string{"could not be looked at"}},
	} {
		level, detail := courierVerdict(tc.facts)
		assert.Equal(t, tc.level, level, tc.name+": "+detail)

		for _, s := range tc.says {
			assert.Contains(t, detail, s, tc.name)
		}

		for _, s := range tc.not {
			assert.NotContains(t, detail, s, tc.name)
		}
	}
}

// The doctor names the command with the crew's file when the crew is not the one a bare zmina reads.
func TestZminaForNamesTheCrewOnlyWhenItIsNotTheDefault(t *testing.T) {
	def, err := filepath.Abs(crew.DefaultPath())
	require.NoError(t, err)

	assert.Equal(t, "zmina", zminaFor(&crew.Crew{Path: def}))
	assert.Equal(t, "zmina --crew '/c/it'\"'\"'s/crew.yaml'", zminaFor(&crew.Crew{Path: "/c/it's/crew.yaml"}))
}

// putUnits writes the unit files this machine's service manager would have after an install, no more.
func putUnits(t *testing.T, c *crew.Crew, ticker, courier bool) {
	t.Helper()

	switch runtime.GOOS {
	case "darwin":
		require.NoError(t, os.MkdirAll(filepath.Dir(plistPath("x")), 0o755))

		if ticker {
			require.NoError(t, os.WriteFile(plistPath(c.LaunchdLabel()), nil, 0o644))
		}

		if courier {
			require.NoError(t, os.WriteFile(plistPath(c.CourierLabel()), nil, 0o644))
		}
	case "linux":
		require.NoError(t, os.MkdirAll(systemdDir(), 0o755))

		if ticker {
			require.NoError(t, os.WriteFile(filepath.Join(systemdDir(), c.SystemdUnit()+".timer"), nil, 0o644))
		}

		if courier {
			require.NoError(t, os.WriteFile(filepath.Join(systemdDir(), c.CourierUnit()+".service"), nil, 0o644))
		}
	default:
		t.Skip("no ticker on " + runtime.GOOS)
	}
}

// What doctor reads off the machine: the unit files of this system and the courier lock of the crew.
func TestCheckCourierReadsTheUnitsAndTheLock(t *testing.T) {
	c := unitCrew(t, "ops", "")
	c.Path = filepath.Join(filepath.Dir(c.Path), "ops", "crew.yaml") // not the default crew's file
	require.NoError(t, os.MkdirAll(filepath.Dir(c.Path), 0o755))

	line := func() (string, string) {
		var level, detail string

		checkCourier(c, func(name, lvl, format string, args ...any) {
			require.Equal(t, "courier", name)

			level, detail = lvl, fmt.Sprintf(format, args...)
		})

		return level, detail
	}

	level, detail := line()
	assert.Equal(t, "ok", level, detail)
	assert.Contains(t, detail, "not installed here either")

	putUnits(t, c, true, false)

	level, detail = line()
	assert.Equal(t, "warn", level)
	assert.Contains(t, detail, "no courier unit beside the ticker")
	assert.Contains(t, detail, "zmina --crew '"+c.Path+"' ticker install", "a crew that is not the default is named")

	putUnits(t, c, true, true)

	level, detail = line()
	assert.Equal(t, "warn", level)
	assert.Contains(t, detail, "unit is installed but no courier runs")
	assert.Contains(t, detail, courierLog(c))

	release, err := lockCourier(filepath.Dir(c.Path))
	require.NoError(t, err)

	level, detail = line()
	assert.Equal(t, "ok", level, detail)
	assert.Contains(t, detail, "pid "+fmt.Sprint(os.Getpid()))
	assert.Contains(t, detail, "kept running by its unit")

	release()

	level, _ = line()
	assert.Equal(t, "warn", level, "the lock is let go: nobody holds it")

	c.Courier = "off"

	level, detail = line()
	assert.Equal(t, "warn", level)
	assert.Contains(t, detail, "still installed")
}

// zmina doctor has the courier line, ok on a machine with no units, and an opt-out in the crew file shows in it.
func TestDoctorHasACourierLine(t *testing.T) {
	file := testCrew(t, "")

	out, _, _ := zm(t, file, "doctor")
	assert.Regexp(t, `ok +courier +none: the ticker is not installed here either`, out)

	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, append([]byte("courier: off\n"), raw...), 0o600))

	out, _, _ = zm(t, file, "doctor")
	assert.Regexp(t, `ok +courier +off in the crew file \(courier: off\)`, out)
}
