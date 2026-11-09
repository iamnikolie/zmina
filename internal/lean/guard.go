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
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	// PermissionMode is the session's: a rewrite goes only to one that bypasses permissions, where the new command
	// asks nobody (elsewhere a cat that was allowed as read-only would become a command that waits for approval).
	PermissionMode string         `json:"permission_mode"`
	ToolName       string         `json:"tool_name"`
	ToolInput      map[string]any `json:"tool_input"`
	AgentID        string         `json:"agent_id,omitempty"`
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

// Guard decides on one tool call. self is the zmina executable a rewritten command runs. roots, asked only for a file
// outside the session's directory, are the other directories whose files the guard takes as the session's own: the
// crew's worktrees directory and its active tasks' worktrees, where a role works away from where it started. It is
// sure or it lets the call go: anything it does not parse plainly passes untouched.
func Guard(in HookInput, self string, roots func() []string) Decision {
	switch in.ToolName {
	case "Read":
		// Outside the session's directory Claude Code would ask before a Read, or a deny rule would stop it: only a
		// session that bypasses permissions gets an outline of a worktree's file.
		if in.PermissionMode != "bypassPermissions" {
			roots = nil
		}

		return guardRead(in, roots)
	case "Bash":
		if in.PermissionMode != "bypassPermissions" {
			return Decision{}
		}

		cmd, _ := in.ToolInput["command"].(string)

		return guardBash(cmd, in.Cwd, self, roots)
	}

	return Decision{}
}

// ownFile: the file is the session's — under its directory, or under one of roots (a task's worktree the role cd'd
// into or names by its path) — with the hidden-part and link checks made against that directory or root.
func ownFile(p, session string, roots func() []string) bool {
	if guardable(p, session) {
		return true
	}

	if roots == nil {
		return false
	}

	for _, r := range roots() {
		if saneRoot(r) && guardable(p, r) {
			return true
		}
	}

	return false
}

// saneRoot: a root is a directory of its own, never "/", the home directory or one of its parents (a task record
// that named one would make every file the session's).
func saneRoot(r string) bool {
	if r == "" || !filepath.IsAbs(r) {
		return false
	}

	r = filepath.Clean(r)
	if r == "/" {
		return false
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(r, filepath.Clean(home)); err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
			return false
		}
	}

	return true
}

// guardRead answers a Read of a big file without offset or limit with its outline. The hook runs before Claude Code's
// permission rules, so the deny reason it writes must never carry what those rules might withhold: only a file under
// the session's directory, not hidden, of a type with a real outline (names and line ranges, never its lines).
func guardRead(in HookInput, roots func() []string) Decision {
	p, _ := in.ToolInput["file_path"].(string)
	if p == "" || in.ToolInput["offset"] != nil || in.ToolInput["limit"] != nil || noOutlineExt[strings.ToLower(filepath.Ext(p))] {
		return Decision{}
	}

	if !filepath.IsAbs(p) {
		p = filepath.Join(in.Cwd, p)
	}

	if Lang(p) == "" || !ownFile(p, in.Cwd, roots) {
		return Decision{}
	}

	if n, ok := fileSize(p, ""); !ok || n <= GuardMax {
		return Decision{}
	}

	f, err := Load(p)
	if err != nil || f.Kind != "" {
		return Decision{}
	}

	// An outline that may hold a string's text in its labels (a SQL file whose backslashes change its statements, a
	// brace file whose brackets did not close by kind) is no answer.
	if o := outlinerFor(p); o.sure != nil && !o.sure(f.Src) {
		return Decision{}
	}

	syms := Outline(p, f.Src)
	if len(syms) == 0 {
		return Decision{}
	}

	var b strings.Builder

	fmt.Fprintf(&b, "read-guard: this file is %s characters, so here is its outline instead of all of it. Read the "+
		"part you need: Read with offset and limit (that also counts as read for Edit), or zmina read %s:Name / "+
		"#Heading / :120-180.\n", kchars(len(f.Src)), p)
	writeSyms(&b, f, syms, p, "== "+p+" · "+thousands(len(f.Lines))+" lines · "+kchars(len(f.Src)), DefaultMax)

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

func guardBash(cmd, cwd, self string, roots func() []string) Decision {
	session := cwd
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
		file := strings.Split(t, ":")[0]
		if what == "sed" || what == "head" || what == "tail" {
			file = t[:strings.LastIndex(t, ":")]
		}

		if strings.ContainsAny(t, "'") || !ownFile(abs(file, dir), session, roots) {
			return Decision{}
		}
	}

	parts := []string{prefix + shellQuote(self), "read", "--via", shellQuote(strings.Join(words, " "))}
	for _, t := range target {
		parts = append(parts, shellQuote(t))
	}

	rewritten := strings.Join(parts, " ")

	first := target[0]
	if what != "cat" && what != "cat-multi" {
		first = first[:strings.LastIndex(first, ":")]
	}

	return Decision{Kind: "rewrite", Command: rewritten, Path: abs(first, dir), Size: size, What: what}
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

// fileSize is a regular file's size, up to MaxFile: a device (/dev/zero), a FIFO or a bigger file is not the guard's.
func fileSize(f, dir string) (int, bool) {
	info, err := os.Stat(abs(f, dir))
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxFile {
		return 0, false
	}

	return int(info.Size()), true
}

// guardable: the file, its links resolved, is under the session's directory and no part of its path there is hidden
// (.env, .git, .ssh): what permission rules usually guard is left to them.
func guardable(p, cwd string) bool {
	if cwd == "" {
		return false
	}

	// By where the file really is: a link under cwd may point outside it or into a hidden directory.
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}

	if c, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = c
	}

	rel, err := filepath.Rel(cwd, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return false
	}

	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}

	return true
}

func fileLines(f, dir string) ([]string, bool) {
	if _, ok := fileSize(f, dir); !ok {
		return nil, false
	}

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
