package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/shift"
)

// todoActions are what the page may do to an item of the todo or the parked list.
var todoActions = map[string]bool{"yes": true, "done": true, "answer": true, "park": true, "ignore": true, "back": true,
	"drop": true, "mine": true}

// localTodoAct is the page's answer to an item of one of this node's crews: the answer goes to that crew's dispatcher;
// «mine» changes the task itself. It is also what a peer's cockpit asks of this node (todo/act).
func (a *app) localTodoAct(key, action, id, text string) (string, error) {
	if !todoActions[action] {
		return "", errors.New("action: yes, done, answer, park, ignore, back, drop or mine")
	}

	c, err := a.crewApp(key)
	if err != nil {
		return "", err
	}

	var line string

	err = c.withState(func(s *shift.Shift) error {
		var err error
		if action == "mine" {
			line, err = c.takeBack(s, id)
		} else {
			line, err = answerTodo(s, "ui", action, id, text)
		}

		return err
	})

	return line, err
}

// localAlarmAct is the page's set, move and off for an alarm of one of this node's crews, and a peer cockpit's
// (alarm/act).
func (a *app) localAlarmAct(key, action, id, when, text, role string) (string, error) {
	c, err := a.crewApp(key)
	if err != nil {
		return "", err
	}

	var line string

	err = c.withState(func(s *shift.Shift) error {
		s.By = "ui"

		var err error
		line, err = alarmAction(s, action, id, when, text, role)

		return err
	})

	return line, err
}

// peerCrew reads a page's crew key that names a crew of another node, <node>/<crew>, as the cockpit labels a peer's
// crews; ok false for this node's own crews.
func peerCrew(key string) (node, crew string, ok bool) {
	node, crew, ok = strings.Cut(key, "/")
	if !ok || node == "" || crew == "" {
		return "", "", false
	}

	me, err := openNode()
	if err != nil || node == me.Name {
		return "", "", false
	}

	return node, crew, true
}

// peerActTimeout is how long an answer to a peer's item may take: one session there, through its relays if any.
const peerActTimeout = 30 * time.Second

// peerAct sends the owner's answer to the node the item lives on, signed with the owner's key: that node does it as
// its own page would, and says what it did.
func (a *app) peerAct(ctx context.Context, node, op string, req mesh.TerminalRequest) (string, error) {
	me, err := openNode()
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, peerActTimeout)
	defer cancel()

	line, err := me.Act(ctx, mesh.Exec{}, node, op, req)
	if err != nil {
		return "", fmt.Errorf("%s: %w", node, err)
	}

	return line, nil
}
