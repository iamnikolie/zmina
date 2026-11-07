package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHHosts(t *testing.T) {
	config := strings.Join([]string{
		"# Host commented-out",
		"Host alpha # a comment after the names",
		"  HostName localhost",
		"Host beta gamma#not-a-comment",
		`Host "quoted host" delta`,
		"Host 'single' 'two words'",
		`Host it\'s back\\slash sp\ ace "in\"side"`,
		`Host mid"dle"word`,
		"Host=epsilon",
		"Host = zeta",
		"host *.wild one? !negated eta",
		"Match host theta",
		`Host 'unclosed`,
		"",
		"\tHost\tiota",
	}, "\n")

	got := sshHosts(strings.NewReader(config))
	want := []string{"alpha", "beta", "gamma#not-a-comment", "quoted host", "delta", "single", "two words",
		"it's", `back\slash`, "sp ace", `in"side`, "middleword", "epsilon", "zeta", "eta", "iota"}

	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("hosts = %q, want %q", got, want)
	}
}

// A host alias written in quotes is the alias itself: a source line naming it bare is a finding.
func TestSSHQuotedHostIsBanned(t *testing.T) {
	for _, config := range []string{"Host 'alpha'\n HostName localhost\n", "Host \"alpha\"\n", "Host=alpha\n"} {
		home := writeHome(t, config)

		rules, err := banned(home, "HEAD")
		if err != nil {
			t.Fatal(err)
		}

		if hits := check(rules, "fixture:1", "ssh alpha"); len(hits) != 1 {
			t.Fatalf("%q: alias not protected: %v", config, hits)
		}

		if hits := check(rules, "fixture:2", "ssh 'alpha'"); len(hits) != 1 {
			t.Fatalf("%q: quoted use not found: %v", config, hits)
		}
	}
}

// Words of a comment after a Host line's names are not hosts: they would flag every line that uses them.
func TestSSHCommentIsNotAHost(t *testing.T) {
	home := writeHome(t, "Host review-example # inbox channel\n  HostName localhost\n")

	rules, err := banned(home, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	if hits := check(rules, "fixture:1", "inbox channel"); len(hits) != 0 {
		t.Fatalf("comment words taken as hosts: %v", hits)
	}

	if hits := check(rules, "fixture:2", "ssh review-example"); len(hits) != 1 {
		t.Fatalf("the host itself: %v", hits)
	}
}

// A short lowercase host's all-caps spelling is another word: the unit GB is not the host gb.
func TestSSHHostCapsIsAnotherWord(t *testing.T) {
	home := writeHome(t, "Host gb\n  HostName localhost\n")

	rules, err := banned(home, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	if hits := check(rules, "fixture:1", "sizes in MB under 1 GB, else GB"); len(hits) != 0 {
		t.Fatalf("the unit taken as the host: %v", hits)
	}

	for _, line := range []string{"ssh gb", "ssh Gb", "GB and gb", "GB gb"} {
		if hits := check(rules, "fixture:2", line); len(hits) != 1 {
			t.Fatalf("%q: host not found: %v", line, hits)
		}
	}

	// A longer name is no abbreviation: in capitals it is still the name.
	home = writeHome(t, "Host review-example\n  HostName localhost\n")

	if rules, err = banned(home, "HEAD"); err != nil {
		t.Fatal(err)
	}

	if hits := check(rules, "fixture:3", "ssh REVIEW-EXAMPLE"); len(hits) != 1 {
		t.Fatalf("a long name in capitals let through: %v", hits)
	}
}

// A machine with no crews, no mesh, no ssh config and no denylist bans nothing of its own, and fails nothing.
func TestNoLocalState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZMINA_PUBLIC_DENYLIST", filepath.Join(home, "missing"))

	rules, err := banned(home, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	if len(rules) != 0 {
		t.Fatalf("rules = %d, want none", len(rules))
	}

	if hits := check(rules, "fixture:1", "alex at acme on 127.0.0.1, /Users/alex/src"); len(hits) != 0 {
		t.Fatalf("neutral line flagged: %v", hits)
	}

	// Built here, so this file does not trip the check it tests.
	if hits := check(rules, "fixture:2", strings.Join([]string{"10", "1", "2", "3"}, ".")+" /"+"home/someone"); len(hits) != 2 {
		t.Fatalf("an address and a private path: %v", hits)
	}
}

func writeHome(t *testing.T, sshConfig string) string {
	t.Helper()

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(sshConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ZMINA_PUBLIC_DENYLIST", filepath.Join(home, "missing"))

	return home
}
