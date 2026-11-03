package handoff

import (
	"encoding/json"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestPacketPreservesMultilineAndLatestNote(t *testing.T) {
	dir := t.TempDir()
	p := Packet{Version: 1, Role: "designer", Task: task.Task{ID: "T-1", Asked: "First line\nSecond line", Note: "Current decision\nOpen question"}}
	path, err := Write(dir, p)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var got Packet
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, p, got)
	same, err := Write(dir, p)
	require.NoError(t, err)
	assert.Equal(t, path, same)
	p.Task.Note = "New decision"
	next, err := Write(dir, p)
	require.NoError(t, err)
	assert.NotEqual(t, path, next)
	old, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, raw, old)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	p.Task.ID = "../escape"
	_, err = Write(dir, p)
	require.Error(t, err)
}
