package crew

import (
	"os"
	"strings"
)

// ExecLine is the command line to start: Line, and for an argv a leading ~ in argv[0] and in every later argument
// turned into the home directory of the user running zmina (ExpandArgvHome). An argv runs without a shell, so nothing else
// would expand it; a string goes to /bin/sh -c, which does, and is left as it is. It is worked out each time the command
// starts, never when the file is read, so one crew file serves two machines with different homes. What it returns is a
// copy: the crew's own argv is not touched.
func (c Command) ExecLine() []string {
	line := c.Line()
	if c.Argv == nil {
		return line
	}

	return ExpandArgvHome(line)
}

// ExpandArgvHome is a copy of argv in which a ~ that is a whole word or stands before its first slash with nothing between
// ("~", "~/bin/check") is the home directory of the user running it, in every word. ~user, a ~ later in a word (a~/b,
// --dir=~/x) and the rest of a word stay as they are. Without a home directory to put (none is set) it is argv as it was.
func ExpandArgvHome(argv []string) []string {
	out := make([]string, len(argv))
	copy(out, argv)

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return out
	}

	for i, arg := range out {
		out[i] = expandLeadingHome(arg, home)
	}

	return out
}

// expandLeadingHome puts home in place of the ~ that opens arg: the whole of it, or the part before its first slash. It
// joins by hand, not with filepath.Join, so a trailing slash (rsync reads one) and a doubled one survive, as in a shell.
func expandLeadingHome(arg, home string) string {
	switch {
	case arg == "~":
		return home
	case strings.HasPrefix(arg, "~/"):
		return strings.TrimSuffix(home, "/") + arg[1:]
	}

	return arg
}
