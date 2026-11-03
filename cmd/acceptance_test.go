package cmd

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestLegacyTaskAcceptNeedsAnActualHandoff(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	_, _, err := zm(t, file, "task", "new", "dev", "Review")
	require.NoError(t, err)
	_, _, err = zm(t, file, "task", "accept", "1", "--handoff", "missing")
	require.ErrorContains(t, err, "handoff missing")
}

func TestExplicitAcceptanceRoleCannotOverrideDifferentCallerRole(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	_, _, err := zm(t, file, "task", "new", "dev", "Review")
	require.NoError(t, err)
	t.Setenv("ZMINA_ROLE", "reviewer")
	_, _, err = zm(t, file, "task", "accept", "1", "--role", "dev", "--handoff", "x")
	require.ErrorContains(t, err, "cannot claim")
}
