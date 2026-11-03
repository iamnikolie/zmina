package cmd

import (
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func TestRestartExplicitModes(t *testing.T) {
	for _, mode := range []string{"--fresh", "--continue"} {
		t.Run(mode, func(t *testing.T) {
			file := testCrew(t, "  dev:\n    resume: always\n")
			a := &app{crew: file}
			require.NoError(t, a.withState(func(s *shift.Shift) error { s.State.Role("dev").LastSession = "previous-session"; return nil }))
			_, _, err := zm(t, file, "restart", "dev", mode)
			require.NoError(t, err)
			var screen string
			require.Eventually(t, func() bool { screen, _, _ = zm(t, file, "peek", "dev"); return strings.Contains(screen, "claude ") }, 15*time.Second, 50*time.Millisecond)
			if mode == "--fresh" {
				assert.NotContains(t, screen, "--resume")
			} else {
				assert.Contains(t, screen, "--resume previous-session")
			}
		})
	}
	file := testCrew(t, "  dev: {}\n")
	_, _, err := zm(t, file, "restart", "dev", "--fresh", "--continue")
	require.Error(t, err)
}

func TestRestartCopyPreservesContinuationBeforeTeardown(t *testing.T) {
	for _, mode := range []string{"--continue", "policy", "--fresh"} {
		t.Run(mode, func(t *testing.T) {
			file := testCrew(t, "  dev:\n    resume: always\n")
			_, _, err := zm(t, file, "up", "dev", "--as", "2")
			require.NoError(t, err)
			require.Eventually(t, func() bool { out, _, _ := zm(t, file, "peek", "dev#2"); return strings.Contains(out, "claude ") }, 15*time.Second, 50*time.Millisecond)
			a := &app{crew: file}
			require.NoError(t, a.withState(func(s *shift.Shift) error { s.State.Role("dev#2").LastSession = "saved-copy-session"; return nil }))
			args := []string{"restart", "dev#2"}
			if mode != "policy" {
				args = append(args, mode)
			}
			_, _, err = zm(t, file, args...)
			require.NoError(t, err)
			var screen string
			require.Eventually(t, func() bool { screen, _, _ = zm(t, file, "peek", "dev#2"); return strings.Contains(screen, "claude ") }, 15*time.Second, 50*time.Millisecond)
			if mode == "--fresh" {
				assert.NotContains(t, screen, "--resume")
			} else {
				assert.Contains(t, screen, "--resume saved-copy-session")
			}
		})
	}
}

func TestRestartIncompatibleCopyFailsBeforeStopping(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	_, _, err := zm(t, file, "up", "dev", "--as", "2")
	require.NoError(t, err)
	require.Eventually(t, func() bool { out, _, _ := zm(t, file, "peek", "dev#2"); return strings.Contains(out, "claude ") }, 15*time.Second, 50*time.Millisecond)
	a := &app{crew: file}
	require.NoError(t, a.withState(func(s *shift.Shift) error {
		rs := s.State.Role("dev#2")
		rs.LastSession = "saved"
		rs.LastRuntime = "codex"
		return nil
	}))
	_, _, err = zm(t, file, "restart", "dev#2", "--continue")
	require.ErrorContains(t, err, "last session belongs to codex")
	_, _, err = zm(t, file, "peek", "dev#2")
	require.NoError(t, err, "invalid continuation must leave the live copy running")
}
