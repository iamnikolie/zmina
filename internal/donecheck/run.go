package donecheck

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// What a Result keeps of the output: the last TailLines lines, each cut at LineRunes, in at most TailBytes. While the
// command runs only the last rawKeep bytes are held, so a command that prints without end costs no more memory.
const (
	TailLines = 40
	LineRunes = 300
	TailBytes = 8 << 10
	rawKeep   = 64 << 10
)

// waitDelay is how long after the command ended zmina still waits for its output pipes: a process it left running that
// holds them must not keep the run for as long as it lives.
const waitDelay = 2 * time.Second

// InCheckEnv is the variable Run sets, to 1, in the environment of a check: a zmina that finds it set is run by a
// check, which acts for no one, so it sets no task's check and forces past none.
const InCheckEnv = "ZMINA_IN_CHECK"

// Run runs s.Run with /bin/sh -c in dir, with env, until it ends or s.Limit passes or ctx is done, and says what came
// of it. Its stdin is closed. It runs in a session and process group of its own (no controlling terminal), which is
// killed when the run ends, so nothing it started outlives it. The environment passed is scrubbed of names that look
// like secrets (see Scrub), InCheckEnv is set in it, and the output never holds those secrets' values, nor the password
// of a scheme://user:password@host address.
//
// Run takes no lock and reads no state: the caller holds none while it runs.
func Run(ctx context.Context, s Spec, dir string, env []string) Result {
	res := Result{Run: s.Run, At: time.Now()}

	ctx, cancel := context.WithTimeout(ctx, s.Limit())
	defer cancel()

	if env == nil {
		env = os.Environ()
	}

	env, secrets := Scrub(env)
	env = append(slicesDelete(env, InCheckEnv), InCheckEnv+"=1")
	out := &tail{}

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", s.Run)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // its own session, so also its own group, led by the shell
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay

	err := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // what the command left running
	}

	res.DurationMS = time.Since(res.At).Milliseconds()
	res.Output, res.Truncated = out.summary(secrets)

	switch {
	case cmd.ProcessState != nil && cmd.ProcessState.Success():
		res.Exit = 0
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Exit, res.TimedOut = -1, true
	case ctx.Err() != nil:
		res.Exit, res.Interrupted = -1, true
	case cmd.ProcessState == nil:
		res.Exit, res.Err = -1, "did not start: "+err.Error()
	case cmd.ProcessState.ExitCode() < 0:
		res.Exit, res.Err = -1, cmd.ProcessState.String()
	default:
		res.Exit = cmd.ProcessState.ExitCode()
	}

	return res
}

// tail keeps the end of what is written to it.
type tail struct {
	buf   []byte
	total int64
}

func (t *tail) Write(p []byte) (int, error) {
	t.total += int64(len(p))
	t.buf = append(t.buf, p...)

	if len(t.buf) > 2*rawKeep {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-rawKeep:]...)
	}

	return len(p), nil
}

var (
	// escapes are the terminal's control sequences: colours, cursor moves.
	escapes = regexp.MustCompile(`\x1b(\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[@-Z\\-_])`)

	// secretName is a variable name that looks like it holds a secret: one of these words as a part of the name (parts
	// are what is between underscores), or a name that ends in one.
	secretName = regexp.MustCompile(`(?i)(^|_)(TOKEN|SECRET|PASSWORD|PASSWD|PASSPHRASE|PASS|PWD|PAT|AUTH|AUTHORIZATION|` +
		`CREDENTIAL|CREDENTIALS|COOKIE|COOKIES|API_?KEY|ACCESS_?KEY|PRIVATE_KEY)(_|$)|TOKEN$|_KEY$|^KEY$|(^|_)DSN$`)

	// addressPassword is the password of a scheme://user:password@host address, wherever it is written.
	addressPassword = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s/:@]*:)[^\s/@]+(@)`)
)

// notSecret are variable names that match secretName and are not secrets: paths, switches and the shell's own.
var notSecret = map[string]bool{
	"SSH_AUTH_SOCK": true, "PASSWORD_STORE_DIR": true, "PWD": true, "GIT_ASKPASS": true, "SSH_ASKPASS": true,
	"GOPRIVATE": true, "GONOSUMDB": true, "GONOPROXY": true, "GONOSUMCHECK": true, "TOKENIZERS_PARALLELISM": true,
}

// secretMin is the shortest value that is blanked in the output: a shorter one would blank ordinary words.
const secretMin = 8

// IsSecretName reports whether a variable's name looks like it holds a secret.
func IsSecretName(name string) bool {
	return !notSecret[strings.ToUpper(name)] && secretName.MatchString(name)
}

// Scrub is env without the variables whose names look like secrets (IsSecretName), and the values of those, which Run
// keeps out of the output, longest first. A check does not need the owner's credentials to say whether tests pass, and
// a command that prints its environment shows none. A variable whose name does not look like one stays, a DATABASE_URL
// among them: only the password of an address is blanked from the output.
func Scrub(env []string) (clean, secrets []string) {
	clean = make([]string, 0, len(env)) // never nil: a nil Env is the whole environment

	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		if !IsSecretName(name) {
			clean = append(clean, kv)

			continue
		}

		if len(value) >= secretMin {
			secrets = append(secrets, value)
		}
	}

	// A secret that holds another is blanked first, so no tail of it is left.
	sort.SliceStable(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })

	return clean, secrets
}

// slicesDelete is env without the variable name.
func slicesDelete(env []string, name string) []string {
	out := env[:0:0]

	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != name {
			out = append(out, kv)
		}
	}

	return out
}

// Clean is text as plain text for a record or a terminal: no control sequences, control characters, format characters
// (zero width, bidirectional marks and overrides) or separators, no password in a scheme://user:password@host address;
// line breaks and tabs stay. A command or an output goes through it before it is shown.
func Clean(text string) string {
	text = strings.ToValidUTF8(text, "?")
	text = addressPassword.ReplaceAllString(text, "${1}[redacted]${2}")
	text = escapes.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "\r\n", "\n")

	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && r != '\r' && unprintable(r) {
			return -1
		}

		return r
	}, text)
}

// Line is Clean on one line, its whitespace folded: a stored command as it is shown in a line, an event or a message.
func Line(s string) string {
	return strings.Join(strings.Fields(Clean(s)), " ")
}

// summary is the end of the output as a Result keeps it: valid text with no control sequences or other control
// characters, none of the secrets' values, the last TailLines lines each cut to LineRunes, in at most TailBytes. The
// second value says anything before it was left out.
func (t *tail) summary(secrets []string) (string, bool) {
	cut := t.total > int64(len(t.buf))
	text := string(t.buf)

	for _, s := range secrets {
		text = strings.ReplaceAll(text, s, "[redacted]")
	}

	text = Clean(text)

	lines := strings.Split(text, "\n")
	if cut {
		lines = lines[1:] // it starts in the middle of a line
	}

	for i, l := range lines {
		// A progress line redraws itself with \r: what stays is what it ended as.
		l = strings.TrimRight(l, "\r")
		if j := strings.LastIndex(l, "\r"); j >= 0 {
			l = l[j+1:]
		}

		lines[i] = clipRunes(l, LineRunes)
	}

	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}

	if len(lines) > TailLines {
		lines, cut = lines[len(lines)-TailLines:], true
	}

	out := strings.Join(lines, "\n")

	if len(out) > TailBytes {
		out, cut = out[len(out)-TailBytes:], true
		// Not in the middle of a character, nor of a line.
		if i := strings.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		}

		for !utf8.ValidString(out) {
			out = out[1:]
		}
	}

	return out, cut
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}

	return s
}

// Tail is the last lines of a Result's output for a message to the person who ran the command.
func (r Result) Tail() string {
	if r.Output == "" {
		return "(no output)"
	}

	if r.Truncated {
		return "…\n" + r.Output
	}

	return r.Output
}
