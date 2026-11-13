package cmd

// The readers as the golden tests call them: by the embedded table, as a crew with no screen-rules.yaml reads. Production code
// builds a screenReader from its shift's table and never calls these.

func compactScreen(text string, n int) string { return screenReader{}.compactScreen(text, n) }

func footerAt(lines []string) int { return screenReader{}.footerAt(lines) }

func activeDialog(screen, question string) ([]dialogOption, bool) {
	return screenReader{}.activeDialog(screen, question)
}

func dialogLines(screen string) []string { return screenReader{}.dialogLines(screen) }

func onScreen(screen, want string, options ...string) bool {
	return screenReader{}.onScreen(screen, want, options...)
}
