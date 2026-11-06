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
			return errors.New("chat action: start, stop or dismiss:<id>")
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
