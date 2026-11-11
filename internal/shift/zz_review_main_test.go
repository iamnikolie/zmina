package shift

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/task"
)

// Compiles on main and on the branch (FlushInbox only). A typed role gets a packet line, never takes it and dies;
// the task is handed again before the beat settles the dead session; the role is started again.
func TestReviewRehandOfATypedPacketToADeadRole(t *testing.T) {
	for _, how := range []string{"up", "beats"} {
		t.Run(how, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("designer"))
			*f.now = f.now.Add(bootPause)
			f.status("designer", "idle", *f.now)
			f.tm.onType = nil // the line is typed, the role never takes it
			f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open, Updated: f.now.UTC()}}

			_, err := f.s.HandTask("designer", f.tk.list[0])
			require.NoError(t, err)
			require.NotEmpty(t, f.s.FlushInbox("designer"))
			require.NotNil(t, f.s.State.Role("designer").Sent)

			*f.now = f.now.Add(20 * time.Second)
			f.tm.die("designer")

			// The dispatcher hands it again (task handoff --queue).
			_, err = f.s.HandTask("designer", f.tk.list[0])
			require.NoError(t, err)

			if how == "up" {
				_ = f.tm.Kill("designer")
				require.NoError(t, f.s.Up("designer"))
			} else {
				f.s.State.Stopped = *f.now // no always-starts, only the settle
				for range 3 {
					*f.now = f.now.Add(time.Minute)
					f.s.Tick()
				}
			}

			rs := f.s.State.Role("designer")
			t.Logf("sent=%v inbox=%v failed=%v", rs.Sent, rs.Inbox, rs.Failed)
			has := false
			for _, l := range rs.Inbox {
				has = has || strings.HasPrefix(l, "[zmina T-1] Read handoff packet ")
			}
			assert.True(t, has, "the packet handed again waits for the role's next session")
		})
	}
}
