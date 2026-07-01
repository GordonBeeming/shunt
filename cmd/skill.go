package cmd

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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
	Key       string // short name for args: claude, codex, opencode
	Name      string
	HomeRel   string // existence => installed
	SkillsRel string // where to write <skillName>/
}

func agentTargets() []agentTarget {
	return []agentTarget{
		{"claude", "Claude Code", ".claude", ".claude/skills"},
		{"codex", "Codex", ".codex", ".codex/skills"},
		{"opencode", "OpenCode", ".config/opencode", ".config/opencode/skills"},
	}
}

func newSkillCmd() *cobra.Command {
	c := &cobra.Command{Use: "skill", Short: "Manage the shunt agent skill"}
	c.AddCommand(newSkillInstallCmd())
	return c
}

func newSkillInstallCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "install [agent...]",
		Short: "Install the bundled skill into installed agents (interactive, or name them)",
		Long: "With no args, lists the detected agents (Claude Code, Codex, OpenCode) and asks which to install " +
			"into. Pass agent keys (claude/codex/opencode) to skip discovery and the prompt, or --all for every " +
			"detected agent. The skill is embedded in this binary, so this is the deploy path — not a hand copy.",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			bin := config.Current().BinaryName

			var detected []agentTarget
			for _, t := range agentTargets() {
				if _, statErr := os.Stat(filepath.Join(home, t.HomeRel)); statErr == nil {
					detected = append(detected, t)
				}
			}

			selected, err := chooseAgents(args, detected, all)
			if err != nil {
				return err
			}
			if len(selected) == 0 {
				fmt.Println("nothing selected.")
				return nil
			}

			fmt.Printf("✓ installed the %q skill:\n", skillName)
			for _, t := range selected {
				dest := filepath.Join(home, t.SkillsRel, skillName)
				if err := writeSkill(dest, bin); err != nil {
					return fmt.Errorf("install to %s: %w", t.Name, err)
				}
				fmt.Printf("    %-12s %s\n", t.Name, dest)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "install to all detected agents without prompting")
	return c
}

// chooseAgents resolves which agents to install to: explicit names (any known
// agent, detected or not), --all (every detected), or an interactive pick.
func chooseAgents(args []string, detected []agentTarget, all bool) ([]agentTarget, error) {
	if len(args) > 0 {
		byKey := map[string]agentTarget{}
		for _, t := range agentTargets() {
			byKey[t.Key] = t
		}
		var out []agentTarget
		for _, a := range args {
			t, ok := byKey[strings.ToLower(a)]
			if !ok {
				return nil, fmt.Errorf("unknown agent %q (known: claude, codex, opencode)", a)
			}
			out = append(out, t)
		}
		return out, nil
	}
	if len(detected) == 0 {
		return nil, fmt.Errorf("no agents detected (looked for ~/.claude, ~/.codex, ~/.config/opencode); name one explicitly to install anyway")
	}
	if all {
		return detected, nil
	}
	return pickAgents(detected)
}

// pickAgents lists detected agents and reads a comma/space-separated choice from
// stdin; empty or "all" selects everything.
func pickAgents(detected []agentTarget) ([]agentTarget, error) {
	fmt.Println("Detected agents — which to install the skill into? (e.g. 1,3 — empty or 'all' = every one)")
	for i, t := range detected {
		fmt.Printf("  %d) %s\n", i+1, t.Name)
	}
	fmt.Print("> ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read selection: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" || strings.EqualFold(line, "all") {
		return detected, nil
	}
	var out []agentTarget
	seen := map[int]bool{}
	for _, tok := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' }) {
		idx, convErr := strconv.Atoi(strings.TrimSpace(tok))
		if convErr != nil || idx < 1 || idx > len(detected) {
			return nil, fmt.Errorf("invalid selection %q", tok)
		}
		if !seen[idx] {
			seen[idx] = true
			out = append(out, detected[idx-1])
		}
	}
	return out, nil
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
