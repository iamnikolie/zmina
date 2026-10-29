package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

// execWatch is an exec watch that runs a shell script with the given body.
func execWatch(t *testing.T, body string) (*crew.Watch, Env) {
	t.Helper()

	dir := t.TempDir()
	script := filepath.Join(dir, "detector")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o700))

	w := &crew.Watch{Name: "ci-red", Kind: crew.WatchExec, Every: "5m", Timeout: "2s", Run: crew.Command{Argv: []string{script}}}
	c := &crew.Crew{Path: filepath.Join(dir, "crew.yaml"), Dir: dir, Watches: map[string]*crew.Watch{"ci-red": w}}
	require.NoError(t, w.Check(c))

	return w, Env{CrewPath: c.Path, Dir: Dir(dir, "ci-red"), Seen: Seen{"main": "abc"}}
}

func TestExecReadsEvents(t *testing.T) {
	w, env := execWatch(t, `
printf '%s\n' '{"key":"main","fingerprint":"def","text":"pipeline red\non main","url":"https://ci/1"}'
echo
echo '{"key":"dev","fingerprint":"1","text":"x","at":"2026-10-26T12:00:00Z"}'
echo "seen: $(cat "$ZMINA_WATCH_SEEN") watch: $ZMINA_WATCH" >&2
test -d "$ZMINA_WATCH_DIR" && test -n "$ZMINA_CREW"`)

	res := Run(context.Background(), w, env)
	require.NoError(t, res.Err)
	require.Len(t, res.Events, 2)
	assert.Equal(t, "pipeline red on main https://ci/1", res.Events[0].Line(), "text folds to one line")
	assert.Equal(t, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), res.Events[1].At.UTC())
	assert.Equal(t, []string{`seen: {"main":"abc"} watch: ci-red`}, res.Stderr, "the source reads the stored fingerprints")
}

func TestExecFailures(t *testing.T) {
	for name, body := range map[string]string{
		"non-zero exit": `echo '{"key":"a","fingerprint":"1","text":"x"}'; echo "dial tcp: no such host" >&2; exit 2`,
		"garbage line":  `echo '{"key":"a","fingerprint":"1","text":"x"}'; echo 'connection reset'`,
		"missing field": `echo '{"key":"a","text":"x"}'`,
		"timeout":       `echo '{"key":"a","fingerprint":"1","text":"x"}'; sleep 30`,
	} {
		t.Run(name, func(t *testing.T) {
			w, env := execWatch(t, body)
			start := time.Now()
			res := Run(context.Background(), w, env)
			require.Error(t, res.Err)
			assert.Nil(t, res.Events)
			assert.Less(t, time.Since(start), 10*time.Second)
		})
	}

	w, env := execWatch(t, `sleep 30`)
	assert.ErrorContains(t, Run(context.Background(), w, env).Err, "timed out after 2s")

	w, env = execWatch(t, `echo "dial tcp: lookup gitlab: no such host" >&2; exit 1`)
	assert.ErrorContains(t, Run(context.Background(), w, env).Err, "no such host", "the first stderr line names the failure")
}

func TestExecShellString(t *testing.T) {
	dir := t.TempDir()
	w := &crew.Watch{Name: "x", Kind: crew.WatchExec, Every: "1m", Run: crew.Command{Shell: `printf '{"key":"k","fingerprint":"%s","text":"t"}\n' "$ZMINA_WATCH"`}}
	require.NoError(t, w.Check(&crew.Crew{Dir: dir}))

	res := Run(context.Background(), w, Env{Dir: Dir(dir, "x")})
	require.NoError(t, res.Err)
	assert.Equal(t, "x", res.Events[0].Fingerprint)
}

func TestOutputIsBounded(t *testing.T) {
	old := MaxOutput
	MaxOutput = 1000

	t.Cleanup(func() { MaxOutput = old })

	w, env := execWatch(t, `i=0; while [ $i -lt 100 ]; do echo '{"key":"k'$i'","fingerprint":"1","text":"x"}'; i=$((i+1)); done`)

	res := Run(context.Background(), w, env)
	require.Error(t, res.Err)
	assert.Contains(t, res.Err.Error(), "printed more than 1000 bytes", "part of an event set is not used")

	c := &capped{max: 10}
	n, err := c.Write([]byte("0123456789abcdef"))
	require.NoError(t, err)
	assert.Equal(t, 16, n, "writes keep being accepted")
	assert.Equal(t, "0123456789", c.String())
	assert.True(t, c.over)
}
