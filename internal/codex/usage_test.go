package codex

import (
	"testing"
	"time"

	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsageSurvivesResumeAndDeduplicates(t *testing.T) {
	reg := Registry{Dir: t.TempDir()}
	at := time.Now()
	save := func(n int, turn string, total, last Tokens, charge bool) {
		u := Usage{Thread: "thread", Turn: turn, Role: "dev#2", Group: "dev", Total: total, Last: last, Charge: charge,
			Message: registry.Message{At: at.Add(time.Duration(n) * time.Second), Model: "gpt-6.1-sol", Cwd: "/work"}}
		require.NoError(t, reg.SaveUsage(u))
	}
	// A restored thread already spent tokens; those are a baseline, not new spend.
	save(0, "old", Tokens{Input: 1000, Output: 100}, Tokens{}, false)
	save(1, "t1", Tokens{Input: 1100, Cached: 40, Output: 120}, Tokens{Input: 100, Cached: 40, Output: 20}, true)
	save(1, "t1", Tokens{Input: 1100, Cached: 40, Output: 120}, Tokens{Input: 100, Cached: 40, Output: 20}, true)
	// Second API response in the same user turn is independently billable.
	save(2, "t1", Tokens{Input: 1300, Cached: 100, Output: 150}, Tokens{Input: 200, Cached: 60, Output: 30}, true)
	save(3, "restored", Tokens{Input: 1300, Cached: 100, Output: 150}, Tokens{}, false)
	save(4, "t2", Tokens{Input: 1350, Cached: 110, Output: 160}, Tokens{Input: 50, Cached: 10, Output: 10}, true)
	got, err := reg.UsageSessions(time.Time{})
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, 60, got[0].Message.Input)
	assert.Equal(t, 40, got[0].Message.Read)
	assert.Equal(t, 20, got[0].Message.Output)
	assert.Equal(t, 140, got[1].Message.Input)
	assert.Empty(t, got[1].Message.UnpricedReason)
	assert.Equal(t, "dev#2", got[2].Role)
	assert.Equal(t, "dev", got[2].Group)
	// Reading a narrow window still needs the older baseline to compute a delta.
	got, err = reg.UsageSessions(at.Add(3500 * time.Millisecond))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 40, got[0].Message.Input)
	assert.Equal(t, 10, got[0].Message.Output)
	// A new Registry instance after process exit sees the same durable accounting.
	again, err := (&Registry{Dir: reg.Dir}).UsageSessions(at.Add(3500 * time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, got, again)
}

func TestUsageResetAndUnknownCacheWrite(t *testing.T) {
	reg := Registry{Dir: t.TempDir()}
	now := time.Now()
	for i, u := range []Usage{
		{Thread: "thread", Turn: "old", Total: Tokens{Input: 1000, Output: 100}},
		{Thread: "thread", Turn: "new", Total: Tokens{Input: 100, Cached: 20, Write: 30, Output: 10}, Last: Tokens{Input: 100, Cached: 20, Write: 30, Output: 10}, Charge: true},
	} {
		u.Message.At = now.Add(time.Duration(i) * time.Second)
		require.NoError(t, reg.SaveUsage(u))
	}
	us, err := reg.UsageSessions(time.Time{})
	require.NoError(t, err)
	require.Len(t, us, 1)
	assert.Equal(t, 50, us[0].Message.Input)
	assert.Equal(t, 20, us[0].Message.Read)
	assert.Equal(t, 30, us[0].Message.Write)
}

func TestLateUsageUsesResolvedModel(t *testing.T) {
	r, _, _ := testRunner(t)
	r.o.Role = "dev#2"
	r.o.Dir = "/work"
	r.o.Model = "requested-model"
	require.NoError(t, r.startThread(""))
	response(t, r, `{"thread":{"id":"thread"},"model":"gpt-6.1-sol","modelProvider":"openai","serviceTier":"priority"}`)
	require.NoError(t, r.input("[zmina T-1] fix"))
	response(t, r, `{"turn":{"id":"t1"}}`)
	notify(t, r, "turn/completed", `{"turn":{"id":"t1","status":"completed"}}`)
	notify(t, r, "thread/tokenUsage/updated", `{"turnId":"t1","tokenUsage":{"last":{"inputTokens":100,"cachedInputTokens":40,"outputTokens":10,"totalTokens":110},"total":{"inputTokens":100,"cachedInputTokens":40,"outputTokens":10,"totalTokens":110}}}`)
	us, err := r.o.Registry.UsageSessions(time.Time{})
	require.NoError(t, err)
	require.Len(t, us, 1)
	assert.Equal(t, "gpt-6.1-sol", us[0].Message.Model)
	assert.Equal(t, "fast", us[0].Message.Speed)
	assert.Equal(t, registry.TriggerHandoff, us[0].Message.Trigger)
	assert.Equal(t, 60, us[0].Message.Input)
}
