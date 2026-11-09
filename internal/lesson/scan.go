package lesson

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Finding is one thing the scan refused: the name of the rule and the line it fired on, counted from 1. It never
// holds the text that matched — a refusal that quoted the secret would only move the leak into the error message.
type Finding struct {
	Rule string
	Line int
}

// Rule names, as a Finding reports them.
const (
	ruleEntropy = "high-entropy string"
	ruleIP      = "ip address"
	ruleEmail   = "email"
	ruleCrew    = "crew word"
)

// pattern is a rule for one kind of secret. A match is the whole text of the regexp, or its first group when it has
// one (the group leaves out the character that a left boundary had to look at).
type pattern struct {
	name   string
	re     *regexp.Regexp
	accept func(match string) bool
}

// The prefixed tokens and credentials. Each is checked on its own, in this order, before the guesses below.
//
// The sk- rules need a boundary on the left, else every kebab-case word that ends in "sk" ("task-force-...",
// "risk-assessment-...") would read as a key.
var patterns = []pattern{
	{name: "aws key", re: regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{name: "github token", re: regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{20,}`)},
	{name: "gitlab token", re: regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`)},
	{name: "anthropic key", re: regexp.MustCompile(`(?:^|[^A-Za-z0-9-])(sk-ant-[A-Za-z0-9_-]{20,})`)},
	{
		name: "openai key",
		re:   regexp.MustCompile(`(?:^|[^A-Za-z0-9-])(sk-(?:proj-)?[A-Za-z0-9_-]{20,})`),
		// An Anthropic key has the same shape; it is named once, as what it is.
		accept: func(match string) bool { return !strings.HasPrefix(match, "sk-ant-") },
	},
	{name: "slack token", re: regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`)},
	{name: "google key", re: regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`)},
	{name: "private key", re: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{name: "jwt", re: regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{name: "url credentials", re: regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s/:@]+:[^\s/@]+@`)},
}

var (
	// runRe is a stretch of characters a key or a token is made of; a long one is looked at for entropy.
	runRe = regexp.MustCompile(`[A-Za-z0-9+/=_-]{32,}`)
	// dottedRe is a number with at least three dots in it, taken whole: a version of five parts is no address.
	dottedRe = regexp.MustCompile(`[0-9]+(?:\.[0-9]+){3,}`)
	// emailRe takes the local part and the domain; the domain ends in letters, so "pkg@4.17.21" is a version.
	emailRe = regexp.MustCompile(`([A-Za-z0-9._%+-]+)@((?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,})`)
)

// entropyBits is the Shannon entropy, in bits a character, from which a long run is taken for a secret. Words and
// identifiers stay under it; a random token of 32 characters or more is over.
const entropyBits = 4.0

// Scan looks for secrets and customer data in a text, line by line, and says what it refused: the rule and the
// line, never the text. The rules are the usual token shapes, a long high-entropy string, an IPv4 address that
// names a host, an email address, and extra — the crew's own words, such as a client's name — which are matched as
// whole words in any case. Documentation addresses and example.com mail are not findings. A rule fires once a line;
// findings come in line order, and in the order of the rules on one line.
func Scan(text string, extra []string) []Finding {
	words := crewWords(extra)

	var out []Finding

	for i, line := range strings.Split(text, "\n") {
		for _, rule := range scanLine(strings.TrimSuffix(line, "\r"), words) {
			out = append(out, Finding{Rule: rule, Line: i + 1})
		}
	}

	return out
}

// scanLine names the rules a line breaks.
func scanLine(line string, words []*regexp.Regexp) []string {
	var (
		hits  []string
		taken [][2]int
	)

	for _, p := range patterns {
		spans := p.find(line)
		if len(spans) > 0 {
			hits = append(hits, p.name)
			taken = append(taken, spans...)
		}
	}

	if hasEntropy(line, taken) {
		hits = append(hits, ruleEntropy)
	}

	if hasIP(line) {
		hits = append(hits, ruleIP)
	}

	if hasEmail(line, taken) {
		hits = append(hits, ruleEmail)
	}

	for _, re := range words {
		if re.MatchString(line) {
			hits = append(hits, ruleCrew)

			break
		}
	}

	return hits
}

// find is where the pattern matches in the line.
func (p pattern) find(line string) [][2]int {
	var spans [][2]int

	for _, m := range p.re.FindAllStringSubmatchIndex(line, -1) {
		start, end := m[0], m[1]
		if len(m) > 3 && m[2] >= 0 {
			start, end = m[2], m[3]
		}

		if p.accept == nil || p.accept(line[start:end]) {
			spans = append(spans, [2]int{start, end})
		}
	}

	return spans
}

func overlaps(spans [][2]int, start, end int) bool {
	for _, s := range spans {
		if start < s[1] && s[0] < end {
			return true
		}
	}

	return false
}

// hasEntropy reports a run that looks like a random secret and that no other rule has taken: 32 characters or
// more, with a capital, a small letter and a digit, not only hex (a git hash or a sha256 is no secret), and over
// entropyBits.
func hasEntropy(line string, taken [][2]int) bool {
	for _, loc := range runRe.FindAllStringIndex(line, -1) {
		if !overlaps(taken, loc[0], loc[1]) && looksRandom(line[loc[0]:loc[1]]) {
			return true
		}
	}

	return false
}

func looksRandom(run string) bool {
	var (
		count               [256]int
		upper, lower, digit bool
		hex                 = true
	)

	for i := 0; i < len(run); i++ {
		c := run[i]
		count[c]++

		upper = upper || c >= 'A' && c <= 'Z'
		lower = lower || c >= 'a' && c <= 'z'
		digit = digit || c >= '0' && c <= '9'
		hex = hex && (c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F')
	}

	if !upper || !lower || !digit || hex {
		return false
	}

	var entropy float64

	for _, c := range count {
		if c > 0 {
			p := float64(c) / float64(len(run))
			entropy -= p * math.Log2(p)
		}
	}

	return entropy >= entropyBits && !(wordy(run) >= 3 && vowelShare(run) >= 0.3)
}

// vowelShare is the share of vowels among a run's small letters: about four in ten in words, two in ten at random.
func vowelShare(run string) float64 {
	letters, vowels := 0, 0

	for i := 0; i < len(run); i++ {
		if c := run[i]; c >= 'a' && c <= 'z' {
			letters++

			if strings.IndexByte("aeiouy", c) >= 0 {
				vowels++
			}
		}
	}

	if letters == 0 {
		return 0
	}

	return float64(vowels) / float64(letters)
}

// wordy is the mean length of a run's pieces — a capital and the small letters after it, a run of small letters, of
// capitals, of digits, of anything else. Names a person writes (TestParseMarkdownBlock2, a path) are made of words
// and come out at 3 or more; a random key changes kind every character or two.
func wordy(run string) float64 {
	kind := func(c byte) int {
		switch {
		case c >= 'a' && c <= 'z':
			return 1
		case c >= 'A' && c <= 'Z':
			return 2
		case c >= '0' && c <= '9':
			return 3
		}

		return 4
	}

	pieces := 0

	for i := 0; i < len(run); i++ {
		// A capital followed by a small letter starts a word that goes on in small letters.
		if i == 0 || kind(run[i]) != kind(run[i-1]) && (kind(run[i-1]) != 2 || kind(run[i]) != 1) {
			pieces++
		}
	}

	return float64(len(run)) / float64(pieces)
}

// hasIP reports an IPv4 address that is not one of the ones made for examples: not 127.0.0.0/8, 0.0.0.0, the three
// documentation ranges or the broadcast address. A private address is a finding, it names a host. A number with an
// octet over 255, with more than four parts, or glued to a letter (a "v" before it) is a version, not an address.
func hasIP(line string) bool {
	for _, loc := range dottedRe.FindAllStringIndex(line, -1) {
		if loc[0] > 0 && isLetter(line[loc[0]-1]) {
			continue
		}

		if octets, ok := parseIP(line[loc[0]:loc[1]]); ok && !documentation(octets) {
			return true
		}
	}

	return false
}

func isLetter(c byte) bool { return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' }

func parseIP(s string) (octets [4]int, ok bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return octets, false
	}

	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || len(p) > 3 || v > 255 {
			return octets, false
		}

		octets[i] = v
	}

	return octets, true
}

func documentation(o [4]int) bool {
	switch {
	case o[0] == 127, o == [4]int{0, 0, 0, 0}, o == [4]int{255, 255, 255, 255}:
		return true
	case o[0] == 192 && o[1] == 0 && o[2] == 2:
		return true
	case o[0] == 198 && o[1] == 51 && o[2] == 100:
		return true
	case o[0] == 203 && o[1] == 0 && o[2] == 113:
		return true
	}

	return false
}

// hasEmail reports an address that names a person or a company. Mail to the reserved example domains (and their
// subdomains), no-reply senders and GitHub's users.noreply addresses are fine; so is what an earlier rule took,
// like the user:password@ part of a URL.
func hasEmail(line string, taken [][2]int) bool {
	for _, m := range emailRe.FindAllStringSubmatchIndex(line, -1) {
		if overlaps(taken, m[0], m[1]) {
			continue
		}

		if !harmlessMail(strings.ToLower(line[m[2]:m[3]]), strings.ToLower(line[m[4]:m[5]])) {
			return true
		}
	}

	return false
}

func harmlessMail(local, domain string) bool {
	// git@host:group/repo is an ssh remote, not mail.
	if local == "noreply" || local == "no-reply" || local == "git" || domain == "users.noreply.github.com" {
		return true
	}

	for _, d := range []string{"example.com", "example.org", "example.net"} {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}

	return false
}

// crewWords makes one matcher of each of the crew's own words: the word whole, so "acme" is not "acmeware", in any
// case. Blank entries are left out.
func crewWords(extra []string) []*regexp.Regexp {
	var out []*regexp.Regexp

	for _, w := range extra {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}

		// The word is quoted, so it only fails to compile when it is absurdly long; such an entry is left out.
		if re, err := regexp.Compile(`(?i)(?:^|[^A-Za-z0-9_])` + regexp.QuoteMeta(w) + `(?:[^A-Za-z0-9_]|$)`); err == nil {
			out = append(out, re)
		}
	}

	return out
}
