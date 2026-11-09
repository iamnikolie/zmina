package cmd

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/cost"
	"github.com/iamnikolie/zmina/internal/shift"
)

// Jumps: a role's average context or price per turn that grew this much from one day to the next, with at least
// jumpTurns turns on both, is named in the morning line.
const (
	jumpRatio = 1.25
	jumpCtx   = 30_000
	jumpUSD   = 0.01
	jumpTurns = 30
)

// morningTick tells the dispatcher, once a day after the crew's morning time (crew file morning:, default 08:00, in
// its timezone:),
// what yesterday cost in one line, with the roles whose context or price per turn jumped from the day before. It
// runs outside the beat's lock.
func (a *app) morningTick(now time.Time) {
	var (
		crewFile string
		h, m     int
		on       bool
		done     string
		loc      *time.Location
	)

	// A crew file that does not load (left broken mid-edit) sends nothing: the beat goes on.
	err := a.readState(func(s *shift.Shift) error {
		h, m, on = s.Crew.MorningAt()
		done, crewFile, loc = s.State.MorningDay, s.Crew.Path, s.Crew.Location()

		return nil
	})
	if err != nil || !on || loc == nil {
		return
	}

	// The crew's clock, as the lessons run's: its time zone's midnight and morning, DST days too.
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	day := today.AddDate(0, 0, -1)

	if local.Before(time.Date(local.Year(), local.Month(), local.Day(), h, m, 0, 0, loc)) ||
		done == day.Format(time.DateOnly) {
		return
	}

	// Claimed before the slow read, so a beat that overlaps this one sends nothing.
	claimed := false

	_ = a.withState(func(s *shift.Shift) error {
		if s.State.MorningDay != day.Format(time.DateOnly) {
			s.State.MorningDay, claimed = day.Format(time.DateOnly), true
		}

		return nil
	})

	if !claimed {
		return
	}

	prices, err := cost.Load(pricesPath())
	if err != nil {
		return
	}

	crews := []uiCrew{{Key: a.crewKey(), Path: crewFile}}
	cells, _, stretches, _ := usageRead(crews, "", day.AddDate(0, 0, -1), prices, nil, loc)
	_, fills, _, _ := usageRead(crews, "", day, prices, &today, loc)

	line := morningLine(cells, fills, stretches, day)
	if line == "" {
		return
	}

	_ = a.withState(func(s *shift.Shift) error {
		s.Log("", "morning", "", line)
		s.AddNotes([]string{line})

		return nil
	})
}

// morningLine is yesterday in one line: its cost, turns and hours, the context per turn of the costliest roles,
// what filled the context most, and the roles whose context or price per turn jumped from the day before. Empty
// when nobody worked.
func morningLine(cells []cost.Cell, fills []usageFill, stretches []usageStretch, day time.Time) string {
	y, before := day.Format(time.DateOnly), day.AddDate(0, 0, -1).Format(time.DateOnly)

	var cy, cb []cost.Cell

	for _, c := range cells {
		switch c.Day {
		case y:
			cy = append(cy, c)
		case before:
			cb = append(cb, c)
		}
	}

	total := cost.Total(cy)
	if total.Turns == 0 {
		return ""
	}

	from, to := cost.StretchOf(day), cost.StretchOf(day.AddDate(0, 0, 1))
	wall := map[int64]bool{}

	for _, s := range stretches {
		for _, st := range s.Stretches {
			if st >= from && st < to {
				wall[st] = true
			}
		}
	}

	parts := []string{fmt.Sprintf("morning: yesterday %s API-equivalent, %d turns", dollars(total.Cost), total.Turns)}
	if len(wall) > 0 {
		parts[0] += fmt.Sprintf(", %.1f h at work (%.1f sessions at once)", stretchHours(len(wall)),
			float64(total.Active)/float64(len(wall)))
	}

	rows := cost.Group(cy, "role")

	var ctx []string

	for _, r := range rows {
		if len(ctx) == 4 {
			break
		}

		ctx = append(ctx, fmt.Sprintf("%s %s", r.Key, shift.Tokens(perTurnCtx(r))))
	}

	parts = append(parts, "context per turn: "+strings.Join(ctx, ", "))

	if f := topFills(fills, 3); f != "" {
		parts = append(parts, "filled by "+f)
	}

	if j := jumps(rows, cost.Group(cb, "role")); len(j) > 0 {
		parts = append(parts, "jumped: "+strings.Join(j, ", "))
	}

	if total.Compacts > 0 {
		parts = append(parts, fmt.Sprintf("compacted %d", total.Compacts))
	}

	return strings.Join(parts, "; ") + " (zmina usage --since 24h, --by ctx, --by tool)"
}

// perTurnCtx is the context a row's turns carried on average.
func perTurnCtx(r cost.Row) int {
	return (r.Input + r.Write + r.Write5m + r.Write1h + r.Read) / max(r.Turns, 1)
}

// topFills is the n tool kinds that put the most text into the context, with their share.
func topFills(fills []usageFill, n int) string {
	rows := groupFills(fills)

	var total int
	for _, r := range rows {
		total += r.Chars
	}

	if total == 0 {
		return ""
	}

	var out []string

	for _, r := range rows[:min(n, len(rows))] {
		out = append(out, fmt.Sprintf("%s %s", strings.TrimPrefix(r.Tool, "Bash: "), share(float64(r.Chars),
			float64(total))))
	}

	return strings.Join(out, ", ")
}

// jumps names the roles whose context or price per turn grew by jumpRatio from the day before, by enough to
// matter, with jumpTurns turns on both days.
func jumps(day, before []cost.Row) []string {
	prev := map[string]cost.Row{}
	for _, r := range before {
		prev[r.Key] = r
	}

	var out []string

	for _, r := range day {
		p, ok := prev[r.Key]
		if !ok || r.Turns < jumpTurns || p.Turns < jumpTurns {
			continue
		}

		if c, pc := perTurnCtx(r), perTurnCtx(p); float64(c) >= jumpRatio*float64(pc) && c-pc >= jumpCtx {
			out = append(out, fmt.Sprintf("%s context %s -> %s", r.Key, shift.Tokens(pc), shift.Tokens(c)))
		}

		u, pu := r.Cost/float64(max(r.Turns-r.Unpriced, 1)), p.Cost/float64(max(p.Turns-p.Unpriced, 1))
		if u >= jumpRatio*pu && u-pu >= jumpUSD {
			out = append(out, fmt.Sprintf("%s %s -> %s a turn", r.Key, dollars(pu), dollars(u)))
		}
	}

	slices.Sort(out)

	return out
}
