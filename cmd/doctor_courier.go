package cmd

import (
	"fmt"
	"path/filepath"
	"runtime"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
)

// courierFacts is everything doctor's courier line is judged from, so the judgement is a function of plain values.
type courierFacts struct {
	// Off is courier: off in the crew file.
	Off bool
	// Ticker and Unit say whether the ticker's unit and the courier's unit are installed.
	Ticker, Unit bool
	// Lock is what a look at courier.lock found; LockErr that it could not be looked at.
	Lock    courierLock
	LockErr error
	Now     time.Time
	// Log is where the unit sends the courier's output; Install the command that installs the units.
	Log, Install string
}

// courierVerdict is the courier line: its level and one plain sentence of how things stand and what to do.
func courierVerdict(f courierFacts) (level, detail string) {
	running := "a courier runs"
	if f.Lock.Held {
		running = "a courier runs (pid " + pidText(f.Lock.PID) + upText(f.Now, f.Lock.Since) + ")"
	}

	switch {
	case f.Off && f.Unit:
		return "warn", "off in the crew file (courier: off), but its unit is still installed: " + f.Install +
			" removes it"
	case f.Off && f.Lock.Held:
		return "ok", "off in the crew file (courier: off), yet " + running + ", started by hand; nothing restarts it"
	case f.Off:
		return "ok", "off in the crew file (courier: off): a line for a role waits for the beat, up to a minute"
	case f.LockErr != nil:
		return "warn", "courier.lock could not be looked at: " + f.LockErr.Error()
	case f.Unit && f.Lock.Held:
		return "ok", running + ", kept running by its unit · output " + f.Log
	case f.Unit:
		return "warn", fmt.Sprintf("its unit is installed but no courier runs: the supervisor should have started one "+
			"again (it waits %d s between starts); its output is %s, and 'zmina ticker status' shows what the "+
			"supervisor knows", courierRestartSeconds, f.Log)
	case f.Lock.Held && f.Ticker:
		return "warn", running + ", started by hand, and no unit keeps it running: when it ends (a new zmina, its " +
			"6 h) nothing starts the next. " + f.Install + " adds the unit"
	case f.Lock.Held:
		return "ok", running + ", started by hand; there is no ticker unit here either, so nothing keeps it running"
	case f.Ticker:
		return "warn", "no courier unit beside the ticker: a line for a role waits for the next beat, up to a " +
			"minute, instead of going at once. " + f.Install + " installs it (a ticker installed before the courier " +
			"existed needs this once)"
	}

	return "ok", "none: the ticker is not installed here either, and nobody runs a courier"
}

// pidText is a pid for a message; unknown when the holder's file had none to read.
func pidText(pid int) string {
	if pid <= 0 {
		return "unknown"
	}

	return fmt.Sprint(pid)
}

// upText is ", up 3h" from when the holder wrote its pid, and nothing when that is not known.
func upText(now, since time.Time) string {
	d := now.Sub(since)
	if since.IsZero() || d < 0 {
		return ""
	}

	if d < time.Minute {
		return ", up under a minute"
	}

	return ", up " + age(d)
}

// installedUnits says whether the ticker's unit and the courier's are on disk for the crew: the files of this
// machine's service manager, not a question put to it. A machine with neither launchd nor systemd has neither.
func installedUnits(c *crew.Crew) (ticker, courier bool) {
	switch runtime.GOOS {
	case "darwin":
		return launchdUnits(c)
	case "linux":
		return systemdUnits(c)
	}

	return false, false
}

// zminaFor is the command that names the crew on the command line: plain zmina for the default crew, else with its file.
func zminaFor(c *crew.Crew) string {
	if def, err := filepath.Abs(crew.DefaultPath()); err == nil && def == c.Path {
		return "zmina"
	}

	return "zmina --crew " + quoteCommandArg(c.Path)
}

// checkCourier is doctor's courier line: whether the courier is expected, installed and running.
func checkCourier(c *crew.Crew, add func(name, level, format string, args ...any)) {
	ticker, unit := installedUnits(c)
	lock, err := probeCourierLock(c.Layout().CourierLock())

	level, detail := courierVerdict(courierFacts{
		Off: !c.CourierOn(), Ticker: ticker, Unit: unit, Lock: lock, LockErr: err, Now: time.Now(),
		Log: courierLog(c), Install: zminaFor(c) + " ticker install",
	})

	add("courier", level, "%s", detail)
}
