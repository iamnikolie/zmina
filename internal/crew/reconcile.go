package crew

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The default and the most starts of one seat the ticker tries before it gives the seat up (reconcile: {tries: N}).
const (
	DefaultReconcileTries = 3
	MaxReconcileTries     = 5
)

// What the ticker does with a tmux session of the crew that no role of the crew file names (reconcile: {orphans: M}).
const (
	// OrphansDown reports one and takes it down when nothing at all says someone is using it.
	OrphansDown = "down"
	// OrphansReport only tells the dispatcher of it, once.
	OrphansReport = "report"
	// OrphansOff leaves them alone and says nothing.
	OrphansOff = "off"
)

// Reconcile is the one rule behind a seat that holds an open task being up: zmina starts it when the task is given to it
// (Assign) and again when it is found down with the task (Restart), and takes down a session nobody owns (Orphans).
// Absent or off: all of it is off and nothing changes. Written `reconcile: on` (every part, 3 tries, orphans taken down),
// `off`, or a block that names the parts it wants, `reconcile: {assign: true, restart: true, tries: 2, orphans: report}`:
// in a block a part it does not name is off, and tries is 3.
type Reconcile struct {
	// Restart has the ticker start a seat that is down and holds an open task, as it does a keep: always seat.
	Restart bool `yaml:"-"`
	// Assign has a task given to a seat that is down (task new, task set --role, later take, the sweep) start the seat, and a
	// line the owner or the dispatcher queues for a down seat.
	Assign bool `yaml:"-"`
	// Tries is how many starts in a row may fail before the seat is marked broken (1 to 5); 0 reads as the default, 3.
	Tries int `yaml:"-"`
	// Orphans is down, report or off; empty reads as off.
	Orphans string `yaml:"-"`
}

// reconcileKeys are the keys of the block, in the order the error names them.
const reconcileKeys = "restart, assign, tries, orphans"

// UnmarshalYAML reads on or off, or a block of restart, assign, tries and orphans: another key is an error, since a
// mistyped key would otherwise be a part that silently stays off.
func (r *Reconcile) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		switch strings.ToLower(strings.TrimSpace(n.Value)) {
		case "on", "true", "yes":
			*r = Reconcile{Restart: true, Assign: true, Tries: DefaultReconcileTries, Orphans: OrphansDown}
		case "off", "false", "no", "":
			*r = Reconcile{}
		default:
			return fmt.Errorf("reconcile: %q: want on, off or {%s}", n.Value, reconcileKeys)
		}

		return nil
	case yaml.MappingNode:
		got := Reconcile{Tries: DefaultReconcileTries, Orphans: OrphansOff}
		seen := map[string]bool{}

		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]

			if seen[key.Value] {
				return fmt.Errorf("line %d: reconcile names %q twice", key.Line, key.Value)
			}

			seen[key.Value] = true

			if err := got.setKey(key, value); err != nil {
				return err
			}
		}

		*r = got

		return nil
	}

	return fmt.Errorf("reconcile: want on, off or {%s}", reconcileKeys)
}

// setKey reads one key of the block.
func (r *Reconcile) setKey(key, value *yaml.Node) error {
	switch key.Value {
	case "restart", "assign":
		on, err := reconcileBool(value)
		if err != nil {
			return fmt.Errorf("line %d: reconcile %s is true or false, not %q", value.Line, key.Value, value.Value)
		}

		if key.Value == "restart" {
			r.Restart = on
		} else {
			r.Assign = on
		}
	case "tries":
		v, err := strconv.Atoi(strings.TrimSpace(value.Value))
		if err != nil || v < 1 || v > MaxReconcileTries {
			return fmt.Errorf("line %d: reconcile tries is a number from 1 to %d, not %q", value.Line, MaxReconcileTries, value.Value)
		}

		r.Tries = v
	case "orphans":
		switch mode := strings.ToLower(strings.TrimSpace(value.Value)); mode {
		case OrphansDown, OrphansReport, OrphansOff:
			r.Orphans = mode
		case "false", "no":
			r.Orphans = OrphansOff
		default:
			return fmt.Errorf("line %d: reconcile orphans is %s, %s or %s, not %q", value.Line, OrphansDown, OrphansReport, OrphansOff, value.Value)
		}
	default:
		return fmt.Errorf("line %d: reconcile has no key %q (%s)", key.Line, key.Value, reconcileKeys)
	}

	return nil
}

// reconcileBool reads a part's switch: true, false, on, off, yes or no.
func reconcileBool(n *yaml.Node) (bool, error) {
	if n.Kind != yaml.ScalarNode {
		return false, fmt.Errorf("not a switch")
	}

	switch strings.ToLower(strings.TrimSpace(n.Value)) {
	case "true", "on", "yes":
		return true, nil
	case "false", "off", "no":
		return false, nil
	}

	return false, fmt.Errorf("not a switch")
}

// On reports whether any part of reconcile is on.
func (r Reconcile) On() bool { return r.Restart || r.Assign || r.OrphanMode() != OrphansOff }

// MaxTries is how many failed starts in a row the ticker puts up with before it marks the seat broken.
func (r Reconcile) MaxTries() int {
	if r.Tries < 1 {
		return DefaultReconcileTries
	}

	return min(r.Tries, MaxReconcileTries)
}

// OrphanMode is what the ticker does with a session no role names: down, report or off (the default).
func (r Reconcile) OrphanMode() string {
	if r.Orphans == "" {
		return OrphansOff
	}

	return r.Orphans
}
