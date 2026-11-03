package cmd

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAttachApprovalPoliciesAvoidNarrowCLIEnum(t *testing.T) {
	for _, policy := range []string{"on-request", "never", "untrusted", "on-failure", ""} {
		t.Run(policy, func(t *testing.T) {
			a, _, _ := attachFixture(t, "dev", "shared")
			raw, err := os.ReadFile(a.crew)
			require.NoError(t, err)
			replacement := "approval_policy: " + policy
			if policy == "" {
				replacement = ""
			}
			require.NoError(t, os.WriteFile(a.crew, []byte(strings.Replace(string(raw), "approval_policy: on-request", replacement, 1)), 0600))
			process, err := a.attachProcess("dev", false)
			require.NoError(t, err)
			require.NotContains(t, process.Args, "--ask-for-approval")
			if policy != "" && policy != "untrusted" {
				require.Contains(t, process.Args, "approval_policy="+strconv.Quote(policy))
			} else {
				for _, arg := range process.Args {
					require.False(t, strings.HasPrefix(arg, "approval_policy="), "inherit the existing daemon policy; never replace retired untrusted with weaker defaults")
				}
			}
		})
	}
}
