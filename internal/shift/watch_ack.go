package shift

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/watch"
)

// WaitingKey is a watch key whose note waits on an ack: the dispatcher was told and has neither acked it nor made a
// task that carries it, and the source still reports it.
type WaitingKey struct {
	Watch   string    `json:"watch"`
	Key     string    `json:"key"`
	Text    string    `json:"text"`
	URL     string    `json:"url,omitempty"`
	Since   time.Time `json:"since"`
	NotedAt time.Time `json:"noted_at"`
	Renotes int       `json:"renotes,omitempty"`
}

// Line is the note it went out in.
func (k WaitingKey) Line() string { return watch.Event{Text: k.Text, URL: k.URL}.Line() }

// AckCommand is the command that acks it: by its url when it has one (that acks every key of the same note), else
// by its key.
func (k WaitingKey) AckCommand() string {
	target := k.Key
	if k.URL != "" {
		target = k.URL
	}

	return "zmina watch ack " + k.Watch + " " + shellWord(target)
}

// allTasks is every task of the ledger, closed ones too; none when it cannot be read.
func (s *Shift) allTasks() []task.Task {
	if s.Tasks == nil {
		return nil
	}

	all, err := s.Tasks.All()
	if err != nil {
		return s.Tasks.Active()
	}

	return all
}

// carrier is the task, made since the key's note went out, that carries it — the dispatcher turned the note into
// work, which acks it: the note's url as the task's url or as a whole word of its asked text, title or note; or the
// key as a whole word, qualified "<watch>/<key>", or bare when it is structured (structuredKey). A short bare key
// (main, prod) is a word any task may say, so it never acks.
func carrier(name, key string, k *watch.Key, tasks []task.Task) string {
	for _, t := range tasks {
		if t.Created.Before(k.Since) {
			continue
		}

		text := t.Asked + " " + t.Title + " " + t.Note

		switch {
		case k.URL != "" && (t.URL == k.URL || hasWord(text, k.URL)),
			hasWord(text, name+"/"+key),
			structuredKey(key) && (t.URL == key || hasWord(text, key)):
			return t.ID
		}
	}

	return ""
}

// structuredKey reports a key no plain word is: six characters at least, with a separator (group/repo!12:review,
// disk:/data).
func structuredKey(key string) bool {
	return len([]rune(key)) >= 6 && strings.ContainsAny(key, ":!/#")
}

// hasWord reports whether text holds word as a whole word, punctuation around it aside.
func hasWord(text, word string) bool {
	for _, f := range strings.Fields(text) {
		if strings.Trim(f, ".,;:!?()[]{}<>«»\"'`") == word {
			return true
		}
	}

	return false
}

// ackByTasks acks the waiting keys a task carries now and returns them, each as "<key> by task <id>".
func ackByTasks(name string, st *watch.State, tasks []task.Task, now time.Time) []string {
	var acked []string

	for _, key := range st.Waiting() {
		k := st.Keys[key]
		if id := carrier(name, key, k, tasks); id != "" {
			k.Ack("task "+id, now)
			acked = append(acked, key+" by task "+id)
		}
	}

	return acked
}

// DryApply diffs a run's events against a copy of the watch's state as a run would, and writes nothing.
func (s *Shift) DryApply(name string, events []watch.Event) []watch.Outcome {
	w := s.Crew.Watches[name]
	st := s.State.Watch[name].Clone()
	now := s.Now()
	ackByTasks(name, st, s.allTasks(), now)

	cfg := watch.Config{Quiet: w.QuietD(), ForgetAfter: w.ForgetAfterD(), Baseline: w.Baselines(), Info: w.Info,
		Renotify: w.RenotifyD()}

	return watch.Apply(st, events, cfg, now, func(e watch.Event) (string, error) {
		if !e.Since.IsZero() && s.stillQueued(st.Keys[e.Key]) {
			return "", watch.ErrStillQueued
		}

		return "", nil
	})
}

// waitingKeys are a watch's keys that still wait on an ack, the ones a task carries left out; none while its
// renotify is off. Nothing is changed.
func waitingKeys(w *crew.Watch, st *watch.State, tasks []task.Task) []WaitingKey {
	if w == nil || st == nil || w.RenotifyD() == 0 {
		return nil
	}

	name := w.Name

	var out []WaitingKey

	for _, key := range st.Waiting() {
		k := st.Keys[key]
		if carrier(name, key, k, tasks) != "" {
			continue
		}

		out = append(out, WaitingKey{Watch: name, Key: key, Text: k.Text, URL: k.URL, Since: k.Since,
			NotedAt: k.NotedAt, Renotes: k.Renotes})
	}

	return out
}

// WaitingKeys are every watch's keys that wait on an ack, by watch and key.
func (s *Shift) WaitingKeys() []WaitingKey {
	tasks := s.allTasks()

	var out []WaitingKey

	for _, name := range s.Crew.WatchNames() {
		out = append(out, waitingKeys(s.Crew.Watches[name], s.State.Watch[name], tasks)...)
	}

	return out
}

// AckWatch ends the wait of keys of a watch — each target a key or a note's url, or every waiting key with all —
// so they are not said again. It returns the keys acked.
func (s *Shift) AckWatch(name string, targets []string, all bool, by string) ([]string, error) {
	if _, err := s.Crew.Watch(name); err != nil {
		return nil, err
	}

	st := s.State.Watch[name]
	now := s.Now()

	if all {
		targets = st.Waiting()
	}

	// Every target is resolved before any is acked: one that names nothing fails the call and changes nothing.
	var acked []string

	for _, target := range targets {
		keys := st.Match(target)
		if len(keys) == 0 {
			return nil, fmt.Errorf("watch %s: no key or url %q waits on an ack (zmina watch ls shows the waiting ones)",
				name, target)
		}

		for _, key := range keys {
			if !slices.Contains(acked, key) {
				acked = append(acked, key)
			}
		}
	}

	for _, key := range acked {
		st.Keys[key].Ack(by, now)
		s.log("watch:"+name, "watch-ack", "", "key="+key+" by "+by)
	}

	return acked, nil
}

// renoteLine is the note for a key said again: its line, since when it waits, and how to ack it.
func (s *Shift) renoteLine(name string, e watch.Event) string {
	k := WaitingKey{Watch: name, Key: e.Key, URL: e.URL}

	return e.Line() + " (still pending since " + e.Since.In(s.Crew.Location()).Format("Mon 2 Jan 15:04") +
		"; ack: " + k.AckCommand() + ")"
}

// stillQueued reports whether the key's last note, exactly as it was queued, still waits in the dispatcher's
// signals, not typed yet.
func (s *Shift) stillQueued(k *watch.Key) bool {
	return k != nil && k.Queued != "" && slices.Contains(s.State.Signals, k.Queued)
}
