package shift

import "github.com/iamnikolie/zmina/internal/registry"

const (
	// WaitPermission is View.WaitingFor of a role whose session waits on a permission prompt.
	WaitPermission = "permission"
	// WaitQuestion is View.WaitingFor of a role whose session waits on a question put to the owner (AskUserQuestion).
	WaitQuestion = "question"
)

// waitKind is what a session waits on, as the short word the views and notes use: Claude Code's own "waitingFor" in
// its session file, read only while the status is waiting. Empty when it is not waiting, when the file does not say
// (a Claude Code that does not write it, a Codex bridge) or says something this zmina does not know.
func waitKind(sess *registry.Session) string {
	if sess == nil || sess.Status != "waiting" {
		return ""
	}

	switch sess.WaitingFor {
	case "permission prompt":
		return WaitPermission
	case "input needed":
		return WaitQuestion
	}

	return ""
}

// waitNoun is how a note names what a role waits on: "a question", "a permission prompt", or "a dialog" when the
// session file does not say which.
func waitNoun(kind string) string {
	switch kind {
	case WaitPermission:
		return "a permission prompt"
	case WaitQuestion:
		return "a question"
	}

	return "a dialog"
}
