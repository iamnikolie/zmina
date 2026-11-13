package donecheck

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// A run that could not run the check says so (Kind), and a run that ran it, however badly, does not: only the shell's 126
// and 127, a shell that did not start, and the spec's own env_fault_rc are the environment's.
func TestRunTellsAnEnvironmentFaultFromAFailedCheck(t *testing.T) {
	for _, tc := range []struct {
		name  string
		run   string
		spec  func(*Spec)
		exit  int
		env   bool
		fault string
	}{
		{name: "passes", run: "true", exit: 0},
		{name: "plain failure", run: "exit 1", exit: 1},
		{name: "another status", run: "exit 3", exit: 3},
		{name: "not executable", run: "exit 126", exit: 126, env: true, fault: "exit 126: the shell found the command and could not execute it"},
		{name: "command not found", run: "exit 127", exit: 127, env: true, fault: "exit 127: the shell did not find the command"},
		{name: "the shell finds none", run: "no-such-tool-for-a-check --version", exit: 127, env: true,
			fault: "exit 127: the shell did not find the command"},
		{name: "a file that cannot run", run: "./not-a-program", exit: 127, env: true},
		{name: "the spec's status", run: "exit 3", spec: func(s *Spec) { s.EnvFaultRC = 3 }, exit: 3, env: true,
			fault: "exit 3: the status the check names as an environment fault (env_fault_rc)"},
		{name: "another status than the spec's", run: "exit 4", spec: func(s *Spec) { s.EnvFaultRC = 3 }, exit: 4},
		{name: "255 is a status like any", run: "exit 255", spec: func(s *Spec) { s.EnvFaultRC = 255 }, exit: 255, env: true},
		{name: "a signal is no fault", run: "kill -9 $$", exit: -1},
		{name: "128 and up are not the shell's", run: "exit 130", exit: 130},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := run(t, tc.run, tc.spec)

			assert.Equal(t, tc.exit, r.Exit, r.Output)
			assert.Equal(t, tc.env, r.Environment(), r.Output)
			assert.Equal(t, tc.exit == 0, r.Passed())

			if tc.env {
				assert.Equal(t, KindEnvironment, r.Kind)
			} else {
				assert.Empty(t, r.Kind)
				assert.Empty(t, r.Fault(), "a run that is no fault says no fault")
			}

			if tc.fault != "" {
				assert.Equal(t, tc.fault, r.Fault())
			}
		})
	}
}

// A shell that cannot start says why, and is a fault of the environment; a run that timed out or was stopped is neither,
// whatever status the spec names.
func TestRunNotStartedIsAFaultAndATimeoutIsNot(t *testing.T) {
	r := Run(context.Background(), Spec{Run: "true"}, filepath.Join(t.TempDir(), "gone"), nil)
	assert.True(t, r.Environment())
	assert.Equal(t, -1, r.Exit)
	assert.Contains(t, r.Fault(), "did not start")

	r = Run(context.Background(), Spec{Run: "sleep 30", Timeout: "1s", EnvFaultRC: 1}, t.TempDir(), nil)
	assert.True(t, r.TimedOut)
	assert.False(t, r.Environment(), "a timeout stays an ordinary failure")
	assert.Equal(t, "timed out after 1s", r.Failure(time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r = Run(ctx, Spec{Run: "exit 127"}, t.TempDir(), nil)
	assert.True(t, r.Interrupted)
	assert.False(t, r.Environment(), "a run that was stopped says nothing")
}

// env_fault_rc is an exit status from 1 to 255: 0 is how an unset one reads, so it cannot be a choice, and no status is
// beyond 255.
func TestSpecEnvFaultRC(t *testing.T) {
	for _, rc := range []int{0, 1, 3, 126, 127, 255} {
		assert.NoError(t, Spec{Run: "x", EnvFaultRC: rc}.Validate(), rc)
	}

	for _, rc := range []int{-1, 256, 1000} {
		assert.ErrorContains(t, Spec{Run: "x", EnvFaultRC: rc}.Validate(), "an exit status from 1 to 255", rc)
	}

	assert.Equal(t, "make test (10m, exit 3 is an environment fault)", Spec{Run: "make test", EnvFaultRC: 3}.String())
	assert.False(t, Spec{Run: "off", EnvFaultRC: 3}.Off(), "a mapping that says more is no 'off'")
}

func TestSpecReadsEnvFaultRC(t *testing.T) {
	var doc struct {
		A *Spec `yaml:"a"`
	}

	for name, tc := range map[string]struct {
		in   string
		want *Spec
		err  string
	}{
		"given":        {in: "a: {run: make test, env_fault_rc: 3}", want: &Spec{Run: "make test", EnvFaultRC: 3}},
		"with others":  {in: "a: {run: x, timeout: 5m, commit: true, env_fault_rc: 255}", want: &Spec{Run: "x", Timeout: "5m", Commit: true, EnvFaultRC: 255}},
		"not given":    {in: "a: {run: x}", want: &Spec{Run: "x"}},
		"zero written": {in: "a: {run: x, env_fault_rc: 0}", err: "env_fault_rc is an exit status from 1 to 255, not 0"},
		"hex zero":     {in: "a: {run: x, env_fault_rc: 0x0}", err: "not 0"},
		"a word":       {in: "a: {run: x, env_fault_rc: many}", err: "cannot unmarshal"},
		"a typo":       {in: "a: {run: x, env_fault: 3}", err: `no key "env_fault"`},
	} {
		t.Run(name, func(t *testing.T) {
			doc.A = nil
			err := yaml.Unmarshal([]byte(tc.in), &doc)

			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, doc.A)
		})
	}
}
