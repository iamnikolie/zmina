package shift

import (
	"os"
	"path/filepath"

	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/crew"
)

func (s *Shift) isCodex(name string) bool { r, err := s.Role(name); return err == nil && r.IsCodex() }

func (s *Shift) registryFor(name string) Registry {
	if s.isCodex(name) {
		return &codex.Registry{Dir: filepath.Join(s.Crew.StateDir(), "codex")}
	}
	return s.Reg
}

func (s *Shift) codexCommand(r *crew.Role, resume string) []string {
	bin, err := os.Executable()
	if err != nil {
		bin = "zmina"
	}
	argv := []string{bin, "_codex", "--bin", s.Crew.Codex, "--session", r.Session, "--role", r.Name, "--state-dir", filepath.Join(s.Crew.StateDir(), "codex")}
	for _, p := range [][2]string{{"--transport", r.CodexTransport}, {"--profile", r.Profile}, {"--resume", resume}, {"--boot", s.BootText(r)}, {"--model", r.Model}, {"--effort", r.ReasoningEffort}, {"--sandbox", r.Sandbox}, {"--approval", r.ApprovalPolicy}} {
		if p[1] != "" {
			argv = append(argv, p[0], p[1])
		}
	}
	return append(append(argv, "--"), r.Args...)
}
