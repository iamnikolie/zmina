package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/mesh"
)

// crewSyncRefusal is why a push to the peer is not even tried, "" when its last session said crew_sync: a zmina from before
// the push does not know the op, so the owner's node never sends it one (the same guard as a certificate with an expiry,
// checkLife).
func crewSyncRefusal(p mesh.Peer) string {
	switch {
	case p.Cert == nil || p.OK.IsZero():
		return fmt.Sprintf("%s: no session with it yet, so it has not said crew_sync: zmina node sync first", p.Name)
	case !p.CrewSync:
		return fmt.Sprintf("%s did not say crew_sync in its last session: a zmina from before crew push, or no session since this one "+
			"was installed: update it, then zmina node sync", p.Name)
	}

	return ""
}

func (a *app) crewPushCommand() *cobra.Command {
	var (
		create bool
		base   string
	)

	c := &cobra.Command{
		Use:   "push <node> [--create] [--base <sum>]",
		Short: "Send this crew's file to another node, in place of its own (owner node only): refused when that node holds another file than the one last sent or confirmed there, and the difference is shown",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// A push changes what another node runs, signed with the owner's key: the owner's hand, never a role's (the role of a session
			// on the owner's node has the key's file within reach, and the command in its environment).
			if who := a.actor(); who != "hand" {
				return fmt.Errorf("zmina crew push is the owner's; a role (%s) does not run it%s", who, codexThreadWords(who))
			}

			cr, err := a.loadCrew()
			if err != nil {
				return err
			}

			me, err := openNode()
			if err != nil {
				return err
			}

			if !me.HoldsOwnerKey() {
				return errors.New("crew push is the owner's: it is signed with owner.key, which only the owner's node holds")
			}

			target := args[0]

			switch {
			case !cr.Spread():
				return errors.New("this crew's file declares no nodes:, so there is nothing to push: a crew over nodes is one file on every node")
			case target == me.Name:
				return fmt.Errorf("%s is this node", target)
			case !cr.HasNode(target):
				return fmt.Errorf("%s is not one of nodes: (%s)", target, strings.Join(cr.NodeNames(), ", "))
			case create && base != "":
				return errors.New("--create makes a crew that is not there, --base replaces the file that is: not both")
			}

			p := peerOf(me, target)
			if why := crewSyncRefusal(p); why != "" {
				return errors.New(why)
			}

			raw, err := os.ReadFile(cr.Path)
			if err != nil {
				return fmt.Errorf("crew: %w", err)
			}

			if len(raw) > mesh.CrewFileMax {
				return fmt.Errorf("the crew file is %d bytes: a push carries at most %d", len(raw), mesh.CrewFileMax)
			}

			key, err := a.pushKey()
			if err != nil {
				return err
			}

			// The file goes to every node in the end: one that does not load for a node of the file is not sent to the first.
			if _, err := crew.CheckPushed(filepath.Dir(cr.Path), raw, target); err != nil {
				return fmt.Errorf("this crew's file is not sent: %w", err)
			}

			// What the push replaces is the file this node last sent there, or last saw there and confirmed (a push that was written or
			// found in place, a move that left two nodes on one file): never what the node says it holds now, which is what an edit made
			// there would be taken for. A node with no such record is based on no file, and one that has a file is refused, with the
			// difference: --base <its sum> once it has been looked at.
			basis := "the file there is not the one --base named"

			if !create && base == "" {
				base = me.PushedSum(target, key)
				basis = "this node has no record of sending to or confirming a file there"

				if base != "" {
					basis = "an edit there since this node last sent or confirmed its file"
				}
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), peerActTimeout)
			defer cancel()

			var transport mesh.Transport = mesh.Exec{}
			if a.pushTransport != nil {
				transport = a.pushTransport
			}

			res, err := me.CrewPut(ctx, transport, target, mesh.TerminalRequest{Crew: key, Payload: raw, Base: base, Create: create})

			var conflict *mesh.CrewConflict
			if errors.As(err, &conflict) {
				return a.pushConflict(conflict, raw, key, basis)
			}

			if err != nil {
				return fmt.Errorf("%s: %w", target, err)
			}

			// A node that says it wrote the file holds it: the sum it tells is the file sent, or it is not believed.
			if mine := crew.SumOf(raw); res.Sum != mine {
				return fmt.Errorf("%s answers that it holds %s after the push, which is not the file sent (%s): it is not recorded as held there; "+
					"zmina node ls shows what it tells", target, short12(res.Sum), short12(mine))
			}

			for _, w := range res.Warnings {
				fmt.Fprintf(a.err, "zmina: warning: %s: %s\n", target, mesh.Plain(w))
			}

			// What was sent is what is there now: the next push to it is based on this file.
			if err := me.MarkPushed(target, key, res.Sum); err != nil {
				fmt.Fprintf(a.err, "zmina: warning: the sum of the file now on %s could not be kept (%v): the next push to it asks for --base\n", target, err)
			}

			fmt.Fprintln(a.out, pushedLine(target, key, res))

			if create {
				fmt.Fprintf(a.out, "no ticker is installed on %s for %s: there, run %s (a crew with no ticker runs nothing there until it is)\n", target, key,
					tickerInstallLine(key))
			}

			return nil
		},
	}

	c.Flags().BoolVar(&create, "create", false, "make the crew on that node when it has none (under ~/.zmina; its ticker is installed there by hand)")
	c.Flags().StringVar(&base, "base", "", "replace the file there whose hash this is, when it is not the one the node last told (a node that told none, or a file you looked at after a refusal)")

	return c
}

// pushKey is the name the other node knows this crew by: its directory under ~/.zmina (default for the default crew). A crew
// file anywhere else has no name there to push to.
func (a *app) pushKey() (string, error) {
	path := a.crewPath()
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	for _, c := range uiCrews("") {
		if c.Path == path {
			return c.Key, nil
		}
	}

	return "", fmt.Errorf("%s is not one of this node's crews (~/.zmina/crew.yaml, or ~/.zmina/<name>/crew.yaml): a push goes to the crew "+
		"of the same name on the other node", path)
}

// tickerInstallLine is the command that installs the ticker of the crew with this key on the node it was made on.
func tickerInstallLine(key string) string {
	if key == "default" {
		return "zmina ticker install"
	}

	return "zmina --config " + key + " ticker install"
}

// pushedLine says what the node did.
func pushedLine(target, key string, res mesh.CrewPutResult) string {
	switch {
	case res.Status == mesh.PutSame:
		return fmt.Sprintf("%s already holds this file (%s): nothing written", target, short12(res.Sum))
	case res.Was == "":
		return fmt.Sprintf("pushed %s to %s: crew made, file %s", key, target, short12(res.Sum))
	}

	return fmt.Sprintf("pushed %s to %s: file %s, which replaced %s (kept in its history: zmina crew history there)", key, target,
		short12(res.Sum), short12(res.Was))
}

func short12(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}

	return sum
}

// pushConflict is what a refused push shows: which file the node holds against which this node sent, and the difference. The
// error carries the way on, never a flag to overrule it blindly. What the node sent (its file, so the diff) is text of a peer and goes
// to the terminal made plain; basis says in words why the file there is not the one the push was based on.
func (a *app) pushConflict(c *mesh.CrewConflict, local []byte, key, basis string) error {
	mine := crew.SumOf(local)

	fmt.Fprintf(a.out, "%s holds file %s; this node's is %s; the push was based on %s\n", c.Node, short12(orNone(c.Have)), short12(mine),
		short12(orNone(c.Base)))

	switch {
	case c.Have == "":
		fmt.Fprintf(a.out, "%s has no file for %s\n", c.Node, key)
	case c.Current == nil:
		fmt.Fprintf(a.out, "its file is too big to show (over %d bytes): compare the two by hand\n", mesh.CrewFileMax)
	default:
		fmt.Fprint(a.out, mesh.PlainText(crew.UnifiedDiff(c.Current, local, c.Node+"/"+key+" crew.yaml", "this node's "+filepath.Base(a.crewPath()))))
	}

	if c.Have == "" {
		return fmt.Errorf("nothing was written: %s has no such crew; zmina crew push %s --create makes it", c.Node, c.Node)
	}

	return fmt.Errorf("nothing was written: %s's file is not the one this push was based on (%s); "+
		"once you have looked at the difference: zmina crew push %s --base %s", c.Node, basis, c.Node, c.Have)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}

	return s
}

// localCrewPut is a push to this node (crew/put), after the mesh has checked it is the owner's, bound to this node, within
// the size and with plain names: it finds the crew by its key among this node's own crews (never by a path), or makes it
// under the zmina home when the owner said create and nothing of that name is there, and puts the file in place (crew.Put:
// it must load here, and replace the file the owner based the push on, else it is a conflict and nothing is written).
func (a *app) localCrewPut(key string, payload []byte, base string, create bool, by string) (mesh.CrewPutResult, error) {
	me, err := openNode()
	if err != nil {
		return mesh.CrewPutResult{}, err
	}

	// By exact key among this node's own crews: localCrew reads "" as the default crew, which a push must never reach by
	// leaving the name out.
	path := ""

	for _, c := range uiCrews("") {
		if key != "" && c.Key == key {
			path = c.Path

			break
		}
	}

	if path == "" {
		if !create {
			return mesh.CrewPutResult{}, fmt.Errorf("no crew %q on %s (crews: %s): zmina crew push --create makes it", key, me.Name,
				strings.Join(crewKeys(), ", "))
		}

		if path, err = newCrewPath(key); err != nil {
			return mesh.CrewPutResult{}, err
		}
	}

	// Nothing is written through a link that leaves the home: the crew's directory, its file and its history are resolved first.
	res, err := crew.Put(path, payload, crew.PutOptions{Node: me.Name, Base: base, Create: create, By: by, How: "crew push",
		Home: crew.ExpandHome("~/.zmina")})
	if err == nil {
		if c, err := crew.Load(path); err == nil {
			logCrewVersions(eventLog(c), res.Saved.Versions)
		}
	}

	return putAnswer(res, err, base)
}

// putAnswer turns what crew.Put did into what the node tells the owner: a file that is not the one the push was based on is a
// conflict that carries the file there (an answer, not an error), a file already in place is "same", anything else that went
// wrong is the error.
func putAnswer(res crew.PutResult, err error, base string) (mesh.CrewPutResult, error) {
	var be *crew.BaseError

	switch {
	case errors.As(err, &be):
		out := mesh.CrewPutResult{Status: mesh.PutConflict, Sum: be.Have, Line: "the file here is not the one the push was based on"}
		if len(be.Current) <= mesh.CrewFileMax {
			out.Current = be.Current
		}

		return out, nil
	case errors.Is(err, crew.ErrNoCrew):
		return mesh.CrewPutResult{}, errors.New("no such crew here: zmina crew push --create makes it")
	case err != nil:
		return mesh.CrewPutResult{}, err
	case !res.Wrote:
		return mesh.CrewPutResult{Status: mesh.PutSame, Sum: res.Sum, Warnings: res.Warnings, Line: "already this file"}, nil
	}

	return mesh.CrewPutResult{Status: mesh.PutDone, Sum: res.Sum, Was: base, Warnings: res.Warnings,
		Line: "crew file replaced, its previous version kept in crew.d/history"}, nil
}

// newCrewPath is where a crew made by a push lives: ~/.zmina/<key>/crew.yaml (the default crew: ~/.zmina/crew.yaml). It is a
// name only crew.NewCrewKey allows, in a directory that is not there yet: a push never writes into a directory of anything
// else.
func newCrewPath(key string) (string, error) {
	if !crew.NewCrewKey(key) {
		return "", fmt.Errorf("crew %q: a crew made by a push is named by lower-case words joined by dashes, and no name of a "+
			"directory zmina keeps under ~/.zmina (%s)", key, strings.Join(crew.ReservedKeys(), ", "))
	}

	home := crew.ExpandHome("~/.zmina")

	if key == "default" {
		return filepath.Join(home, "crew.yaml"), nil
	}

	dir := filepath.Join(home, key)
	if _, err := os.Lstat(dir); err == nil {
		return "", fmt.Errorf("%s is there already and holds no crew file: a push does not write into it", dir)
	}

	return filepath.Join(dir, "crew.yaml"), nil
}
