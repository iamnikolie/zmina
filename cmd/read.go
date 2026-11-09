package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/lean"
	"github.com/spf13/cobra"
)

// readCommand is zmina read: a file the way an agent should pay for it. Needs no crew file.
func (a *app) readCommand() *cobra.Command {
	var (
		o   lean.Options
		max string
	)

	c := &cobra.Command{
		Use:   "read <target>...",
		Short: "Read files lean: a small file whole, a big one as an outline; then a function, section or lines",
		Long: `Read files lean. Several targets go in one call, under one cap (default 12k characters).

  path            up to the cap: the file, numbered; bigger: its outline (line ranges and sizes)
  path:Name       a Go func, type or method (Type.Method), a YAML/JSON key (a.b below the top)
  path#Heading    a Markdown section with its subsections
  path:120-180    lines; path:412- to the end; path:120 the lines around 120

A cut says how to go on (next: zmina read path:N-). Lock, minified and generated files are named, not shown.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := chars(max)
			if err != nil {
				return err
			}

			o.Max = n

			dir, _ := os.Getwd()

			var targets []lean.Target
			for _, arg := range args {
				targets = append(targets, lean.ParseTarget(arg, dir))
			}

			text := lean.Read(targets, dir, o)
			fmt.Fprint(a.out, text)

			a.meter(func(r *lean.Record) {
				r.Kind, r.Out = "read", len(text)
				t := targets[0]
				r.Path, r.From, r.To = absPath(t.Path, dir), t.From, t.To
				r.What = readWhat(t, o)

				if info, err := os.Stat(r.Path); err == nil {
					r.Size, r.MTime = int(info.Size()), info.ModTime().Unix()
				}
			})

			return nil
		},
	}

	c.Flags().StringVar(&max, "max", "", "the call's cap in characters (12k by default; 30k, 30000)")
	c.Flags().BoolVar(&o.Full, "full", false, "no cap, no outline: the file as it is")
	c.Flags().BoolVar(&o.Outline, "outline", false, "the outline only")
	c.Flags().StringVar(&o.Via, "via", "", "the command the read guard replaced (set by the guard)")
	_ = c.Flags().MarkHidden("via")

	return c
}

func readWhat(t lean.Target, o lean.Options) string {
	switch {
	case t.Symbol != "" || t.Heading != "":
		return "symbol"
	case t.From > 0:
		return "range"
	case o.Outline:
		return "outline"
	}

	return "whole"
}

// grepCommand is zmina grep: hits grouped by file and by the function or section they sit in, within a budget.
func (a *app) grepCommand() *cobra.Command {
	var (
		o   lean.GrepOptions
		max string
	)

	c := &cobra.Command{
		Use:   "grep <regex> [path...]",
		Short: "Search lean: hits grouped by file and by the function or section they sit in, within a budget",
		Long: `Search files (default: this directory, without .git, node_modules, vendor, dist, build and hidden
directories) for a Go regexp. Hits are grouped by file and under the Go function, Markdown section or YAML/JSON
key they sit in; 5 a file, 4k characters in all, then how many were left out and how to see them.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := chars(max)
			if err != nil {
				return err
			}

			o.Max = n

			dir, _ := os.Getwd()

			text, err := lean.Grep(args[0], args[1:], dir, o)
			if err != nil {
				return err
			}

			fmt.Fprint(a.out, text)
			a.meter(func(r *lean.Record) { r.Kind, r.Out = "grep", len(text) })

			return nil
		},
	}

	c.Flags().StringVar(&max, "max", "", "the call's budget in characters (4k by default)")
	c.Flags().IntVar(&o.PerFile, "per-file", 0, "hits shown a file (default 5)")
	c.Flags().BoolVarP(&o.IgnoreCase, "ignore-case", "i", false, "ignore case")
	c.Flags().BoolVarP(&o.Fixed, "fixed", "F", false, "the pattern is a plain string")
	c.Flags().StringVarP(&o.Glob, "glob", "g", "", "only files whose name matches (*.go)")

	return c
}

// readGuardCommand is the PreToolUse hook of a role with read_guard (roleSettings): it reads the hook's JSON and
// answers only when it steps in. Any error, any doubt: no output, exit 0, the call goes as typed.
func (a *app) readGuardCommand() *cobra.Command {
	var role string

	c := &cobra.Command{
		Use:           "_read-guard",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A Go panic exits 2, which a PreToolUse hook reads as "block this call": the guard fails open instead.
			defer func() { _ = recover() }()

			raw, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 4<<20))
			if err != nil {
				return nil
			}

			var in lean.HookInput
			if json.Unmarshal(raw, &in) != nil {
				return nil
			}

			d := lean.Guard(in, self())
			if d.Kind == "" {
				return nil
			}

			out := guardOutput(in, d)
			if out == nil {
				return nil
			}

			fmt.Fprintln(a.out, string(out))

			a.meterAs(role, in.SessionID, in.AgentID, lean.Record{Kind: "guard-" + d.Kind, What: d.What, Path: d.Path,
				Size: d.Size, Out: len(d.Reason)})

			return nil
		},
	}

	c.Flags().StringVar(&role, "role", "", "the role the hook runs for")

	return c
}

// guardOutput is the hook's answer. A rewrite replaces the whole tool input and leaves the permission decision to
// the usual rules, which Claude Code applies to the new command. An outline is a deny whose reason — the outline —
// is what the model reads.
func guardOutput(in lean.HookInput, d lean.Decision) []byte {
	spec := map[string]any{"hookEventName": "PreToolUse"}

	switch d.Kind {
	case "rewrite":
		input := map[string]any{}
		for k, v := range in.ToolInput {
			input[k] = v
		}

		input["command"] = d.Command
		spec["updatedInput"] = input
	case "outline":
		spec["permissionDecision"] = "deny"
		spec["permissionDecisionReason"] = d.Reason
	default:
		return nil
	}

	raw, err := json.Marshal(map[string]any{"hookSpecificOutput": spec})
	if err != nil {
		return nil
	}

	return raw
}

// meter appends a record for a role's call ($ZMINA_ROLE) to its crew's reads.jsonl; an owner's own call is not
// metered.
func (a *app) meter(fill func(r *lean.Record)) {
	role := os.Getenv("ZMINA_ROLE")
	if role == "" {
		return
	}

	session := os.Getenv("CLAUDE_CODE_SESSION_ID")
	if session == "" {
		session = os.Getenv("CODEX_THREAD_ID")
	}

	var r lean.Record

	fill(&r)
	a.meterAs(role, session, "", r)
}

func (a *app) meterAs(role, session, agent string, r lean.Record) {
	if role == "" {
		return
	}

	path := a.crewPath()
	if _, err := os.Stat(path); err != nil {
		return
	}

	r.At, r.Role, r.Session, r.Agent = time.Now().UTC(), role, session, agent
	lean.Append(crew.Layout{Dir: filepath.Dir(path)}.Reads(), r)
}

func absPath(p, dir string) string {
	if filepath.IsAbs(p) {
		return p
	}

	return filepath.Join(dir, p)
}

// chars reads a size in characters: 12000, 12k, 1.5k; empty is 0 (the default).
func chars(s string) (int, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, nil
	}

	mult := 1.0
	if strings.HasSuffix(s, "k") {
		mult, s = 1000, strings.TrimSuffix(s, "k")
	}

	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil || f <= 0 {
		return 0, fmt.Errorf("--max: characters, such as 12000 or 30k, not %q", s)
	}

	return int(f * mult), nil
}
