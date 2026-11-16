package crew

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// SeatKeys are the role keys 'zmina seat set' may change.
var SeatKeys = []string{"runtime", "profile", "codex_transport", "codex_home", "reasoning_effort", "service_tier", "sandbox", "approval_policy", "session", "dir", "boot", "keep", "model", "args", "about", "remote_control", "read_guard", "delivery", "resume",
	"handoff", "schedule", "prompt", "expires", "context_max", "context_action", "drop_tools", "keep_tools",
	"release_idle_minutes", "restart_at", "restart_idle", "restart_done", "budget"}

// listKeys are the seat keys whose value is a YAML list.
var listKeys = []string{"args", "drop_tools", "keep_tools"}

// Edit is the crew file as a YAML tree, so a seat can be added, changed or removed with its comments kept.
type Edit struct {
	path string
	doc  yaml.Node

	by, how string

	// sum is the hash of the file as Open read it, "" for an edit that is not of a file on disk: Save replaces only that file.
	sum string

	// Saved is what the last Save kept in the crew file's history; the command that saved logs it.
	Saved Saved
}

// Note says who changes the file and by which command, for the history ("zmina seat set dev").
func (e *Edit) Note(by, how string) { e.by, e.how = by, how }

// Open reads the crew file for editing.
func Open(path string) (*Edit, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("crew: %w", err)
	}

	e := &Edit{path: path, sum: sumOf(raw)}
	if err := yaml.Unmarshal(raw, &e.doc); err != nil {
		return nil, fmt.Errorf("crew: %s: %w", path, err)
	}

	if e.doc.Kind != yaml.DocumentNode || len(e.doc.Content) == 0 || e.doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("crew: %s is not a mapping", path)
	}

	return e, nil
}

// refusePlace turns away a seat edit that names place: where a role runs is part of the declaration every node holds, and
// zmina move changes it on both ends; it is no seat key.
func (e *Edit) refusePlace(set map[string]string) error {
	if _, ok := set["place"]; !ok {
		return nil
	}

	if lookup(e.doc.Content[0], "nodes") == nil {
		return errors.New("place: the crew file declares no nodes:; declare nodes: and home: in the file first")
	}

	return errors.New("place: this crew runs on more than one node, and where a role runs is one declaration for every node: " +
		"use zmina move <role> <node>, which changes it on both nodes")
}

// Add appends a role with the given keys (in SeatKeys order). It refuses a role that exists.
func (e *Edit) Add(name string, set map[string]string) error {
	if err := e.refusePlace(set); err != nil {
		return err
	}

	if !roleRe.MatchString(name) {
		return fmt.Errorf("role %q: names are lowercase words joined by dashes", name)
	}

	roles := e.roles(true)
	if lookup(roles, name) != nil {
		return fmt.Errorf("role %s is already in the crew; 'zmina seat set' changes it", name)
	}

	body := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	roles.Content = append(roles.Content, scalar(name), body)

	for _, k := range SeatKeys {
		if v, ok := set[k]; ok {
			if err := setKey(body, k, v); err != nil {
				return err
			}
		}
	}

	return nil
}

// Set changes keys of a role; an empty value removes the key.
func (e *Edit) Set(name string, set map[string]string) error {
	if err := e.refusePlace(set); err != nil {
		return err
	}

	body := lookup(e.roles(false), name)
	if body == nil {
		return fmt.Errorf("no role %q in %s", name, e.path)
	}

	if body.Kind != yaml.MappingNode {
		// "designer:" with nothing under it.
		*body = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}

	for k, v := range set {
		if err := setKey(body, k, v); err != nil {
			return err
		}
	}

	return nil
}

// Remove drops a role.
func (e *Edit) Remove(name string) error {
	roles := e.roles(false)
	if roles != nil {
		for i := 0; i+1 < len(roles.Content); i += 2 {
			if roles.Content[i].Value == name {
				roles.Content = append(roles.Content[:i], roles.Content[i+2:]...)

				return nil
			}
		}
	}

	return fmt.Errorf("no role %q in %s", name, e.path)
}

// SetCrewKey sets a key of the crew itself, not of a role, or removes it when value is empty.
func (e *Edit) SetCrewKey(key, value string) {
	top := e.doc.Content[0]

	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value != key {
			continue
		}

		if value == "" {
			top.Content = append(top.Content[:i], top.Content[i+2:]...)

			return
		}

		v := scalar(value)
		v.LineComment = top.Content[i+1].LineComment
		top.Content[i+1] = v

		return
	}

	if value != "" {
		top.Content = append(top.Content, scalar(key), scalar(value))
	}
}

// SetNodeKey is SetCrewKey for a key a node sets for itself (NodeKeys: retired, moved_to, spare ...) when the crew file
// declares nodes:: the key goes inside nodes.<node>, not at the top, so a retire made here stays this node's and does not
// travel with the file. For a file with no nodes:, a machine that is no node (node is empty) or a key that is no node's
// own it is SetCrewKey. A node the file does not list cannot hold a key of its own.
func (e *Edit) SetNodeKey(node, key, value string) error {
	top := e.doc.Content[0]

	nodes := lookup(top, "nodes")
	if node == "" || nodes == nil || nodes.Kind != yaml.MappingNode || len(nodes.Content) == 0 || !slices.Contains(NodeKeys, key) {
		e.SetCrewKey(key, value)

		return nil
	}

	slot := -1

	for i := 0; i+1 < len(nodes.Content); i += 2 {
		if nodes.Content[i].Value == node {
			slot = i + 1
		}
	}

	if slot < 0 {
		return fmt.Errorf("this node (%s) is not one of nodes: (%s): list it there before it keeps a key of its own", node,
			strings.Join(mapKeys(nodes), ", "))
	}

	body := nodes.Content[slot]
	if body.Kind != yaml.MappingNode {
		// "alpha:" with nothing under it.
		body = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		nodes.Content[slot] = body
	}

	if value == "" && lookup(top, key) != nil {
		// The top-level key says it for every node: this node says nothing, which has to be said.
		return setPlain(body, key, scalar(""))
	}

	if value == "" {
		return setPlain(body, key, nil)
	}

	return setPlain(body, key, scalar(value))
}

// setPlain sets key in a mapping node to v, or removes it for a nil v.
func setPlain(body *yaml.Node, key string, v *yaml.Node) error {
	for i := 0; i+1 < len(body.Content); i += 2 {
		if body.Content[i].Value != key {
			continue
		}

		if v == nil {
			body.Content = append(body.Content[:i], body.Content[i+2:]...)

			return nil
		}

		v.LineComment = body.Content[i+1].LineComment
		body.Content[i+1] = v

		return nil
	}

	if v != nil {
		body.Content = append(body.Content, scalar(key), v)
	}

	return nil
}

func mapKeys(m *yaml.Node) []string {
	var out []string

	for i := 0; i+1 < len(m.Content); i += 2 {
		out = append(out, m.Content[i].Value)
	}

	return out
}

// Save checks the edited file loads, a role's dir having to exist too, keeps the old one in the crew file's history
// (crew.d/history, see History), and writes it. A file that would not load is not written.
func (e *Edit) Save() error { return e.save(LoadStrict) }

// SaveCrewKeys is Save for an edit that touches no role: the file is held to what Load asks, since a crew is retired
// most often after its project directories are gone.
func (e *Edit) SaveCrewKeys() error { return e.save(Load) }

func (e *Edit) save(check func(string) (*Crew, error)) error {
	var buf bytes.Buffer

	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)

	if err := enc.Encode(&e.doc); err != nil {
		return fmt.Errorf("crew: %w", err)
	}

	how := e.how
	if how == "" {
		how = "zmina"
	}

	saved, err := commitIf(e.path, buf.Bytes(), check, e.sum, e.by, how)
	e.Saved = saved

	// The file is now what this edit wrote: a second save of the same edit goes on from it.
	if err == nil {
		e.sum = sumOf(buf.Bytes())
	}

	return err
}

// ErrChangedMeanwhile is the refusal of a save of an edit whose file is no longer the one it read: a push, another command or a hand
// edit replaced it meanwhile, and writing over it would lose it.
var ErrChangedMeanwhile = errors.New("the crew file changed meanwhile, run it again")

// Restore puts raw back as the crew file at path, as a change of its own: it must load (Load), what is there now is
// kept, and raw becomes the newest version.
func Restore(path string, raw []byte, by, how string) (Saved, error) {
	return commit(path, raw, Load, by, how)
}

// commit is the end of every change of the crew file: raw has to load, then the history keeps the file as it is and
// writes raw over it.
func commit(path string, raw []byte, check func(string) (*Crew, error), by, how string) (Saved, error) {
	return commitIf(path, raw, check, "", by, how)
}

// commitIf is commit for a change made to the file whose hash is base ("" for a change that takes whatever file is there): the
// history's lock is held over the compare and the write, the same lock a push takes, so an edit and a push never both write, and
// the one that read a file that is gone is refused (ErrChangedMeanwhile) and writes nothing.
func commitIf(path string, raw []byte, check func(string) (*Crew, error), base, by, how string) (Saved, error) {
	if err := loads(path, raw, check); err != nil {
		return Saved{}, err
	}

	if base == "" {
		return HistoryOf(path).Commit(path, raw, by, how)
	}

	saved, _, err := HistoryOf(path).CommitIf(path, raw, base, by, how)

	var be *BaseError
	if errors.As(err, &be) {
		return Saved{}, ErrChangedMeanwhile
	}

	return saved, err
}

// loads checks that raw would load as the crew file at path, in a temp file beside it (so its dir defaults are resolved as for
// the real one) that is gone when it returns. It is a file of its own: two edits at once check theirs apart.
func loads(path string, raw []byte, check func(string) (*Crew, error)) error {
	return withTemp(path, raw, func(tmp string) error {
		if _, err := check(tmp); err != nil {
			return fmt.Errorf("not saved, the crew would not load: %w", err)
		}

		return nil
	})
}

// withTemp runs fn on a temp file holding raw, beside the crew file at path, and removes it.
func withTemp(path string, raw []byte, fn func(tmp string) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("crew: %w", err)
	}

	tmp := f.Name()

	defer func() { _ = os.Remove(tmp) }()

	_, err = f.Write(raw)
	if cerr := f.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		return fmt.Errorf("crew: %w", err)
	}

	return fn(tmp)
}

func (e *Edit) roles(create bool) *yaml.Node {
	top := e.doc.Content[0]
	if n := lookup(top, "roles"); n != nil {
		if n.Kind != yaml.MappingNode && create {
			*n = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		}

		return n
	}

	if !create {
		return nil
	}

	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	top.Content = append(top.Content, scalar("roles"), n)

	return n
}

func setKey(body *yaml.Node, key, value string) error {
	if !known(key) {
		return fmt.Errorf("key %q: one of %s", key, strings.Join(SeatKeys, ", "))
	}

	for i := 0; i+1 < len(body.Content); i += 2 {
		if body.Content[i].Value != key {
			continue
		}

		if value == "" {
			body.Content = append(body.Content[:i], body.Content[i+2:]...)

			return nil
		}

		v, err := valueNode(key, value)
		if err != nil {
			return err
		}

		// Keep the comment that sat on the old value.
		v.LineComment = body.Content[i+1].LineComment
		body.Content[i+1] = v

		return nil
	}

	if value == "" {
		return nil
	}

	v, err := valueNode(key, value)
	if err != nil {
		return err
	}

	body.Content = append(body.Content, scalar(key), v)

	return nil
}

// valueNode is a string for every key but the list keys (args, drop_tools, keep_tools: [--chrome] or
// --chrome,--verbose), remote_control and read_guard, bools, and budget, a number.
func valueNode(key, value string) (*yaml.Node, error) {
	if key == "budget" {
		f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("budget: dollars a day, a number such as 20 or 12.5, not %q", value)
		}

		// Untagged and plain, so it is written as the number it is: a quoted "100" does not load into a float.
		return &yaml.Node{Kind: yaml.ScalarNode, Value: strconv.FormatFloat(f, 'f', -1, 64)}, nil
	}

	if key == "handoff" {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(value), &doc); err != nil {
			return nil, err
		}
		if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
			return nil, fmt.Errorf("handoff: expected a mapping with sections")
		}
		return doc.Content[0], nil
	}

	if key == "remote_control" || key == "read_guard" {
		if value != "true" && value != "false" {
			return nil, fmt.Errorf("%s: true or false, not %q", key, value)
		}

		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: value}, nil
	}

	if !slices.Contains(listKeys, key) {
		return scalar(value), nil
	}

	var list []string
	if strings.HasPrefix(strings.TrimSpace(value), "[") {
		if err := yaml.Unmarshal([]byte(value), &list); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
	} else {
		for a := range strings.SplitSeq(value, ",") {
			if a = strings.TrimSpace(a); a != "" {
				list = append(list, a)
			}
		}
	}

	if len(list) == 0 {
		return nil, fmt.Errorf("%s: an empty list; pass %s= to remove the key", key, key)
	}

	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, a := range list {
		n.Content = append(n.Content, scalar(a))
	}

	return n, nil
}

func lookup(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}

	return nil
}

func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func known(key string) bool { return slices.Contains(SeatKeys, key) }

// SetPlace is the edit zmina move makes to where a role runs: place: <node> in the role's mapping, written even when the node is
// home, so that the same edit on two files that are one file leaves them one file. It is no seat key: Set and Add refuse it.
func (e *Edit) SetPlace(role, node string) error {
	roles := e.roles(false)

	body := lookup(roles, role)
	if body == nil {
		return fmt.Errorf("no role %q in %s", role, e.path)
	}

	if body.Kind != yaml.MappingNode {
		// "dev:" with nothing under it.
		*body = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}

	return setPlain(body, "place", scalar(node))
}

// Bytes is the file as Save would write it.
func (e *Edit) Bytes() ([]byte, error) {
	var buf bytes.Buffer

	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)

	if err := enc.Encode(&e.doc); err != nil {
		return nil, fmt.Errorf("crew: %w", err)
	}

	return buf.Bytes(), nil
}

// PlaceAfter is the crew file raw as it is after a move of role to node: the bytes the move writes, made in memory, so that the
// two ends of a move can say beforehand which sum they will both hold afterwards.
func PlaceAfter(raw []byte, role, node string) ([]byte, error) {
	e := &Edit{path: "crew.yaml"}

	if err := yaml.Unmarshal(raw, &e.doc); err != nil {
		return nil, fmt.Errorf("crew: %w", err)
	}

	if e.doc.Kind != yaml.DocumentNode || len(e.doc.Content) == 0 || e.doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("crew: the file is not a mapping")
	}

	if err := e.SetPlace(role, node); err != nil {
		return nil, err
	}

	return e.Bytes()
}

// MovePlace writes the move of role to node into the crew file at path (place: <node> in the role's mapping, PlaceAfter), keeping the
// old file in its history by by and how. It replaces only the file whose hash is base (compare and swap under the history's
// lock; a *BaseError says otherwise), and the result must load. A file that is the result already is left alone (wrote is false):
// the same move made twice. sum is the hash of the file there afterwards.
func MovePlace(path, role, node, base, by, how string) (saved Saved, sum string, wrote bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return saved, "", false, fmt.Errorf("crew: %w", err)
	}

	after, err := PlaceAfter(raw, role, node)
	if err != nil {
		return saved, "", false, err
	}

	if sumOf(raw) == sumOf(after) {
		return saved, sumOf(after), false, nil
	}

	// The result is held to what Load asks (a role's dir is a thing of the node it runs on, and its own take checks it), for this node
	// and for every node the file lists: it is the file of each of them, and a node that could not read it would stop.
	lerr := withTemp(path, after, func(tmp string) error {
		c, err := Load(tmp)
		if err != nil {
			return err
		}

		for _, n := range c.NodeNames() {
			if _, err := LoadFor(tmp, n); err != nil {
				return fmt.Errorf("on %s: %w", n, err)
			}
		}

		return nil
	})
	if lerr != nil {
		return saved, "", false, fmt.Errorf("not saved, the crew would not load: %w", lerr)
	}

	saved, wrote, err = HistoryOf(path).CommitIf(path, after, base, by, how)

	return saved, sumOf(after), wrote, err
}
