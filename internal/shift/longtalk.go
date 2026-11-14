package shift

import (
	"fmt"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/crew"
)

const (
	// longTalkBy is who files the check about a long conversation.
	longTalkBy = "context"
	// longTalkAt is how full a conversation is long at the least, whatever the role's restart_at: past 200k a Claude
	// turn is priced higher, and the whole of it is read again with every turn after.
	longTalkAt = 200_000
)

// longTalk files one check for the owner about a Claude role whose conversation has grown long and that nothing of
// zmina's is about to shorten: it holds no task a restart_at would restart it for (a role the owner talks to), that
// restart was given up for this conversation (no note came, a fresh start failed), and its context_max is off, not
// reached yet, or its trim left it over the limit. Every turn of such a conversation reads the whole of it again,
// and the owner is the one who decides whether it goes on: he is told once a conversation. The dispatcher is left
// out: its recovery pack and context_max keep it. It returns a line for the log.
func (s *Shift) longTalk(name string, r *crew.Role, now time.Time, tasks []string) string {
	if s.Asks == nil || name == s.Crew.Dispatcher || r.IsCodex() {
		return ""
	}

	rs := s.State.Role(name)
	if !rs.Up || !rs.Downing.IsZero() || !rs.Releasing.IsZero() || !rs.RestartAsked.IsZero() {
		return ""
	}

	sess := s.session(name)
	if sess == nil || sess.SessionID == "" {
		return ""
	}

	// A restart that is still to come shortens it: nothing for the owner to decide.
	if r.RestartContext() > 0 && len(tasks) > 0 && rs.RestartGaveUp != sess.SessionID {
		return ""
	}

	turn, err := s.registryFor(name).Last(sess.SessionID)
	if err != nil {
		return ""
	}

	at := max(r.RestartContext(), longTalkAt)

	if turn.Context < at {
		return ""
	}

	// A trim that is due compacts it at the next idle moment; one that left it over the limit will not come again.
	limit := r.ContextLimit()
	stuck := rs.TrimSession == sess.SessionID && rs.TrimBase > limit

	if limit > 0 && turn.Context >= limit && !stuck {
		return ""
	}

	mark := "conversation " + name + " " + shortSession(sess.SessionID)

	all, _, err := s.Asks.Scan()
	if err != nil {
		return ""
	}

	for _, a := range all {
		if a.By == longTalkBy && strings.Contains(a.Text, mark) {
			return ""
		}
	}

	ask, err := s.Asks.Add(asks.Ask{Kind: asks.Check, Role: "ticker", By: longTalkBy,
		Title: fmt.Sprintf("%s: a long conversation, %s", name, Tokens(turn.Context)),
		Text: fmt.Sprintf("%s's conversation holds %s and nothing is about to shorten it. Every turn reads all of it again; "+
			"turns this full are about half of what the crews cost. To start it again from its note: have it save "+
			"zmina task checkpoint for what it holds, then zmina restart %s --fresh. To keep it as it is, ignore this: "+
			"it is said once a conversation. (%s)", name, Tokens(turn.Context), name, mark)}, now)
	if err != nil {
		return name + ": long conversation: " + err.Error()
	}

	s.log(name, "long-talk", "", fmt.Sprintf("%s at %s, told the owner (%s)", mark, Tokens(turn.Context), ask.ID))

	return fmt.Sprintf("%s: a long conversation (%s): told the owner (%s)", name, Tokens(turn.Context), ask.ID)
}

// shortSession is the head of a conversation's id, enough to tell one from another in a text.
func shortSession(id string) string {
	if len(id) > 8 {
		return id[:8]
	}

	return id
}
