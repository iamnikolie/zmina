package mesh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/iamnikolie/zmina/internal/chat"
)

// TerminalTarget names a configured role, never an arbitrary command or tmux target.
type TerminalTarget struct {
	Crew    string `json:"crew"`
	Role    string `json:"role"`
	State   string `json:"state"`
	Runtime string `json:"runtime,omitempty"`
}

// Terminal is an attached client. Closing it detaches; the role keeps running.
type Terminal interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
}

// TerminalServices resolve roles on the node that owns them.
type TerminalServices struct {
	List          func() ([]TerminalTarget, error)
	Open          func(context.Context, string, string, uint16, uint16) (Terminal, error)
	History       func(string, string, int) (string, error)
	ChatRead      func(string, string) (chat.Snapshot, error)
	ChatReadSince func(string, string, string) (chat.Snapshot, error)
	ChatSend      func(string, string, string, string) (chat.Snapshot, error)
	ChatUpload    func(string, string, chat.Upload) (chat.Attachment, error)
	ChatFile      func(string, string, string) (chat.Upload, error)
	ChatSendFiles func(string, string, string, string, []string) (chat.Snapshot, error)
	ChatAction    func(string, string, string) (chat.Snapshot, error)
	// ChatPaths says which paths a role named (newline-separated) are files Chat shows; ChatRoleFile reads one.
	ChatPaths    func(crew, role, paths string) (chat.Snapshot, error)
	ChatRoleFile func(crew, role, path string) (chat.Upload, error)
	// Status is what the node's crews show on the cockpit, as JSON: kind "" their state, tasks, asks and alarms;
	// "cost" their spend. Read-only.
	Status func(kind string) ([]byte, error)
	// TodoAct is the cockpit's answer to an item of one of the node's crews (crew, action, id, text): what the
	// owner does on the item's own node, there, as the page there would. It returns the line that says what was done.
	TodoAct func(crew, action, id, text string) (string, error)
	// AlarmAct sets, moves or takes off an alarm of one of the node's crews (crew, action, id, when, text, role).
	AlarmAct func(crew, action, id, when, text, role string) (string, error)
	// Items resolves the ids a conversation names (crew, comma-separated ids) as the node's JSON: tasks, parked items
	// and asks, each with its chain. Read-only.
	Items func(crew, ids string) ([]byte, error)
}

// TerminalRequest is signed against a fresh server challenge, so a certified peer cannot turn a
// dispatcher's data message into terminal access or replay a captured owner request.
type TerminalRequest struct {
	Cursor string       `json:"cursor,omitempty"`
	Crew   string       `json:"crew,omitempty"`
	Role   string       `json:"role,omitempty"`
	Cols   uint16       `json:"cols,omitempty"`
	Rows   uint16       `json:"rows,omitempty"`
	Lines  int          `json:"lines,omitempty"`
	Sig    []byte       `json:"sig,omitempty"`
	ID     string       `json:"id,omitempty"`
	Text   string       `json:"text,omitempty"`
	Action string       `json:"action,omitempty"`
	Files  []string     `json:"files,omitempty"`
	Upload *chat.Upload `json:"upload,omitempty"`
	FileID string       `json:"file_id,omitempty"`
	// When is an alarm's time, as the owner wrote it (alarm/act).
	When string `json:"when,omitempty"`
	// To is the node the owner meant the request for, inside what the owner signs: a write (todo/act, alarm/act) is
	// done only by the node it names, so a peer that got it cannot pass it on to another node's session.
	To string `json:"to,omitempty"`
}

func terminalMessage(op string, challenge []byte, req TerminalRequest) []byte {
	req.Sig = nil
	raw, _ := json.Marshal(req)
	return append(append([]byte("zmina-terminal\x00"+op+"\x00"), challenge...), raw...)
}

func (n *Node) terminalRequest(c *conn, op string, req TerminalRequest) error {
	key, err := readKey(filepath.Join(n.Dir, "owner.key"))
	if err != nil || !key.Public().(ed25519.PublicKey).Equal(n.Owner) {
		return errors.New("mesh terminals need the owner node: run zmina ui there; keep owner.key there only")
	}
	req.Sig = ed25519.Sign(key, terminalMessage(op, c.challenge, req))
	return c.send(wire{Op: op, Terminal: &req})
}

func terminalSize(cols, rows uint16) bool {
	return cols >= 2 && cols <= 500 && rows >= 2 && rows <= 300
}

func (n *Node) serveTerminal(ctx context.Context, c *conn, w wire, s *TerminalServices) error {
	fail := func(err error) error { _ = c.send(wire{Op: "err", Err: err.Error()}); return err }
	if w.Terminal == nil || !ed25519.Verify(n.Owner, terminalMessage(w.Op, c.challenge, *w.Terminal), w.Terminal.Sig) {
		return fail(errors.New("terminal access requires the owner's signature"))
	}
	if s == nil {
		return fail(errors.New("this node has no terminal service; update zmina"))
	}
	if w.Op == "terminal/list" {
		targets, err := s.List()
		if err != nil {
			return fail(err)
		}
		return c.send(wire{Op: "terminal/targets", Targets: targets})
	}
	req := w.Terminal
	if w.Op == "crew/status" {
		if s.Status == nil {
			return fail(errors.New("crew status unavailable; update zmina on this node"))
		}
		data, err := s.Status(req.Action)
		if err != nil {
			return fail(err)
		}
		return c.send(wire{Op: "crew/status-result", Data: data})
	}
	if w.Op == "item/read" {
		if s.Items == nil {
			return fail(errors.New("item cards unavailable; update zmina on this node"))
		}
		data, err := s.Items(req.Crew, req.Text)
		if err != nil {
			return fail(err)
		}
		return c.send(wire{Op: "item/result", Data: data})
	}
	if w.Op == "todo/act" || w.Op == "alarm/act" {
		if req.To != n.Name {
			return fail(fmt.Errorf("this request is for node %q, not %s", req.To, n.Name))
		}
		var line string
		var err error
		switch {
		case w.Op == "todo/act" && s.TodoAct != nil:
			line, err = s.TodoAct(req.Crew, req.Action, req.ID, req.Text)
		case w.Op == "alarm/act" && s.AlarmAct != nil:
			line, err = s.AlarmAct(req.Crew, req.Action, req.ID, req.When, req.Text, req.Role)
		default:
			err = errors.New("answering from another node's cockpit is unavailable; update zmina on this node")
		}
		if err != nil {
			return fail(err)
		}
		return c.send(wire{Op: "act/result", Text: line})
	}
	if strings.HasPrefix(w.Op, "chat/") {
		var snapshot chat.Snapshot
		var err error
		switch {
		case w.Op == "chat/read" && s.ChatReadSince != nil:
			snapshot, err = s.ChatReadSince(req.Crew, req.Role, req.Cursor)
		case w.Op == "chat/read" && s.ChatRead != nil:
			snapshot, err = s.ChatRead(req.Crew, req.Role)
		case w.Op == "chat/upload" && s.ChatUpload != nil && req.Upload != nil:
			var meta chat.Attachment
			meta, err = s.ChatUpload(req.Crew, req.Role, *req.Upload)
			snapshot.Uploaded = &meta
		case w.Op == "chat/file" && s.ChatFile != nil:
			var file chat.Upload
			file, err = s.ChatFile(req.Crew, req.Role, req.FileID)
			snapshot.File = &file
		case w.Op == "chat/send" && s.ChatSendFiles != nil:
			snapshot, err = s.ChatSendFiles(req.Crew, req.Role, req.ID, req.Text, req.Files)
		case w.Op == "chat/send" && s.ChatSend != nil && len(req.Files) == 0:
			snapshot, err = s.ChatSend(req.Crew, req.Role, req.ID, req.Text)
		case w.Op == "chat/action" && s.ChatAction != nil:
			snapshot, err = s.ChatAction(req.Crew, req.Role, req.Action)
		case w.Op == "chat/paths" && s.ChatPaths != nil:
			snapshot, err = s.ChatPaths(req.Crew, req.Role, req.Text)
		case w.Op == "chat/rolefile" && s.ChatRoleFile != nil:
			var file chat.Upload
			file, err = s.ChatRoleFile(req.Crew, req.Role, req.Text)
			snapshot.File = &file
		default:
			err = errors.New("chat unavailable; update zmina on this node")
		}
		if err != nil {
			return fail(err)
		}
		return c.send(wire{Op: "chat/result", Chat: &snapshot})
	}
	if w.Op == "terminal/history" {
		if req.Lines < 1 || req.Lines > 10000 {
			return fail(errors.New("history lines: 1–10000"))
		}
		if s.History == nil {
			return fail(errors.New("terminal history unavailable; update zmina on this node"))
		}
		text, err := s.History(req.Crew, req.Role, req.Lines)
		if err != nil {
			return fail(err)
		}
		return c.send(wire{Op: "terminal/history-result", Text: HistoryText(text)})
	}
	if !terminalSize(req.Cols, req.Rows) {
		return fail(errors.New("terminal size: 2–500 columns, 2–300 rows"))
	}
	tty, err := s.Open(ctx, req.Crew, req.Role, req.Cols, req.Rows)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = tty.Close() }()
	if err = c.send(wire{Op: "terminal/ready"}); err != nil {
		return err
	}
	// One encoder per direction. A terminal's output ending also interrupts the blocking request read.
	done := make(chan error, 1)
	go func() {
		var result error
		for {
			var frame wire
			if result = c.dec.Decode(&frame); result != nil {
				break
			}
			switch frame.Op {
			case "terminal/input":
				if len(frame.Data) > 64<<10 {
					result = errors.New("terminal input too large")
				} else {
					_, result = tty.Write(frame.Data)
				}
			case "terminal/resize":
				if !terminalSize(frame.Cols, frame.Rows) {
					result = errors.New("invalid terminal size")
				} else {
					result = tty.Resize(frame.Cols, frame.Rows)
				}
			case "terminal/close":
				result = io.EOF
			default:
				result = fmt.Errorf("unknown terminal frame %q", frame.Op)
			}
			if result != nil {
				break
			}
		}
		_ = tty.Close()
		done <- result
	}()
	buf := make([]byte, 16<<10)
	for {
		count, rerr := tty.Read(buf)
		if count > 0 {
			if err = c.send(wire{Op: "terminal/output", Data: buf[:count]}); err != nil {
				return err
			}
		}
		if rerr != nil {
			break
		}
	}
	_ = c.send(wire{Op: "terminal/closed"})
	// Serve's caller owns the connection and closes it on return, releasing the input reader.
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			return err
		}
	default:
	}
	return nil
}

// TerminalList discovers roles through the same authenticated, routed mesh transport as attachments.
func (n *Node) TerminalList(ctx context.Context, t Transport, node string) ([]TerminalTarget, error) {
	rw, c, err := n.terminalConnect(ctx, t, node)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rw.Close() }()
	if err = n.terminalRequest(c, "terminal/list", TerminalRequest{}); err != nil {
		return nil, err
	}
	w, err := c.expect("terminal/targets")
	return w.Targets, err
}

// CrewStatus reads what a node's crews show on the cockpit (kind "" or "cost"), as the node's JSON. Owner only.
func (n *Node) CrewStatus(ctx context.Context, t Transport, node, kind string) ([]byte, error) {
	rw, c, err := n.terminalConnect(ctx, t, node)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rw.Close() }()
	if err = n.terminalRequest(c, "crew/status", TerminalRequest{Action: kind}); err != nil {
		return nil, err
	}
	w, err := c.expect("crew/status-result")
	return w.Data, err
}

// ItemRead resolves ids of one of a node's crews (comma-separated) as that node's JSON. Owner only.
func (n *Node) ItemRead(ctx context.Context, t Transport, node, crew, ids string) ([]byte, error) {
	rw, c, err := n.terminalConnect(ctx, t, node)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rw.Close() }()
	if err = n.terminalRequest(c, "item/read", TerminalRequest{Crew: crew, Text: ids}); err != nil {
		return nil, err
	}
	w, err := c.expect("item/result")
	return w.Data, err
}

// HistoryText bounds history snapshots by bytes as well as lines. Keep a valid UTF-8 suffix.
func HistoryText(text string) string {
	const maxBytes = 2 << 20
	if len(text) > maxBytes {
		text = text[len(text)-maxBytes:]
		for len(text) > 0 && !utf8.RuneStart(text[0]) {
			text = text[1:]
		}
	}
	return text
}

// TerminalHistory reads retained role history without entering copy mode or sending input to the role.
func (n *Node) TerminalHistory(ctx context.Context, t Transport, node, crew, role string, lines int) (string, error) {
	rw, c, err := n.terminalConnect(ctx, t, node)
	if err != nil {
		return "", err
	}
	defer func() { _ = rw.Close() }()
	if err = n.terminalRequest(c, "terminal/history", TerminalRequest{Crew: crew, Role: role, Lines: lines}); err != nil {
		return "", err
	}
	w, err := c.expect("terminal/history-result")
	return w.Text, err
}

// OpenTerminal attaches to an existing remote role. The connection lasts until Close or ctx cancellation.
func (n *Node) OpenTerminal(ctx context.Context, t Transport, node, crew, role string, cols, rows uint16) (Terminal, error) {
	rw, c, err := n.terminalConnect(ctx, t, node)
	if err != nil {
		return nil, err
	}
	if err = n.terminalRequest(c, "terminal/open", TerminalRequest{Crew: crew, Role: role, Cols: cols, Rows: rows}); err == nil {
		_, err = c.expect("terminal/ready")
	}
	if err != nil {
		_ = rw.Close()
		return nil, err
	}
	return &remoteTerminal{rw: rw, c: c}, nil
}

func (n *Node) terminalConnect(ctx context.Context, t Transport, node string) (io.ReadWriteCloser, *conn, error) {
	child, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(30*time.Second, cancel)
	rw, err := n.route(child, t, node, nil)
	if err != nil {
		timer.Stop()
		cancel()
		return nil, nil, err
	}
	rw = &cancelStream{ReadWriteCloser: rw, cancel: cancel}
	c := newConn(rw)
	if _, err = n.open(c, node, time.Now); err != nil {
		timer.Stop()
		_ = rw.Close()
		return nil, nil, err
	}
	timer.Stop()
	return rw, c, nil
}

// route creates a byte tunnel; the destination still authenticates the original caller end to end.
// Unlike queued notes, live terminals need every hop online, and never wait in an outbox.
func (n *Node) route(ctx context.Context, t Transport, target string, via []string) (io.ReadWriteCloser, error) {
	if !ValidName(target) || target == n.Name || len(via) >= maxHops || slices.Contains(via, n.Name) {
		return nil, errors.New("invalid or looping terminal route")
	}
	return n.routeVia(ctx, t, target, via, nil)
}

func (n *Node) routeVia(ctx context.Context, t Transport, target string, via, visiting []string) (io.ReadWriteCloser, error) {
	if slices.Contains(visiting, target) || len(visiting) >= maxHops {
		return nil, errors.New("looping terminal relay discovery")
	}
	visiting = append(slices.Clone(visiting), target)
	book, err := n.Peers()
	if err != nil {
		return nil, err
	}
	var dest *Peer
	for i := range book {
		if book[i].Name == target {
			dest = &book[i]
			break
		}
	}
	if dest == nil {
		return nil, fmt.Errorf("unknown mesh node %q", target)
	}
	var errs []error
	for _, addr := range dest.Addrs {
		rw, err := t.Dial(ctx, addr)
		if err == nil {
			return rw, nil
		}
		errs = append(errs, err)
	}
	via = append(slices.Clone(via), n.Name)
	for _, relay := range dest.Through {
		if relay == target || slices.Contains(via, relay) {
			continue
		}
		rw, err := n.routeVia(ctx, t, relay, via[:len(via)-1], visiting)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		c := newConn(rw)
		if _, err = n.open(c, relay, time.Now); err == nil {
			err = c.send(wire{Op: "terminal/tunnel", Node: target, Via: via})
			if err == nil {
				_, err = c.expect("terminal/tunnel-ready")
			}
		}
		if err == nil {
			return &bufferedTunnel{ReadWriteCloser: rw, reader: io.MultiReader(c.dec.Buffered(), rw)}, nil
		}
		_ = rw.Close()
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("no live route to %s: no address or relay", target)
	}
	return nil, fmt.Errorf("no live route to %s: %w", target, errors.Join(errs...))
}

type bufferedTunnel struct {
	io.ReadWriteCloser
	reader io.Reader
}

func (t *bufferedTunnel) Read(p []byte) (int, error) { return t.reader.Read(p) }

func (n *Node) serveTunnel(ctx context.Context, rw io.ReadWriter, c *conn, w wire, t Transport) error {
	fail := func(err error) error { _ = c.send(wire{Op: "err", Err: err.Error()}); return err }
	if t == nil {
		return fail(errors.New("terminal routing unavailable; update zmina"))
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	dst, err := n.route(child, t, w.Node, w.Via)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = dst.Close() }()
	if err = c.send(wire{Op: "terminal/tunnel-ready"}); err != nil {
		return err
	}
	go func() { _, _ = io.Copy(dst, io.MultiReader(c.dec.Buffered(), rw)); cancel(); _ = dst.Close() }()
	_, err = io.Copy(rw, dst)
	// Closing the outer transport releases the other copy on return.
	return err
}

type remoteTerminal struct {
	rw  io.ReadWriteCloser
	c   *conn
	buf bytes.Buffer
	mu  sync.Mutex
}

func (t *remoteTerminal) Read(p []byte) (int, error) {
	for t.buf.Len() == 0 {
		var w wire
		if err := t.c.dec.Decode(&w); err != nil {
			return 0, err
		}
		switch w.Op {
		case "terminal/output":
			if len(w.Data) > 64<<10 {
				return 0, errors.New("terminal output too large")
			}
			t.buf.Write(w.Data)
		case "terminal/closed":
			return 0, io.EOF
		case "err":
			return 0, errors.New(w.Err)
		default:
			return 0, fmt.Errorf("unknown terminal frame %q", w.Op)
		}
	}
	return t.buf.Read(p)
}
func (t *remoteTerminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for len(p) > 0 {
		size := min(len(p), 16<<10)
		if err := t.c.send(wire{Op: "terminal/input", Data: p[:size]}); err != nil {
			return count, err
		}
		p = p[size:]
		count += size
	}
	return count, nil
}
func (t *remoteTerminal) Resize(cols, rows uint16) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.c.send(wire{Op: "terminal/resize", Cols: cols, Rows: rows})
}
func (t *remoteTerminal) Close() error { return t.rw.Close() }

// cancelStream ends the SSH process before waiting for it, including on a browser disconnect.
type cancelStream struct {
	io.ReadWriteCloser
	cancel context.CancelFunc
}

func (s *cancelStream) Close() error { s.cancel(); return s.ReadWriteCloser.Close() }

// Chat transports structured owner operations through the same authenticated mesh routes as terminals.
func (n *Node) Chat(ctx context.Context, t Transport, node, op string, request TerminalRequest) (chat.Snapshot, error) {
	rw, c, err := n.terminalConnect(ctx, t, node)
	if err != nil {
		return chat.Snapshot{}, err
	}
	defer func() { _ = rw.Close() }()
	if err = n.terminalRequest(c, op, request); err != nil {
		return chat.Snapshot{}, err
	}
	w, err := c.expect("chat/result")
	if err != nil {
		return chat.Snapshot{}, err
	}
	if w.Chat == nil {
		return chat.Snapshot{}, errors.New("peer returned no chat snapshot")
	}
	return *w.Chat, nil
}

// Act is an owner-signed write to one of a peer's crews from this node's cockpit: todo/act or alarm/act. It returns
// what the peer says was done.
func (n *Node) Act(ctx context.Context, t Transport, node, op string, request TerminalRequest) (string, error) {
	rw, c, err := n.terminalConnect(ctx, t, node)
	if err != nil {
		return "", err
	}
	defer func() { _ = rw.Close() }()
	request.To = node
	if err = n.terminalRequest(c, op, request); err != nil {
		return "", err
	}
	w, err := c.expect("act/result")
	if err != nil {
		return "", err
	}
	return w.Text, nil
}

// HoldsOwnerKey is true on the owner's node: the one that may read and open other nodes' crews.
func (n *Node) HoldsOwnerKey() bool {
	key, err := readKey(filepath.Join(n.Dir, "owner.key"))
	return err == nil && key.Public().(ed25519.PublicKey).Equal(n.Owner)
}
