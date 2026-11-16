package crew

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// An argv runs with no shell, so a leading ~ in it (argv[0] and every later argument) is the home of whoever runs zmina,
// worked out when the command starts; ~user, a ~ that is not first and a string (the shell's own) stay as written.
func TestCommandExecLineExpandsHome(t *testing.T) {
	t.Setenv("HOME", "/home/alex")

	for _, c := range []struct {
		name string
		cmd  Command
		want []string
	}{
		{"argv[0]", Command{Argv: []string{"~/bin/check"}}, []string{"/home/alex/bin/check"}},
		{"later arguments", Command{Argv: []string{"check", "~/acme", "--out", "~/out/"}},
			[]string{"check", "/home/alex/acme", "--out", "/home/alex/out/"}},
		{"argv[0] and arguments", Command{Argv: []string{"~/bin/check", "~/x", "plain"}},
			[]string{"/home/alex/bin/check", "/home/alex/x", "plain"}},
		{"the bare home", Command{Argv: []string{"ls", "~"}}, []string{"ls", "/home/alex"}},
		{"a trailing and a doubled slash stay", Command{Argv: []string{"sync", "~/src/", "~//x"}},
			[]string{"sync", "/home/alex/src/", "/home/alex//x"}},
		{"another user's home", Command{Argv: []string{"~bob/x", "~bob", "~+/x"}}, []string{"~bob/x", "~bob", "~+/x"}},
		{"a ~ that is not first", Command{Argv: []string{"a~/b", "--dir=~/x", "x/~/y", " ~/x", "\\~/x"}},
			[]string{"a~/b", "--dir=~/x", "x/~/y", " ~/x", "\\~/x"}},
		{"no ~", Command{Argv: []string{"/usr/bin/true", "-v"}}, []string{"/usr/bin/true", "-v"}},
		{"a string is the shell's", Command{Shell: "~/bin/check ~/x"}, []string{"/bin/sh", "-c", "~/bin/check ~/x"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, c.cmd.ExecLine())
		})
	}
}

// Expanding is for the start of a command: the crew's own argv is not rewritten, and a home that is "/" does not double
// its slash.
func TestCommandExecLineKeepsTheCrewsArgv(t *testing.T) {
	t.Setenv("HOME", "/home/alex")

	c := Command{Argv: []string{"~/bin/check", "~/x"}}
	assert.Equal(t, []string{"/home/alex/bin/check", "/home/alex/x"}, c.ExecLine())
	assert.Equal(t, []string{"~/bin/check", "~/x"}, c.Argv, "the same file serves a machine with another home")
	assert.Equal(t, []string{"~/bin/check", "~/x"}, c.Line(), "Line is the command as written")

	t.Setenv("HOME", "/home/acme")
	assert.Equal(t, []string{"/home/acme/bin/check", "/home/acme/x"}, c.ExecLine(), "worked out at each start")

	t.Setenv("HOME", "/")
	assert.Equal(t, []string{"/bin/check", "/x"}, c.ExecLine())

	t.Setenv("HOME", "")
	assert.Equal(t, []string{"~/bin/check", "~/x"}, c.ExecLine(), "no home to put: as written")
}

// The crew file is read as it is written: a ~ in a run or a notify is not turned into a path by the loader, so
// writing the file back keeps it.
func TestCommandKeepsTheTildeWhenRead(t *testing.T) {
	t.Setenv("HOME", "/home/alex")

	var got struct {
		Run Command `yaml:"run"`
	}

	require.NoError(t, yaml.Unmarshal([]byte("run: [\"~/bin/check\", \"~/x\"]\n"), &got))
	assert.Equal(t, []string{"~/bin/check", "~/x"}, got.Run.Argv)

	raw, err := yaml.Marshal(got)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "~/bin/check")
	assert.NotContains(t, string(raw), "/home/alex")
}
