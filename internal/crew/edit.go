package crew

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// SeatKeys are the role keys 'zmina seat set' may change.
var SeatKeys = []string{"runtime", "profile", "codex_transport", "codex_home", "reasoning_effort", "sandbox", "approval_policy", "session", "dir", "boot", "keep", "model", "args", "about", "remote_control", "delivery", "resume",
	"handoff", "schedule", "prompt", "expires", "context_max", "context_action", "drop_tools", "keep_tools",
	"release_idle_minutes", "budget"}

// listKeys are the seat keys whose value is a YAML list.
var listKeys = []string{"args", "drop_tools", "keep_tools"}

// Edit is the crew file as a YAML tree, so a seat can be added, changed or removed with its comments kept.
type Edit struct {
	path string
	doc  yaml.Node
}

// Open reads the crew file for editing.
func Open(path string) (*Edit, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("crew: %w", err)
	}

	e := &Edit{path: path}
	if err := yaml.Unmarshal(raw, &e.doc); err != nil {
		return nil, fmt.Errorf("crew: %s: %w", path, err)
	}

	if e.doc.Kind != yaml.DocumentNode || len(e.doc.Content) == 0 || e.doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("crew: %s is not a mapping", path)
	}

	return e, nil
}

// Add appends a role with the given keys (in SeatKeys order). It refuses a role that exists.
func (e *Edit) Add(name string, set map[string]string) error {
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

// Save checks the edited file loads, a role's dir having to exist too, keeps the old one as crew.yaml.bak, and writes
// it. A file that would not load is not written.
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

	tmp := e.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("crew: %w", err)
	}

	// Loading the temp file checks it, with dir defaults resolved as for the real one.
	if _, err := check(tmp); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("not saved, the crew would not load: %w", err)
	}

	if old, err := os.ReadFile(e.path); err == nil {
		if err := os.WriteFile(e.path+".bak", old, 0o600); err != nil {
			return fmt.Errorf("crew: %w", err)
		}
	}

	return os.Rename(tmp, e.path)
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
// --chrome,--verbose), remote_control, a bool, and budget, a number.
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

	if key == "remote_control" {
		if value != "true" && value != "false" {
			return nil, fmt.Errorf("remote_control: true or false, not %q", value)
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
