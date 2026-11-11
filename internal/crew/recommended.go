package crew

// The recommended context thresholds, in tokens. They are advice, not defaults: nothing reads them to decide what a
// running crew does. `zmina doctor` measures a crew file against them and `zmina init` writes them into the starter
// file; the defaults (no context_max, DefaultRestartAt, DefaultRestartDone) stay as they are. Measured on a live
// Claude crew, the mean context of a turn was most of the bill.
const (
	// RecommendedContextMax is the context_max past which an idle role is compacted.
	RecommendedContextMax = 250_000
	// RecommendedRestartAt is the restart_at past which a role holding a task restarts fresh from its note.
	RecommendedRestartAt = 180_000
	// RecommendedRestartDone is the restart_done past which a role restarts fresh once its task closed: with a low
	// value, in effect a session per task.
	RecommendedRestartDone = 60_000
)

// ContextLimits are the context thresholds of the crew in tokens; 0 is off (context_max: none).
type ContextLimits struct {
	ContextMax, RestartAt, RestartDone int
}

// CrewContext is what a role that sets none of its own gets: the crew file's context_max, restart_at and
// restart_done, else the defaults.
func (c *Crew) CrewContext() ContextLimits {
	// The file was checked on load, so a value that does not parse cannot be here; it reads as off.
	contextMax, _ := ParseTokens(c.ContextMax)
	restartAt, _ := parseRestartAt(c.RestartAt, DefaultRestartAt)
	restartDone, _ := parseRestartTokens("restart_done", c.RestartDone, DefaultRestartDone)

	return ContextLimits{ContextMax: contextMax, RestartAt: restartAt, RestartDone: restartDone}
}
