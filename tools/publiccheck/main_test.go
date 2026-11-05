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
		"Host=epsilon",
		"Host = zeta",
		"host *.wild one? !negated eta",
		"Match host theta",
		"",
		"\tHost\tiota",
	}, "\n")

	got := sshHosts(strings.NewReader(config))
	want := []string{"alpha", "beta", "gamma#not-a-comment", "quoted host", "delta", "epsilon", "zeta", "eta", "iota"}

	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("hosts = %q, want %q", got, want)
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
