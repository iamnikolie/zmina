package shift

import (
	"encoding"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A zmina reads state.json whole, keeps only the fields it knows and writes the file back whole. A long-lived process of
// an older build (the courier, a ticker beat that began before an install) that reads a state with a field it does not
// know therefore loses that field at its next save. The one guard is stateVersion: a build refuses a state whose number
// is above its own (ErrStateFuture) and never writes over it. That holds only while the number moves with the fields,
// so TestStateFieldsGolden ties the two: testdata/state_fields.golden records every JSON field reachable from State and
// the number it was recorded under, and a test run fails when the fields differ from the record while the number is the
// same.
//
// A change that fails it: bump stateVersion in state.go, then `go test ./internal/shift -run TestStateFieldsGolden
// -update`. The update refuses to record a changed field set under the number it already stands for.

var updateStateGolden = flag.Bool("update", false, "rewrite internal/shift/testdata/state_fields.golden from the types")

const stateGoldenPath = "testdata/state_fields.golden"

var (
	jsonMarshaler = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
	timeType      = reflect.TypeFor[time.Time]()
	rawType       = reflect.TypeFor[json.RawMessage]()
)

// stateFields lists, sorted, every JSON node reachable from State as "<path> <kind>": the path is the JSON keys from
// the root, "*" for a map's values and "[]" for a list's items; the kind is what an older reader would meet there
// (string, number, bool, time, object, map, array, raw, any). A type that marshals itself is a leaf, named by its Go
// type; its insides are its own to keep readable.
func stateFields() []string {
	set := map[string]bool{}

	walkJSON(reflect.TypeFor[State](), "", nil, set)

	out := make([]string, 0, len(set))
	for line := range set {
		out = append(out, line)
	}

	slices.Sort(out)

	return out
}

// walkJSON adds the node of type t at path, and what lies under it. stack holds the struct types on the way down, so a
// type that contains itself is listed once instead of walked for ever.
func walkJSON(t reflect.Type, path string, stack []reflect.Type, set map[string]bool) {
	t = indirect(t)

	name := path
	if name == "" {
		name = "."
	}

	add := func(kind string) { set[name+" "+kind] = true }

	switch {
	case t == timeType:
		add("time")

		return
	case t == rawType:
		add("raw")

		return
	case t.Implements(jsonMarshaler) || reflect.PointerTo(t).Implements(jsonMarshaler) ||
		t.Implements(textMarshaler) || reflect.PointerTo(t).Implements(textMarshaler):
		add("custom " + t.String())

		return
	}

	switch t.Kind() {
	case reflect.Struct:
		add("object")

		if !slices.Contains(stack, t) {
			walkFields(t, path, append(stack[:len(stack):len(stack)], t), set)
		}
	case reflect.Map:
		add("map")
		walkJSON(t.Elem(), path+".*", stack, set)
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			add("bytes")

			return
		}

		add("array")
		walkJSON(t.Elem(), path+"[]", stack, set)
	case reflect.Bool:
		add("bool")
	case reflect.String:
		add("string")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		add("number")
	default:
		add("any")
	}
}

func indirect(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	return t
}

// walkFields walks the fields encoding/json writes for struct type t: not an unexported one, not one tagged "-", and a
// struct embedded with no key of its own has its fields written as the outer struct's.
func walkFields(t reflect.Type, path string, stack []reflect.Type, set map[string]bool) {
	for i := range t.NumField() {
		f := t.Field(i)

		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}

		key, _, _ := strings.Cut(tag, ",")

		if et := indirect(f.Type); f.Anonymous && key == "" && et.Kind() == reflect.Struct {
			walkFields(et, path, stack, set)

			continue
		}

		if !f.IsExported() {
			continue
		}

		if key == "" {
			key = f.Name
		}

		if path != "" {
			key = path + "." + key
		}

		walkJSON(f.Type, key, stack, set)
	}
}

// readStateGolden reads the record: its schema number and its lines (sorted as written). missing is that there is no
// file yet.
func readStateGolden(t *testing.T) (schema int, lines []string, missing bool) {
	t.Helper()

	raw, err := os.ReadFile(filepath.FromSlash(stateGoldenPath))
	if os.IsNotExist(err) {
		return 0, nil, true
	}

	require.NoError(t, err)

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, " \r")

		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "schema "):
			n, err := strconv.Atoi(strings.TrimPrefix(line, "schema "))
			require.NoError(t, err, "the schema line of %s", stateGoldenPath)

			schema = n
		default:
			lines = append(lines, line)
		}
	}

	return schema, lines, false
}

func writeStateGolden(t *testing.T, schema int, lines []string) {
	t.Helper()

	var b strings.Builder

	b.WriteString("# The JSON fields of state.json (everything reachable from shift.State) and the schema number they were\n")
	b.WriteString("# recorded under. Written by TestStateFieldsGolden -update; do not edit by hand. A change to the fields\n")
	b.WriteString("# needs a higher stateVersion in state.go first (see state_schema_test.go).\n")
	fmt.Fprintf(&b, "schema %d\n", schema)

	for _, line := range lines {
		b.WriteString(line + "\n")
	}

	require.NoError(t, os.WriteFile(filepath.FromSlash(stateGoldenPath), []byte(b.String()), 0o644))
}

// fieldDiff is the lines only in want ("-") and only in got ("+").
func fieldDiff(want, got []string) (added, removed []string) {
	for _, l := range got {
		if !slices.Contains(want, l) {
			added = append(added, l)
		}
	}

	for _, l := range want {
		if !slices.Contains(got, l) {
			removed = append(removed, l)
		}
	}

	return added, removed
}

func listed(lines []string) string {
	if len(lines) == 0 {
		return "(none)"
	}

	return strings.Join(lines, "\n  ")
}

func TestStateFieldsGolden(t *testing.T) {
	got := stateFields()
	schema, want, missing := readStateGolden(t)
	added, removed := fieldDiff(want, got)
	changed := len(added)+len(removed) > 0

	bump := fmt.Sprintf("the fields of state.json changed but stateVersion is still %d. A zmina of an older build that "+
		"reads such a state drops the fields it does not know and saves it back, unless the number tells it to refuse.\n"+
		"Bump stateVersion in internal/shift/state.go, then regenerate the record:\n"+
		"  go test ./internal/shift -run TestStateFieldsGolden -update\n"+
		"added:\n  %s\nremoved:\n  %s", stateVersion, listed(added), listed(removed))

	if *updateStateGolden {
		require.GreaterOrEqual(t, stateVersion, schema, "stateVersion went down: a number once written is never reused")

		if !missing && changed && schema == stateVersion {
			t.Fatal("not recorded: " + bump)
		}

		writeStateGolden(t, stateVersion, got)

		return
	}

	require.False(t, missing, "%s is missing: run go test ./internal/shift -run TestStateFieldsGolden -update", stateGoldenPath)
	require.GreaterOrEqual(t, stateVersion, schema, "stateVersion is lower than the %d recorded in %s: a number once written "+
		"is never reused", schema, stateGoldenPath)

	if changed && schema == stateVersion {
		t.Fatal(bump)
	}

	require.Equal(t, schema, stateVersion, "stateVersion moved to %d but %s still records %d: regenerate it with "+
		"go test ./internal/shift -run TestStateFieldsGolden -update", stateVersion, stateGoldenPath, schema)

	require.False(t, changed, "%s does not match the types: regenerate it with go test ./internal/shift -run "+
		"TestStateFieldsGolden -update\nadded:\n  %s\nremoved:\n  %s", stateGoldenPath, listed(added), listed(removed))
}

// The walk sees what the guard has to see: a field of a type nested in a map, a list and a pointer, one that is left
// out, and a type that contains itself.
func TestStateFieldsWalkSeesNestedFields(t *testing.T) {
	type leaf struct {
		Name string `json:"name"`
		Skip string `json:"-"`
		hid  string
	}

	_ = leaf{hid: ""} // an unexported field is not written, so not listed

	type node struct {
		Kids  []node `json:"kids,omitempty"`
		Value int    `json:"value,omitzero"`
	}

	type embedded struct {
		Inner string
	}

	type root struct {
		embedded
		ByKey map[string][]*leaf `json:"by_key"`
		At    time.Time          `json:"at"`
		Tree  node               `json:"tree"`
		Free  any                `json:"free"`
	}

	set := map[string]bool{}
	walkJSON(reflect.TypeFor[root](), "", nil, set)

	got := make([]string, 0, len(set))
	for l := range set {
		got = append(got, l)
	}

	slices.Sort(got)

	assert.Equal(t, []string{
		". object",
		"Inner string",
		"at time",
		"by_key map",
		"by_key.* array",
		"by_key.*[] object",
		"by_key.*[].name string",
		"free any",
		"tree object",
		"tree.kids array",
		"tree.kids[] object",
		"tree.value number",
	}, got)
}

// A state written under the number before this one still reads: nothing but optional fields were added since, so it
// is read as it is, saved under the current number, and the file it replaced stays beside it as state.json.prev.
func TestStateOfAnOlderSchemaStillLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	old := `{"version": 2, "roles": {"dev": {"up": true, "last_session": "sid-dev", "inbox": ["line one"]}}, ` +
		`"signals": ["T-1 finished"], "ack_seq": 4}`

	require.NoError(t, os.WriteFile(path, []byte(old), 0o600))

	l, err := Lock(dir)
	require.NoError(t, err)
	assert.False(t, l.Legacy, "a numbered file is not the first format")
	assert.Equal(t, "sid-dev", l.Role("dev").LastSession)
	assert.Equal(t, []string{"line one"}, l.Role("dev").Inbox)
	assert.Equal(t, []string{"T-1 finished"}, l.Signals)
	assert.Equal(t, 4, l.AckSeq)
	require.NoError(t, l.Save())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var back struct {
		Version int `json:"version"`
	}

	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, stateVersion, back.Version, "saved under the number this zmina writes")

	prev, err := os.ReadFile(path + ".prev")
	require.NoError(t, err)
	assert.JSONEq(t, old, string(prev), "the state as it was stays beside the new one")

	_, err = os.Stat(path + ".v1.bak")
	assert.True(t, os.IsNotExist(err), "the first format's backup is for the first format only")
}
