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

// OwnerCommand runs the peer's zmina at a with args for the owner at this terminal (zmina ls/peek/attach <node>/<crew>),
// never letting ssh ask for a password: ssh, the --ssh address or a command line whose first word is ssh, runs with
// BatchMode=yes ahead of the line's own options (ssh takes an option's first value), and with tty a terminal is
// requested (-t just before the host, after any -T or RequestTTY of the line). Any other command line runs as it is.
// An ssh line this cannot change safely (ownerLine) is refused.
func OwnerCommand(ctx context.Context, a Addr, tty bool, args ...string) (*exec.Cmd, error) {
	words := strings.Join(args, " ")
	if a.SSH != "" {
		argv := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
		if tty {
			argv = append(argv, "-t")
		}

		return exec.CommandContext(ctx, "ssh", append(argv, "--", a.SSH,
			`PATH="$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:$PATH" zmina `+words)...), nil
	}

	line, err := ownerLine(a.Cmd, tty)
	if err != nil {
		return nil, err
	}

	return exec.CommandContext(ctx, "sh", "-c", line+" "+words), nil
}

// sshArgOpts are ssh's one-letter options that take an argument.
const sshArgOpts = "BbcDEeFIiJLlmOoPpQRSWw"

// shellSpecial is what makes a command line more than words split on blanks: quotes, escapes, expansions that may
// split or join words, operators. A ~ stays one word.
const shellSpecial = "'\"\\$`;&|<>(){}*?[]#!\n\r"

// ownerLine is a command line address made safe for the owner's terminal: one whose first word is ssh gets
// -o BatchMode=yes after it and, with tty, -t before its host, inserted into the line as written. Only a plain line
// is changed — words split on blanks, nothing a shell would read otherwise (shellSpecial); an ssh line with quotes,
// escapes or expansions is refused, since its words cannot be told without a shell. Other lines run as written.
func ownerLine(line string, tty bool) (string, error) {
	spans := regexp.MustCompile(`[^ \t]+`).FindAllStringIndex(line, -1)
	if len(spans) == 0 || line[spans[0][0]:spans[0][1]] != "ssh" {
		return line, nil
	}

	if strings.ContainsAny(line, shellSpecial) {
		return "", fmt.Errorf("the command line %q has quotes, escapes or shell syntax, so zmina cannot add BatchMode "+
			"and a terminal to its ssh safely: add the node with --ssh [user@]host instead, and put ssh options in "+
			"~/.ssh/config", line)
	}

	word := func(i int) string { return line[spans[i][0]:spans[i][1]] }

	host := len(spans)
	for i := 1; i < len(spans); i++ {
		w := word(i)
		if w == "--" || len(w) < 2 || w[0] != '-' {
			host = i

			break
		}

		// A run of flags; the first that takes an argument takes the rest of the word, or the next one.
		for j := 1; j < len(w); j++ {
			if strings.IndexByte(sshArgOpts, w[j]) >= 0 {
				if j == len(w)-1 {
					i++
				}

				break
			}
		}
	}

	out := line[:spans[0][1]] + " -o BatchMode=yes"
	if !tty || host >= len(spans) {
		return out + line[spans[0][1]:], nil
	}

	at := spans[host][0]

	return out + line[spans[0][1]:at] + "-t " + line[at:], nil
}

type cmdStream struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    io.Reader
	once   sync.Once
	err    error
	stderr lockedBuffer
}

// lockedBuffer is the command's stderr: os/exec copies into it from its own goroutine until Wait returns, while
// Read looks at it as soon as stdout ends, before Wait.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
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
