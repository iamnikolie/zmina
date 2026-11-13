package limits

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/fsx"
)

// ClaudeFile is the file the tap writes in the limits directory.
const ClaudeFile = "claude.json"

// statusline is the part of Claude Code's status line JSON the tap reads: rate_limits.five_hour and .seven_day, each
// with used_percentage and resets_at (epoch seconds). Either may be missing: Claude Code sends the limits only for a
// subscription, and only once the session has had an API reply.
type statusline struct {
	RateLimits struct {
		FiveHour *statusWindow `json:"five_hour"`
		SevenDay *statusWindow `json:"seven_day"`
	} `json:"rate_limits"`
}

type statusWindow struct {
	Used   *float64        `json:"used_percentage"`
	Resets json.RawMessage `json:"resets_at"`
}

// window is the figures of one status-line window; nil when it holds no usable percentage.
func (s *statusWindow) window() *Window {
	if s == nil || s.Used == nil || math.IsNaN(*s.Used) || math.IsInf(*s.Used, 0) || *s.Used < 0 {
		return nil
	}

	return &Window{Used: *s.Used, Resets: resetTime(s.Resets)}
}

// resetTime reads a reset moment: epoch seconds as a number or a numeric string, or an RFC 3339 string. Anything
// else is no moment (nil), never a guess.
func resetTime(raw json.RawMessage) *time.Time {
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return epoch(n)
	}

	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}

	if n, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return epoch(n)
	}

	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(s)); err == nil {
		return &t
	}

	return nil
}

// epochMax is the biggest number of seconds a reset time may be: the year 5138. A bigger number is milliseconds, and read as
// such once; a number still bigger is no moment.
const epochMax = 1e11

func epoch(sec float64) *time.Time {
	if math.IsNaN(sec) || math.IsInf(sec, 0) || sec <= 0 {
		return nil
	}

	if sec > epochMax {
		sec /= 1000
	}

	if sec > epochMax {
		return nil
	}

	t := time.Unix(int64(sec), 0)

	return &t
}

// ParseStatusline reads the figures in one status-line JSON, seen at now. Nil when it carries none (a session that
// has had no reply yet, an API key instead of a subscription, input that is no JSON).
func ParseStatusline(raw []byte, now time.Time) *Claude {
	var s statusline
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}

	c := &Claude{FiveHour: s.RateLimits.FiveHour.window(), SevenDay: s.RateLimits.SevenDay.window(), At: now, Source: SourceStatusline}
	if c.FiveHour == nil && c.SevenDay == nil {
		return nil
	}

	// A reset a window cannot have is no reset: it never gets into the file.
	c.FiveHour.bound(WindowFiveHour, now)
	c.SevenDay.bound(WindowSevenDay, now)

	return c
}

// ReadClaude reads the tap's file in dir. A file that is not there is no reading (nil, nil); one that does not parse is
// an error and no reading.
func ReadClaude(dir string) (*Claude, error) {
	path := filepath.Join(dir, ClaudeFile)

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var c Claude
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	if c.At.IsZero() || (c.FiveHour == nil && c.SevenDay == nil) {
		return nil, fmt.Errorf("%s: holds no reading", path)
	}

	// Stale is the reader's verdict, never the file's.
	for _, w := range []*Window{c.FiveHour, c.SevenDay} {
		if w != nil {
			w.Stale = false
		}
	}

	return &c, nil
}

// Tap records one status line's figures in dir: the file is written, whole or not at all, when a figure differs from
// the one in the file, and otherwise at most every Heartbeat to say the status line is still drawn. It reports
// whether it wrote. A status line without figures writes nothing.
//
// Sessions share the file, and one that redraws with figures it saw long ago (a stale status line input) must not
// take back a newer reading: a window whose reset is earlier than the stored one's is the stored window's past, and
// at the same reset the higher percentage stands (a window only fills, and a lower figure is an older one: a session shows the figure of its last
// reply). The stored window stays then, and a draw that brought nothing newer than the file leaves it as it is, At included: the file says
// when its figures were seen, not when an old one was drawn again. A stored window nobody drew for ten minutes yields to any newer reading, a
// lower figure at the same reset included (Window.latest); the beat never takes a lower figure of the same period for the window having emptied
// (limits.Evaluate): only a reset of another period ends a hold.
func Tap(dir string, raw []byte, now time.Time) (bool, error) {
	c := ParseStatusline(raw, now)
	if c == nil {
		return false, nil
	}

	// A file that cannot be read is simply replaced.
	if old, _ := ReadClaude(dir); old != nil {
		drawnAgain := old.sameFigures(c)

		c.FiveHour, c.SevenDay = c.FiveHour.latest(old.FiveHour, c.At, old.At), c.SevenDay.latest(old.SevenDay, c.At, old.At)

		if old.sameFigures(c) {
			if !drawnAgain {
				return false, nil
			}

			if d := now.Sub(old.At); d >= 0 && d < Heartbeat {
				return false, nil
			}
		}
	}

	data, err := json.Marshal(c)
	if err != nil {
		return false, err
	}

	if err := fsx.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}

	if err := fsx.WriteFile(filepath.Join(dir, ClaudeFile), append(data, '\n'), 0o600); err != nil {
		return false, err
	}

	return true, nil
}
