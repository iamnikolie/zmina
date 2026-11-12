package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A session file Claude Code left empty or cut short (a full disk does that) is named for what it is, with what to do,
// not a bare parse error; a file that is not there is still "no entry yet".
func TestSessionFileThatIsEmptyOrCutShort(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	whole := entry(7, "zm-dev", "idle", time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))

	files := map[string]string{
		"11.json": "",
		"12.json": "\n  \n",
		"13.json": whole[:len(whole)/2],
		"14.json": `{"pid": 14, "status": `,
		"15.json": "not json at all",
	}
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}

	r := &Registry{Namespace: ns}

	for name, pid := range map[string]int{"11.json": 11, "12.json": 12} {
		sess, err := r.ByPID(pid)
		require.Error(t, err, name)
		assert.Nil(t, sess, name)

		var bad *BadFileError
		require.ErrorAs(t, err, &bad, name)
		assert.True(t, bad.Empty, "%s: nothing in it", name)
		assert.Equal(t, filepath.Join(dir, name), bad.Path)
		assert.Contains(t, err.Error(), "is empty")
		assert.Contains(t, err.Error(), "likely written while the disk was full")
		assert.Contains(t, err.Error(), "restart the role to get a fresh one")
		assert.NotContains(t, err.Error(), "unexpected end of JSON input", "not a bare parse error")
	}

	for name, pid := range map[string]int{"13.json": 13, "14.json": 14, "15.json": 15} {
		sess, err := r.ByPID(pid)
		require.Error(t, err, name)
		assert.Nil(t, sess, name)

		var bad *BadFileError
		require.ErrorAs(t, err, &bad, name)
		assert.False(t, bad.Empty, name)
		assert.Contains(t, err.Error(), "does not parse")
		assert.Contains(t, err.Error(), "restart the role to get a fresh one")

		var syntax *json.SyntaxError
		assert.True(t, errors.As(err, &syntax), "%s: the parse error stays reachable", name)
	}

	sess, err := r.ByPID(99)
	require.NoError(t, err)
	assert.Nil(t, sess, "no file: no entry yet, not a bad file")

	// A bad file is no live session by name, whoever it belongs to.
	assert.Nil(t, r.LiveByName("zm-dev"))
}

// ReadEntry names the same two cases, and a file with no field in it counts as empty.
func TestReadEntryNamesAnEmptyOrCutShortFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))

		return p
	}

	for name, body := range map[string]string{"zero": "", "object": "{}", "null": "null"} {
		_, err := ReadEntry(write(name+".json", body))

		var bad *BadFileError
		require.ErrorAs(t, err, &bad, name)
		assert.True(t, bad.Empty, name)
	}

	_, err := ReadEntry(write("cut.json", `{"pid": 1, "sessionId": "x"`))

	var bad *BadFileError
	require.ErrorAs(t, err, &bad)
	assert.False(t, bad.Empty)
	assert.True(t, strings.Contains(err.Error(), "does not parse"), err.Error())

	_, err = ReadEntry(filepath.Join(dir, "gone.json"))
	assert.ErrorIs(t, err, os.ErrNotExist, "a file that is not there is not a bad file")

	got, err := ReadEntry(write("ok.json", entry(5, "zm-dev", "idle", time.Now())))
	require.NoError(t, err)
	assert.Equal(t, "zm-dev", got["name"])
}
