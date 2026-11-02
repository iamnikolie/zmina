// Package terminal attaches a disposable PTY client to an existing tmux role.
package terminal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
)

// Client owns the tmux attachment, never the agent's session.
type Client struct {
	file   *os.File
	cmd    *exec.Cmd
	cancel context.CancelFunc
	once   sync.Once
}

// Attach executes a fixed tmux argv chosen from the crew, with a real PTY for full-screen applications.
func Attach(ctx context.Context, argv []string, cols, rows uint16) (*Client, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty terminal command")
	}
	child, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(child, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.WaitDelay = 2 * time.Second
	file, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		cancel()
		return nil, err
	}
	client := &Client{file: file, cmd: cmd, cancel: cancel}
	go func() { <-child.Done(); _ = client.Close() }()
	return client, nil
}
func (c *Client) Read(p []byte) (int, error)  { return c.file.Read(p) }
func (c *Client) Write(p []byte) (int, error) { return c.file.Write(p) }
func (c *Client) Resize(cols, rows uint16) error {
	return pty.Setsize(c.file, &pty.Winsize{Cols: cols, Rows: rows})
}
func (c *Client) Close() error {
	c.once.Do(func() { c.cancel(); _ = c.file.Close(); _ = c.cmd.Wait() })
	return nil
}
