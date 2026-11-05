package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/terminal"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// terminalServices only exposes roles resolved from this node's crew files, including running copies.
func (a *app) terminalServices() *mesh.TerminalServices {
	return &mesh.TerminalServices{List: a.localTerminals, Open: a.localTerminal, History: a.localTerminalHistory, ChatRead: a.localChatRead, ChatReadSince: a.localChatReadSince, ChatSend: a.localChatSend, ChatSendFiles: a.localChatSendFiles, ChatUpload: a.localChatUpload, ChatFile: a.localChatFile, ChatAction: a.localChatAction, Status: a.localStatus}
}

func (a *app) localTerminalHistory(key, role string, lines int) (string, error) {
	if lines < 1 || lines > 10000 {
		return "", errors.New("history lines: 1–10000")
	}
	c, err := a.crewApp(key)
	if err != nil {
		return "", err
	}
	var text string
	err = c.readState(func(s *shift.Shift) error {
		r, err := s.Role(role)
		if err != nil {
			return err
		}
		if r.Runtime != "codex" {
			if session := s.Reg.LiveByName(r.Session); session != nil {
				transcript, err := (&registry.Registry{Namespace: s.Crew.Namespace}).History(session.SessionID, lines)
				if err != nil {
					return err
				}
				if transcript != "" {
					text = "Recent conversation (saved messages)\n\n" + transcript
					return nil
				}
			}
		}
		t, err := s.Tmux.(*lazyTmux).client()
		if err != nil {
			return err
		}
		text, err = t.Capture(role, lines)
		return err
	})
	return mesh.HistoryText(text), err
}

func (a *app) terminalHistory(ctx context.Context, node, key, role string, lines int) (string, error) {
	if node == "" {
		return a.localTerminalHistory(key, role, lines)
	}
	n, err := openNode()
	if err != nil {
		return "", err
	}
	if node == n.Name {
		return a.localTerminalHistory(key, role, lines)
	}
	return n.TerminalHistory(ctx, mesh.Exec{}, node, key, role, lines)
}
func (a *app) localTerminals() ([]mesh.TerminalTarget, error) {
	out := []mesh.TerminalTarget{}
	for _, c := range uiCrews(a.extraCrew()) {
		err := a.forCrew(c.Path).readState(func(s *shift.Shift) error {
			for _, v := range s.Views() {
				out = append(out, mesh.TerminalTarget{Crew: c.Key, Role: v.Role, State: string(v.State), Runtime: v.Runtime})
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Key, err)
		}
	}
	return out, nil
}
func (a *app) localTerminal(ctx context.Context, key, role string, cols, rows uint16) (mesh.Terminal, error) {
	if cols < 2 || cols > 500 || rows < 2 || rows > 300 {
		return nil, errors.New("invalid terminal size")
	}
	c, err := a.crewApp(key)
	if err != nil {
		return nil, err
	}
	var argv []string
	err = c.readState(func(s *shift.Shift) error {
		if _, err := s.Role(role); err != nil {
			return err
		}
		t, err := s.Tmux.(*lazyTmux).client()
		if err != nil {
			return err
		}
		p, err := t.Probe(role)
		if err != nil {
			return err
		}
		if p.State != tmux.Up {
			return fmt.Errorf("%s is not running; start the role before attaching", role)
		}
		argv = t.AttachArgv(role)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return terminal.Attach(ctx, argv, cols, rows)
}
func (a *app) terminalList(ctx context.Context, node string) ([]mesh.TerminalTarget, error) {
	if node == "" {
		return a.localTerminals()
	}
	n, err := openNode()
	if err != nil {
		return nil, err
	}
	if node == n.Name {
		return a.localTerminals()
	}
	return n.TerminalList(ctx, mesh.Exec{}, node)
}
func (a *app) openTerminal(ctx context.Context, node, crew, role string, cols, rows uint16) (mesh.Terminal, error) {
	if node == "" {
		return a.localTerminal(ctx, crew, role, cols, rows)
	}
	n, err := openNode()
	if err != nil {
		return nil, err
	}
	if node == n.Name {
		return a.localTerminal(ctx, crew, role, cols, rows)
	}
	return n.OpenTerminal(ctx, mesh.Exec{}, node, crew, role, cols, rows)
}

func (a *app) terminalCommand() *cobra.Command {
	c := &cobra.Command{Use: "terminal", Short: "List or attach role terminals through the mesh (from the owner node)"}
	var asJSON bool
	list := &cobra.Command{Use: "ls [node]", Short: "List configured role terminals on this node or a mesh peer", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		node := ""
		if len(args) > 0 {
			node = args[0]
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		targets, err := a.terminalList(ctx, node)
		if err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(a.out).Encode(targets)
		}
		for _, t := range targets {
			fmt.Fprintf(a.out, "%s/%s · %s\n", t.Crew, t.Role, t.State)
		}
		return nil
	}}
	list.Flags().BoolVar(&asJSON, "json", false, "print terminal targets as JSON")
	var node, crew string
	attach := &cobra.Command{Use: "attach <role>", Short: "Attach to the same tmux client over mesh; detach with Ctrl-B then D", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		fd := int(os.Stdin.Fd())
		if !term.IsTerminal(fd) {
			return errors.New("terminal attach needs a terminal")
		}
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()
		tty, err := a.openTerminal(ctx, node, crew, args[0], uint16(cols), uint16(rows))
		if err != nil {
			return err
		}
		defer func() { _ = tty.Close() }()
		old, err := term.MakeRaw(fd)
		if err != nil {
			return err
		}
		defer func() { _ = term.Restore(fd, old) }()
		sizes := make(chan os.Signal, 1)
		signal.Notify(sizes, syscall.SIGWINCH)
		defer signal.Stop(sizes)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-sizes:
					if x, y, e := term.GetSize(fd); e == nil {
						_ = tty.Resize(uint16(x), uint16(y))
					}
				}
			}
		}()
		go func() { _, _ = io.Copy(tty, os.Stdin) }()
		_, err = io.Copy(a.out, tty)
		return err
	}}
	attach.Flags().StringVar(&node, "node", "", "mesh node (default this node)")
	attach.Flags().StringVar(&crew, "crew-key", "default", "crew key on the destination node")
	c.AddCommand(list, attach)
	return c
}
