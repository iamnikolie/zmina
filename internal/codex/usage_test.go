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

// What started a Codex turn is told from the prompt as zmina's bridge inbox gave it: behind zmina's tag, one line or
// several joined or a doorbell, none of it the owner's.
func TestUsageTriggerOfInboxLines(t *testing.T) {
	const (
		tag  = "[zmina:abcdefghijkl] "
		bell = "[zmina] 2 lines wait for you (dev on T-1): zmina inbox show in-0123456789ab"
		chat = "[zmina chat 0123456789abcdef] go on"
	)

	for _, c := range []struct{ name, text, want string }{
		{"handoff", tag + "[zmina T-1] fix", registry.TriggerHandoff},
		{"notice", tag + "[zmina] dev idle 40m", registry.TriggerNotice},
		{"remote line", tag + `[zmina] remote acme/ops: "hi"`, registry.TriggerRemote},
		{"line of another role", tag + "[dispatcher] look at T-2", registry.TriggerDelivery},
		{"joined handoffs", tag + "1. [zmina T-1] fix\n2. [zmina T-2] fix", registry.TriggerHandoff},
		{"joined lines", tag + "1. [dispatcher] look at T-2\n2. [zmina] dev idle 40m", registry.TriggerDelivery},
		{"joined with the owner's chat line", tag + "1. [zmina] dev idle 40m\n2. " + chat, registry.TriggerOwner},
		{"doorbell", tag + bell, registry.TriggerDelivery},
		{"owner's list", "1. buy milk\n2. call alex", registry.TriggerOwner},
		{"owner quotes zmina", "see [zmina T-1] above", registry.TriggerOwner},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _, _ := testRunner(t)
			r.o.Role, r.o.Dir, r.o.Model = "dev", "/work", "gpt-6.1-sol"
			r.rec.Inbox = InboxDir(t.TempDir(), "zm-dev")
			require.NoError(t, openInbox(r.rec.Inbox))
			require.NoError(t, r.startThread(""))
			response(t, r, `{"thread":{"id":"thread"},"model":"gpt-6.1-sol","modelProvider":"openai"}`)

			_, err := Inbox{}.Post("", &registry.Session{Inbox: r.rec.Inbox}, c.text)
			require.NoError(t, err)
			require.NoError(t, r.drainInbox())
			response(t, r, `{"turn":{"id":"t1"}}`)
			notify(t, r, "turn/completed", `{"turn":{"id":"t1","status":"completed"}}`)
			notify(t, r, "thread/tokenUsage/updated", `{"turnId":"t1","tokenUsage":{"last":{"inputTokens":100,"cachedInputTokens":40,"outputTokens":10,"totalTokens":110},"total":{"inputTokens":100,"cachedInputTokens":40,"outputTokens":10,"totalTokens":110}}}`)

			us, err := r.o.Registry.UsageSessions(time.Time{})
			require.NoError(t, err)
			require.Len(t, us, 1)
			assert.Equal(t, c.want, us[0].Message.Trigger)
		})
	}
}
