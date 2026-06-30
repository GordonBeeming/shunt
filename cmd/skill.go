package cmd

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/skillfiles"
	"github.com/spf13/cobra"
)

// skillName is the installed skill directory name, shared across channels so
// there's one shunt skill (the installing channel decides which binary it calls).
const skillName = "shunt"

// agentTarget is an AI coding agent that may be installed and reads skills from a
// skills/<name>/ dir. Presence of HomeRel under $HOME means it's installed. Add
// new agents here as their skills layout is confirmed.
type agentTarget struct {
	Name      string
	HomeRel   string // existence => installed
	SkillsRel string // where to write <skillName>/
}

func agentTargets() []agentTarget {
	return []agentTarget{
		{"Claude Code", ".claude", ".claude/skills"},
		{"Codex", ".codex", ".codex/skills"},
		{"OpenCode", ".config/opencode", ".config/opencode/skills"},
	}
}

func newSkillCmd() *cobra.Command {
	c := &cobra.Command{Use: "skill", Short: "Manage the shunt agent skill"}
	c.AddCommand(newSkillInstallCmd())
	return c
}

func newSkillInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install the bundled skill into every detected agent (Claude Code, Codex, …)",
		Long: "Deploys the skill embedded in this binary to each installed agent's skills/<name>/ dir. " +
			"Re-run after upgrading shunt to refresh it. This is the deploy path — the skill is no longer copied by hand.",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			bin := config.Current().BinaryName

			var installed []string
			for _, t := range agentTargets() {
				if _, err := os.Stat(filepath.Join(home, t.HomeRel)); err != nil {
					continue // agent not installed
				}
				dest := filepath.Join(home, t.SkillsRel, skillName)
				if err := writeSkill(dest, bin); err != nil {
					return fmt.Errorf("install to %s: %w", t.Name, err)
				}
				installed = append(installed, fmt.Sprintf("%-12s %s", t.Name, dest))
			}
			if len(installed) == 0 {
				return fmt.Errorf("no supported agents detected (looked for ~/.claude, ~/.codex, ~/.config/opencode)")
			}
			fmt.Printf("✓ installed the %q skill to %d agent(s):\n", skillName, len(installed))
			for _, l := range installed {
				fmt.Printf("    %s\n", l)
			}
			return nil
		},
	}
}

// writeSkill walks the embedded skill tree into dest, replacing the dev binary
// token with this build's command name and marking scripts executable.
func writeSkill(dest, bin string) error {
	const root = "files"
	return fs.WalkDir(skillfiles.FS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dest, rel), 0o755)
		}
		b, err := skillfiles.FS.ReadFile(p)
		if err != nil {
			return err
		}
		// Source uses the dev command name; rewrite to this channel's binary.
		content := strings.ReplaceAll(string(b), "shunt-dev", bin)
		mode := os.FileMode(0o644)
		if strings.HasPrefix(rel, "scripts"+string(os.PathSeparator)) {
			mode = 0o755
		}
		return os.WriteFile(filepath.Join(dest, rel), []byte(content), mode)
	})
}
