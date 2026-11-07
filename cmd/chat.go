package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/chat"
	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/inbox"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/tmux"
)

var chatInboxRef = regexp.MustCompile(`in-[a-f0-9]{12}`)

func chatSnapshot(s *shift.Shift, role string) (chat.Snapshot, error) {
	r, err := s.Role(role)
	if err != nil {
		return chat.Snapshot{}, err
	}
	out := chat.Snapshot{PresentationVersion: 1, AttachmentsSupported: true, RetryWindowSeconds: 7 * 24 * 60 * 60, Role: role, Runtime: r.Runtime, Profile: r.Profile, Messages: []chat.Message{}, Pending: []chat.Message{}}
	if out.Runtime == "" {
		out.Runtime = "claude"
	}
	for _, v := range s.Views() {
		if v.Role == role {
			out.Session = v.SessionID
			out.State = string(v.State)
			out.Status = v.Status
			out.NeedsInput = v.Prompt
			out.Context = v.Context
			out.Since = v.Since
			break
		}
	}
	rs := s.State.Role(role)
	lastRuntime := rs.LastRuntime
	if lastRuntime == "" {
		lastRuntime = "claude"
	}
	if out.Session == "" && lastRuntime == out.Runtime {
		out.Session = rs.LastSession
	}
	if out.State == "up" {
		if text, ok, err := s.Tmux.Prompt(role); err == nil && ok && strings.TrimSpace(text) != "" {
			out.NeedsInput = true
		}
	}
	reg := s.Reg
	if r.IsCodex() {
		reg = &codex.Registry{Dir: s.Crew.StateDir() + "/codex"}
	}
	if out.Session != "" {
		turn, err := reg.Last(out.Session)
		if err != nil {
			return out, err
		}
		out.Active = out.State == "up" && !turn.Ended && (out.Status == "busy" || out.Status == "shell" || out.Status == "working") && !out.NeedsInput
		// A question on the screen: Chat shows it with its options and answers it.
		if out.NeedsInput {
			if asker, ok := reg.(interface {
				PendingQuestion(string) (*chat.Question, error)
			}); ok {
				if q, err := asker.PendingQuestion(out.Session); err == nil {
					out.Question = q
				}
			}
		}
		if turn.Ended && !out.NeedsInput {
			out.Status = "idle"
		}
		if r.IsCodex() {
			home := r.CodexHome
			if home == "" {
				home = os.Getenv("CODEX_HOME")
			}
			if home == "" {
				home = crew.ExpandHome("~/.codex")
			}
			out.Messages, err = chat.CodexMessages(home, out.Session)
		} else {
			out.Messages, err = (&registry.Registry{Namespace: s.Crew.Namespace}).ChatMessages(out.Session)
		}
		if err != nil {
			return out, err
		}
	}
	seen := map[string]bool{}
	receipts := map[string]chat.Receipt{}
	for _, receipt := range rs.ChatReceipts {
		receipts["web:"+receipt.ID] = receipt
	}
	for i, m := range out.Messages {
		seen[m.ID] = true
		if receipt, ok := receipts[m.ID]; ok {
			out.Messages[i].Text = receipt.Text
			out.Messages[i].Attachments = receipt.Attachments
			out.Messages[i].State = "accepted"
		} else if m.Peer {
			// Posted to the inbox socket with an ID no browser of the owner's sent: a session's words, not his.
			out.Messages[i].ID = "peer:" + strings.TrimPrefix(m.ID, "web:")
		} else if m.Role == "user" && strings.HasPrefix(m.ID, "web:") {
			out.Messages[i].State = "accepted"
			if id := chatInboxRef.FindString(m.Text); id != "" {
				if entry, err := s.Inbox.Get(id); err == nil {
					out.Messages[i].Text = entry.Body
				}
			}
		}
	}
	for i, m := range out.Messages {
		out.Messages[i] = chat.PresentInput(m)
	}
	for _, receipt := range rs.ChatReceipts {
		id := "web:" + receipt.ID
		if seen[id] || !receipt.Dismissed.IsZero() {
			continue
		}
		state := "sent"
		if slices.Contains(rs.Inbox, receipt.Line) {
			state = "queued"
		} else {
			for _, failed := range rs.Failed {
				if failed.Line == receipt.Line {
					state = "failed"
					break
				}
			}
			if state == "sent" && receipt.Session != "" && receipt.Session != out.Session {
				state = "unconfirmed"
			}
		}
		out.Pending = append(out.Pending, chat.Message{ID: id, Role: "user", Text: receipt.Text, At: receipt.At, State: state, Attachments: receipt.Attachments})
	}
	out.Pending = chat.Limit(out.Pending)
	return out, nil
}

func (a *app) localChatRead(key, role string) (chat.Snapshot, error) {
	return a.localChatReadSince(key, role, "")
}

func (a *app) localChatReadSince(key, role, cursor string) (chat.Snapshot, error) {
	c, err := a.crewApp(key)
	if err != nil {
		return chat.Snapshot{}, err
	}
	var snapshot chat.Snapshot
	err = c.readState(func(s *shift.Shift) error { var err error; snapshot, err = chatSnapshot(s, role); return err })
	return chat.Page(snapshot, cursor), err
}

func (a *app) localChatSend(key, role, id, text string) (chat.Snapshot, error) {
	return a.localChatSendFiles(key, role, id, text, nil)
}

func (a *app) localChatSendFiles(key, role, id, text string, files []string) (chat.Snapshot, error) {
	if !chat.ValidID(id) {
		return chat.Snapshot{}, errors.New("message request ID is invalid")
	}
	if len(files) > chat.MaxFiles {
		return chat.Snapshot{}, errors.New("at most four attachments per message")
	}
	if len(text) > 16<<10 || (strings.TrimSpace(text) == "" && len(files) == 0) || tmux.Printable(strings.NewReplacer("\r", "", "\n", "", "\t", "").Replace(text)) != strings.NewReplacer("\r", "", "\n", "", "\t", "").Replace(text) {
		return chat.Snapshot{}, errors.New("message must contain text, at most 16 KiB, without terminal control characters")
	}
	c, err := a.crewApp(key)
	if err != nil {
		return chat.Snapshot{}, err
	}
	err = c.withState(func(s *shift.Shift) error {
		if _, err := s.Role(role); err != nil {
			return err
		}
		var attachments []chat.Attachment
		var paths []string
		seen := map[string]bool{}
		for _, file := range files {
			if seen[file] {
				return errors.New("duplicate attachment ID")
			}
			seen[file] = true
			meta, path, err := (chat.FileStore{Dir: filepath.Join(s.Crew.StateDir(), "chat-files"), Role: role}).Get(file)
			if err != nil {
				return err
			}
			attachments = append(attachments, meta)
			paths = append(paths, path)
		}
		payloadHash := chat.MessageHash(text, attachments)
		rs := s.State.Role(role)
		for _, receipt := range rs.ChatReceipts {
			if receipt.ID == id {
				if receipt.Hash != payloadHash {
					return errors.New("message ID already accepted with different text or attachments")
				}
				return nil
			}
		}
		rs.ChatReceipts = slices.DeleteFunc(rs.ChatReceipts, func(receipt chat.Receipt) bool {
			if s.Now().Sub(receipt.At) < 7*24*time.Hour || slices.Contains(rs.Inbox, receipt.Line) || (rs.Sent != nil && rs.Sent.Line == receipt.Line) {
				return false
			}
			for _, failed := range rs.Failed {
				if failed.Line == receipt.Line {
					return false
				}
			}
			return true
		})
		if len(rs.ChatReceipts) >= 4096 {
			return errors.New("chat receipt capacity reached; retry after older receipts expire")
		}
		snapshot, err := chatSnapshot(s, role)
		if err != nil {
			return err
		}
		delivered := text
		if len(attachments) > 0 {
			delivered += "\n\nAttached files from the owner (open with file-reading tools):"
			for i, meta := range attachments {
				delivered += fmt.Sprintf("\n- %q (%d bytes): %q", meta.Name, meta.Size, paths[i])
			}
		}
		line := strings.Join(strings.Fields(delivered), " ")
		if strings.ContainsAny(delivered, "\r\n") || len(line) > 1000 {
			entry, err := inbox.NewID()
			if err != nil {
				return err
			}
			if err = s.Inbox.Put(inbox.Entry{ID: entry, From: "owner chat", Body: delivered}); err != nil {
				return err
			}
			head := []rune(line)
			if len(head) > 400 {
				head = head[:400]
			}
			line = fmt.Sprintf("%s — full owner message (preserve formatting): zmina inbox show %s", string(head), entry)
		}
		line = chat.Prefix(id) + line
		if err = s.Queue(role, line); err != nil {
			return err
		}
		rs.ChatReceipts = append(rs.ChatReceipts, chat.Receipt{ID: id, Hash: payloadHash, Attachments: attachments, Text: text, Line: line, At: s.Now(), Session: snapshot.Session})
		return nil
	})
	if err != nil {
		return chat.Snapshot{}, err
	}
	// Acceptance and request identity are durable before any terminal input is attempted.
	_ = c.withState(func(s *shift.Shift) error { s.FlushInbox(role); return nil })
	return a.localChatRead(key, role)
}

func (a *app) localChatAction(key, role, action string) (chat.Snapshot, error) {
	c, err := a.crewApp(key)
	if err != nil {
		return chat.Snapshot{}, err
	}
	err = c.withState(func(s *shift.Shift) error {
		snapshot, err := chatSnapshot(s, role)
		if err != nil {
			return err
		}
		switch action {
		case "start":
			if snapshot.State == "up" {
				return nil
			}
			return s.Up(role)
		case "stop":
			if !snapshot.Active {
				return errors.New("no active turn to stop")
			}
			if draft, ok, err := s.Tmux.Prompt(role); err != nil {
				return err
			} else if ok && draft != "" {
				return errors.New("terminal has a draft; use Terminal to stop explicitly")
			}
			return s.PressKeys(role, []string{"C-c"})
		default:
			if id, ok := strings.CutPrefix(action, "dismiss:"); ok {
				return dismissChat(s, role, id, snapshot)
			}
			if pick, ok := strings.CutPrefix(action, "choose:"); ok {
				return chooseDialog(s, role, pick)
			}
			if raw, ok := strings.CutPrefix(action, "answer:"); ok {
				return answerQuestion(s, role, raw, snapshot)
			}
			return errors.New("chat action: start, stop, dismiss:<id>, choose:<n>:<id> or answer:<json>")
		}
	})
	if err != nil {
		return chat.Snapshot{}, err
	}
	return a.localChatRead(key, role)
}

// chooseDialog answers the question on a role's screen with one of its numbered choices, "<n>:<dialog id>": the id is
// the fingerprint of the whole question the owner read, so a question that changed — even one with the same choices —
// is never answered blind. Only while the session itself says it waits on a dialog. It moves the dialog's cursor there
// and takes it, as the owner would with the arrows and Enter.
func chooseDialog(s *shift.Shift, role, pick string) error {
	ns, id, ok := strings.Cut(pick, ":")
	n, err := strconv.Atoi(ns)
	if !ok || err != nil || n < 1 || n > 9 {
		return errors.New("choose:<n>:<label>")
	}
	screen, err := s.Tmux.Capture(role, 80)
	if err != nil {
		return err
	}
	if _, ok, err := s.Tmux.Prompt(role); err != nil || ok {
		return errors.New("no question on its screen now")
	}
	if v := viewOf(s, role); v == nil || v.Status != "waiting" {
		return errors.New("it is not waiting on a question now")
	}
	d := shift.ParseDialog(screen)
	if d == nil {
		return errors.New("no question on its screen now")
	}
	i := slices.IndexFunc(d.Options, func(o shift.DialogOption) bool { return o.N == n })
	if i < 0 || d.ID != id {
		return errors.New("the question on its screen changed: look again")
	}
	return s.PressKeys(role, d.ChooseKeys(n))
}

// questionAnswer is the owner's answer to the question on a role's screen, from Chat: per item the chosen options
// (1-based) or, for a single-choice item, words of their own.
type questionAnswer struct {
	ID    string `json:"id"`
	Items []struct {
		Choices []int  `json:"choices"`
		Other   string `json:"other"`
	} `json:"items"`
}

// answerStep is what one press does: named keys, or a line of text typed and entered.
type answerStep struct {
	Keys []string
	Text string
	// Expect is the question the screen must show before this step is pressed, Options its option labels (empty:
	// no check of the dialog's text; the input line is checked before every step).
	Expect  string
	Options []string
	// Current is the row (1-based) the dialog's cursor must be on before this step (0: any row).
	Current int
	// Want is the options a several-choice part must end ticked: the keys are worked out on the live dialog, so an
	// option already ticked is not ticked off.
	Want []int
}

// answerSteps turns an answer into what Claude Code's question dialog takes: a single-choice item is its option's
// digit (the dialog moves on), its own words are the digit after the last option, the text and Enter; a
// multi-choice item is the digits that tick its options, then Tab; a question of several items ends on the review
// screen, whose 1 submits. The last digit, «Chat about this», is never pressed.
func answerSteps(q *chat.Question, a questionAnswer) ([]answerStep, error) {
	if q == nil || a.ID != q.ID {
		return nil, errors.New("the question on its screen changed: look again")
	}
	if len(a.Items) != len(q.Items) {
		return nil, fmt.Errorf("the question has %d parts, the answer %d", len(q.Items), len(a.Items))
	}
	var steps []answerStep
	digit := func(n int) string { return strconv.Itoa(n) }
	for i, item := range q.Items {
		ans, n := a.Items[i], len(item.Options)
		if n < 1 || n > 8 {
			return nil, fmt.Errorf("part %d: %d options; answer it in Terminal", i+1, n)
		}
		for _, c := range ans.Choices {
			if c < 1 || c > n {
				return nil, fmt.Errorf("part %d: no option %d", i+1, c)
			}
		}
		other := strings.TrimSpace(ans.Other)
		first := len(steps)
		if item.MultiSelect {
			if other != "" {
				return nil, fmt.Errorf("part %d: words of your own go as a reply, not with ticks", i+1)
			}
			if len(ans.Choices) == 0 {
				return nil, fmt.Errorf("part %d: tick at least one option", i+1)
			}
			if len(q.Items) == 1 {
				return nil, errors.New("a single question with ticks: answer it in Terminal")
			}
			want := []int{}
			for _, c := range ans.Choices {
				if !slices.Contains(want, c) {
					want = append(want, c)
				}
			}
			slices.Sort(want)
			steps = append(steps, answerStep{Want: want, Keys: []string{"Tab"}})
			steps[first].Expect, steps[first].Options = item.Question, labels(item)
			continue
		}
		switch {
		case other != "" && len(ans.Choices) == 0:
			steps = append(steps, answerStep{Keys: []string{digit(n + 1)}}, answerStep{Text: other, Expect: item.Question, Options: labels(item), Current: n + 1})
		case other == "" && len(ans.Choices) == 1:
			steps = append(steps, answerStep{Keys: []string{digit(ans.Choices[0])}})
		default:
			return nil, fmt.Errorf("part %d: one option, or words of your own", i+1)
		}
		steps[first].Expect, steps[first].Options = item.Question, labels(item)
	}
	if len(q.Items) > 1 {
		steps = append(steps, answerStep{Keys: []string{"1"}, Expect: "Review your answers", Options: []string{"Submit answers", "Cancel"}})
	}
	return steps, nil
}

// labels are an item's option labels as the dialog draws them.
func labels(item chat.QuestionItem) []string {
	out := make([]string, 0, len(item.Options))
	for _, o := range item.Options {
		out = append(out, o.Label)
	}
	return out
}

// dialogOption is a numbered row of the active question dialog: its number, label and, for ticks, whether ticked.
type dialogOption struct {
	N       int
	Label   string
	Ticked  bool
	Current bool
}

var dialogRow = regexp.MustCompile(`^\s*(❯\s*)?(\d+)\.\s+(?:\[([ ✔])\]\s+)?(.*?)\s*$`)

// activeDialog reads the question dialog at the bottom of the screen: the last place the question starts, its whole
// text from there, and the numbered rows under it in order. ok is false when that question is not the active one.
func activeDialog(screen, question string) ([]dialogOption, bool) {
	flat := func(t string) string { return strings.Join(strings.Fields(t), " ") }
	q := flat(question)
	head := []rune(q)
	if len(head) > 20 {
		head = head[:20]
	}
	// The question's text is read without the dialog's frame; its rows are read as drawn: a row inside a box (another
	// dialog's) is not one of this dialog's.
	raw, lines := strings.Split(screen, "\n"), dialogLines(screen)
	at := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(flat(lines[i]), string(head)) {
			at = i
			break
		}
	}
	if q == "" || at < 0 || !strings.Contains(flat(strings.Join(lines[at:], " ")), q) {
		return nil, false
	}
	var out []dialogOption
	current := 0
	for _, l := range raw[at:] {
		m := dialogRow.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		if n == 1 && len(out) > 0 {
			return nil, false // numbering starts again: another dialog below this text, the active one is not it
		}
		if n != len(out)+1 {
			continue // a number in the question's own text, not a row
		}
		out = append(out, dialogOption{N: n, Label: flat(m[4]), Ticked: m[3] == "✔", Current: m[1] != ""})
		if m[1] != "" {
			current++
		}
	}
	// The active dialog has its cursor on exactly one of its rows; text that only looks like one has none.
	return out, len(out) > 0 && current == 1
}

// dialogBar is the rule Claude Code draws down the left of a question's text (and a box's sides): a frame, not words.
var dialogBar = regexp.MustCompile(`^\s*[│┃▏▎]\s?|\s*[│┃]\s*$`)

// dialogLines are the screen's lines without the dialog's frame, so a question wrapped inside it reads as its words;
// one line for each of the screen's, in order.
func dialogLines(screen string) []string {
	lines := strings.Split(screen, "\n")
	for i, l := range lines {
		lines[i] = dialogBar.ReplaceAllString(l, "")
	}
	return lines
}

// dialogMatches reports whether the active dialog's rows are the part's options, in order, by number.
func dialogMatches(rows []dialogOption, labels []string) bool {
	if len(rows) < len(labels) {
		return false
	}
	for i, l := range labels {
		if rows[i].Label != strings.Join(strings.Fields(l), " ") {
			return false
		}
	}
	return true
}

// onScreen reports whether the screen shows the whole question and every one of its options, line breaks and runs
// of spaces aside: another question that starts alike is not this one.
func onScreen(screen, want string, options ...string) bool {
	flat := func(t string) string { return strings.Join(strings.Fields(t), " ") }
	s, w := flat(strings.Join(dialogLines(screen), "\n")), flat(want)
	if w == "" || !strings.Contains(s, w) {
		return false
	}
	for _, o := range options {
		if !strings.Contains(s, flat(o)) {
			return false
		}
	}
	return true
}

// answerQuestion answers the question on a role's screen with the owner's choices, if it is still the one shown.
func answerQuestion(s *shift.Shift, role, raw string, snapshot chat.Snapshot) error {
	var a questionAnswer
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return errors.New("answer:<json>")
	}
	if !snapshot.NeedsInput || snapshot.Question == nil {
		return errors.New("no question on its screen now")
	}
	steps, err := answerSteps(snapshot.Question, a)
	if err != nil {
		return err
	}
	// Never twice: a second request for the same question (a retry, another browser) is refused. The mark is made
	// before the first key, under the state lock the whole answer holds.
	rs := s.State.Role(role)
	if slices.Contains(rs.AnsweredQuestions, a.ID) {
		return errors.New("this question was answered from Chat already; look at Terminal")
	}
	if _, ok, err := s.Tmux.Prompt(role); err != nil || ok {
		return errors.New("no question on its screen now")
	}
	rs.AnsweredQuestions = append(rs.AnsweredQuestions, a.ID)
	if n := len(rs.AnsweredQuestions); n > 20 {
		rs.AnsweredQuestions = rs.AnsweredQuestions[n-20:]
	}
	// An answer stopped before its first key reached the dialog is no answer: the owner may try again.
	pressed := false
	defer func() {
		if !pressed {
			rs.AnsweredQuestions = slices.DeleteFunc(rs.AnsweredQuestions, func(id string) bool { return id == a.ID })
		}
	}()
	for i, step := range steps {
		if i > 0 {
			// The dialog redraws between parts; a key sent into the redraw is lost.
			time.Sleep(250 * time.Millisecond)
		}
		// Each step is checked on the live screen first: no input line (the dialog is still up), and the part's
		// whole question with its options — a question closed or changed in Terminal meanwhile stops the answer
		// instead of typing into the role's input line.
		if i > 0 {
			if _, ok, err := s.Tmux.Prompt(role); err != nil || ok {
				return fmt.Errorf("stopped at step %d: the question is gone from its screen; look at Terminal", i+1)
			}
		}
		if step.Expect != "" {
			screen, err := s.Tmux.Capture(role, 120)
			if err != nil {
				return err
			}
			stop := fmt.Errorf("stopped at step %d: the question on its screen is not the one answered; look at Terminal", i+1)
			if len(step.Options) == 0 {
				if !onScreen(screen, step.Expect) {
					return stop
				}
			} else {
				// The active dialog, at the bottom: this part's whole question and its options by number, in order.
				rows, ok := activeDialog(screen, step.Expect)
				if !ok || !dialogMatches(rows, step.Options) || step.Current > 0 && (len(rows) < step.Current || !rows[step.Current-1].Current) {
					return stop
				}
				if step.Want != nil {
					// Ticks: press only the options whose tick differs from the one wanted.
					keys := []string{}
					for k, r := range rows[:len(step.Options)] {
						if r.Ticked != slices.Contains(step.Want, k+1) {
							keys = append(keys, strconv.Itoa(k+1))
						}
					}
					step.Keys = append(keys, step.Keys...)
				}
			}
		}
		pressed = true
		if step.Text != "" {
			if err := s.Tmux.Type(role, step.Text); err != nil {
				return err
			}
			continue
		}
		if err := s.PressKeys(role, step.Keys); err != nil {
			return err
		}
	}
	s.Log(role, "answered", "", fmt.Sprintf("question %s from Chat", a.ID))
	return nil
}

// dismissChat sets aside an owner message whose delivery stayed unconfirmed (its conversation changed before it was
// seen there): the owner has looked and need not see it under every newer reply. Only such a message: one still
// queued, sent or failed has a delivery in play.
func dismissChat(s *shift.Shift, role, id string, snapshot chat.Snapshot) error {
	if !chat.ValidID(id) {
		return errors.New("message request ID is invalid")
	}
	if !slices.ContainsFunc(snapshot.Pending, func(m chat.Message) bool { return m.ID == "web:"+id && m.State == "unconfirmed" }) {
		return errors.New("only a message whose delivery is unconfirmed can be dismissed")
	}
	rs := s.State.Role(role)
	for i := range rs.ChatReceipts {
		if rs.ChatReceipts[i].ID == id {
			rs.ChatReceipts[i].Dismissed = s.Now()
		}
	}
	return nil
}

func (a *app) chatCall(ctx context.Context, node, op string, req mesh.TerminalRequest) (chat.Snapshot, error) {
	if node != "" {
		n, err := openNode()
		if err != nil {
			return chat.Snapshot{}, err
		}
		if n.Name != node {
			snapshot, err := n.Chat(ctx, mesh.Exec{}, node, op, req)
			// A downgraded worker may not know the signed cursor field cached
			// by this browser. Retry this read without it, never a mutation.
			if err != nil && op == "chat/read" && req.Cursor != "" && ctx.Err() == nil {
				req.Cursor = ""
				return n.Chat(ctx, mesh.Exec{}, node, op, req)
			}
			return snapshot, err
		}
	}
	switch op {
	case "chat/read":
		return a.localChatReadSince(req.Crew, req.Role, req.Cursor)
	case "chat/send":
		return a.localChatSendFiles(req.Crew, req.Role, req.ID, req.Text, req.Files)
	case "chat/upload":
		if req.Upload == nil {
			return chat.Snapshot{}, errors.New("missing upload")
		}
		meta, err := a.localChatUpload(req.Crew, req.Role, *req.Upload)
		return chat.Snapshot{Uploaded: &meta}, err
	case "chat/file":
		file, err := a.localChatFile(req.Crew, req.Role, req.FileID)
		return chat.Snapshot{File: &file}, err
	case "chat/action":
		return a.localChatAction(req.Crew, req.Role, req.Action)
	}
	return chat.Snapshot{}, errors.New("unknown chat operation")
}

func (a *app) uiChat(mux *http.ServeMux) {
	a.uiChatFiles(mux)
	mux.HandleFunc("GET /api/chat", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		snapshot, err := a.chatCall(ctx, r.URL.Query().Get("node"), "chat/read", mesh.TerminalRequest{Crew: r.URL.Query().Get("crew"), Role: r.URL.Query().Get("role"), Cursor: r.URL.Query().Get("cursor")})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, snapshot)
	})
	for _, op := range []string{"send", "action"} {
		mux.HandleFunc("POST /api/chat/"+op, func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Node, Crew, Role, ID, Text, Action string
				Files                              []string
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			snapshot, err := a.chatCall(ctx, body.Node, "chat/"+op, mesh.TerminalRequest{Crew: body.Crew, Role: body.Role, ID: body.ID, Text: body.Text, Action: body.Action, Files: body.Files})
			if err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			writeJSON(w, snapshot)
		})
	}
}

// viewOf is the role's view as ls shows it, nil when it has none.
func viewOf(s *shift.Shift, role string) *shift.View {
	for _, v := range s.Views() {
		if v.Role == role {
			return &v
		}
	}
	return nil
}
