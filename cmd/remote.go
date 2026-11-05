package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"

	"github.com/iamnikolie/zmina/internal/mesh"
)

// remoteWord is what a word sent to another node's command line may be: role names, flags and numbers, never
// anything its shells would read.
var remoteWord = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._#=,:+-]*$|^--?[A-Za-z][A-Za-z0-9-]*(=[A-Za-z0-9._#,:+-]*)?$`)

// remoteLookup is how long the check of a far crew may take.
const remoteLookup = 30 * time.Second

// splitAddress reads a <node>/<crew> address at the head of args: the address and the rest, ok false when the first
// argument names no address (a role of this crew, as before).
func splitAddress(args []string) (mesh.Address, []string, bool, error) {
	if len(args) == 0 || !strings.Contains(args[0], "/") {
		return mesh.Address{}, args, false, nil
	}

	ad, err := mesh.ParseAddress(args[0])
	if err != nil || ad.Crew == "" {
		return mesh.Address{}, nil, false, fmt.Errorf("address %q: want <node>/<crew> (zmina node ls)", args[0])
	}

	return ad, args[1:], true, nil
}

// remote runs cmd on the crew an address names, when args start with one: on this node it aims this run at that
// crew and returns false so the command goes on here; on another it runs the far zmina through the node's address
// (the command line `zmina node peer add` stored) with the same arguments and flags, and returns true. interactive
// runs it on this terminal (ssh -t), for attach.
func (a *app) remote(cmd *cobra.Command, args []string, interactive bool) (bool, []string, error) {
	ad, rest, ok, err := splitAddress(args)
	if err != nil || !ok {
		return false, rest, err
	}

	// peek and attach name one role (ls none or several): said here, not by a far zmina's arity error.
	if cmd.Name() != "ls" && len(rest) != 1 {
		return false, nil, fmt.Errorf("%s takes a role after the address: zmina %s <node>/<crew> <role>", cmd.Name(), cmd.Name())
	}

	me, err := mesh.Open(meshDir())
	if err != nil {
		return false, nil, fmt.Errorf("%s: this machine is no node (zmina node init): %w", ad, err)
	}

	if ad.Node == me.Name {
		c, err := localCrew(ad.Crew)
		if err != nil {
			return false, nil, fmt.Errorf("%s: %w", ad, err)
		}

		a.crew = c.Path

		return false, rest, nil
	}

	addr, err := peerAddr(me, ad.Node)
	if err != nil {
		return false, nil, err
	}

	if err := checkFarCrew(addr, ad); err != nil {
		return false, nil, err
	}

	// Always named, the default crew too (--config default): the far zmina's $ZMINA_CREW never picks the crew.
	words := []string{"--config", ad.Crew}

	words = append(words, cmd.Name())
	words = append(words, rest...)
	// The command's own flags as given (LocalFlags is a fresh set: it knows no flag was set); --crew and --config
	// are this node's.
	local := cmd.LocalFlags()
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if local.Lookup(f.Name) != nil {
			words = append(words, "--"+f.Name+"="+f.Value.String())
		}
	})

	for _, w := range words {
		if !remoteWord.MatchString(w) {
			return false, nil, fmt.Errorf("%q cannot go to another node's command line: plain names, flags and numbers only", w)
		}
	}

	var c *exec.Cmd
	if interactive {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return false, nil, fmt.Errorf("no terminal here; run it in one: zmina %s %s", cmd.Name(), strings.Join(args, " "))
		}

		c = mesh.OwnerCommand(context.Background(), addr, true, words...)
		c.Stdin = os.Stdin
	} else {
		c = mesh.OwnerCommand(context.Background(), addr, false, words...)
	}

	c.Stdout, c.Stderr = a.out, a.err

	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() > 0 {
			// The far zmina said why on stderr already.
			cmd.SilenceErrors, cmd.SilenceUsage = true, true

			return true, nil, &exitError{code: ee.ExitCode(), msg: fmt.Sprintf("%s: exit %d", ad, ee.ExitCode())}
		}

		return true, nil, fmt.Errorf("%s: %w", ad, err)
	}

	return true, nil, nil
}

// peerAddr is the address this node reaches a peer at, or why there is none.
func peerAddr(me *mesh.Node, name string) (mesh.Addr, error) {
	peers, err := me.Peers()
	if err != nil {
		return mesh.Addr{}, err
	}

	i := slices.IndexFunc(peers, func(p mesh.Peer) bool { return p.Name == name })
	if i < 0 {
		names := []string{me.Name}
		for _, p := range peers {
			names = append(names, p.Name)
		}

		return mesh.Addr{}, fmt.Errorf("no node %q (nodes: %s; zmina node ls)", name, strings.Join(names, ", "))
	}

	p := peers[i]
	for _, ad := range p.Addrs {
		if f := strings.Fields(ad.Cmd); slices.ContainsFunc(f, func(w string) bool { return w == "--crew" || strings.HasPrefix(w, "--crew=") }) {
			return mesh.Addr{}, fmt.Errorf("node %q: its address picks a crew file itself (--crew in %q), so no crew of it can "+
				"be named; give it a command line that ends in zmina (zmina node peer add %s --cmd …)", name, ad.Cmd, name)
		}
	}

	if len(p.Addrs) == 0 {
		how := "it calls in"
		if len(p.Through) > 0 {
			how = "reached through " + strings.Join(p.Through, ", ")
		}

		return mesh.Addr{}, fmt.Errorf("node %q has no address of its own here (%s): run this on a node that reaches it, "+
			"or zmina terminal attach <role> --node %s --crew-key <crew>", name, how, name)
	}

	return p.Addrs[0], nil
}

// checkFarCrew asks the far node for its crews (zmina node ls --json there) and refuses a crew it does not have. A
// far zmina too old to list them is left to say so itself.
func checkFarCrew(addr mesh.Addr, ad mesh.Address) error {
	ctx, cancel := context.WithTimeout(context.Background(), remoteLookup)
	defer cancel()

	c := mesh.OwnerCommand(ctx, addr, false, "node", "ls", "--json")

	var errb bytes.Buffer
	c.Stderr = &errb

	out, err := c.Output()
	if err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return fmt.Errorf("%s: %s", ad.Node, msg)
		}

		return fmt.Errorf("%s: %w", ad.Node, err)
	}

	var v struct {
		Crews []string `json:"crews"`
	}
	if json.Unmarshal(out, &v) != nil || v.Crews == nil {
		return nil
	}

	if !slices.Contains(v.Crews, ad.Crew) {
		return fmt.Errorf("no crew %q on node %s (crews: %s)", ad.Crew, ad.Node, strings.Join(v.Crews, ", "))
	}

	return nil
}
