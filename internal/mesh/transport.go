package mesh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

// Addr is one way to reach a node. SSH is [user@]host and may be told to other nodes; Cmd is a command line on
// this machine that reaches the peer's zmina (`ssh box sudo -iu ops zmina`) and is never told to anyone: a peer
// could otherwise make this node run a command of its choosing.
type Addr struct {
	SSH string `json:"ssh,omitempty"`
	Cmd string `json:"cmd,omitempty"`
}

func (a Addr) String() string {
	if a.SSH != "" {
		return "ssh " + a.SSH
	}

	return a.Cmd
}

// sshRe is a [user@]host an ssh address may name: nothing ssh would read as an option.
var sshRe = regexp.MustCompile(`^([A-Za-z0-9._-]+@)?[A-Za-z0-9][A-Za-z0-9.-]*$`)

// Check refuses an address that is neither or both kinds, or an ssh host that is not a plain name.
func (a Addr) Check() error {
	switch {
	case (a.SSH == "") == (a.Cmd == ""):
		return errors.New("an address is either ssh or cmd")
	case a.SSH != "" && !sshRe.MatchString(a.SSH):
		return fmt.Errorf("ssh address %q: want [user@]host", a.SSH)
	}

	return nil
}

// Transport opens a stream to a node's `zmina node serve --stdio`. Another network under the mesh is another
// Transport (or another kind of Addr); the protocol over the stream stays the same.
type Transport interface {
	Dial(ctx context.Context, a Addr) (io.ReadWriteCloser, error)
}

// Discovery tells which nodes there are and how to reach them. The address book is one; a tailnet, mDNS or DNS
// would be others, merged into the same book.
type Discovery interface {
	Peers(ctx context.Context) ([]Peer, error)
}

// Exec is the Transport that runs a command: ssh for an ssh address, sh -c for a command line.
type Exec struct{}

// Dial starts the command; closing the stream ends it and reports what it wrote to stderr if it failed.
func (Exec) Dial(ctx context.Context, a Addr) (io.ReadWriteCloser, error) {
	if err := a.Check(); err != nil {
		return nil, err
	}

	c := Command(ctx, a, "node", "serve", "--stdio")

	in, err := c.StdinPipe()
	if err != nil {
		return nil, err
	}

	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}

	s := &cmdStream{cmd: c, in: in, out: out}
	c.Stderr = &s.stderr

	if err := c.Start(); err != nil {
		return nil, err
	}

	return s, nil
}

// Command runs the peer's zmina at a with args: plain words this package or the command line chose (a node name
// at most), never a peer's text.
func Command(ctx context.Context, a Addr, args ...string) *exec.Cmd {
	words := strings.Join(args, " ")
	if a.SSH != "" {
		return exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "--", a.SSH,
			`PATH="$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:$PATH" zmina `+words)
	}

	return exec.CommandContext(ctx, "sh", "-c", a.Cmd+" "+words)
}

type cmdStream struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    io.Reader
	once   sync.Once
	err    error
	stderr bytes.Buffer
}

func (s *cmdStream) Read(p []byte) (int, error) {
	n, err := s.out.Read(p)
	if errors.Is(err, io.EOF) {
		// The peer's side ended: say why, if it said.
		if msg := s.why(); msg != "" {
			return n, errors.New(msg)
		}
	}

	return n, err
}

func (s *cmdStream) Write(p []byte) (int, error) { return s.in.Write(p) }

func (s *cmdStream) Close() error {
	s.once.Do(func() {
		_ = s.in.Close()
		if err := s.cmd.Wait(); err != nil {
			s.err = err
			if msg := s.why(); msg != "" {
				s.err = errors.New(msg)
			}
		}
	})

	return s.err
}

// why is the last line the command wrote to stderr.
func (s *cmdStream) why() string {
	lines := strings.Split(strings.TrimSpace(s.stderr.String()), "\n")

	return strings.TrimSpace(lines[len(lines)-1])
}
