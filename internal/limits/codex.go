package limits

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Home is one Codex home zmina knows: the directory whose sessions/ holds the rollouts, and who runs with it (the
// seats' names, or the directory's own name for a home no seat names).
type Home struct {
	Path  string
	Label string
}

const (
	// weekMinutes is the length of Codex's weekly window; the only window a reading's Week may be.
	weekMinutes = 7 * 24 * 60
	// rolloutsTried is how many of a home's newest rollouts are looked into when the newest holds no limits yet (a
	// session that has not had its first turn).
	rolloutsTried = 5
	// scanChunk and scanMax bound how far back from a rollout's end the last limits line is searched.
	scanChunk = 256 << 10
	scanMax   = 32 << 20
)

// ReadCodex finds the latest Codex reading: in each home the newest rollout under sessions/ by modification time,
// whichever day's folder it sits in, and in it the last "rate_limits" object; of the homes' readings the one recorded
// last. Nil when no rollout of any home holds one.
func ReadCodex(homes []Home) *Codex {
	var best *Codex

	for _, h := range homes {
		for i, path := range newestRollouts(filepath.Join(h.Path, "sessions")) {
			if i >= rolloutsTried {
				break
			}

			c, ok := lastCodexReading(path)
			if !ok {
				continue
			}

			c.Source = "rollout " + h.Label
			if best == nil || c.At.After(best.At) {
				best = c
			}

			break
		}
	}

	return best
}

// newestRollouts lists the rollout files under dir, the most recently modified first.
func newestRollouts(dir string) []string {
	type file struct {
		path string
		mod  time.Time
	}

	var files []file

	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), "rollout-") || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}

		if info, err := d.Info(); err == nil {
			files = append(files, file{path, info.ModTime()})
		}

		return nil
	})

	sort.Slice(files, func(i, j int) bool {
		if !files[i].mod.Equal(files[j].mod) {
			return files[i].mod.After(files[j].mod)
		}

		return files[i].path > files[j].path
	})

	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.path
	}

	return out
}

// lastCodexReading reads one rollout from its end for the last line that carries usable rate limits. A half-written
// last line, a line without them, or one that is not JSON is passed over.
func lastCodexReading(path string) (*Codex, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, false
	}

	var (
		pos     = info.Size()
		scanned int64
		partial []byte // the start of a line whose beginning is in an earlier chunk
	)

	for pos > 0 && scanned < scanMax {
		n := min(int64(scanChunk), pos)
		pos -= n
		scanned += n

		buf := make([]byte, int(n)+len(partial))
		if _, err := f.ReadAt(buf[:n], pos); err != nil && err != io.EOF {
			return nil, false
		}

		copy(buf[n:], partial)

		lines := bytes.Split(buf, []byte{'\n'})
		first := 0

		if pos > 0 {
			partial, first = lines[0], 1
		} else {
			partial = nil
		}

		for i := len(lines) - 1; i >= first; i-- {
			if c, ok := parseRollout(lines[i]); ok {
				return c, true
			}
		}
	}

	return nil, false
}

// rollout is one line of a Codex rollout, as far as the limits go: the limits sit in an event's payload.
type rollout struct {
	Timestamp string `json:"timestamp"`
	Payload   *struct {
		RateLimits *rateLimits `json:"rate_limits"`
	} `json:"payload"`
	RateLimits *rateLimits `json:"rate_limits"`
}

type rateLimits struct {
	Primary   *rateWindow `json:"primary"`
	Secondary *rateWindow `json:"secondary"`
	PlanType  string      `json:"plan_type"`
}

type rateWindow struct {
	UsedPercent   *float64 `json:"used_percent"`
	WindowMinutes int      `json:"window_minutes"`
	ResetsAt      *float64 `json:"resets_at"`
}

// parseRollout reads a line as a reading: its timestamp, the weekly window and the plan. A line with no weekly
// window, no percentage or no parsable timestamp is no reading.
func parseRollout(line []byte) (*Codex, bool) {
	if !bytes.Contains(line, []byte(`"rate_limits"`)) {
		return nil, false
	}

	var l rollout
	if json.Unmarshal(line, &l) != nil {
		return nil, false
	}

	rl := l.RateLimits
	if l.Payload != nil && l.Payload.RateLimits != nil {
		rl = l.Payload.RateLimits
	}

	if rl == nil {
		return nil, false
	}

	at, err := time.Parse(time.RFC3339Nano, l.Timestamp)
	if err != nil {
		return nil, false
	}

	for _, w := range []*rateWindow{rl.Primary, rl.Secondary} {
		if w == nil || w.WindowMinutes != weekMinutes || w.UsedPercent == nil || *w.UsedPercent < 0 {
			continue
		}

		week := &Window{Used: *w.UsedPercent}
		if w.ResetsAt != nil {
			week.Resets = epoch(*w.ResetsAt)
		}

		return &Codex{Week: week, Plan: rl.PlanType, At: at}, true
	}

	return nil, false
}
