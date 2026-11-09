package lean

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// GuardMax is the size above which the guard steps in: below it a whole read is cheaper than an outline and a second
// read (design/READER.md, "an extra turn costs ~7k characters").
const GuardMax = DefaultMax

// HookInput is what Claude Code (and Codex) give a PreToolUse hook on stdin; only the fields the guard reads.
type HookInput struct {
	SessionID string         `json:"session_id"`
	Cwd       string         `json:"cwd"`
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
	AgentID   string         `json:"agent_id,omitempty"`
}

// Decision is what the guard answers. Kind "" lets the call go as it is.
type Decision struct {
	Kind string // "", "rewrite" (a Bash command becomes zmina read), "outline" (a Read is answered by the outline)
	// Command is the rewritten Bash command (Kind rewrite).
	Command string
	// Reason is what the model reads: for an outline, the outline itself.
	Reason string
	// Path and Size are for the meter.
	Path string
	Size int
	What string // cat, cat-multi, sed, head, tail, Read
}

// noOutlineExt are files Claude Code's Read shows as more than text: never answered with an outline.
var noOutlineExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".pdf": true, ".ipynb": true, ".svg": true}

// Guard decides on one tool call. self is the zmina executable a rewritten command runs. It is sure or it lets the
// call go: anything it does not parse plainly passes untouched.
func Guard(in HookInput, self string) Decision {
	switch in.ToolName {
	case "Read":
		return guardRead(in)
	case "Bash", "shell", "exec_command", "local_shell":
		cmd, _ := in.ToolInput["command"].(string)
		return guardBash(cmd, in.Cwd, self)
	}

	return Decision{}
}

func guardRead(in HookInput) Decision {
	p, _ := in.ToolInput["file_path"].(string)
	if p == "" || in.ToolInput["offset"] != nil || in.ToolInput["limit"] != nil || noOutlineExt[strings.ToLower(filepath.Ext(p))] {
		return Decision{}
	}

	info, err := os.Stat(p)
	if err != nil || info.IsDir() || info.Size() <= GuardMax {
		return Decision{}
	}

	f, err := Load(p)
	if err != nil || f.Kind == "binary" {
		return Decision{}
	}

	var b strings.Builder

	fmt.Fprintf(&b, "read-guard: this file is %s characters, so here is its outline instead of all of it. Read the "+
		"part you need: Read with offset and limit (that also counts as read for Edit), or zmina read %s:Name / "+
		"#Heading / :120-180.\n", kchars(len(f.Src)), p)
	writeOutline(&b, f, p, "== "+p+" · "+thousands(len(f.Lines))+" lines · "+kchars(len(f.Src)), DefaultMax)

	return Decision{Kind: "outline", Reason: b.String(), Path: p, Size: len(f.Src), What: "Read"}
}

var (
	cdPrefix = regexp.MustCompile(`^\s*cd\s+('[^']*'|"[^"$` + "`" + `\\]*"|[^\s;&|'"$` + "`" + `]+)\s*&&\s*`)
	sedRange = regexp.MustCompile(`^(\d+),(\d+)p$`)
	// countFlag is head's or tail's -20 or -n20.
	countFlag = regexp.MustCompile(`^-n?\d+$`)
	// shellMeta: anything that makes a command more than one plain command with plain words.
	shellMeta = regexp.MustCompile("[|;&<>`$(){}*?\\[\\]~!\\\\\n]")
)

func guardBash(cmd, cwd, self string) Decision {
	if self == "" {
		return Decision{}
	}

	prefix := ""
	dir := cwd

	if m := cdPrefix.FindStringSubmatch(cmd); m != nil {
		prefix = m[0]
		d := strings.Trim(m[1], `'"`)

		if !filepath.IsAbs(d) {
			d = filepath.Join(cwd, d)
		}

		dir = d
		cmd = cmd[len(m[0]):]
	}

	words, ok := plainWords(cmd)
	if !ok || len(words) < 2 {
		return Decision{}
	}

	var (
		target []string
		what   string
		size   int
	)

	switch words[0] {
	case "cat":
		files := words[1:]
		if files[0] == "-n" {
			files = files[1:]
		}

		if len(files) == 0 {
			return Decision{}
		}

		for _, f := range files {
			if strings.HasPrefix(f, "-") {
				return Decision{}
			}

			n, ok := fileSize(f, dir)
			if !ok {
				return Decision{}
			}

			size += n
		}

		if size <= GuardMax {
			return Decision{}
		}

		target, what = files, "cat"
		if len(files) > 1 {
			what = "cat-multi"
		}
	case "sed":
		if len(words) != 4 || words[1] != "-n" {
			return Decision{}
		}

		m := sedRange.FindStringSubmatch(words[2])
		if m == nil {
			return Decision{}
		}

		a, _ := strconv.Atoi(m[1])
		z, _ := strconv.Atoi(m[2])

		n, ok := rangeSize(words[3], dir, a, z)
		if !ok || n <= GuardMax {
			return Decision{}
		}

		target, what, size = []string{words[3] + ":" + m[1] + "-" + m[2]}, "sed", n
	case "head", "tail":
		f, lines, ok := headTail(words)
		if !ok {
			return Decision{}
		}

		all, ok := fileLines(f, dir)
		if !ok {
			return Decision{}
		}

		a, z := 1, min(lines, len(all))
		if words[0] == "tail" {
			a, z = max(1, len(all)-lines+1), len(all)
		}

		n := 0
		for i := a; i <= z; i++ {
			n += len(all[i-1]) + 1
		}

		if n <= GuardMax {
			return Decision{}
		}

		target, what, size = []string{f + ":" + strconv.Itoa(a) + "-" + strconv.Itoa(z)}, words[0], n
	default:
		return Decision{}
	}

	for _, t := range target {
		if strings.ContainsAny(t, "'") {
			return Decision{}
		}
	}

	parts := []string{prefix + shellQuote(self), "read", "--via", shellQuote(strings.Join(words, " "))}
	for _, t := range target {
		parts = append(parts, shellQuote(t))
	}

	rewritten := strings.Join(parts, " ")

	return Decision{Kind: "rewrite", Command: rewritten, Path: filepath.Join(dir, strings.Split(target[0], ":")[0]), Size: size, What: what}
}

// plainWords splits a command into words when it is one plain command: words, single- or double-quoted words with
// no expansion in them, nothing else.
func plainWords(cmd string) ([]string, bool) {
	var (
		words []string
		cur   strings.Builder
		in    bool
	)

	cmd = strings.TrimSpace(cmd)

	for i := 0; i < len(cmd); i++ {
		c := cmd[i]

		switch {
		case c == '\'' || c == '"':
			j := strings.IndexByte(cmd[i+1:], c)
			if j < 0 {
				return nil, false
			}

			q := cmd[i+1 : i+1+j]
			if c == '"' && strings.ContainsAny(q, "$`\\!") {
				return nil, false
			}

			cur.WriteString(q)

			in = true
			i += j + 1
		case c == ' ' || c == '\t':
			if in {
				words = append(words, cur.String())
				cur.Reset()

				in = false
			}
		case shellMeta.MatchString(string(c)):
			return nil, false
		default:
			cur.WriteByte(c)

			in = true
		}
	}

	if in {
		words = append(words, cur.String())
	}

	return words, true
}

func headTail(words []string) (string, int, bool) {
	lines := 10

	var file string

	for i := 1; i < len(words); i++ {
		w := words[i]

		switch {
		case w == "-n" && i+1 < len(words):
			n, err := strconv.Atoi(strings.TrimPrefix(words[i+1], "+"))
			if err != nil || strings.HasPrefix(words[i+1], "+") {
				return "", 0, false
			}

			lines = n
			i++
		case countFlag.MatchString(w):
			lines, _ = strconv.Atoi(strings.TrimLeft(w, "-n"))
		case strings.HasPrefix(w, "-"):
			return "", 0, false
		case file != "":
			return "", 0, false
		default:
			file = w
		}
	}

	return file, lines, file != ""
}

func fileSize(f, dir string) (int, bool) {
	info, err := os.Stat(abs(f, dir))
	if err != nil || !info.Mode().IsRegular() {
		return 0, false
	}

	return int(info.Size()), true
}

func fileLines(f, dir string) ([]string, bool) {
	file, err := Load(abs(f, dir))
	if err != nil || file.Kind == "binary" {
		return nil, false
	}

	return file.Lines, true
}

func rangeSize(f, dir string, a, z int) (int, bool) {
	lines, ok := fileLines(f, dir)
	if !ok {
		return 0, false
	}

	n := 0
	for i := max(a, 1); i <= z && i <= len(lines); i++ {
		n += len(lines[i-1]) + 1
	}

	return n, true
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"$`\\|&;<>()*?[]{}~#!") {
		return s
	}

	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
