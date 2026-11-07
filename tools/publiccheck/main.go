// Command publiccheck fails when commits not yet pushed would put a live crew's names into this public repository:
// its crew names, node and ssh host names, task ids of its ledgers, hosts and IP addresses, private paths. It reads
// every unpushed commit — each commit's added lines and its message, not only the final tree, since history is
// published too — and takes what is banned from this machine at run time (so the list itself never enters the
// repository):
//
//   - crew names: the directories of ~/.zmina/<name>/crew.yaml (not "default");
//   - node names: ~/.zmina/mesh/node.json and peers.json;
//   - ssh hosts: the Host lines of ~/.ssh/config, wildcards left out;
//   - live task ids: T-<n> of every ledger under ~/.zmina (n >= 10), unless the base tree already has that id;
//   - extra patterns, one extended regular expression a line: $ZMINA_PUBLIC_DENYLIST, else ~/.zmina/public-denylist;
//   - always: IPv4 addresses outside loopback and the documentation ranges, paths under /Users/<name> or
//     /home/<name> other than the neutral alex.
//
// The base is the merge base with origin/main, else main; -base overrides it. Exit 1 lists every finding.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

type rule struct {
	what string
	re   *regexp.Regexp
	// lower: a short word (three letters at most) with no capital, so its all-caps spelling is another word, an
	// abbreviation (host "gb", the unit "GB"); a longer name in capitals is still the name.
	lower bool
}

var (
	ipv4    = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)
	private = regexp.MustCompile(`/(?:Users|home)/([A-Za-z0-9._-]+)`)
	hunk    = regexp.MustCompile(`\+([0-9]+)`)
)

func main() {
	base := flag.String("base", "", "the commit unpushed work starts after (default: merge base with origin/main, else main)")
	flag.Parse()

	from := *base
	if from == "" {
		from = mergeBase()
	}

	if from == "" {
		fmt.Fprintln(os.Stderr, "publiccheck: no origin/main or main to compare with; nothing checked")

		return
	}

	home, _ := os.UserHomeDir()
	rules, err := banned(home, from)
	if err != nil {
		fmt.Fprintln(os.Stderr, "publiccheck:", err)
		os.Exit(2)
	}

	findings, err := scan(from, rules)
	if err != nil {
		fmt.Fprintln(os.Stderr, "publiccheck:", err)
		os.Exit(2)
	}

	if len(findings) == 0 {
		fmt.Printf("publiccheck: %s..HEAD clean (%d local names and ids checked)\n", short(from), len(rules))

		return
	}

	for _, f := range findings {
		fmt.Println(f)
	}

	fmt.Printf("publiccheck: %d findings in %s..HEAD: use neutral names (alex, acme, box-ops, T-1), rewrite the unpushed commits\n",
		len(findings), short(from))
	os.Exit(1)
}

func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()

	return string(out), err
}

func mergeBase() string {
	for _, ref := range []string{"origin/main", "main"} {
		if out, err := git("merge-base", "HEAD", ref); err == nil {
			return strings.TrimSpace(out)
		}
	}

	return ""
}

func short(rev string) string { return rev[:min(len(rev), 7)] }

// banned is every word this machine's crews must not leak, as whole-word rules.
func banned(home, base string) ([]rule, error) {
	words := map[string]string{}

	add := func(what, w string) {
		w = strings.TrimSpace(w)
		if len(w) >= 2 && w != "default" && !neutral[strings.ToLower(w)] {
			words[w] = what
		}
	}

	zmina := filepath.Join(home, ".zmina")

	if dirs, err := filepath.Glob(filepath.Join(zmina, "*", "crew.yaml")); err == nil {
		for _, d := range dirs {
			add("crew name", filepath.Base(filepath.Dir(d)))
		}
	}

	var node struct {
		Name string `json:"name"`
	}
	if raw, err := os.ReadFile(filepath.Join(zmina, "mesh", "node.json")); err == nil && json.Unmarshal(raw, &node) == nil {
		add("node name", node.Name)
	}

	var peers []struct {
		Name string `json:"name"`
	}
	if raw, err := os.ReadFile(filepath.Join(zmina, "mesh", "peers.json")); err == nil && json.Unmarshal(raw, &peers) == nil {
		for _, p := range peers {
			add("node name", p.Name)
		}
	}

	if f, err := os.Open(filepath.Join(home, ".ssh", "config")); err == nil {
		for _, h := range sshHosts(f) {
			add("ssh host", h)
		}

		_ = f.Close()
	}

	// Task ids of the live ledgers; one the base tree already has (a doc's example) is not news.
	known := map[string]bool{}
	if out, err := git("grep", "-h", "-o", "-w", "-E", `T-[0-9]+`, base); err == nil {
		for _, id := range strings.Fields(out) {
			known[id] = true
		}
	}

	ledgers, _ := filepath.Glob(filepath.Join(zmina, "tasks", "T-*.json"))
	more, _ := filepath.Glob(filepath.Join(zmina, "*", "tasks", "T-*.json"))

	for _, p := range append(ledgers, more...) {
		id := strings.TrimSuffix(filepath.Base(p), ".json")
		if n, err := strconv.Atoi(strings.TrimPrefix(id, "T-")); err == nil && n >= 10 && !known[id] {
			add("live task id", id)
		}
	}

	var rules []rule

	keys := make([]string, 0, len(words))
	for w := range words {
		keys = append(keys, w)
	}

	slices.Sort(keys)

	for _, w := range keys {
		re := regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_-])(` + regexp.QuoteMeta(w) + `)(?:$|[^A-Za-z0-9_])`)
		rules = append(rules, rule{words[w] + " " + w, re, len(w) <= 3 && w == strings.ToLower(w) && w != strings.ToUpper(w)})
	}

	deny := os.Getenv("ZMINA_PUBLIC_DENYLIST")
	if deny == "" {
		deny = filepath.Join(zmina, "public-denylist")
	}

	if raw, err := os.ReadFile(deny); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}

			re, err := regexp.Compile(line)
			if err != nil {
				return nil, fmt.Errorf("%s: %q: %w", deny, line, err)
			}

			rules = append(rules, rule{what: "denylist pattern", re: re})
		}
	}

	return rules, nil
}

// sshHosts are the names the Host lines of an ssh config give, read as ssh reads them (sshWords); a pattern
// (wildcards) or a negated one names no host. A line ssh would refuse (an unclosed quote) gives none.
func sshHosts(r io.Reader) []string {
	var hosts []string

	sc := bufio.NewScanner(r)
	for sc.Scan() {
		words, ok := sshWords(sc.Text())
		if !ok || len(words) < 2 || !strings.EqualFold(words[0], "host") {
			continue
		}

		for _, h := range words[1:] {
			if h != "" && !strings.ContainsAny(h, "*?!") {
				hosts = append(hosts, h)
			}
		}
	}

	return hosts
}

// sshWords splits an ssh config line as OpenSSH does: the keyword ends at whitespace or "=" ("Host=alex",
// "Host = alex"); its arguments are split as argv_split splits them — single or double quotes group a word and
// are taken off, a backslash escapes a quote, a backslash, or (outside quotes) a space, and an unquoted word that
// starts with # ends the line. ok is false for a line with an unclosed quote, which ssh refuses.
func sshWords(line string) (words []string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil, true
	}

	end := strings.IndexFunc(line, func(r rune) bool { return r == '=' || unicode.IsSpace(r) })
	if end < 0 {
		return []string{line}, true
	}

	words = []string{line[:end]}
	rest := strings.TrimLeftFunc(line[end:], unicode.IsSpace)
	rest = strings.TrimLeftFunc(strings.TrimPrefix(rest, "="), unicode.IsSpace)

	in := []rune(rest)
	for i := 0; i < len(in); {
		switch {
		case unicode.IsSpace(in[i]):
			i++
		case in[i] == '#':
			return words, true
		default:
			var word string
			if word, i, ok = sshWord(in, i); !ok {
				return nil, false
			}

			words = append(words, word)
		}
	}

	return words, true
}

// sshWord is the word of an ssh config line that starts at i, and where the next one may start; ok is false when
// it opens a quote it never closes.
func sshWord(in []rune, i int) (string, int, bool) {
	var (
		word  []rune
		quote rune
	)

	for ; i < len(in); i++ {
		c := in[i]

		switch {
		case quote == 0 && unicode.IsSpace(c):
			return string(word), i, true
		case c == '\\' && i+1 < len(in) && (in[i+1] == '\'' || in[i+1] == '"' || in[i+1] == '\\' ||
			quote == 0 && in[i+1] == ' '):
			i++
			word = append(word, in[i])
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
		case quote != 0 && c == quote:
			quote = 0
		default:
			word = append(word, c)
		}
	}

	return string(word), i, quote == 0
}

// neutral are the names fixtures and docs use on purpose.
var neutral = map[string]bool{"alex": true, "acme": true, "box": true, "box-ops": true, "ops": true, "laptop": true,
	"dev": true, "web": true, "localhost": true, "github.com": true, "gitlab.com": true}

// matches says whether line names the rule's word; for a short lowercase word its all-caps spelling does not count.
func (r rule) matches(line string) bool {
	if !r.lower {
		return r.re.MatchString(line)
	}

	// Each search starts after the last word, not after the separator the match took ("GB gb").
	for pos := 0; pos < len(line); {
		m := r.re.FindStringSubmatchIndex(line[pos:])
		if m == nil {
			return false
		}

		if w := line[pos+m[2] : pos+m[3]]; w != strings.ToUpper(w) {
			return true
		}

		pos += m[3]
	}

	return false
}

// scan checks each unpushed commit's message and added lines.
func scan(base string, rules []rule) ([]string, error) {
	revs, err := git("rev-list", "--reverse", base+"..HEAD")
	if err != nil {
		return nil, err
	}

	var out []string

	for _, rev := range strings.Fields(revs) {
		msg, err := git("show", "-s", "--format=%B", rev)
		if err != nil {
			return nil, err
		}

		for i, line := range strings.Split(msg, "\n") {
			out = append(out, check(rules, short(rev)+" message:"+strconv.Itoa(i+1), line)...)
		}

		patch, err := git("show", "--format=", "--unified=0", "--no-color", rev)
		if err != nil {
			return nil, err
		}

		file, n := "", 0

		for _, line := range strings.Split(patch, "\n") {
			switch {
			case strings.HasPrefix(line, "+++ "):
				file = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
			case strings.HasPrefix(line, "@@"):
				if m := hunk.FindStringSubmatch(line); m != nil {
					n, _ = strconv.Atoi(m[1])
				}
			case strings.HasPrefix(line, "+"):
				out = append(out, check(rules, short(rev)+" "+file+":"+strconv.Itoa(n), line[1:])...)
				n++
			}
		}
	}

	return out, nil
}

func check(rules []rule, where, line string) []string {
	var out []string

	for _, r := range rules {
		if r.matches(line) {
			out = append(out, where+": "+r.what)
		}
	}

	for _, ip := range ipv4.FindAllString(line, -1) {
		if !harmlessIP(ip) {
			out = append(out, where+": IP address "+ip)
		}
	}

	for _, m := range private.FindAllStringSubmatch(line, -1) {
		if !neutral[strings.ToLower(m[1])] && m[1] != "you" && m[1] != "me" && m[1] != "user" {
			out = append(out, where+": private path "+m[0])
		}
	}

	return out
}

// harmlessIP is loopback, unspecified, the documentation ranges, or not an address at all (a version, 1.2.3.400).
func harmlessIP(ip string) bool {
	parts := strings.Split(ip, ".")
	for _, p := range parts {
		if n, err := strconv.Atoi(p); err != nil || n > 255 {
			return true
		}
	}

	for _, prefix := range []string{"127.", "0.0.0.0", "192.0.2.", "198.51.100.", "203.0.113.", "255.255.255."} {
		if strings.HasPrefix(ip, prefix) {
			return true
		}
	}

	return false
}
