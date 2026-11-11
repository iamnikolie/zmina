package crew

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/iamnikolie/zmina/internal/route"
)

// The values of busy and route_override.
const (
	// RouteOverrideOptional is route_override's default: a seat picked by hand against what the routes say may say why.
	RouteOverrideOptional = "optional"
	// RouteOverrideRequired makes a pick that disagrees with the routes say why (task new --because).
	RouteOverrideRequired = "required"
)

// The crew file's routes: key is an ordered list of rules, the first that matches and yields a seat wins (zmina
// route). A rule is one key, its kind, and a value; the kinds are fixed, one per rule the dispatcher has always
// followed, not an expression language:
//
//	routes:
//	  - other_crew: {repo_under: ~/work/acme-lg, tell: box/acme}   # a repo under it is another crew's: tell it
//	  - continuation: {fresh_over: 80}                              # --after T-n: the seat that did it, a fresh copy past 80% of its context
//	  - project: {app: app-dev, docs: writer}                       # a project's owner (--project, else the repo's directory name)
//	  - kind: {design: designer, film: director}                    # a kind's seat (--kind)
//	  - review: {first: codex-reviewer, again: [codex-dev, dev]}    # --kind review --of T-n: never the author
//	  - pool: {seats: [dev, dev2, codex-dev], by: limits}           # the rest: the emptier plan's free seat
//	busy: copy-or-pool                                              # a busy seat: its copy, or a free seat of its plan (pool: no copies)
//	route_override: optional                                        # required: a hand pick against the routes says why (--because)
//
// Routes holds the rules as the file wrote them; checkRoutes reads them (a bad rule is an error that names it) and
// RouteTable is the result. A crew file without routes: behaves as it always did.
func (c *Crew) checkRoutes() error {
	switch c.Busy {
	case "", string(route.CopyOrPool), string(route.PoolOnly):
	default:
		return fmt.Errorf("busy is %s or %s, not %q", route.CopyOrPool, route.PoolOnly, c.Busy)
	}

	switch c.RouteOverride {
	case "", RouteOverrideOptional, RouteOverrideRequired:
	default:
		return fmt.Errorf("route_override is %s or %s, not %q", RouteOverrideOptional, RouteOverrideRequired, c.RouteOverride)
	}

	c.routes = route.Table{Busy: route.BusyPolicy(c.Busy)}

	for i := range c.Routes {
		rule, err := c.parseRule(&c.Routes[i])
		if err != nil {
			return fmt.Errorf("routes: rule %d (line %d): %w", i+1, c.Routes[i].Line, err)
		}

		c.routes.Rules = append(c.routes.Rules, rule)
	}

	return nil
}

// RouteTable is the crew file's routes; empty (no rules) for a crew file without routes:.
func (c *Crew) RouteTable() route.Table { return c.routes }

// RouteOverrideRequired reports route_override: required: a seat picked by hand against what the routes say has to
// say why.
func (c *Crew) RouteOverrideRequired() bool { return c.RouteOverride == RouteOverrideRequired }

// ContextBudget is how many tokens the role's conversation may fill before it counts as full: its context_max, else
// the context past which zmina restarts it fresh (restart_at); 0 when it has neither. The routes' fresh_over is a
// share of it.
func (r *Role) ContextBudget() int {
	if r.contextMax > 0 {
		return r.contextMax
	}

	return r.restartAt
}

// ResolvePath makes a path absolute the way the crew file's own paths are (a leading ~, else relative to dir), clean,
// and with symbolic links followed when the path is there.
func ResolvePath(dir, p string) string {
	p = filepath.Clean(anchor(dir, ExpandHome(p)))

	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}

	return p
}

var ruleKinds = func() string {
	var names []string
	for _, k := range route.Kinds {
		names = append(names, string(k))
	}

	return strings.Join(names, ", ")
}()

// parseRule reads one rule: a mapping of exactly one key, its kind.
func (c *Crew) parseRule(n *yaml.Node) (route.Rule, error) {
	n = deref(n)

	if n.Kind != yaml.MappingNode {
		return route.Rule{}, fmt.Errorf("a rule is one kind and its value, such as pool: {seats: [dev]} (kinds: %s)", ruleKinds)
	}

	if len(n.Content) != 2 {
		var keys []string
		for i := 0; i < len(n.Content); i += 2 {
			keys = append(keys, n.Content[i].Value)
		}

		return route.Rule{}, fmt.Errorf("a rule has one key, its kind, not %d (%s): one dash for each rule", len(keys), strings.Join(keys, ", "))
	}

	kind, value := route.Kind(n.Content[0].Value), deref(n.Content[1])
	if !kind.Known() {
		return route.Rule{}, fmt.Errorf("unknown rule kind %q: one of %s", n.Content[0].Value, ruleKinds)
	}

	rule := route.Rule{Kind: kind}

	var err error

	switch kind {
	case route.OtherCrew:
		err = c.parseOtherCrew(&rule, value)
	case route.Continuation:
		err = parseContinuation(&rule, value)
	case route.Project, route.WorkKind:
		err = c.parseOwners(&rule, value)
	case route.Review:
		err = c.parseReview(&rule, value)
	case route.Pool:
		err = c.parsePool(&rule, value)
	}

	return rule, err
}

func (c *Crew) parseOtherCrew(r *route.Rule, v *yaml.Node) error {
	f, err := fields(v, "other_crew", "repo_under", "tell")
	if err != nil {
		return err
	}

	under, err := word(f["repo_under"], "other_crew.repo_under", "a directory")
	if err != nil {
		return err
	}

	tell, err := word(f["tell"], "other_crew.tell", "a <node>/<crew> address")
	if err != nil {
		return err
	}

	if !movedTo.MatchString(tell) {
		return fmt.Errorf("other_crew.tell %q: want <node>/<crew> in plain words", tell)
	}

	r.UnderText, r.Tell = under, tell
	r.Under = ResolvePath(filepath.Dir(c.Path), under)

	return nil
}

func parseContinuation(r *route.Rule, v *yaml.Node) error {
	r.FreshOver = route.DefaultFreshOver

	f, err := fields(v, "continuation", "fresh_over")
	if err != nil {
		return err
	}

	if f["fresh_over"] == nil {
		return nil
	}

	text, err := word(f["fresh_over"], "continuation.fresh_over", "a share of the context, 1 to 100")
	if err != nil {
		return err
	}

	n, err := strconv.Atoi(text)
	if err != nil || n < 1 || n > 100 {
		return fmt.Errorf("continuation.fresh_over is a share of the seat's context, 1 to 100, not %q", text)
	}

	r.FreshOver = n

	return nil
}

// parseOwners reads project: and kind:, a mapping of names to the seats that own them.
func (c *Crew) parseOwners(r *route.Rule, v *yaml.Node) error {
	key := string(r.Kind)

	if v.Kind != yaml.MappingNode || len(v.Content) == 0 {
		return fmt.Errorf("%s: a mapping of names to seats, such as {design: designer}", key)
	}

	r.Owners = map[string]string{}

	for i := 0; i+1 < len(v.Content); i += 2 {
		name := strings.TrimSpace(v.Content[i].Value)

		seat, err := word(v.Content[i+1], key+"."+name, "a seat")
		if err != nil {
			return err
		}

		if name == "" {
			return fmt.Errorf("%s: a name is empty", key)
		}

		if _, dup := r.Owners[name]; dup {
			return fmt.Errorf("%s.%s: named twice", key, name)
		}

		if err := c.seatExists(seat, key+"."+name); err != nil {
			return err
		}

		r.Owners[name] = seat
	}

	return nil
}

func (c *Crew) parseReview(r *route.Rule, v *yaml.Node) error {
	f, err := fields(v, "review", "first", "again")
	if err != nil {
		return err
	}

	if f["first"] != nil {
		if r.First, err = word(f["first"], "review.first", "a seat"); err != nil {
			return err
		}

		if err := c.seatExists(r.First, "review.first"); err != nil {
			return err
		}
	}

	if f["again"] != nil {
		if r.Again, err = seatList(f["again"], "review.again"); err != nil {
			return err
		}

		for _, s := range r.Again {
			if err := c.seatExists(s, "review.again"); err != nil {
				return err
			}
		}
	}

	if r.First == "" && len(r.Again) == 0 {
		return fmt.Errorf("review: names no seat: first: <seat>, again: [<seat>, …]")
	}

	return nil
}

func (c *Crew) parsePool(r *route.Rule, v *yaml.Node) error {
	f, err := fields(v, "pool", "seats", "by")
	if err != nil {
		return err
	}

	if by := f["by"]; by != nil {
		text, err := word(by, "pool.by", "limits")
		if err != nil {
			return err
		}

		if text != "limits" {
			return fmt.Errorf("pool.by is limits, not %q", text)
		}
	}

	if f["seats"] == nil {
		return fmt.Errorf("pool: names no seats: seats: [<seat>, …]")
	}

	if r.Seats, err = seatList(f["seats"], "pool.seats"); err != nil {
		return err
	}

	if len(r.Seats) == 0 {
		return fmt.Errorf("pool.seats is empty: a pool needs at least one seat")
	}

	for i, s := range r.Seats {
		if err := c.seatExists(s, "pool.seats"); err != nil {
			return err
		}

		if slices.Contains(r.Seats[:i], s) {
			return fmt.Errorf("pool.seats: %s is listed twice", s)
		}
	}

	return nil
}

// seatExists refuses a seat the crew does not have, naming where it is written; a copy (dev#2) is no seat of a rule.
func (c *Crew) seatExists(seat, where string) error {
	if _, ok := c.Roles[seat]; ok {
		return nil
	}

	if strings.Contains(seat, "#") {
		return fmt.Errorf("%s: %q is a copy: name the role (%s)", where, seat, strings.SplitN(seat, "#", 2)[0])
	}

	return fmt.Errorf("%s: no role %q in the crew (roles: %s)", where, seat, strings.Join(c.Names(), ", "))
}

// deref follows a YAML alias to the node it stands for.
func deref(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}

	return n
}

func isNull(n *yaml.Node) bool {
	return n == nil || (n.Kind == yaml.ScalarNode && n.Tag == "!!null")
}

// fields reads the keys of a rule's mapping, refusing one the rule does not have (a typo there would silently change
// where work goes); a rule with no value has none.
func fields(n *yaml.Node, rule string, allowed ...string) (map[string]*yaml.Node, error) {
	out := map[string]*yaml.Node{}

	if isNull(n) {
		return out, nil
	}

	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: a mapping of %s", rule, strings.Join(allowed, ", "))
	}

	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		if !slices.Contains(allowed, key) {
			return nil, fmt.Errorf("%s: unknown key %q (%s)", rule, key, strings.Join(allowed, ", "))
		}

		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("%s: %s is given twice", rule, key)
		}

		out[key] = deref(n.Content[i+1])
	}

	return out, nil
}

// word reads a value that is one word; what says what was wanted.
func word(n *yaml.Node, key, what string) (string, error) {
	if isNull(n) {
		return "", fmt.Errorf("%s is missing: %s", key, what)
	}

	if n.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("%s is %s", key, what)
	}

	if v := strings.TrimSpace(n.Value); v != "" {
		return v, nil
	}

	return "", fmt.Errorf("%s is empty: %s", key, what)
}

// seatList reads a list of seat names.
func seatList(n *yaml.Node, key string) ([]string, error) {
	if n.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s is a list of seats, such as [dev, dev2]", key)
	}

	out := make([]string, 0, len(n.Content))

	for _, item := range n.Content {
		s, err := word(deref(item), key, "a seat")
		if err != nil {
			return nil, err
		}

		out = append(out, s)
	}

	return out, nil
}
