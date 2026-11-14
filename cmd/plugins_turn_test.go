package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/plugin"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
)

// A role that ends its turn on a question put in words: nothing on the todo, no dialog on its screen. Its Stop hook recorded the
// question; these tests keep that record, and the session file, as the hook and Claude Code write them.

const (
	// askedInWords is a last reply that ends on a question to the owner.
	askedInWords = "I found the cause of the failing deploy.\n\nFix it now? I also need to know whether it was dev or prod.\nThe log does not say."
	// askedInWordsHeld is what the record keeps of it: from the paragraph of the question on.
	askedInWordsHeld = "Fix it now? I also need to know whether it was dev or prod.\nThe log does not say."
)

// session writes the role's session file: its status as of a moment, in a session.
func (f *dialogScreenTmux) session(t *testing.T, ns, id, status string, since time.Time) {
	t.Helper()

	f.mu.Lock()
	f.input = true
	f.since = since
	f.mu.Unlock()

	body := fmt.Sprintf(`{"pid": %d, "sessionId": %q, "status": %q, "statusUpdatedAt": %d}`, f.pid, id, status, since.UnixMilli())
	require.NoError(t, os.MkdirAll(filepath.Join(ns, "sessions"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ns, "sessions", strconv.Itoa(f.pid)+".json"), []byte(body), 0o600))
}

// endTurn makes the role idle at its input line since now, and its Stop hook record the reply the turn ended on, as it does at that
// moment. It returns the moment.
func (f *dialogScreenTmux) endTurn(t *testing.T, file, ns, reply string) time.Time {
	t.Helper()

	at := time.Now()
	f.session(t, ns, "sid-1", "idle", at)
	require.NoError(t, shift.RecordTurn(crew.Layout{Dir: filepath.Dir(file)}, "dev", "sid-1", reply, at))

	return at
}

// turnWorld is a crew whose role dev is idle since a turn that ended on a question in words, and a plugin that is told at once what
// is open for a minute.
func turnWorld(t *testing.T, plugins string) (file string, f *dialogScreenTmux, seams func(s *shift.Shift), ns string, log string) {
	t.Helper()

	file, f, seams, ns = dialogWorld(t, plugins)
	log = recorder(t, file, "pager", "")
	f.endTurn(t, file, ns, askedInWords)

	return file, f, seams, ns, log
}

// turnFacts is the one question of a turn the world holds.
func turnFacts(t *testing.T, file string, seams func(s *shift.Shift)) (*pluginWorld, plugin.DialogFacts) {
	t.Helper()

	a := &app{crew: file, out: io.Discard, err: io.Discard, shiftSeams: seams}
	w, err := a.pluginWorld(mustCrew(t, file), time.Now())
	require.NoError(t, err)
	require.Len(t, w.dialogs, 1)

	for _, d := range w.dialogs {
		return w, d
	}

	return w, plugin.DialogFacts{}
}

// The question goes to the plugin once the role has stood idle on it a minute (so a reply that the owner answers at once pages
// nobody), once, with the words, and the argv that gives the owner's answer to the role through the plugin as its answer: no choices,
// nothing to press, and no mark armed. When the role takes another turn the plugin hears the end: gone.
func TestPluginsTellOfAQuestionTheTurnEndedOn(t *testing.T) {
	file, f, seams, ns, log := turnWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	now := time.Now().Add(-5 * time.Minute)

	deliver(t, file, now, seams)
	deliver(t, file, now.Add(30*time.Second), seams)
	assert.Empty(t, told(t, log), "it has not stood long enough")

	deliver(t, file, now.Add(time.Minute), seams)
	deliver(t, file, now.Add(2*time.Minute), seams)
	require.Len(t, told(t, log), 1, "once")

	ev := told(t, log)[0]
	assert.Equal(t, "dialog", ev.Event)
	id := ev.Line["id"].(string)
	assert.Regexp(t, `^dev:[0-9a-f]{16}:[0-9a-f]{16}$`, id, "role:fingerprint:mark, as any dialog's")
	assert.Equal(t, "turn", ev.Line["prompt"])
	assert.Equal(t, "dev", ev.Line["role"])
	assert.Equal(t, strings.ReplaceAll(askedInWordsHeld, "\n", " "), ev.Line["question"])
	assert.NotContains(t, ev.Line, "options")
	assert.NotContains(t, ev.Line, "needs_terminal")
	assert.NotContains(t, ev.Line, "tool")
	assert.NotContains(t, ev.Line, "subject")

	answer := ev.Line["answer"].(map[string]any)
	assert.Equal(t, []any{"zmina", "--crew", file, "send", "--by", "pager", "--dialog", id, "--", "dev", "<text>"}, answer["text"])
	assert.NotContains(t, answer, "options")

	// Nothing is pressed, so nothing is armed.
	assert.NoFileExists(t, crew.Layout{Dir: filepath.Dir(file)}.PluginArmed("pager"), "no mark")
	assert.Empty(t, f.pressed())

	// The role took another turn: the plugin hears the end of what it was told, once.
	f.session(t, ns, "sid-1", "busy", time.Now().Add(time.Second))
	deliver(t, file, now.Add(3*time.Minute), seams)
	deliver(t, file, now.Add(4*time.Minute), seams)
	assert.Equal(t, []string{"dialog " + id, "answered " + id}, toldIDs(t, log))

	end := told(t, log)[1].Line
	assert.Equal(t, "dialog", end["for"])
	assert.Equal(t, "gone", end["how"])
	assert.Nil(t, end["by"])
}

// What is on no screen cannot be pressed: a choose with the id of a turn's question is refused, whatever its mark.
func TestAChooseWithATurnQuestionsIdPressesNothing(t *testing.T) {
	file, f, seams, _, log := turnWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	now := time.Now().Add(-5 * time.Minute)

	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	require.Len(t, told(t, log), 1)

	id := told(t, log)[0].Line["id"].(string)

	t.Setenv("ZMINA_PLUGIN", "pager")

	_, err := runWith(t, file, seams, "choose", "dev", "1", "--dialog", id, "--by", "pager")
	require.Error(t, err)
	assert.Empty(t, f.pressed())
}

// toldTurn runs the delivery until the plugin has been told of the turn's question, and returns the id of its event.
func toldTurn(t *testing.T, file string, seams func(s *shift.Shift), log string) string {
	t.Helper()

	now := time.Now().Add(-5 * time.Minute)
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	require.Len(t, told(t, log), 1)

	return told(t, log)[0].Line["id"].(string)
}

// waitingLines is what waits in a role's queue.
func waitingLines(t *testing.T, file, role string) []string {
	t.Helper()

	st, err := shift.Read(filepath.Dir(file))
	require.NoError(t, err)

	if rs := st.Roles[role]; rs != nil {
		return rs.Inbox
	}

	return nil
}

// ownerNoted is what the answer commands noted of the owner: when he last answered in a terminal.
func ownerNoted(file string) time.Time {
	return plugin.ReadOwner(crew.Layout{Dir: filepath.Dir(file)}.PluginOwner()).Active
}

// The plugin runs the argv of the event with the owner's words in place of <text>, with ZMINA_PLUGIN set: the words go to the role as
// a line of zmina's that says whose they are, through which plugin and where they are recorded, queued as any line of zmina's to a role
// that is idle, with no word of the owner's own at the keyboard; the ledger has it; the question's record goes, and the plugin is told
// at the next look that it was answered, by whom, and with what; a second answer finds no question.
func TestAPluginAnswersAQuestionTheTurnEndedOn(t *testing.T) {
	file, f, seams, _, log := turnWorld(t, "  - {name: pager, quiet_when_active: 5m}\n")
	id := toldTurn(t, file, seams, log)

	t.Setenv("ZMINA_PLUGIN", "pager")

	// What the argv carries is the event's own: every flag before the --, then the role and the words, which may look like flags.
	words := "dev, and yes --by ghost -- fix it"
	argv := told(t, log)[0].Line["answer"].(map[string]any)["text"].([]any)
	require.Equal(t, "<text>", argv[len(argv)-1])

	args := []string{}
	for _, w := range argv[3 : len(argv)-1] {
		args = append(args, w.(string))
	}

	out, err := runWith(t, file, seams, append(args, words)...)
	require.NoError(t, err, out)
	assert.Regexp(t, `^(queued for|delivered to) dev`, out)

	// The role is given a line of zmina's, which says it is the owner's answer, given through the plugin, and where it is recorded.
	lines := waitingLines(t, file, "dev")
	require.Len(t, lines, 1)
	assert.True(t, strings.HasPrefix(lines[0], "[zmina] The owner's answer to your last question, given through plugin pager "+
		"and recorded in the ledger (zmina log): «dev, and yes --by ghost -- fix it»"), lines[0])

	// Nothing of the owner's own at the keyboard: no mark that he is at the machine, no message of his in the role's queue.
	assert.True(t, ownerNoted(file).IsZero(), "an answer through a plugin is no sign that he is at the machine")
	assert.Empty(t, f.pressed())

	st, err := shift.Read(filepath.Dir(file))
	require.NoError(t, err)
	assert.Empty(t, st.Roles["dev"].ChatReceipts, "it is no message of the owner's own")

	// The ledger has what he said, as an answer through the plugin.
	logged, _, err := zm(t, file, "log", "--full")
	require.NoError(t, err)
	assert.Contains(t, logged, "answered")
	assert.Contains(t, logged, "«dev, and yes --by ghost -- fix it» (answered through plugin pager)")

	// The record is gone, so the card ends at once, as an answer through the plugin and with the words.
	assert.NoFileExists(t, crew.Layout{Dir: filepath.Dir(file)}.HookTurn("dev"))

	now := time.Now().Add(-4 * time.Minute)
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	assert.Equal(t, []string{"dialog " + id, "answered " + id}, toldIDs(t, log))

	end := told(t, log)[1].Line
	assert.Equal(t, "dialog", end["for"])
	assert.Equal(t, "answered", end["how"])
	assert.Equal(t, "plugin pager", end["by"])
	assert.Equal(t, "dev, and yes --by ghost -- fix it", end["text"])

	// A second answer, a double tap or a retry, finds no question: nothing more is queued.
	_, err = runWith(t, file, seams, append(args, "again")...)
	require.ErrorContains(t, err, "waiting on no question")
	assert.Len(t, waitingLines(t, file, "dev"), 1)
}

// The owner's answer given through a plugin reaches the role as a line of zmina's, which the look at the role's conversation that tells
// when the owner last wrote to it (the one a push is held by, and a restart put off by) does not take for the owner writing, whether the
// line arrives posted or typed. The same words typed at the prompt as plain words are his.
func TestAnAnswerThroughAPluginIsNotTheOwnersOwnLineToTheRole(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	when := time.Now().Add(-time.Minute).UTC()
	typed := func(text string) string {
		return `{"type":"user","timestamp":"` + when.Format(time.RFC3339) + `","message":{"role":"user","content":` + strconv.Quote(text) + `}}` + "\n"
	}

	owner := func(text string) time.Time {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "sid-1.jsonl"), []byte(typed(text)), 0o600))

		a, err := (&registry.Registry{Namespace: ns}).Activity("sid-1")
		require.NoError(t, err)

		return a.Owner
	}

	assert.True(t, owner(turnAnswerLine("pager", "dev, and yes")).IsZero(), "typed or posted, it is zmina's line")
	assert.WithinDuration(t, when, owner("dev, and yes"), time.Second, "while the same words typed by hand are his")
}

// An answer is taken only through the plugin's own run, only to the question the card was told, and only while the role waits on it.
func TestAnAnswerToATurnQuestionIsTakenOnlyWhereItStands(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup runs after the plugin was told; it returns the words of the command after `send`, given the card's id.
		setup func(t *testing.T, f *dialogScreenTmux, file, ns, id string) []string
		want  string
	}{
		{"a plugin's own run only", func(t *testing.T, _ *dialogScreenTmux, _, _, id string) []string {
			t.Setenv("ZMINA_PLUGIN", "")

			return []string{"--by", "pager", "--dialog", id, "--", "dev", "yes"}
		}, "ZMINA_PLUGIN=pager is not set"},
		{"never a role's shell", func(t *testing.T, _ *dialogScreenTmux, _, _, id string) []string {
			t.Setenv("ZMINA_ROLE", "dev")

			return []string{"--by", "pager", "--dialog", id, "--", "dev", "yes"}
		}, "a role (dev) does not answer in a plugin's voice"},
		{"a plugin of the crew", func(t *testing.T, _ *dialogScreenTmux, _, _, id string) []string {
			t.Setenv("ZMINA_PLUGIN", "ghost")

			return []string{"--by", "ghost", "--dialog", id, "--", "dev", "yes"}
		}, "the crew file has no plugin of that name"},
		{"--by needs --dialog", func(*testing.T, *dialogScreenTmux, string, string, string) []string {
			return []string{"--by", "pager", "--", "dev", "yes"}
		}, "--by needs --dialog"},
		{"--dialog needs --by", func(_ *testing.T, _ *dialogScreenTmux, _, _, id string) []string {
			return []string{"--dialog", id, "--", "dev", "yes"}
		}, "--dialog needs --by"},
		{"no queueing with it", func(_ *testing.T, _ *dialogScreenTmux, _, _, id string) []string {
			return []string{"--by", "pager", "--dialog", id, "--when-idle", "--", "dev", "yes"}
		}, "takes neither --when-idle nor --task"},
		{"the question the card was told, not another", func(_ *testing.T, _ *dialogScreenTmux, _, _, id string) []string {
			return []string{"--by", "pager", "--dialog", "dev:0000000000000000:" + strings.Split(id, ":")[2], "--", "dev", "yes"}
		}, "not the one this card was told of"},
		{"an id with its mark", func(_ *testing.T, _ *dialogScreenTmux, _, _, id string) []string {
			return []string{"--by", "pager", "--dialog", strings.Join(strings.Split(id, ":")[:2], ":"), "--", "dev", "yes"}
		}, "give the id of the event as it was told"},
		{"the role's own id", func(_ *testing.T, _ *dialogScreenTmux, _, _, id string) []string {
			return []string{"--by", "pager", "--dialog", "ops" + strings.TrimPrefix(id, "dev"), "--", "dev", "yes"}
		}, "give the id of the event as it was told"},
		{"a role that is not in the crew", func(_ *testing.T, _ *dialogScreenTmux, _, _, id string) []string {
			return []string{"--by", "pager", "--dialog", "ops" + strings.TrimPrefix(id, "dev"), "--", "ops", "yes"}
		}, "not a role of this crew"},
		{"a role that took another turn", func(t *testing.T, f *dialogScreenTmux, _, ns, id string) []string {
			f.session(t, ns, "sid-1", "busy", time.Now().Add(time.Second))

			return []string{"--by", "pager", "--dialog", id, "--", "dev", "yes"}
		}, "waiting on no question"},
		{"a role in another session", func(t *testing.T, f *dialogScreenTmux, _, ns, id string) []string {
			f.session(t, ns, "sid-2", "idle", time.Now())

			return []string{"--by", "pager", "--dialog", id, "--", "dev", "yes"}
		}, "waiting on no question"},
		{"the card of an earlier question", func(t *testing.T, f *dialogScreenTmux, file, ns, id string) []string {
			at := time.Now().Add(20 * time.Second)
			f.session(t, ns, "sid-1", "idle", at)
			require.NoError(t, shift.RecordTurn(crew.Layout{Dir: filepath.Dir(file)}, "dev", "sid-1", "Done.\n\nShall I merge it?", at))

			return []string{"--by", "pager", "--dialog", id, "--", "dev", "yes"}
		}, "not the one this card was told of"},
		{"a record that is gone", func(t *testing.T, _ *dialogScreenTmux, file, _, id string) []string {
			require.NoError(t, os.Remove(crew.Layout{Dir: filepath.Dir(file)}.HookTurn("dev")))

			return []string{"--by", "pager", "--dialog", id, "--", "dev", "yes"}
		}, "waiting on no question"},
		{"a record that does not read", func(t *testing.T, _ *dialogScreenTmux, file, _, id string) []string {
			require.NoError(t, os.WriteFile(crew.Layout{Dir: filepath.Dir(file)}.HookTurn("dev"), []byte("{"), 0o600))

			return []string{"--by", "pager", "--dialog", id, "--", "dev", "yes"}
		}, "does not read"},
		{"words to say", func(_ *testing.T, _ *dialogScreenTmux, _, _, id string) []string {
			return []string{"--by", "pager", "--dialog", id, "--", "dev", "   "}
		}, "the answer is empty"},
		{"words that are not cut", func(_ *testing.T, _ *dialogScreenTmux, _, _, id string) []string {
			return []string{"--by", "pager", "--dialog", id, "--", "dev", strings.Repeat("é", plugin.TextMax+1)}
		}, "longer than"},
		{"one line", func(_ *testing.T, _ *dialogScreenTmux, _, _, id string) []string {
			return []string{"--by", "pager", "--dialog", id, "--", "dev", "yes\nand more"}
		}, "send takes one line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, f, seams, ns, log := turnWorld(t, "  - {name: pager, quiet_when_active: off}\n")
			id := toldTurn(t, file, seams, log)

			t.Setenv("ZMINA_PLUGIN", "pager")

			args := tc.setup(t, f, file, ns, id)

			_, err := runWith(t, file, seams, append([]string{"send"}, args...)...)
			require.ErrorContains(t, err, tc.want)

			assert.Empty(t, waitingLines(t, file, "dev"), "nothing was given to the role")
			assert.True(t, ownerNoted(file).IsZero())

			logged, _, err := zm(t, file, "log", "--full")
			require.NoError(t, err)
			assert.NotContains(t, logged, "answered through plugin", "nothing is recorded as answered")
		})
	}
}

// A role that took another turn since is not awaiting what the record says: busy, idle since a later moment, in another session, or
// with the record of an earlier turn left by a hook that did not run.
func TestPluginsDoNotTellOfAQuestionOfAnEarlierTurn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after func(t *testing.T, f *dialogScreenTmux, file, ns string, at time.Time)
		// told is true for the controls: what changes nothing that matters still is told.
		told bool
	}{
		{"control: nothing changed", func(*testing.T, *dialogScreenTmux, string, string, time.Time) {}, true},
		{"control: idle within moments of the hook", func(t *testing.T, f *dialogScreenTmux, _, ns string, at time.Time) {
			f.session(t, ns, "sid-1", "idle", at.Add(3*time.Second))
		}, true},
		{"busy again", func(t *testing.T, f *dialogScreenTmux, _, ns string, at time.Time) {
			f.session(t, ns, "sid-1", "busy", at.Add(time.Minute))
		}, false},
		{"idle since a later turn", func(t *testing.T, f *dialogScreenTmux, _, ns string, at time.Time) {
			f.session(t, ns, "sid-1", "idle", at.Add(time.Minute))
		}, false},
		{"idle since an earlier moment", func(t *testing.T, f *dialogScreenTmux, _, ns string, at time.Time) {
			f.session(t, ns, "sid-1", "idle", at.Add(-time.Minute))
		}, false},
		{"another session", func(t *testing.T, f *dialogScreenTmux, _, ns string, at time.Time) {
			f.session(t, ns, "sid-2", "idle", at)
		}, false},
		{"no session file", func(t *testing.T, f *dialogScreenTmux, _, ns string, _ time.Time) {
			require.NoError(t, os.Remove(filepath.Join(ns, "sessions", strconv.Itoa(f.pid)+".json")))
		}, false},
		{"a dialog on its screen", func(t *testing.T, f *dialogScreenTmux, _, ns string, at time.Time) {
			f.show(t, ns, true)
		}, false},
		{"no record", func(t *testing.T, _ *dialogScreenTmux, file, _ string, _ time.Time) {
			require.NoError(t, os.Remove(crew.Layout{Dir: filepath.Dir(file)}.HookTurn("dev")))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off, dialog_after: 0s}\n")
			log := recorder(t, file, "pager", "")
			at := f.endTurn(t, file, ns, askedInWords)
			tc.after(t, f, file, ns, at)

			now := time.Now().Add(-5 * time.Minute)
			deliver(t, file, now, seams)
			deliver(t, file, now.Add(time.Minute), seams)
			deliver(t, file, now.Add(2*time.Minute), seams)

			turns := 0

			for _, g := range told(t, log) {
				if g.Line["prompt"] == "turn" {
					turns++
				}
			}

			if tc.told {
				assert.Equal(t, 1, turns)
			} else {
				assert.Zero(t, turns, "a question of an earlier turn is told")
			}
		})
	}
}

// A question the turn ended on is told as it stands: when the role moves on and ends another turn, the first is gone, and a new
// question is a new thing with an id of its own; one that asks nothing is none.
func TestPluginsATurnQuestionThatIsReplacedByTheNextTurn(t *testing.T) {
	file, f, seams, ns, log := turnWorld(t, "  - {name: pager, quiet_when_active: off, dialog_after: 0s}\n")
	l := crew.Layout{Dir: filepath.Dir(file)}
	now := time.Now().Add(-10 * time.Minute)

	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Second), seams)
	require.Len(t, told(t, log), 1)

	first := told(t, log)[0].Line["id"].(string)

	// The owner answered at the keyboard; the next turn asks again, in other words.
	at := time.Now().Add(20 * time.Second)
	f.session(t, ns, "sid-1", "idle", at)
	require.NoError(t, shift.RecordTurn(l, "dev", "sid-1", "Done.\n\nShall I merge it?", at))

	deliver(t, file, now.Add(2*time.Second), seams)
	deliver(t, file, now.Add(3*time.Second), seams)

	ids := toldIDs(t, log)
	require.Len(t, ids, 3, ids)
	assert.Equal(t, "dialog "+first, ids[0])
	assert.Equal(t, "answered "+first, ids[1])
	assert.True(t, strings.HasPrefix(ids[2], "dialog dev:"), ids[2])
	assert.NotEqual(t, "dialog "+first, ids[2])
	assert.Equal(t, "Shall I merge it?", told(t, log)[2].Line["question"])

	// Then a turn that asks nothing: the hook takes the record away, and the question is gone.
	at = time.Now().Add(40 * time.Second)
	f.session(t, ns, "sid-1", "idle", at)
	require.NoError(t, shift.RecordTurn(l, "dev", "sid-1", "Merged.", at))

	deliver(t, file, now.Add(4*time.Second), seams)

	ids = toldIDs(t, log)
	require.Len(t, ids, 4, ids)
	assert.True(t, strings.HasPrefix(ids[3], "answered dev:"), ids[3])
	assert.Equal(t, "gone", told(t, log)[3].Line["how"])
}

// A role that has an open item of its own on the owner's todo, put there around the turn's end, asked properly: a card exists, and
// the words of its last reply are not told as well. An item of another role, or one from long before, does not hide it.
func TestPluginsDoNotTellOfAQuestionTheRoleAlsoPutOnTheTodo(t *testing.T) {
	plugins := "  - {name: pager, quiet_when_active: off, dialog_after: 0s}\n"

	asked := func(t *testing.T, file, from string) {
		t.Helper()

		_, _, err := zm(t, file, "todo", "add", "Which environment?", "--from", from)
		require.NoError(t, err)
	}

	turnTold := func(t *testing.T, log string) int {
		n := 0

		for _, g := range told(t, log) {
			if g.Line["prompt"] == "turn" {
				n++
			}
		}

		return n
	}

	run := func(t *testing.T, file string, seams func(s *shift.Shift)) {
		t.Helper()

		now := time.Now().Add(-5 * time.Minute)
		deliver(t, file, now, seams)
		deliver(t, file, now.Add(time.Minute), seams)
	}

	t.Run("its own item", func(t *testing.T) {
		file, _, seams, _, log := turnWorld(t, plugins)
		asked(t, file, "dev")
		run(t, file, seams)

		assert.Zero(t, turnTold(t, log))
		assert.Equal(t, []string{"ask A-1"}, toldIDs(t, log), "the card is told as an ask")
	})

	t.Run("another role's item", func(t *testing.T) {
		file, _, seams, _, log := turnWorld(t, plugins)
		asked(t, file, "designer")
		run(t, file, seams)

		assert.Equal(t, 1, turnTold(t, log))
	})

	t.Run("an item from long before the turn", func(t *testing.T) {
		file, f, seams, ns := dialogWorld(t, plugins)
		log := recorder(t, file, "pager", "")

		asked(t, file, "dev")

		// The turn ended twenty minutes after the item was put on the list.
		at := time.Now().Add(20 * time.Minute)
		f.session(t, ns, "sid-1", "idle", at)
		require.NoError(t, shift.RecordTurn(crew.Layout{Dir: filepath.Dir(file)}, "dev", "sid-1", askedInWords, at))

		run(t, file, seams)

		assert.Equal(t, 1, turnTold(t, log))
	})

	// The owner has answered the item already, or done it, or set it aside: the role asked, a card was told, and the words of its last
	// reply are not a second one.
	for _, closing := range [][]string{{"answer", "A-1", "dev"}, {"done", "A-1"}, {"ignore", "A-1"}, {"park", "A-1"}} {
		t.Run("an item closed since: "+closing[0], func(t *testing.T) {
			file, _, seams, _, log := turnWorld(t, plugins)
			asked(t, file, "dev")

			_, _, err := zm(t, file, append([]string{"todo"}, closing...)...)
			require.NoError(t, err)

			run(t, file, seams)

			assert.Zero(t, turnTold(t, log), "it was asked properly: no second card")
			assert.Empty(t, toldIDs(t, log), "and the item itself is closed, so no card of it either")
		})
	}

	t.Run("an item closed before the turn's window", func(t *testing.T) {
		file, f, seams, ns := dialogWorld(t, plugins)
		log := recorder(t, file, "pager", "")

		asked(t, file, "dev")

		_, _, err := zm(t, file, "todo", "answer", "A-1", "dev")
		require.NoError(t, err)

		at := time.Now().Add(20 * time.Minute)
		f.session(t, ns, "sid-1", "idle", at)
		require.NoError(t, shift.RecordTurn(crew.Layout{Dir: filepath.Dir(file)}, "dev", "sid-1", askedInWords, at))

		run(t, file, seams)

		assert.Equal(t, 1, turnTold(t, log), "a question of an hour ago says nothing of this one")
	})
}

// A question the turn ended on is held while the owner is at the machine, as any push, and goes when the span has passed if the
// role is still waiting on it.
func TestPluginsHoldATurnQuestionWhileTheOwnerIsActive(t *testing.T) {
	file, f, seams, ns, log := turnWorld(t, "  - {name: pager, quiet_when_active: 5m, dialog_after: 0s}\n")
	now := time.Now()

	// He answered something in a terminal just now.
	out, _, err := zm(t, file, "todo", "add", "Old question?")
	require.NoError(t, err)
	_, _, err = zm(t, file, "todo", "answer", askID(t, out), "yes")
	require.NoError(t, err)

	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	assert.Empty(t, told(t, log), "held while he is at the machine")

	deliver(t, file, now.Add(10*time.Minute), seams)
	require.Len(t, told(t, log), 1, "the span passed and the role still waits")
	assert.Equal(t, "turn", told(t, log)[0].Line["prompt"])

	// And one the role moved on from while it was held never goes.
	f.session(t, ns, "sid-1", "busy", time.Now())
	deliver(t, file, now.Add(11*time.Minute), seams)
	assert.Equal(t, []string{"dialog " + told(t, log)[0].Line["id"].(string), "answered " + told(t, log)[0].Line["id"].(string)}, toldIDs(t, log))
}

func TestPluginsDoNotTellOfAQuestionTheRoleMovedOnFromWhileItWasHeld(t *testing.T) {
	file, f, seams, ns, log := turnWorld(t, "  - {name: pager, quiet_when_active: 5m, dialog_after: 0s}\n")
	now := time.Now()

	out, _, err := zm(t, file, "todo", "add", "Old question?")
	require.NoError(t, err)
	_, _, err = zm(t, file, "todo", "answer", askID(t, out), "yes")
	require.NoError(t, err)

	deliver(t, file, now, seams)
	assert.Empty(t, told(t, log))

	f.session(t, ns, "sid-1", "busy", time.Now())
	deliver(t, file, now.Add(10*time.Minute), seams)
	assert.Empty(t, told(t, log), "never told, and so no end of it either")
}

// A dialog on the screen wins: the role is waiting on it, not idle, and the question of its turn before is not told beside it.
func TestAStandingDialogWinsOverAnEarlierTurnsQuestion(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off, dialog_after: 0s}\n")
	log := recorder(t, file, "pager", "")

	f.endTurn(t, file, ns, askedInWords)
	f.show(t, ns, true)

	now := time.Now().Add(-5 * time.Minute)
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)

	require.Len(t, told(t, log), 1)
	assert.Equal(t, "question", told(t, log)[0].Line["prompt"])
	assert.Contains(t, told(t, log)[0].Line["question"], "Ship it?")
}

// The question is read again right before it is told: the record the same and the role idle in the same stand; another record, or
// another turn, and it is not.
func TestATurnQuestionIsReadAgainBeforeItIsTold(t *testing.T) {
	file, f, seams, ns, _ := turnWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	l := crew.Layout{Dir: filepath.Dir(file)}

	w, facts := turnFacts(t, file, seams)
	assert.Equal(t, plugin.PromptTurn, facts.Prompt)
	assert.Equal(t, askedInWordsHeld, facts.Question)
	assert.False(t, facts.Since.IsZero())
	assert.True(t, w.dialogStill(facts), "as it was read")

	// The same words in the next turn are another question.
	at := time.Now().Add(20 * time.Second)
	f.session(t, ns, "sid-1", "idle", at)
	require.NoError(t, shift.RecordTurn(l, "dev", "sid-1", askedInWords, at))
	assert.False(t, w.dialogStill(facts), "a turn of its own")

	w, facts = turnFacts(t, file, seams)
	assert.True(t, w.dialogStill(facts))

	f.session(t, ns, "sid-1", "busy", time.Now())
	assert.False(t, w.dialogStill(facts), "gone")

	f.session(t, ns, "sid-1", "idle", at)
	assert.True(t, w.dialogStill(facts), "and back to the stand the record is of")

	require.NoError(t, os.Remove(l.HookTurn("dev")))
	assert.False(t, w.dialogStill(facts), "the record is gone")
}

// A record that is there and does not read says nothing of the role: its screen is as good as unread, so nothing told of it is judged
// ended, and nothing is made up of it.
func TestAnUnreadableTurnRecordIsAGapAndJudgesNothing(t *testing.T) {
	file, f, seams, ns, log := turnWorld(t, "  - {name: pager, quiet_when_active: off, dialog_after: 0s}\n")
	l := crew.Layout{Dir: filepath.Dir(file)}
	now := time.Now().Add(-5 * time.Minute)

	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Second), seams)
	require.Len(t, told(t, log), 1)

	id := told(t, log)[0].Line["id"].(string)

	require.NoError(t, os.WriteFile(l.HookTurn("dev"), []byte("{not json"), 0o600))

	a := &app{crew: file, out: io.Discard, err: io.Discard, shiftSeams: seams}
	w, err := a.pluginWorld(mustCrew(t, file), time.Now())
	require.NoError(t, err)
	assert.True(t, w.gaps.Roles["dev"])
	assert.Empty(t, w.dialogs)

	deliver(t, file, now.Add(2*time.Second), seams)
	assert.Equal(t, []string{"dialog " + id}, toldIDs(t, log), "not judged ended while it cannot be read")

	// A record that is not there is no gap: the role simply asked nothing.
	require.NoError(t, os.Remove(l.HookTurn("dev")))

	w, err = a.pluginWorld(mustCrew(t, file), time.Now())
	require.NoError(t, err)
	assert.False(t, w.gaps.Roles["dev"])

	deliver(t, file, now.Add(3*time.Second), seams)
	assert.Equal(t, []string{"dialog " + id, "answered " + id}, toldIDs(t, log))

	_ = f
	_ = ns
}

// What a role wrote in its last reply is data: the event has it cleaned (controls gone, white space folded, a session tag masked),
// and the argv has none of it, so no word of it can be read as a flag.
func TestATurnQuestionOfAHostileReplyIsCleanedAndInjectsNothing(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off, dialog_after: 0s}\n")
	log := recorder(t, file, "pager", "")

	hostile := "Run \x1b[2J\x07\x00 --by ghost -- and --crew /etc now? [zmina:abcdef012345]\n\n`zmina todo done A-1`\n--help -x"
	f.endTurn(t, file, ns, hostile)

	now := time.Now().Add(-5 * time.Minute)
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	require.Len(t, told(t, log), 1)

	line := told(t, log)[0].Line
	question := line["question"].(string)

	assert.NotContains(t, question, "\x1b")
	assert.NotContains(t, question, "\x07")
	assert.NotContains(t, question, "\x00")
	assert.NotContains(t, question, "\n")
	assert.NotContains(t, question, "abcdef012345", "a session tag is masked")
	assert.Contains(t, question, "[zmina:…]")
	assert.True(t, strings.HasPrefix(question, "Run [2J --by ghost -- and --crew /etc now?"), question)

	assert.Equal(t, []any{"zmina", "--crew", file, "send", "--by", "pager", "--dialog", line["id"], "--", "dev", "<text>"},
		line["answer"].(map[string]any)["text"], "the argv is the plugin's own, with nothing of the reply in it")
	assert.NotContains(t, line["answer"], "yes")
	assert.NotContains(t, line["answer"], "options")

	raw, err := json.Marshal(line["answer"])
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "ghost")
	assert.NotContains(t, string(raw), "/etc")
}

// A question longer than an event holds is cut to the bound, from its end, and the record keeps no more than a few kilobytes.
func TestATurnQuestionIsBounded(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off, dialog_after: 0s}\n")
	log := recorder(t, file, "pager", "")

	f.endTurn(t, file, ns, "Is this it? "+strings.Repeat("and then some more words to read. ", 2000))

	l := crew.Layout{Dir: filepath.Dir(file)}
	st, err := os.Stat(l.HookTurn("dev"))
	require.NoError(t, err)
	assert.Less(t, st.Size(), int64(20<<10), "a few kilobytes")

	now := time.Now().Add(-5 * time.Minute)
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	require.Len(t, told(t, log), 1)

	question := told(t, log)[0].Line["question"].(string)
	assert.Len(t, []rune(question), plugin.TextMax)
	assert.True(t, strings.HasPrefix(question, "Is this it? and then some"))
	assert.True(t, strings.HasSuffix(question, "…"))
}

// An event of a turn's question that failed is tried again as it was built, while the role is still idle on the same question; when
// the role has moved on meanwhile it is never sent.
func TestAFailedTurnQuestionEventIsRetriedWhileItStands(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off, dialog_after: 0s}\n")
	log := recorder(t, file, "pager", `if [ ! -e "$ZMINA_CREW.once" ]; then : > "$ZMINA_CREW.once"; exit 1; fi`)
	f.endTurn(t, file, ns, askedInWords)

	now := time.Now()

	deliver(t, file, now, seams)
	assert.Len(t, told(t, log), 1, "told, and the plugin failed")

	deliver(t, file, now.Add(30*time.Second), seams)
	assert.Len(t, told(t, log), 1, "not yet due")

	deliver(t, file, now.Add(2*time.Minute), seams)
	require.Len(t, told(t, log), 2, "tried again")
	assert.Equal(t, told(t, log)[0].Line, told(t, log)[1].Line, "as it was built")
	assert.Equal(t, "turn", told(t, log)[1].Line["prompt"])

	// When the role moved on before the retry, it is never sent again: the plugin never took it, so there is no end to tell either.
	file, f, seams, ns = dialogWorld(t, "  - {name: pager, quiet_when_active: off, dialog_after: 0s}\n")
	log = recorder(t, file, "pager", "exit 1")
	f.endTurn(t, file, ns, askedInWords)

	deliver(t, file, now, seams)
	require.Len(t, told(t, log), 1)

	f.session(t, ns, "sid-1", "busy", time.Now())
	deliver(t, file, now.Add(2*time.Minute), seams)
	assert.Len(t, told(t, log), 1, "no retry of a question the role moved on from")
}

// A plugin that takes no dialogs is told nothing of a turn's question either.
func TestAPluginThatTakesNoDialogsIsToldNothingOfATurnQuestion(t *testing.T) {
	file, _, seams, _, _ := turnWorld(t, "  - {name: lazy, on: [ask], quiet_when_active: off, dialog_after: 0s}\n")
	log := recorder(t, file, "lazy", "")

	now := time.Now().Add(-5 * time.Minute)
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)

	assert.Empty(t, told(t, log))
}

// The Stop hook records the question its input's last reply holds, before it returns and with no word to the role's screen, and a
// reply that asks nothing takes an earlier record away; a hook that has no crew, no role, no session or no reply it can read says
// nothing and writes nothing.
func TestTurnEndHookRecordsTheQuestionOfTheLastReply(t *testing.T) {
	crewFile := testCrew(t, "  dev: {}\n")
	l := crew.Layout{Dir: filepath.Dir(crewFile)}

	hook := func(stdin string, args ...string) {
		t.Helper()

		var out, errb bytes.Buffer

		a := &app{out: &out, err: &errb, spawn: func([]string) error { return nil }}
		root := newRoot(a)
		root.SetIn(strings.NewReader(stdin))
		root.SetArgs(append([]string{"--crew", crewFile, "_turn-end"}, args...))
		require.NoError(t, root.Execute())
		assert.Empty(t, out.String()+errb.String(), "the hook says nothing")
	}

	input := func(session, reply string) string {
		raw, err := json.Marshal(map[string]string{"session_id": session, "hook_event_name": "Stop", "last_assistant_message": reply})
		require.NoError(t, err)

		return string(raw)
	}

	before := time.Now()

	hook(input("sid-1", "Done.\n\n"+askedInWords), "--role", "dev")

	rec, err := shift.ReadTurn(l, "dev")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, "sid-1", rec.SessionID)
	assert.Equal(t, askedInWordsHeld, rec.Text)
	assert.Regexp(t, `^[0-9a-f]{16}$`, rec.ID)
	assert.WithinDuration(t, before, rec.At, 5*time.Second)

	// A reply with no word in the record's place, no role, no session: the record stays as it was, nothing else is written.
	for _, c := range []struct {
		name, stdin string
		args        []string
	}{
		{"no role", input("sid-2", "Ship it?"), nil},
		{"no session", input("", "Ship it?"), []string{"--role", "dev"}},
		{"not json", "Ship it?", []string{"--role", "dev"}},
		{"a role that is no file name", input("sid-2", "Ship it?"), []string{"--role", "../escape"}},
	} {
		hook(c.stdin, c.args...)

		again, err := shift.ReadTurn(l, "dev")
		require.NoError(t, err, c.name)
		assert.Equal(t, rec, again, c.name)
	}

	assert.NoFileExists(t, filepath.Join(l.Dir, "escape.turn.json"))

	// Another question replaces it, and a reply that asks nothing takes it away.
	hook(input("sid-1", "Shall I merge it?"), "--role", "dev")

	next, err := shift.ReadTurn(l, "dev")
	require.NoError(t, err)
	assert.Equal(t, "Shall I merge it?", next.Text)
	assert.NotEqual(t, rec.ID, next.ID)

	hook(input("sid-1", "Merged. Nothing to ask."), "--role", "dev")
	assert.NoFileExists(t, l.HookTurn("dev"))

	// A reply that is not a string is no reply: nothing asked.
	hook(input("sid-1", "Shall I merge it?"), "--role", "dev")
	require.FileExists(t, l.HookTurn("dev"))
	hook(`{"session_id":"sid-1","last_assistant_message":{"text":"Ship it?"}}`, "--role", "dev")
	assert.NoFileExists(t, l.HookTurn("dev"))
}

// Only the role's own hook records for it: run in another role's environment, or for a role it is not, it writes and removes nothing.
func TestTurnEndHookRecordsOnlyForTheRoleItRunsIn(t *testing.T) {
	crewFile := testCrew(t, "  dev: {}\n  ops: {}\n")
	l := crew.Layout{Dir: filepath.Dir(crewFile)}
	input := `{"session_id":"sid-1","hook_event_name":"Stop","last_assistant_message":"Ship it?"}`

	t.Setenv("ZMINA_ROLE", "ops")

	_, err := zmIn(t, crewFile, input, "_turn-end", "--role", "dev")
	require.NoError(t, err)
	assert.NoFileExists(t, l.HookTurn("dev"), "another role's hook says nothing of dev")

	// The role's own hook does; and another role's that would take the question away does not.
	t.Setenv("ZMINA_ROLE", "dev")

	_, err = zmIn(t, crewFile, input, "_turn-end", "--role", "dev")
	require.NoError(t, err)
	require.FileExists(t, l.HookTurn("dev"))

	t.Setenv("ZMINA_ROLE", "ops")

	_, err = zmIn(t, crewFile, `{"session_id":"sid-1","last_assistant_message":"Nothing asked."}`, "_turn-end", "--role", "dev")
	require.NoError(t, err)
	assert.FileExists(t, l.HookTurn("dev"))
}

// A turn that ends on an API error ran no Stop hook, and the question an earlier turn ended on is no one's now: the StopFailure hook
// takes the record away, for the role it runs for.
func TestStopFailureHookRemovesTheTurnRecord(t *testing.T) {
	crewFile := testCrew(t, "  dev: {}\n  ops: {}\n")
	l := crew.Layout{Dir: filepath.Dir(crewFile)}

	keep := func(role string) {
		t.Helper()

		require.NoError(t, shift.RecordTurn(l, role, "s-1", "Ship it?", time.Now()))
		require.FileExists(t, l.HookTurn(role))
	}

	keep("dev")
	keep("ops")

	// A command of another role's environment names dev and does not touch its record.
	t.Setenv("ZMINA_ROLE", "ops")

	_, err := zmIn(t, crewFile, stopFailure, "_stop-failure", "--role", "dev")
	require.NoError(t, err)
	assert.FileExists(t, l.HookTurn("dev"))

	t.Setenv("ZMINA_ROLE", "dev")

	out, err := zmIn(t, crewFile, stopFailure, "_stop-failure", "--role", "dev")
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.NoFileExists(t, l.HookTurn("dev"))
	assert.FileExists(t, l.HookFailure("dev"), "and the failure is recorded as it was")
	assert.FileExists(t, l.HookTurn("ops"), "no other role's")
}

// A compaction or a clear starts a conversation the role was not asked its last question in: the SessionStart hook takes the record
// away. A start or a resume does not (the record names its session, and what is of another stays unread), nor does a hook that is not
// the role's own.
func TestSessionStartAfterACompactionRemovesTheTurnRecord(t *testing.T) {
	crewFile := testCrew(t, "  dev: {}\n")
	l := crew.Layout{Dir: filepath.Dir(crewFile)}

	keep := func() {
		t.Helper()

		require.NoError(t, shift.RecordTurn(l, "dev", "sid-1", "Ship it?", time.Now()))
		require.FileExists(t, l.HookTurn("dev"))
	}

	brief := func(source string) {
		t.Helper()

		_, err := zmIn(t, crewFile, `{"session_id":"sid-1","source":"`+source+`"}`, "pending", "--role", "dev", "--brief")
		require.NoError(t, err)
	}

	keep()

	for _, source := range []string{"startup", "resume"} {
		t.Setenv("ZMINA_ROLE", "dev")
		brief(source)
		assert.FileExists(t, l.HookTurn("dev"), source)
	}

	// By hand, or as another role: nothing.
	t.Setenv("ZMINA_ROLE", "")
	brief("compact")
	t.Setenv("ZMINA_ROLE", "ops")
	brief("compact")
	assert.FileExists(t, l.HookTurn("dev"))

	for _, source := range []string{"compact", "clear"} {
		keep()
		t.Setenv("ZMINA_ROLE", "dev")
		brief(source)
		assert.NoFileExists(t, l.HookTurn("dev"), source)
	}
}

// The hook is silent, and records nothing, for a crew whose directory is not there: it makes none.
func TestTurnEndHookMakesNoCrewDirectory(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "nope", "crew.yaml")

	var out, errb bytes.Buffer

	a := &app{out: &out, err: &errb, spawn: func([]string) error { return nil }}
	root := newRoot(a)
	root.SetIn(strings.NewReader(`{"session_id":"sid-1","last_assistant_message":"Ship it?"}`))
	root.SetArgs([]string{"--crew", gone, "_turn-end", "--role", "dev"})
	require.NoError(t, root.Execute())

	assert.Empty(t, out.String()+errb.String())
	assert.NoDirExists(t, filepath.Dir(gone))
}
