package screenrules

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// overrideMax caps how much of an override file is read: it is a few rows.
const overrideMax = 1 << 20

// Overrides says what a crew's override file did to the table.
type Overrides struct {
	// Path is the file looked at, Present that it is there.
	Path    string
	Present bool
	// Count is how many rows the file lists; zero when there is no file or it was refused.
	Count int
	// Added, Replaced and Disabled are the ids it added, replaced and took away.
	Added, Replaced, Disabled []string
	// Err is why the file was refused: it does not read, or a row of it is wrong (the error names the id). The
	// embedded table stays in force, and nothing else is changed.
	Err error
}

// InForce reports whether the file changed the table: it is there, it was accepted, and it lists a row.
func (o Overrides) InForce() bool { return o.Present && o.Err == nil && o.Count > 0 }

// LoadOverrides reads an override file over a table. A file that is not there changes nothing and is no error. The file
// lists rows under rules:, each by id: a row with the id of an embedded one replaces it (a whole row, as in the
// embedded files), `disabled: true` takes it away, and any other id adds a row. A builtin row cannot be disabled or
// replaced, an unknown id cannot be disabled, and a field that is not known, a row that does not read or an id used twice
// refuses the whole file, so that half of a file is never in force: base is returned as it is, and Err says why.
func LoadOverrides(base *Table, path string) (*Table, Overrides) {
	o := Overrides{Path: path}

	// Only a plain file is read. A FIFO would hold the command until something wrote to it, a device could be endless: what the
	// path is, a symlink followed, is asked before it is opened.
	fi, err := os.Lstat(path)
	if err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		fi, err = os.Stat(path)
	}

	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return base, o
		}

		o.Present, o.Err = true, err

		return base, o
	}

	o.Present = true

	if !fi.Mode().IsRegular() {
		o.Err = fmt.Errorf("%s is not a regular file (it is a %s): it is not read", path, kindOf(fi.Mode()))

		return base, o
	}

	f, err := openPlain(path)
	if err != nil {
		o.Err = err

		return base, o
	}

	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, overrideMax+1))
	if err != nil {
		o.Err = err

		return base, o
	}

	if len(data) > overrideMax {
		o.Err = fmt.Errorf("%s is over %d bytes", path, overrideMax)

		return base, o
	}

	rows, err := decode(path, data)
	if err != nil {
		o.Err = err

		return base, o
	}

	t, o2, err := apply(base, rows)
	if err != nil {
		o.Err = err

		return base, o
	}

	o2.Path, o2.Present = path, true

	return t, o2
}

// apply puts override rows over a table.
func apply(base *Table, rows []Rule) (*Table, Overrides, error) {
	var o Overrides

	if len(rows) > maxOverrideRows {
		return nil, o, fmt.Errorf("the override file lists %d rows, at most %d are read", len(rows), maxOverrideRows)
	}

	seen := map[string]bool{}
	program := 0 // the instructions the file's regexps unroll to, all rows added
	next := make([]Rule, 0, len(base.rules)+len(rows))

	for _, c := range base.rules {
		next = append(next, c.Rule)
	}

	at := func(id string) int {
		for i, r := range next {
			if r.ID == id {
				return i
			}
		}

		return -1
	}

	for _, r := range rows {
		if r.ID == "" {
			return nil, o, errors.New("a row of the override file has no id")
		}

		if seen[r.ID] {
			return nil, o, fmt.Errorf("rule %q: the id is used twice in the override file", r.ID)
		}

		seen[r.ID] = true
		i := at(r.ID)

		if i >= 0 && next[i].Kind == KindBuiltin {
			return nil, o, fmt.Errorf("rule %q: it is a builtin (%s): the logic stays in Go and cannot be disabled or replaced",
				r.ID, next[i].Means)
		}

		// An override changes the words zmina reads, never the keys it presses: a row that decides a key, or gates the dialog a key
		// is pressed into, is not the file's to add, replace or disable.
		for _, sets := range []string{rowSets(next, i), rowSetsOf(r)} {
			if effects[sets].keys {
				return nil, o, fmt.Errorf("rule %q: it sets %s, which decides the keys zmina presses (or the dialog they are pressed into): "+
					"an override cannot add, replace or disable such a row", r.ID, sets)
			}
		}

		// What the file costs a beat is its rows' programs added up: each is bounded, and so is their sum. (A regexp that is too long
		// or does not read is left to build, which says so.)
		if n, err := programSize(r.Match.Regexp); !r.Disabled && r.Match.Regexp != "" && len(r.Match.Regexp) <= maxRegexpSource && err == nil {
			program += n

			switch {
			case n > maxOverrideProgram:
				return nil, o, fmt.Errorf("rule %q: the regexp unrolls to a program of %d instructions, more than the %d a row of an override file may have",
					r.ID, n, maxOverrideProgram)
			case program > maxOverrideBudget:
				return nil, o, fmt.Errorf("rule %q: the regexps of the override file unroll to %d instructions in all, more than the %d the file may have",
					r.ID, program, maxOverrideBudget)
			}
		}

		switch {
		case r.Disabled && i < 0:
			return nil, o, fmt.Errorf("rule %q: disabled, but no embedded rule has that id", r.ID)
		case r.Disabled:
			next = append(next[:i], next[i+1:]...)
			o.Disabled = append(o.Disabled, r.ID)
		case i >= 0:
			next[i] = r
			o.Replaced = append(o.Replaced, r.ID)
		default:
			next = append(next, r)
			o.Added = append(o.Added, r.ID)
		}
	}

	t, err := build(next)
	if err != nil {
		return nil, o, err
	}

	o.Count = len(rows)

	return t, o, nil
}

// rowSets is the flag the row at i of the rows sets; empty for none (no such row, or a builtin).
func rowSets(rows []Rule, i int) string {
	if i < 0 {
		return ""
	}

	return rows[i].Sets
}

// rowSetsOf is the flag an override row sets; none for a row that only disables.
func rowSetsOf(r Rule) string {
	if r.Disabled {
		return ""
	}

	return r.Sets
}

// kindOf names what a file that is not a plain file is.
func kindOf(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "directory"
	case m&fs.ModeNamedPipe != 0:
		return "named pipe"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeDevice != 0:
		return "device"
	default:
		return "special file"
	}
}
