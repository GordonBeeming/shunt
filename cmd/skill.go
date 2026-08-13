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

const universalPrerequisitesHeading = "## Host prerequisites shared by every channel"

const universalPrerequisitesTemplate = `
## Host prerequisites shared by every channel

Before %s init, install Apple container, a reviewed Go patch release, xcaddy, and the .NET SDK on PATH. Supported Go releases are 1.25.13 or newer on the 1.25 line and 1.26.6 or newer on the 1.26 line; newer minor lines are rejected until reviewed. Docker runs inside each guest; Docker Desktop and OrbStack are not prerequisites. Install xcaddy with:

    go install github.com/caddyserver/xcaddy/cmd/xcaddy@v0.4.6

Ensure Go's bin directory is on PATH. The Aspire CLI is required only for .NET Aspire apps.
`

const nightlyPrerequisitesTemplate = `
## Nightly host prerequisites

Before %s init, install Apple's container runtime and the .NET SDK on PATH. Homebrew's go@1.25 formula is keg-only, so select the canonical Go binary, install xcaddy with it, and export both its bin directory and its GOPATH bin directory before initialising:

    GO_BIN="$(brew --prefix go@1.25)/bin/go"
    GOPATH_BIN="$("$GO_BIN" env GOPATH)/bin"
    "$GO_BIN" install github.com/caddyserver/xcaddy/cmd/xcaddy@v0.4.6
    export PATH="$(brew --prefix go@1.25)/bin:$GOPATH_BIN:$PATH"

The nightly package gate accepts canonical darwin/arm64 Go 1.25.13 or a later patch on the 1.25 line. Docker runs inside each guest; Docker Desktop and OrbStack are not prerequisites. The Aspire CLI is required only for .NET Aspire apps.
`

const standardOnboardingTemplate = `
This skill targets the **%s** channel.

%s
With %s already on PATH, initialise its channel once and deploy this one shared %s skill to the agents you use:

    %s init
    %s skill install --all
    %s version
`

const nightlyOnboardingTemplate = `
When the public nightly package is available, install it from Homebrew on macOS 26 or newer on Apple silicon (arm64):

    brew update
    brew install gordonbeeming/tap/shunt-nightly

%s
With %s already on PATH, initialise the channel and deploy this one shared %s skill:

    %s init
    %s skill install --all
    %s version

Later, upgrade with brew update && brew upgrade gordonbeeming/tap/shunt-nightly. version includes the channel and build version, such as version=0.0.0-nightly.123.
`

// Placeholders are intentionally distinct from legacy state names such as
// .shunt-dev. The renderer owns only operational channel-specific text; the
// literal dev-to-nightly migration warning stays in the nightly rendering.
const (
	commandPlaceholder           = "{{shunt-command}}"
	channelDirectoryPlaceholder  = "{{shunt-channel-directory}}"
	channelOnboardingPlaceholder = "{{shunt-channel-onboarding}}"
	nightlyMigrationPlaceholder  = "{{shunt-nightly-migration}}"
)

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
			identity := config.Current()

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

			fmt.Printf("%s installed the %q skill:\n", tick(), skillName)
			for _, t := range selected {
				dest := filepath.Join(home, t.SkillsRel, skillName)
				if err := writeSkill(dest, identity); err != nil {
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

// writeSkill walks the embedded skill tree into dest, rendering the installing
// channel's command, operational directory, and onboarding, then marking
// scripts executable.
func writeSkill(dest string, identity config.Identity) error {
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
		content := renderSkill(string(b), identity)
		mode := os.FileMode(0o644)
		if strings.HasPrefix(rel, "scripts"+string(os.PathSeparator)) {
			mode = 0o755
		}
		return os.WriteFile(filepath.Join(dest, rel), []byte(content), mode)
	})
}

func renderSkill(source string, identity config.Identity) string {
	replacer := strings.NewReplacer(
		commandPlaceholder, identity.BinaryName,
		channelDirectoryPlaceholder, identity.ProjectDirName,
		channelOnboardingPlaceholder, channelOnboarding(identity),
		nightlyMigrationPlaceholder, nightlyMigration(identity),
	)
	return replacer.Replace(source)
}

func channelOnboarding(identity config.Identity) string {
	prerequisites := universalPrerequisites(identity.BinaryName)
	if identity.Channel == "nightly" {
		prerequisites = nightlyPrerequisites(identity.BinaryName)
		return fmt.Sprintf(nightlyOnboardingTemplate, prerequisites, identity.BinaryName, skillName, identity.BinaryName, identity.BinaryName, identity.BinaryName)
	}

	return fmt.Sprintf(standardOnboardingTemplate, identity.Channel, prerequisites, identity.BinaryName, skillName, identity.BinaryName, identity.BinaryName, identity.BinaryName)
}

func universalPrerequisites(binary string) string {
	return fmt.Sprintf(universalPrerequisitesTemplate, binary)
}

func nightlyPrerequisites(binary string) string {
	return fmt.Sprintf(nightlyPrerequisitesTemplate, binary)
}

func nightlyMigration(identity config.Identity) string {
	if identity.Channel != "nightly" {
		return ""
	}
	return "Nightly keeps its own ports, launch agents, project registrations, sidings, and data baselines. When a project already uses dev, run `" + identity.BinaryName + " app add` from the original checkout for the nightly channel and create a fresh `" + identity.BinaryName + " new <name>` siding. Do not copy `.shunt-dev`, reuse a dev siding, or migrate dev data."
}
