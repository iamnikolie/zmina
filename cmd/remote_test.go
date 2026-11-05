package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeFar is a hand node's command line: it answers node ls --json with its crews and prints every other call, and
// exits with the code in exit next to it.
const fakeFar = `#!/bin/sh
dir=$(dirname "$0")
if [ "$*" = "node ls --json" ]; then
  if [ -f "$dir/files" ]; then
    echo '{"name":"far","crews":["default","ops"],"crew_files":{"default":"/srv/far/.zmina/crew.yaml","ops":"/srv/far/.zmina/ops/crew.yaml"}}'
  else
    echo '{"name":"far","crews":["default","ops"]}'
  fi
  exit 0
fi
echo "far: $*"
echo "far err: $*" >&2
exit "$(cat "$dir/exit" 2>/dev/null || echo 0)"
`

// buildEnv is the environment before any test moved HOME.
var buildEnv = os.Environ()

// remoteCrew is a crew on a node "here" with one hand peer "far", reached through fakeFar.
func remoteCrew(t *testing.T) (file, dir string) {
	t.Helper()
	file = testCrew(t, "  dev: {}\n")
	dir = t.TempDir()
	far := filepath.Join(dir, "far")
	require.NoError(t, os.WriteFile(far, []byte(fakeFar), 0o700))

	_, _, err := zm(t, file, "node", "init", "--name", "here", "--owner")
	require.NoError(t, err)
	_, _, err = zm(t, file, "node", "peer", "add", "far", "--cmd", far)
	require.NoError(t, err)

	return file, dir
}

func TestRemoteLsAndPeekRunTheFarZmina(t *testing.T) {
	file, _ := remoteCrew(t)

	out, errOut, err := zm(t, file, "ls", "far/ops", "--all")
	require.NoError(t, err)
	assert.Equal(t, "far: --config ops ls --all=true\n", out)
	assert.Equal(t, "far err: --config ops ls --all=true\n", errOut, "its stderr passes")

	_, _, err = zm(t, file, "ls", "far/default", "dev")
	require.ErrorContains(t, err, "update zmina on far", "a far zmina that names no crew files: its default crew is never guessed")

	out, _, err = zm(t, file, "peek", "far/ops", "dev#2", "-n", "5")
	require.NoError(t, err)
	assert.Equal(t, "far: --config ops peek dev#2 --lines=5\n", out)
}

// A far zmina that says where its crews are is given the crew's file: --crew, which every zmina reads alike.
func TestRemoteNamesTheFarCrewFile(t *testing.T) {
	file, dir := remoteCrew(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "files"), nil, 0o600))

	out, _, err := zm(t, file, "ls", "far/default", "dev")
	require.NoError(t, err)
	assert.Equal(t, "far: --crew /srv/far/.zmina/crew.yaml ls dev\n", out)

	out, _, err = zm(t, file, "peek", "far/ops", "dev")
	require.NoError(t, err)
	assert.Equal(t, "far: --crew /srv/far/.zmina/ops/crew.yaml peek dev\n", out)
}

func TestRemoteExitCodePasses(t *testing.T) {
	file, dir := remoteCrew(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "exit"), []byte("3"), 0o600))

	_, _, err := zm(t, file, "peek", "far/ops", "dev")
	var ee *exitError
	require.True(t, errors.As(err, &ee), "%v", err)
	assert.Equal(t, 3, ee.code)
}

func TestRemoteRefusals(t *testing.T) {
	file, _ := remoteCrew(t)

	_, _, err := zm(t, file, "ls", "nowhere/ops")
	require.ErrorContains(t, err, `no node "nowhere" (nodes: here, far; zmina node ls)`)

	_, _, err = zm(t, file, "peek", "far/web", "dev")
	require.ErrorContains(t, err, `no crew "web" on node far (crews: default, ops)`)

	_, _, err = zm(t, file, "peek", "far/ops", "dev;rm")
	require.ErrorContains(t, err, "cannot go to another node's command line")

	_, _, err = zm(t, file, "peek", "far", "dev")
	require.ErrorContains(t, err, `"far" is no <node>/<crew> address`, "a bare node is no crew address")

	_, _, err = zm(t, file, "peek", "far/ops")
	require.ErrorContains(t, err, "peek takes a role after the address", "said here, not by the far zmina")

	_, _, err = zm(t, file, "attach", "far/ops", "dev")
	require.ErrorContains(t, err, "no terminal here", "attach needs a terminal")

	_, _, err = zm(t, file, "ls", "here/web")
	require.ErrorContains(t, err, `no crew "web" here`, "this node's own crews by name")

	home, _ := os.UserHomeDir()
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".zmina"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".zmina", "crew.yaml"), raw, 0o600))
	for _, c := range []string{"peek", "attach"} {
		_, _, err = zm(t, file, c, "here/default")
		require.ErrorContains(t, err, c+" takes a role after the address", "an address with no role")
	}

	_, _, err = zm(t, file, "node", "peer", "add", "pinned", "--cmd", "ssh box zmina --crew /srv/ops.yaml")
	require.NoError(t, err)
	_, _, err = zm(t, file, "ls", "pinned/ops")
	require.ErrorContains(t, err, "its address picks a crew itself")
	_, _, err = zm(t, file, "node", "peer", "add", "pinned2", "--cmd", "ssh box zmina --config ops")
	require.NoError(t, err)
	_, _, err = zm(t, file, "ls", "pinned2/ops")
	require.ErrorContains(t, err, "its address picks a crew itself")

	_, _, err = zm(t, file, "node", "peer", "add", "quoted", "--cmd", "ssh -i '/tmp/key file' box zmina")
	require.NoError(t, err)
	_, _, err = zm(t, file, "ls", "quoted/ops")
	require.ErrorContains(t, err, "add the node with --ssh", "an ssh line zmina cannot change safely")

	_, _, err = zm(t, file, "peek", "dev", "designer")
	require.ErrorContains(t, err, `"dev" is no <node>/<crew> address`, "a local peek keeps its one argument")
}

// buildZmina builds zmina at the commit rev ("" for this tree) into dir.
func buildZmina(t *testing.T, dir, rev string) string {
	t.Helper()
	src := ".."
	if rev != "" {
		if exec.Command("git", "-C", "..", "cat-file", "-e", rev+"^{commit}").Run() != nil {
			t.Skip("no commit " + rev + " in this clone")
		}

		src = filepath.Join(dir, "src-"+rev)
		require.NoError(t, os.MkdirAll(src, 0o700))
		archive := exec.Command("sh", "-c", "git archive "+rev+" | tar -x -C "+src)
		archive.Dir = ".."
		out, err := archive.CombinedOutput()
		require.NoError(t, err, string(out))
	}

	bin := filepath.Join(dir, "zmina-"+rev)
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir, build.Env = src, buildEnv // the go caches of the HOME the tests started with
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	return bin
}

// farNode is a node "far" run by the zmina at bin, with a default crew (role alpha) and an ops crew (role beta),
// its $ZMINA_CREW set to ops; withDefaultDir adds a crew dir named default (role gamma). It is this crew's peer.
func farNode(t *testing.T, file, bin string, withDefaultDir bool) {
	t.Helper()
	far := t.TempDir()
	crews := map[string]string{".zmina": "alpha", ".zmina/ops": "beta"}
	if withDefaultDir {
		crews[".zmina/default"] = "gamma"
	}
	for crewDir, role := range crews {
		require.NoError(t, os.MkdirAll(filepath.Join(far, crewDir), 0o700))
		body := "name: far" + role + "\nnamespace: " + filepath.Join(far, "ns") + "\ndir: " + far + "\nclaude: /bin/false\nroles:\n  dispatcher: {}\n  " + role + ": {}\n"
		require.NoError(t, os.WriteFile(filepath.Join(far, crewDir, "crew.yaml"), []byte(body), 0o600))
	}
	line := "env HOME=" + far + " ZMINA_CREW=" + filepath.Join(far, ".zmina", "ops", "crew.yaml") + " " + bin
	out, err := exec.Command("sh", "-c", line+" node init --name far --owner").CombinedOutput()
	require.NoError(t, err, string(out))
	_, _, _ = zm(t, file, "node", "peer", "rm", "far")
	_, _, err = zm(t, file, "node", "peer", "add", "far", "--cmd", line)
	require.NoError(t, err)
}

// A real far zmina, its $ZMINA_CREW set to another crew: <node>/default is its default crew all the same, and
// <node>/ops its ops crew — also next to a crew dir named default.
func TestRemoteDefaultIsTheDefaultCrew(t *testing.T) {
	if testing.Short() {
		t.Skip("builds zmina")
	}

	file, dir := remoteCrew(t)
	bin := buildZmina(t, dir, "")

	for _, withDefaultDir := range []bool{false, true} {
		farNode(t, file, bin, withDefaultDir)

		got, _, err := zm(t, file, "ls", "far/default", "--all")
		require.NoError(t, err)
		assert.Contains(t, got, "alpha")
		assert.NotContains(t, got, "beta", "its $ZMINA_CREW does not pick the crew")
		assert.NotContains(t, got, "gamma", "nor a crew dir named default")

		got, _, err = zm(t, file, "ls", "far/ops", "--all")
		require.NoError(t, err)
		assert.Contains(t, got, "beta")
	}
}

// A far zmina older than this one (it names no crew files): its named crews as before; its default crew is refused,
// with or without a crew dir named default, never another crew.
func TestRemoteOlderFarZmina(t *testing.T) {
	if testing.Short() {
		t.Skip("builds zmina")
	}

	file, dir := remoteCrew(t)
	old := buildZmina(t, dir, "6e1307a")

	for _, withDefaultDir := range []bool{false, true} {
		farNode(t, file, old, withDefaultDir)

		_, _, err := zm(t, file, "ls", "far/default", "--all")
		require.ErrorContains(t, err, "update zmina on far")

		got, _, err := zm(t, file, "ls", "far/ops", "--all")
		require.NoError(t, err)
		assert.Contains(t, got, "beta")
		assert.NotContains(t, got, "alpha")
	}
}
