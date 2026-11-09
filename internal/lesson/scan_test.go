package lesson

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests build every secret-shaped text and every address while they run: the source holds no token and no
// address of its own, so the scan of the repository has nothing to find in it.

func fakeIP(a, b, c, d int) string { return fmt.Sprintf("%d.%d.%d.%d", a, b, c, d) }

// randomRun is n distinct characters of the three kinds, in a scrambled order: it reads as a secret.
func randomRun(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[(i*7)%len(alphabet)]
	}

	return string(b)
}

func noDigit(r rune) rune {
	if r >= '0' && r <= '9' {
		return 'q'
	}

	return r
}

// ruleNames are the rules the scan named, in order.
func ruleNames(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Rule)
	}

	return out
}

func TestScanTokens(t *testing.T) {
	for _, tc := range []struct{ rule, secret string }{
		{"aws key", "AK" + "IA" + strings.Repeat("Q7", 8)},
		{"github token", "gh" + "p_" + strings.Repeat("a1", 18)},
		{"github token", "gh" + "o_" + strings.Repeat("B2", 20)},
		{"github token", "gh" + "s_" + strings.Repeat("c", 36)},
		{"github token", "github_" + "pat_" + strings.Repeat("x_", 12)},
		{"gitlab token", "gl" + "pat-" + strings.Repeat("ab_", 8)},
		{"anthropic key", "sk-" + "ant-" + strings.Repeat("a1", 12)},
		{"openai key", "sk-" + strings.Repeat("a1", 12)},
		{"openai key", "sk-" + "proj-" + strings.Repeat("a1_", 8)},
		{"slack token", "xox" + "b-" + strings.Repeat("1", 12) + "-" + strings.Repeat("a", 8)},
		{"google key", "AI" + "za" + strings.Repeat("a1", 17) + "b"},
		{"private key", "-----BEGIN RSA " + "PRIVATE KEY-----"},
		{"private key", "-----BEGIN " + "PRIVATE KEY-----"},
		{"private key", "-----BEGIN OPENSSH " + "PRIVATE KEY-----"},
		{"jwt", "ey" + "J" + strings.Repeat("a", 12) + "." + "ey" + "J" + strings.Repeat("b", 12) + "." + strings.Repeat("c", 12)},
		{"url credentials", "postgres://alex:" + "pw" + strings.Repeat("9", 4) + "@db.acme.test:5432/app"},
		{"url credentials", "https://alex:" + "pw@acme.test"},
		{"stripe key", "sk" + "_live_" + strings.Repeat("a1", 12)},
		{"stripe key", "rk" + "_test_" + strings.Repeat("b2", 12)},
		{"telegram bot token", "123456789:" + "AA" + strings.Repeat("x1", 16) + "z"},
		{"long hex", strings.Repeat("ab12", 16)},
	} {
		for _, text := range []string{
			tc.secret,
			"the key is " + tc.secret + " and more",
			"export API_KEY=" + tc.secret,
			`"token": "` + tc.secret + `",`,
			"line one\n\n" + tc.secret,
		} {
			got := Scan(text, nil)
			assert.Equal(t, []string{tc.rule}, ruleNames(got), "%s in %q", tc.rule, text)
		}
	}
}

func TestScanFindingNeverHoldsTheSecret(t *testing.T) {
	secrets := []string{
		"AK" + "IA" + strings.Repeat("Q7", 8),
		"gh" + "p_" + strings.Repeat("a1", 18),
		"sk-" + "ant-" + strings.Repeat("a1", 12),
		randomRun(40),
		fakeIP(10, 0, 0, 5),
		"alex@acme.test",
		"postgres://alex:" + "hunter2" + "@db.acme.test/app",
	}

	for _, s := range secrets {
		got := Scan("here it is: "+s, []string{"acme"})
		require.NotEmpty(t, got, s)

		for _, shown := range []string{fmt.Sprintf("%v", got), fmt.Sprintf("%+v", got), fmt.Sprintf("%#v", got)} {
			assert.NotContains(t, shown, s)
			assert.NotContains(t, shown, s[len(s)/2:])
		}
	}

	// Only the rule and the line are in a finding; there is no field to put the text in.
	assert.Equal(t, Finding{Rule: "aws key", Line: 1}, Scan("AK"+"IA"+strings.Repeat("Q7", 8), nil)[0])
}

func TestScanSkNeedsABoundary(t *testing.T) {
	for _, text := range []string{
		"task-force-with-long-names-here-today",
		"risk-assessment-framework-document-v2",
		"the desk-ant-colony-with-many-members",
		"sk-short",
		"sk-" + strings.Repeat("a", 19),
	} {
		assert.Empty(t, Scan(text, nil), text)
	}

	assert.Equal(t, []string{"openai key"}, ruleNames(Scan("--key sk-"+strings.Repeat("a1", 12), nil)))
	assert.Equal(t, []string{"openai key"}, ruleNames(Scan("key=sk-"+strings.Repeat("a1", 12), nil)))
	assert.Equal(t, []string{"anthropic key"}, ruleNames(Scan("(sk-ant-"+strings.Repeat("a1", 12)+")", nil)))
}

func TestScanPublicKeyHeaderIsFine(t *testing.T) {
	assert.Empty(t, Scan("-----BEGIN PUBLIC KEY-----", nil))
	assert.Empty(t, Scan("-----BEGIN CERTIFICATE-----", nil))
	assert.Empty(t, Scan("ssh-ed25519 is the key type", nil))
}

func TestScanHighEntropy(t *testing.T) {
	secret := randomRun(40)

	for _, text := range []string{
		secret,
		"password: " + secret,
		`"secret": "` + secret + `"`,
		secret + "==",
		"Authorization: Bearer " + secret,
		"line one\nline two\n" + secret,
		randomRun(32),
		randomRun(62),
	} {
		assert.Equal(t, []string{"high-entropy string"}, ruleNames(Scan(text, nil)), text)
	}
}

func TestScanHighEntropyNeedsAllOfItsParts(t *testing.T) {
	for name, text := range map[string]string{
		"31 characters":        randomRun(31),
		"no capital":           strings.ToLower(randomRun(40)),
		"no small letter":      strings.ToUpper(randomRun(40)),
		"no digit":             strings.Map(noDigit, randomRun(40)),
		"repeats, low entropy": strings.Repeat("aB3", 12),
		"git sha-1":            strings.Repeat("0123456789abcdef", 3)[:40],
		"a sha in a sentence":  "fixed in commit " + strings.Repeat("9f8e7d6c5b4a3210", 2) + " on main",
		"lowercase slug":       "feature-add-new-user-authentication-flow-for-the-admin-panel",
		"uuid":                 "123e4567-e89b-12d3-a456-426614174000",
		"dots split a run":     randomRun(20) + "." + randomRun(20),
	} {
		assert.Empty(t, Scan(text, nil), name)
	}
}

func TestScanTakenRunsAreNotCountedTwice(t *testing.T) {
	// A token is long and random, and still it is one finding, under its own name.
	token := "gh" + "p_" + randomRun(40)[:36]
	assert.Equal(t, []string{"github token"}, ruleNames(Scan(token, nil)))

	jwt := "ey" + "J" + randomRun(40) + "." + "ey" + "J" + randomRun(40) + "." + randomRun(43)
	assert.Equal(t, []string{"jwt"}, ruleNames(Scan(jwt, nil)))

	// A random run next to a token is its own finding.
	got := Scan("gh"+"p_"+strings.Repeat("a1", 18)+" "+randomRun(40), nil)
	assert.Equal(t, []string{"github token", "high-entropy string"}, ruleNames(got))
}

func TestScanIP(t *testing.T) {
	for _, addr := range []string{
		fakeIP(10, 0, 0, 5),
		fakeIP(192, 168, 1, 10),
		fakeIP(172, 16, 0, 1),
		fakeIP(169, 254, 1, 1),
		fakeIP(8, 8, 8, 8),
		fakeIP(198, 51, 101, 1),
		fakeIP(192, 0, 3, 1),
		fakeIP(203, 0, 114, 1),
		fakeIP(128, 0, 0, 1),
		fakeIP(255, 255, 255, 254),
		fakeIP(10, 0, 0, 5) + "/8",
	} {
		for _, text := range []string{addr, "ssh to " + addr + ":22", "the box (" + addr + ") is up", "at " + addr + ".", "host=" + addr} {
			assert.Equal(t, []string{"ip address"}, ruleNames(Scan(text, nil)), text)
		}
	}
}

func TestScanIPNotAnAddress(t *testing.T) {
	for _, text := range []string{
		// Made for examples.
		fakeIP(127, 0, 0, 1), fakeIP(127, 255, 255, 254), fakeIP(0, 0, 0, 0), fakeIP(255, 255, 255, 255),
		fakeIP(192, 0, 2, 1), fakeIP(192, 0, 2, 255), fakeIP(198, 51, 100, 7), fakeIP(203, 0, 113, 99),
		"listen on " + fakeIP(127, 0, 0, 1) + ":8080",
		// Versions and other numbers.
		"1.2.3.400", "version 1.2.3.400 is out", "go 1.24", "v2.0.1", "release 10.4.7", "3.14159",
		"v" + fakeIP(1, 2, 3, 4), "build" + fakeIP(1, 2, 3, 4), fakeIP(1, 2, 3, 4) + ".5", "1." + fakeIP(1, 2, 3, 4),
		fakeIP(256, 1, 1, 1), fakeIP(1, 1, 1, 256), fakeIP(1, 1, 1, 1000), "1.2.3", "10.0.0",
		// Not numbers at all.
		"a.b.c.d", "1,2,3,4",
	} {
		assert.Empty(t, Scan(text, nil), text)
	}
}

func TestScanEmail(t *testing.T) {
	for _, text := range []string{
		"alex@acme.test",
		"mail alex@acme.test.",
		"(Alex.Smith+news@Acme.Test)",
		"to: alex@acme.co.uk, bob@acme.test",
		"alex@example.com.acme.test",
		"alex@notexample.com",
		"alex@example.company",
	} {
		assert.Equal(t, []string{"email"}, ruleNames(Scan(text, nil)), text)
	}
}

func TestScanEmailNotAnAddress(t *testing.T) {
	for _, text := range []string{
		"alex@example.com", "alex@example.org", "alex@example.net", "Alex@EXAMPLE.COM", "alex@mail.example.com",
		"noreply@acme.test", "no-reply@acme.test", "NoReply@acme.test",
		"1234+alex@users.noreply.github.com", "alex@users.noreply.github.com",
		"lodash@4.17.21", "react@18.2.0", "@acme/package", "alex@localhost", "an @ sign alone", "user@",
	} {
		assert.Empty(t, Scan(text, nil), text)
	}
}

func TestScanURLWithoutCredentials(t *testing.T) {
	for _, text := range []string{
		"https://acme.test:8443/a/b",
		"http://localhost:8080/path",
		"see https://example.com/docs#x",
		"https://alex:@acme.test/x",
		"ssh://host.test:22/repo",
		"file:///tmp/a:b",
	} {
		assert.Empty(t, Scan(text, nil), text)
	}

	// The password part of a URL is not also an email.
	got := Scan("postgres://alex:"+"secret"+"@db.acme.test/app", nil)
	assert.Equal(t, []string{"url credentials"}, ruleNames(got))
}

func TestScanCrewWords(t *testing.T) {
	extra := []string{"Acme", "  ", "", "big client", "c++", "acme.io"}

	for _, text := range []string{
		"we ship to acme next week",
		"ACME",
		"Acme.",
		"(acme)",
		"pre-acme era",
		"acme-corp",
		"acme/repo",
		"the Big Client contract",
		"we write c++ here",
		"mail acme.io about it",
		"line one\nacme",
	} {
		assert.Equal(t, []string{"crew word"}, ruleNames(Scan(text, extra)), text)
	}

	for _, text := range []string{
		"acmeware",
		"megacme",
		"an acme_corp",
		"acme2",
		"2acme",
		"the bigclient",
		"big  client",
		"acmexio",
		"nothing here",
		"",
	} {
		assert.Empty(t, Scan(text, extra), text)
	}
}

func TestScanCrewWordsIgnoreBlanks(t *testing.T) {
	assert.Empty(t, Scan("anything at all", []string{"", " ", "\t"}))
	assert.Empty(t, Scan("anything at all", nil))
	assert.Equal(t, []string{"crew word"}, ruleNames(Scan("Acme", []string{" acme "})), "an entry is trimmed")
}

func TestScanCrewWordOnceALine(t *testing.T) {
	got := Scan("acme and globex and acme again", []string{"acme", "globex"})
	assert.Equal(t, []Finding{{Rule: "crew word", Line: 1}}, got)
}

func TestScanLinesAndOrder(t *testing.T) {
	aws := "AK" + "IA" + strings.Repeat("Q7", 8)
	text := strings.Join([]string{
		"fine",
		"key " + aws,
		"also fine",
		"box " + fakeIP(10, 0, 0, 5) + " and " + fakeIP(10, 0, 0, 6) + " mail alex@acme.test",
		"key " + aws + " and again " + aws,
		"globex " + aws + " " + fakeIP(10, 0, 0, 7),
	}, "\n")

	assert.Equal(t, []Finding{
		{Rule: "aws key", Line: 2},
		{Rule: "ip address", Line: 4},
		{Rule: "email", Line: 4},
		{Rule: "aws key", Line: 5},
		{Rule: "aws key", Line: 6},
		{Rule: "ip address", Line: 6},
		{Rule: "crew word", Line: 6},
	}, Scan(text, []string{"globex"}), "line order, rule order on a line, a rule once a line")
}

func TestScanCRLF(t *testing.T) {
	text := "fine\r\nbox " + fakeIP(10, 0, 0, 5) + "\r\nacme\r\n"
	assert.Equal(t, []Finding{{Rule: "ip address", Line: 2}, {Rule: "crew word", Line: 3}}, Scan(text, []string{"acme"}))
}

func TestScanOrdinaryText(t *testing.T) {
	for _, text := range []string{
		"",
		"Run make test before every push; CI is strict and a red pipeline costs a round trip.",
		"Quote zsh globs: an unquoted glob that matches nothing aborts the whole command.",
		"Keep go 1.24 in go.mod; no sync.WaitGroup.Go and other 1.25+ API.",
		"The state file is versioned (state.json, v2) and decodeV1 reads the first format.",
		"/Users/alex/development/acme/internal/lesson/lesson.go",
		"internal/lesson/lesson_test.go and cmd/skill.md change together",
		"../../some/very/long/relative/path/with/many/segments/in/it/file.go",
		"/usr/local/bin/some-tool --flag=value --other-flag value",
		"git commit -m 'one line' && git push origin zmina/t-1",
		"tmux targets are =name or =name: and in zsh they need quotes",
		"2026-10-29T14:00:00Z at 12:30:45.123",
		"Fixes T-1, see !12 and #34",
		"- [ ] check the box\n- [x] done",
		"| a | b |\n|---|---|\n| 1 | 2 |",
		"=====================================================",
		"-----------------------------------------------------",
		"a_very_long_snake_case_identifier_name_for_a_helper_function",
		pilotText,
	} {
		assert.Empty(t, Scan(text, nil), text)
	}
}

// Names a person writes are words, not keys: long test names, file names, git remotes do not hit.
func TestScanWordyNamesAreNoSecret(t *testing.T) {
	for _, line := range []string{
		"run TestParseMarkdownMultiLineWhyBlock2 alone",
		"TestRestartAfterTheTaskIsDoneHeldByBackground1 is flaky",
		"zmina_0.10.0_darwin_arm64_TarGz_Checksum1",
		"push to git@gitlab.example.org:acme/agentic-os.git",
	} {
		if f := Scan(line, nil); len(f) != 0 {
			t.Errorf("%q: %v", line, f)
		}
	}

	// A random key of the same length still does.
	key := "Q7v" + "Zp2Lk9" + "Xw4Rb8Nc1" + "Tm6Hs3Jd5Fg0Ye7a"
	if f := Scan("key "+key, nil); len(f) != 1 {
		t.Errorf("random key not found: %v", f)
	}
}

// A git hash, full or short, is no secret; a longer hex run (a sha256, an HMAC key) is taken for one: a rule needs
// neither.
func TestScanGitHashIsNoSecretLongerHexIs(t *testing.T) {
	assert.Empty(t, Scan("fixed in "+strings.Repeat("ab12", 10)+" and 8c14e7d", nil))

	for _, text := range []string{strings.Repeat("0123456789ABCDEF", 4), "sha256:" + strings.Repeat("0123456789abcdef", 4),
		strings.Repeat("0123456789abcdefABCDEF", 3)} {
		assert.Equal(t, []string{"long hex"}, ruleNames(Scan(text, nil)), text)
	}
}
