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
	// longTalkAt is how full a conversation is long when the role names no restart_at: Claude Code's turns cost the
	// same up to 200k and the whole of it is read again with every turn after.
	longTalkAt = 250_000
)

// longTalk files one check for the owner about a role whose conversation has grown long and that nothing of zmina's
// will shorten: its restarts are off, or the restart of this conversation was called off (the owner wrote into it, a
// note never came). Every turn of such a conversation reads the whole of it again, and the owner is the one who
// decides whether it goes on: he is told once a conversation. The dispatcher is left out: its recovery pack and
// context_max keep it. It returns a line for the log.
func (s *Shift) longTalk(name string, r *crew.Role, now time.Time) string {
	if s.Asks == nil || name == s.Crew.Dispatcher {
		return ""
	}

	rs := s.State.Role(name)
	if !rs.Up || !rs.Downing.IsZero() || !rs.RestartAsked.IsZero() {
		return ""
	}

	sess := s.session(name)
	if sess == nil || sess.SessionID == "" {
		return ""
	}

	// A restart that is still to come shortens it: nothing for the owner to decide.
	if s.restarts(r) && rs.RestartGaveUp != sess.SessionID {
		return ""
	}

	turn, err := s.registryFor(name).Last(sess.SessionID)
	if err != nil {
		return ""
	}

	at := r.RestartContext()
	if at == 0 {
		at = longTalkAt
	}

	if turn.Context < at {
		return ""
	}

	mark := "conversation " + shortSession(sess.SessionID)

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
		Text: fmt.Sprintf("%s's conversation holds %s and nothing shortens it by itself. Every turn reads all of it again; "+
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
