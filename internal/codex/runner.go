package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/iamnikolie/zmina/internal/registry"
)

// Options configure a standalone or shared App Server connection and its persistent role thread.
type Options struct {
	Bin, Name, Dir, Resume, Boot, Model, Effort, Sandbox, Approval string
	// ServiceTier is the role's service_tier; empty takes the profile's, else DefaultServiceTier.
	ServiceTier string
	Role        string
	Transport   string
	Profile     string
	Config      map[string]any
	Args        []string
	Registry    Registry
}

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

type runner struct {
	o                                    Options
	out                                  io.Writer
	enc                                  *json.Encoder
	rec                                  Record
	next                                 int
	pending                              map[int]string
	request                              *message
	requests                             []message
	turn                                 string
	model, tier, provider, trigger, from string
	accountingTurns                      map[string]registry.Message
}

// Run serves a line-oriented console in tmux. Lifecycle comes from JSON-RPC, not screen heuristics.
func Run(o Options, input io.Reader, output, stderr io.Writer) error {
	var pipe io.WriteCloser
	var stdout io.Reader
	var closeTransport func()
	if o.Transport == "shared" {
		if len(o.Args) > 0 {
			return errors.New("shared Codex transport does not accept App Server process args; use a profile")
		}
		socket, err := sharedSocket()
		if err != nil {
			return err
		}
		stream, err := dialShared(socket)
		if err != nil {
			return err
		}
		pipe, stdout = stream, stream
		closeTransport = func() { _ = stream.Close() }
	} else {
		if o.Transport != "" && o.Transport != "stdio" {
			return fmt.Errorf("unknown Codex transport %q", o.Transport)
		}
		cmd := exec.Command(o.Bin, append([]string{"app-server"}, o.Args...)...)
		cmd.Dir, cmd.Stderr = o.Dir, stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		var err error
		pipe, err = cmd.StdinPipe()
		if err != nil {
			return err
		}
		stdout, err = cmd.StdoutPipe()
		if err != nil {
			return err
		}
		if err = cmd.Start(); err != nil {
			return err
		}
		closeTransport = func() {
			_ = pipe.Close()
			exited := make(chan struct{})
			go func() { _ = cmd.Wait(); close(exited) }()
			select {
			case <-exited:
			case <-time.After(2 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-exited
			}
		}
	}
	defer closeTransport()

	r := &runner{o: o, out: output, enc: json.NewEncoder(pipe), pending: map[int]string{}}
	r.rec.PID, r.rec.Name, r.rec.Cwd = os.Getpid(), o.Name, o.Dir
	// zmina's lines come through the inbox, never typed into this console; one it cannot make is left to tmux.
	if dir := InboxDir(o.Registry.Dir, o.Name); o.Registry.Dir != "" && o.Name != "" && openInbox(dir) == nil {
		r.rec.Inbox = dir
	}
	if o.Transport == "shared" {
		defer func() {
			if r.turn != "" {
				_ = r.call("turn/interrupt", map[string]string{"threadId": r.rec.SessionID, "turnId": r.turn})
			}
			if r.rec.SessionID != "" {
				_ = r.call("thread/unsubscribe", map[string]string{"threadId": r.rec.SessionID})
			}
		}()
	}
	if err := r.status("busy"); err != nil {
		return err
	}
	defer func() { _ = os.Remove(fmt.Sprintf("%s/%d.json", o.Registry.Dir, r.rec.PID)) }()
	incoming := make(chan message)
	failures := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		scan := bufio.NewScanner(stdout)
		scan.Buffer(make([]byte, 4096), 16<<20)
		for scan.Scan() {
			var m message
			if err := json.Unmarshal(scan.Bytes(), &m); err != nil {
				failures <- err
				return
			}
			select {
			case incoming <- m:
			case <-done:
				return
			}
		}
		err := scan.Err()
		if err == nil {
			err = io.EOF
		}
		failures <- err
	}()
	lines := make(chan string)
	go func() {
		scan := bufio.NewScanner(input)
		scan.Buffer(make([]byte, 4096), 1<<20)
		for scan.Scan() {
			select {
			case lines <- scan.Text():
			case <-done:
				return
			}
		}
		close(lines)
	}()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	if err := r.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "zmina", "version": "1"}}); err != nil {
		return err
	}
	timer := time.NewTimer(90 * time.Second)
	defer timer.Stop()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	var interrupted time.Time
	for {
		select {
		case <-timer.C:
			if r.rec.SessionID == "" {
				return errors.New("codex startup timed out")
			}
		case <-poll.C:
			if err := r.drainInbox(); err != nil {
				return err
			}
		case s := <-sig:
			if s != os.Interrupt || r.turn == "" || time.Since(interrupted) < time.Second {
				return nil
			}
			interrupted = time.Now()
			if err := r.call("turn/interrupt", map[string]string{"threadId": r.rec.SessionID, "turnId": r.turn}); err != nil {
				return err
			}
		case err := <-failures:
			return fmt.Errorf("codex App Server stopped: %w", err)
		case m := <-incoming:
			if err := r.handle(m); err != nil {
				return err
			}
		case line, ok := <-lines:
			if !ok {
				return nil
			}
			if strings.TrimSpace(line) == "/exit" {
				return nil
			}
			if err := r.input(line); err != nil {
				return err
			}
		}
	}
}

func (r *runner) call(method string, params any) error {
	r.next++
	r.pending[r.next] = method
	return r.enc.Encode(map[string]any{"id": r.next, "method": method, "params": params})
}

func (r *runner) status(status string) error {
	r.rec.Status, r.rec.StatusUpdatedAt = status, time.Now().UnixMilli()
	if err := r.o.Registry.Save(r.rec); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "\n[zmina codex: %s]\n", status)
	if status == "idle" {
		fmt.Fprintln(r.out, "────────────────────────────────────────")
		fmt.Fprintln(r.out, "❯ ")
	}
	return nil
}

func (r *runner) startThread(resume string) error {
	p := map[string]any{"cwd": r.o.Dir, "config": r.o.threadConfig()}
	if r.o.Model != "" {
		p["model"] = r.o.Model
	}
	if r.o.Sandbox != "" {
		p["sandbox"] = r.o.Sandbox
	}
	if r.o.Approval != "" {
		p["approvalPolicy"] = r.o.Approval
	}
	method := "thread/start"
	if resume != "" {
		method = "thread/resume"
		p["threadId"] = resume
	}
	return r.call(method, p)
}

func (r *runner) input(line string) error {
	if r.request != nil {
		var result any
		if r.request.Method == "item/commandExecution/requestApproval" || r.request.Method == "item/fileChange/requestApproval" {
			decision := ""
			switch strings.TrimSpace(line) {
			case "y":
				decision = "accept"
			case "n":
				decision = "decline"
			case "cancel":
				decision = "cancel"
			}
			if decision == "" {
				fmt.Fprintln(r.out, "Type y, n, or cancel, then Enter.")
				return nil
			}
			result = map[string]string{"decision": decision}
		} else {
			// Advanced prompts can be answered explicitly without inventing user choices.
			var raw any
			if err := json.Unmarshal([]byte(line), &raw); err != nil {
				fmt.Fprintln(r.out, "Enter the JSON result required by this request, or /exit.")
				return nil
			}
			result = raw
		}
		if err := r.enc.Encode(map[string]any{"id": r.request.ID, "result": result}); err != nil {
			return err
		}
		r.request = nil
		if len(r.requests) > 0 {
			next := r.requests[0]
			r.requests = r.requests[1:]
			return r.ask(next)
		}
		return r.status("busy")
	}
	if r.rec.Status != "idle" {
		if r.turn != "" && !strings.HasPrefix(strings.TrimSpace(line), "/") {
			return r.call("turn/steer", map[string]any{"threadId": r.rec.SessionID, "expectedTurnId": r.turn, "input": []any{map[string]string{"type": "text", "text": line}}})
		}
		fmt.Fprintln(r.out, "Codex is starting; use zmina queue for pending work.")
		return nil
	}
	switch strings.TrimSpace(line) {
	case "":
		return nil
	case "/compact":
		if err := r.status("busy"); err != nil {
			return err
		}
		return r.call("thread/compact/start", map[string]string{"threadId": r.rec.SessionID})
	case "/clear":
		r.rec.Turn = Record{}.Turn
		if err := r.status("busy"); err != nil {
			return err
		}
		return r.startThread("")
	}
	if strings.HasPrefix(strings.TrimSpace(line), "/") {
		fmt.Fprintln(r.out, "Supported commands: /compact, /clear, /exit")
		return nil
	}
	return r.startTurn(line)
}

// drainInbox starts a turn with the oldest line zmina queued, once the thread is idle and no request is open.
func (r *runner) drainInbox() error {
	if r.rec.Inbox == "" || r.rec.Status != "idle" || r.request != nil || r.rec.SessionID == "" {
		return nil
	}

	l, path, err := nextLine(r.rec.Inbox)
	if err != nil || l == nil {
		return nil // a read error is tried again next second
	}

	fmt.Fprintln(r.out, "\n› "+l.Text)

	if err := r.startTurn(l.Text); err != nil {
		return err
	}

	// A line that could not be marked taken does not end the bridge: take sets it aside, and if even that failed
	// the inbox is left alone from here on rather than start the same turn every second.
	if err := take(r.rec.Inbox, path, l.ID); err != nil {
		fmt.Fprintln(r.out, "zmina inbox: "+err.Error())

		if _, serr := os.Stat(path); serr == nil {
			fmt.Fprintln(r.out, "zmina inbox: the line is still there; this bridge reads its inbox no more")
			r.rec.Inbox = ""
		}
	}

	return nil
}

func (r *runner) startTurn(text string) error {
	r.rec.Turn.Ended, r.rec.Turn.At, r.rec.Turn.Error, r.rec.Turn.ErrorID = false, time.Time{}, "", ""
	r.trigger, r.from = registry.TextTrigger(text)
	if err := r.status("busy"); err != nil {
		return err
	}
	p := map[string]any{"threadId": r.rec.SessionID, "input": []any{map[string]string{"type": "text", "text": text}}}
	if r.o.Effort != "" {
		p["effort"] = r.o.Effort
	}
	return r.call("turn/start", p)
}

func (r *runner) handle(m message) error {
	if m.Method != "" && len(m.ID) > 0 {
		if r.request != nil {
			r.requests = append(r.requests, m)
			return nil
		}
		return r.ask(m)
	}
	if m.Method == "" {
		var id int
		_ = json.Unmarshal(m.ID, &id)
		method := r.pending[id]
		delete(r.pending, id)
		if len(m.Error) > 0 && string(m.Error) != "null" {
			return r.rpcError(method, m.Error)
		}
		switch method {
		case "initialize":
			if err := r.enc.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
				return err
			}
			return r.startThread(r.o.Resume)
		case "thread/start", "thread/resume":
			var p struct {
				Model    string `json:"model"`
				Provider string `json:"modelProvider"`
				Tier     string `json:"serviceTier"`
				Thread   struct {
					ID string `json:"id"`
				} `json:"thread"`
			}
			if err := json.Unmarshal(m.Result, &p); err != nil {
				return err
			}
			if p.Thread.ID == "" {
				return errors.New("codex returned no thread id")
			}
			r.rec.SessionID = p.Thread.ID
			if method == "thread/start" {
				if err := r.o.Registry.SaveUsage(Usage{Thread: p.Thread.ID, Message: registry.Message{At: time.Now()}}); err != nil {
					return err
				}
			}
			r.model, r.tier, r.provider = p.Model, p.Tier, p.Provider
			if r.model == "" {
				r.model = r.o.Model
			}
			fmt.Fprintf(r.out, "Codex thread: %s\n", p.Thread.ID)
			if r.o.Boot != "" {
				return r.startTurn(r.o.Boot)
			}
			return r.status("idle")
		case "turn/start":
			var p struct {
				Turn struct {
					ID string `json:"id"`
				} `json:"turn"`
			}
			_ = json.Unmarshal(m.Result, &p)
			r.turn = p.Turn.ID
			r.noteTurn(p.Turn.ID)
		}
		return nil
	}
	switch m.Method {
	case "thread/status/changed", "serverRequest/resolved", "item/agentMessage/delta", "item/started", "turn/started", "turn/completed", "model/rerouted", "thread/tokenUsage/updated", "item/completed":
	default:
		return nil
	}
	var p struct {
		ThreadID  string          `json:"threadId"`
		TurnID    string          `json:"turnId"`
		ToModel   string          `json:"toModel"`
		RequestID json.RawMessage `json:"requestId"`
		Delta     string          `json:"delta"`
		Status    json.RawMessage `json:"status"`
		Turn      struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"turn"`
		TokenUsage struct {
			Last  Tokens `json:"last"`
			Total Tokens `json:"total"`
		} `json:"tokenUsage"`
		Item struct {
			Type string `json:"type"`
		} `json:"item"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		return err
	}
	if p.ThreadID != "" && p.ThreadID != r.rec.SessionID {
		return nil
	}
	switch m.Method {
	case "thread/status/changed":
		var status struct {
			Type        string   `json:"type"`
			ActiveFlags []string `json:"activeFlags"`
		}
		if err := json.Unmarshal(p.Status, &status); err != nil {
			return err
		}
		switch status.Type {
		case "idle":
			return r.status("idle")
		case "active":
			for _, flag := range status.ActiveFlags {
				if flag == "waitingOnApproval" || flag == "waitingOnUserInput" {
					return r.status("waiting")
				}
			}
			return r.status("busy")
		}
	case "serverRequest/resolved":
		if r.request != nil && string(r.request.ID) == string(p.RequestID) {
			r.request = nil
			if len(r.requests) > 0 {
				next := r.requests[0]
				r.requests = r.requests[1:]
				return r.ask(next)
			}
			return r.status("busy")
		}
		for i, q := range r.requests {
			if string(q.ID) == string(p.RequestID) {
				r.requests = append(r.requests[:i], r.requests[i+1:]...)
				break
			}
		}
	case "item/agentMessage/delta":
		fmt.Fprint(r.out, p.Delta)
	case "item/started":
		fmt.Fprintf(r.out, "\n[%s]\n", p.Item.Type)
	case "turn/started":
		r.turn = p.Turn.ID
		r.noteTurn(p.Turn.ID)
		return r.status("busy")
	case "turn/completed":
		r.turn = ""
		r.request = nil
		r.requests = nil
		r.rec.Turn.Ended, r.rec.Turn.At = true, time.Now()
		if p.Turn.Error != nil {
			r.rec.Turn.Error = p.Turn.Error.Message
			r.rec.Turn.ErrorID = p.Turn.ID
			fmt.Fprintln(r.out, "\n"+p.Turn.Error.Message)
		}
		fmt.Fprintln(r.out)
		return r.status("idle")
	case "model/rerouted":
		if p.ToModel != "" {
			r.model = p.ToModel
			r.noteTurn(r.turn)
		}
	case "thread/tokenUsage/updated":
		r.rec.Turn.Context = p.TokenUsage.Last.Total
		role := r.o.Role
		if role == "" {
			role = r.o.Name
		}
		group, _, _ := strings.Cut(role, "#")
		base, owns := r.accountingTurns[p.TurnID]
		base.At = time.Now()
		if !owns {
			base.Cwd = r.o.Dir
		}
		u := Usage{Thread: r.rec.SessionID, Turn: p.TurnID, Role: role, Group: group, Total: p.TokenUsage.Total, Last: p.TokenUsage.Last,
			Charge: owns, Message: base}
		u.Message.ID = usageID(u)
		if err := r.o.Registry.SaveUsage(u); err != nil {
			return err
		}
		return r.o.Registry.Save(r.rec)
	case "item/completed":
		if p.Item.Type == "contextCompaction" {
			r.rec.Turn.Compacted = time.Now()
		}
		if p.Item.Type == "contextCompaction" && r.turn == "" {
			r.rec.Turn.Context = 0
			r.rec.Turn.Ended = true
			return r.status("idle")
		}
	}
	return nil
}

func (r *runner) ask(m message) error {
	r.request = &m
	fmt.Fprintf(r.out, "\nCodex request: %s\n%s\n", m.Method, m.Params)
	if m.Method == "item/commandExecution/requestApproval" || m.Method == "item/fileChange/requestApproval" {
		fmt.Fprintln(r.out, "Type y, n, or cancel, then Enter.")
	} else {
		fmt.Fprintln(r.out, "Enter JSON result, then Enter.")
	}
	return r.status("waiting")
}

func (r *runner) noteTurn(id string) {
	if id == "" {
		return
	}
	if r.accountingTurns == nil {
		r.accountingTurns = map[string]registry.Message{}
	}
	speed := r.tier
	switch speed {
	case "default", "standard", "":
		speed = ""
	case "priority":
		speed = "fast"
	}
	model := r.model
	if r.provider != "" && r.provider != "openai" {
		model = r.provider + "/" + model
	}
	r.accountingTurns[id] = registry.Message{Model: model, Speed: speed, Cwd: r.o.Dir, Trigger: r.trigger, From: r.from}
}
