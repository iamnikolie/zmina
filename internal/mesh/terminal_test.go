package mesh

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/iamnikolie/zmina/internal/chat"
	"github.com/stretchr/testify/require"
)

type terminalPipes struct {
	nodes    map[string]*Node
	services map[string]*TerminalServices
}

func (p *terminalPipes) Dial(ctx context.Context, a Addr) (io.ReadWriteCloser, error) {
	left, right := net.Pipe()
	stop := context.AfterFunc(ctx, func() { _ = left.Close() })
	go func() {
		defer stop()
		defer func() { _ = right.Close() }()
		_ = p.nodes[a.Cmd].ServeTerminals(ctx, right, func(string, string, string, string, time.Time) error { return nil }, time.Now, p.services[a.Cmd], p)
	}()
	return left, nil
}

type echoTerminal struct {
	net.Conn
	sizes  chan [2]uint16
	closed chan struct{}
	once   sync.Once
}

func (t *echoTerminal) Resize(x, y uint16) error { t.sizes <- [2]uint16{x, y}; return nil }
func (t *echoTerminal) Close() error {
	t.once.Do(func() { close(t.closed); _ = t.Conn.Close() })
	return nil
}
func echoService(t *testing.T) (*TerminalServices, chan *echoTerminal) {
	t.Helper()
	opened := make(chan *echoTerminal, 4)
	return &TerminalServices{
		History: func(crew, role string, lines int) (string, error) {
			require.Equal(t, "ops", crew)
			require.Equal(t, "dev", role)
			require.Equal(t, 10000, lines)
			return "oldest output\nlatest output", nil
		},
		List: func() ([]TerminalTarget, error) {
			return []TerminalTarget{{Crew: "ops", Role: "dev", State: "up", Runtime: "codex"}}, nil
		},
		Open: func(_ context.Context, crew, role string, x, y uint16) (Terminal, error) {
			require.Equal(t, "ops", crew)
			require.Equal(t, "dev", role)
			a, b := net.Pipe()
			tty := &echoTerminal{Conn: a, sizes: make(chan [2]uint16, 4), closed: make(chan struct{})}
			tty.sizes <- [2]uint16{x, y}
			opened <- tty
			go func() {
				defer func() { _ = b.Close() }()
				buf := make([]byte, 1024)
				for {
					n, err := b.Read(buf)
					if n > 0 {
						_, _ = b.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}()
			return tty, nil
		}}, opened
}

func TestMeshTerminalDirectAndRelay(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "relay"}[relay], func(t *testing.T) {
			owner := newOwner(t, "owner")
			jump := enroll(t, owner, "jump")
			worker := enroll(t, owner, "worker")
			service, opened := echoService(t)
			transport := &terminalPipes{nodes: map[string]*Node{"owner": owner, "jump": jump, "worker": worker}, services: map[string]*TerminalServices{"worker": service}}
			if relay {
				addPeer(t, owner, "jump")
				addPeer(t, jump, "worker")
				require.NoError(t, owner.UpdatePeers(func(ps []Peer) []Peer { return append(ps, Peer{Name: "worker", Through: []string{"jump"}}) }))
			} else {
				addPeer(t, owner, "worker")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			targets, err := owner.TerminalList(ctx, transport, "worker")
			require.NoError(t, err)
			require.Equal(t, "dev", targets[0].Role)
			history, err := owner.TerminalHistory(ctx, transport, "worker", "ops", "dev", 10000)
			require.NoError(t, err)
			require.Equal(t, "oldest output\nlatest output", history)
			require.Empty(t, opened, "reading history must not attach or type into the role")
			terminal, err := owner.OpenTerminal(ctx, transport, "worker", "ops", "dev", 80, 24)
			require.NoError(t, err)
			live := <-opened
			require.Equal(t, [2]uint16{80, 24}, <-live.sizes)
			require.NoError(t, terminal.Resize(43, 15))
			require.Equal(t, [2]uint16{43, 15}, <-live.sizes)
			input := []byte("hello \x1b[A\x03\rПривет")
			go func() { _, _ = terminal.Write(input) }()
			output := make([]byte, len(input))
			_, err = io.ReadFull(terminal, output)
			require.NoError(t, err)
			require.Equal(t, input, output)
			require.NoError(t, terminal.Close())
			select {
			case <-live.closed:
			case <-ctx.Done():
				t.Fatal("disconnect did not detach terminal")
			}
			// A new connection reaches the same target, without creating another agent or changing crew state.
			next, err := owner.OpenTerminal(ctx, transport, "worker", "ops", "dev", 90, 25)
			require.NoError(t, err)
			require.NoError(t, next.Close())
		})
	}
}

func TestMeshTerminalDoesNotGrantPeerCommandAuthority(t *testing.T) {
	owner := newOwner(t, "owner")
	peer := enroll(t, owner, "peer")
	worker := enroll(t, owner, "worker")
	service, _ := echoService(t)
	transport := &terminalPipes{nodes: map[string]*Node{"peer": peer, "worker": worker}, services: map[string]*TerminalServices{"worker": service}}
	addPeer(t, peer, "worker")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := peer.TerminalList(ctx, transport, "worker")
	require.ErrorContains(t, err, "owner node")
	// Even a caller that writes the terminal frame itself needs the separate owner proof.
	rw, err := transport.Dial(ctx, Addr{Cmd: "worker"})
	require.NoError(t, err)
	defer func() { _ = rw.Close() }()
	c := newConn(rw)
	_, err = peer.open(c, "worker", time.Now)
	require.NoError(t, err)
	require.NoError(t, c.send(wire{Op: "terminal/list", Terminal: &TerminalRequest{}}))
	_, err = c.expect("terminal/targets")
	require.ErrorContains(t, err, "owner's signature")
}

func TestTerminalSignatureCannotBeReplayedOrChanged(t *testing.T) {
	owner := newOwner(t, "owner")
	worker := enroll(t, owner, "worker")
	service, _ := echoService(t)
	transport := &terminalPipes{nodes: map[string]*Node{"worker": worker}, services: map[string]*TerminalServices{"worker": service}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rw, err := transport.Dial(ctx, Addr{Cmd: "worker"})
	require.NoError(t, err)
	defer func() { _ = rw.Close() }()
	c := newConn(rw)
	_, err = owner.open(c, "worker", time.Now)
	require.NoError(t, err)
	// Sign against a different connection's challenge.
	c.challenge = []byte("old handshake")
	require.NoError(t, owner.terminalRequest(c, "terminal/list", TerminalRequest{}))
	_, err = c.expect("terminal/targets")
	require.ErrorContains(t, err, "owner's signature")
}

func TestTerminalRelayLoopsAreBounded(t *testing.T) {
	owner := newOwner(t, "owner")
	require.NoError(t, owner.UpdatePeers(func(ps []Peer) []Peer {
		return []Peer{{Name: "a", Through: []string{"b"}}, {Name: "b", Through: []string{"a"}}}
	}))
	_, err := owner.TerminalList(context.Background(), &terminalPipes{}, "a")
	require.ErrorContains(t, err, "looping")
}

func TestTerminalHistoryCapKeepsValidUTF8(t *testing.T) {
	text := strings.Repeat("界", (2<<20)/3+2)
	bounded := HistoryText(text)
	require.LessOrEqual(t, len(bounded), 2<<20)
	require.True(t, utf8.ValidString(bounded))
	require.True(t, strings.HasSuffix(text, bounded))
}

func TestChatOwnerOperationsThroughRelay(t *testing.T) {
	owner := newOwner(t, "owner")
	jump := enroll(t, owner, "jump")
	worker := enroll(t, owner, "worker")
	addPeer(t, owner, "jump")
	addPeer(t, jump, "worker")
	require.NoError(t, owner.UpdatePeers(func(ps []Peer) []Peer { return append(ps, Peer{Name: "worker", Through: []string{"jump"}}) }))
	service := &TerminalServices{ChatRead: func(crew, role string) (chat.Snapshot, error) {
		require.Equal(t, "ops", crew)
		require.Equal(t, "dev", role)
		return chat.Snapshot{Role: role, State: "up", Messages: []chat.Message{{ID: "one", Role: "assistant", Text: "hello"}}}, nil
	}, ChatSend: func(crew, role, id, text string) (chat.Snapshot, error) {
		require.Equal(t, "request", id)
		return chat.Snapshot{Role: role, Pending: []chat.Message{{ID: id, Text: text, State: "queued"}}}, nil
	}}
	transport := &terminalPipes{nodes: map[string]*Node{"jump": jump, "worker": worker}, services: map[string]*TerminalServices{"worker": service}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := owner.Chat(ctx, transport, "worker", "chat/read", TerminalRequest{Crew: "ops", Role: "dev"})
	require.NoError(t, err)
	require.Equal(t, "hello", got.Messages[0].Text)
	service.ChatReadSince = func(crew, role, cursor string) (chat.Snapshot, error) {
		require.Equal(t, "resume-cursor", cursor)
		return chat.Snapshot{Role: role, Cursor: "next-cursor", Messages: []chat.Message{}}, nil
	}
	resumed, err := owner.Chat(ctx, transport, "worker", "chat/read", TerminalRequest{Crew: "ops", Role: "dev", Cursor: "resume-cursor"})
	require.NoError(t, err)
	require.Equal(t, "next-cursor", resumed.Cursor)

	got, err = owner.Chat(ctx, transport, "worker", "chat/send", TerminalRequest{Crew: "ops", Role: "dev", ID: "request", Text: "question"})
	require.NoError(t, err)
	require.Equal(t, "question", got.Pending[0].Text)
	_, err = jump.Chat(ctx, transport, "worker", "chat/read", TerminalRequest{Crew: "ops", Role: "dev"})
	require.ErrorContains(t, err, "owner node")
}

func TestChatAttachmentsFollowOwnerSignedRelayRoute(t *testing.T) {
	owner := newOwner(t, "owner")
	jump := enroll(t, owner, "jump")
	worker := enroll(t, owner, "worker")
	addPeer(t, owner, "jump")
	addPeer(t, jump, "worker")
	require.NoError(t, owner.UpdatePeers(func(peers []Peer) []Peer { return append(peers, Peer{Name: "worker", Through: []string{"jump"}}) }))
	file := chat.Upload{ID: "abcdefab-1234-1234-1234-123456789abc", Name: "image.png", Data: []byte{0, 255, 1, 2}}
	service := &TerminalServices{ChatUpload: func(crew, role string, in chat.Upload) (chat.Attachment, error) {
		require.Equal(t, "ops", crew)
		require.Equal(t, "dev", role)
		require.Equal(t, file, in)
		return chat.Attachment{ID: in.ID, Name: in.Name, Size: len(in.Data)}, nil
	}, ChatFile: func(crew, role, id string) (chat.Upload, error) { require.Equal(t, file.ID, id); return file, nil }, ChatSend: func(string, string, string, string) (chat.Snapshot, error) {
		t.Fatal("must not discard attachments on an old handler")
		return chat.Snapshot{}, nil
	}}
	transport := &terminalPipes{nodes: map[string]*Node{"jump": jump, "worker": worker}, services: map[string]*TerminalServices{"worker": service}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	uploaded, err := owner.Chat(ctx, transport, "worker", "chat/upload", TerminalRequest{Crew: "ops", Role: "dev", Upload: &file})
	require.NoError(t, err)
	require.Equal(t, file.ID, uploaded.Uploaded.ID)
	downloaded, err := owner.Chat(ctx, transport, "worker", "chat/file", TerminalRequest{Crew: "ops", Role: "dev", FileID: file.ID})
	require.NoError(t, err)
	require.Equal(t, file, *downloaded.File)
	_, err = owner.Chat(ctx, transport, "worker", "chat/send", TerminalRequest{Crew: "ops", Role: "dev", Files: []string{file.ID}})
	require.ErrorContains(t, err, "update")
	_, err = jump.Chat(ctx, transport, "worker", "chat/upload", TerminalRequest{Crew: "ops", Role: "dev", Upload: &file})
	require.ErrorContains(t, err, "owner node")
}

func TestCrewStatusIsOwnerOnlyAndRelayed(t *testing.T) {
	owner := newOwner(t, "owner")
	jump := enroll(t, owner, "jump")
	worker := enroll(t, owner, "worker")
	old := enroll(t, owner, "old")
	addPeer(t, owner, "jump")
	addPeer(t, owner, "old")
	addPeer(t, jump, "worker")
	require.NoError(t, owner.UpdatePeers(func(ps []Peer) []Peer { return append(ps, Peer{Name: "worker", Through: []string{"jump"}}) }))
	service := &TerminalServices{Status: func(kind string) ([]byte, error) { return []byte(`{"kind":"` + kind + `"}`), nil }}
	transport := &terminalPipes{nodes: map[string]*Node{"jump": jump, "worker": worker, "old": old},
		services: map[string]*TerminalServices{"worker": service, "old": {}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got, err := owner.CrewStatus(ctx, transport, "worker", "")
	require.NoError(t, err)
	require.JSONEq(t, `{"kind":""}`, string(got))
	got, err = owner.CrewStatus(ctx, transport, "worker", "cost")
	require.NoError(t, err)
	require.JSONEq(t, `{"kind":"cost"}`, string(got))

	_, err = jump.CrewStatus(ctx, transport, "worker", "")
	require.ErrorContains(t, err, "owner node")
	_, err = owner.CrewStatus(ctx, transport, "old", "")
	require.ErrorContains(t, err, "update zmina")
}

// The owner answers a peer's item from its own cockpit: signed with the owner's key, relayed, done on the item's node;
// a certified peer that is not the owner cannot, and an older node says to update.
func TestActIsOwnerOnlyAndRelayed(t *testing.T) {
	owner := newOwner(t, "owner")
	jump := enroll(t, owner, "jump")
	worker := enroll(t, owner, "worker")
	old := enroll(t, owner, "old")
	addPeer(t, owner, "jump")
	addPeer(t, owner, "old")
	addPeer(t, jump, "worker")
	require.NoError(t, owner.UpdatePeers(func(ps []Peer) []Peer { return append(ps, Peer{Name: "worker", Through: []string{"jump"}}) }))
	var did []string
	service := &TerminalServices{
		TodoAct: func(crew, action, id, text string) (string, error) {
			did = append(did, crew+" "+action+" "+id+" "+text)
			return id + " · " + action + " · queued for the dispatcher", nil
		},
		AlarmAct: func(crew, action, id, when, text, role string) (string, error) {
			did = append(did, crew+" "+action+" "+id+" "+when)
			return id + " moved", nil
		},
	}
	transport := &terminalPipes{nodes: map[string]*Node{"jump": jump, "worker": worker, "old": old},
		services: map[string]*TerminalServices{"worker": service, "old": {}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	line, err := owner.Act(ctx, transport, "worker", "todo/act", TerminalRequest{Crew: "ops", Action: "answer", ID: "A-3", Text: "go"})
	require.NoError(t, err)
	require.Equal(t, "A-3 · answer · queued for the dispatcher", line)
	line, err = owner.Act(ctx, transport, "worker", "alarm/act", TerminalRequest{Crew: "ops", Action: "move", ID: "R-2", When: "+2h"})
	require.NoError(t, err)
	require.Equal(t, "R-2 moved", line)
	require.Equal(t, []string{"ops answer A-3 go", "ops move R-2 +2h"}, did)

	_, err = jump.Act(ctx, transport, "worker", "todo/act", TerminalRequest{Crew: "ops", Action: "yes", ID: "A-3"})
	require.ErrorContains(t, err, "owner node")
	_, err = owner.Act(ctx, transport, "old", "todo/act", TerminalRequest{Crew: "ops", Action: "yes", ID: "A-3"})
	require.ErrorContains(t, err, "update zmina")
	require.Len(t, did, 2)
}

// A write signed for one node is not done by another: a peer that got the owner's proof cannot pass it to a session
// with a third node.
func TestActIsDoneOnlyByTheNodeItNames(t *testing.T) {
	owner := newOwner(t, "owner")
	worker := enroll(t, owner, "worker")
	addPeer(t, owner, "worker")
	ran := false
	service := &TerminalServices{TodoAct: func(crew, action, id, text string) (string, error) { ran = true; return "done", nil }}
	transport := &terminalPipes{nodes: map[string]*Node{"worker": worker}, services: map[string]*TerminalServices{"worker": service}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rw, c, err := owner.terminalConnect(ctx, transport, "worker")
	require.NoError(t, err)
	defer func() { _ = rw.Close() }()
	require.NoError(t, owner.terminalRequest(c, "todo/act", TerminalRequest{Crew: "ops", Action: "yes", ID: "A-1", To: "jump"}))
	_, err = c.expect("act/result")
	require.ErrorContains(t, err, `this request is for node "jump", not worker`)
	require.False(t, ran)

	line, err := owner.Act(ctx, transport, "worker", "todo/act", TerminalRequest{Crew: "ops", Action: "yes", ID: "A-1"})
	require.NoError(t, err)
	require.Equal(t, "done", line)
}

// A conversation's ids resolve on the node its crew lives on: owner only, relayed, an older node says to update.
func TestItemReadIsOwnerOnlyAndRelayed(t *testing.T) {
	owner := newOwner(t, "owner")
	jump := enroll(t, owner, "jump")
	worker := enroll(t, owner, "worker")
	old := enroll(t, owner, "old")
	addPeer(t, owner, "jump")
	addPeer(t, owner, "old")
	addPeer(t, jump, "worker")
	require.NoError(t, owner.UpdatePeers(func(ps []Peer) []Peer { return append(ps, Peer{Name: "worker", Through: []string{"jump"}}) }))
	service := &TerminalServices{Items: func(crew, ids string) ([]byte, error) { return []byte(`{"crew":"` + crew + `","ids":"` + ids + `"}`), nil }}
	transport := &terminalPipes{nodes: map[string]*Node{"jump": jump, "worker": worker, "old": old},
		services: map[string]*TerminalServices{"worker": service, "old": {}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got, err := owner.ItemRead(ctx, transport, "worker", "ops", "T-1,L-2")
	require.NoError(t, err)
	require.JSONEq(t, `{"crew":"ops","ids":"T-1,L-2"}`, string(got))

	_, err = jump.ItemRead(ctx, transport, "worker", "ops", "T-1")
	require.ErrorContains(t, err, "owner node")
	_, err = owner.ItemRead(ctx, transport, "old", "ops", "T-1")
	require.ErrorContains(t, err, "update zmina")
}

// A role's files resolve and open on the node the role runs on: owner only, relayed, an older node says to update.
func TestChatRoleFilesAreOwnerOnlyAndRelayed(t *testing.T) {
	owner := newOwner(t, "owner")
	jump := enroll(t, owner, "jump")
	worker := enroll(t, owner, "worker")
	old := enroll(t, owner, "old")
	addPeer(t, owner, "jump")
	addPeer(t, owner, "old")
	addPeer(t, jump, "worker")
	require.NoError(t, owner.UpdatePeers(func(ps []Peer) []Peer { return append(ps, Peer{Name: "worker", Through: []string{"jump"}}) }))
	service := &TerminalServices{
		ChatPaths: func(crew, role, paths string) (chat.Snapshot, error) {
			return chat.Snapshot{Paths: []chat.PathInfo{{Path: paths, Name: crew + "/" + role, Size: 3, Type: "image/png"}}}, nil
		},
		ChatRoleFile: func(crew, role, path string) (chat.Upload, error) { return chat.Upload{Name: path, Data: []byte("png")}, nil },
	}
	transport := &terminalPipes{nodes: map[string]*Node{"jump": jump, "worker": worker, "old": old},
		services: map[string]*TerminalServices{"worker": service, "old": {}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got, err := owner.Chat(ctx, transport, "worker", "chat/paths", TerminalRequest{Crew: "ops", Role: "dev", Text: "/tmp/a.png"})
	require.NoError(t, err)
	require.Equal(t, []chat.PathInfo{{Path: "/tmp/a.png", Name: "ops/dev", Size: 3, Type: "image/png"}}, got.Paths)
	got, err = owner.Chat(ctx, transport, "worker", "chat/rolefile", TerminalRequest{Crew: "ops", Role: "dev", Text: "/tmp/a.png"})
	require.NoError(t, err)
	require.Equal(t, "png", string(got.File.Data))

	_, err = jump.Chat(ctx, transport, "worker", "chat/paths", TerminalRequest{Crew: "ops", Role: "dev", Text: "/tmp/a.png"})
	require.ErrorContains(t, err, "owner node")
	_, err = owner.Chat(ctx, transport, "old", "chat/paths", TerminalRequest{Crew: "ops", Role: "dev", Text: "/tmp/a.png"})
	require.ErrorContains(t, err, "update")
}
