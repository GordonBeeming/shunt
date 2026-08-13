package cmd

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/skillfiles"
)

func TestWriteSkillRendersCompleteTreeForEachChannel(t *testing.T) {
	channels := []config.Identity{
		{Channel: "release", BinaryName: "shunt", ProjectDirName: ".shunt"},
		{Channel: "beta", BinaryName: "shunt-beta", ProjectDirName: ".shunt-beta"},
		{Channel: "nightly", BinaryName: "shunt-nightly", ProjectDirName: ".shunt-nightly"},
		{Channel: "dev", BinaryName: "shunt-dev", ProjectDirName: ".shunt-dev"},
	}

	for _, identity := range channels {
		t.Run(identity.Channel, func(t *testing.T) {
			dest := t.TempDir()
			if err := writeSkill(dest, identity); err != nil {
				t.Fatal(err)
			}
			assertRenderedSkillTree(t, dest, identity)
		})
	}
}

func TestEveryChannelUsesOneSharedSkillAndUniversalPrerequisites(t *testing.T) {
	if skillName != "shunt" {
		t.Fatalf("skillName = %q, want the intentional shared name %q", skillName, "shunt")
	}
	channels := []config.Identity{
		{Channel: "release", BinaryName: "shunt", ProjectDirName: ".shunt"},
		{Channel: "beta", BinaryName: "shunt-beta", ProjectDirName: ".shunt-beta"},
		{Channel: "nightly", BinaryName: "shunt-nightly", ProjectDirName: ".shunt-nightly"},
		{Channel: "dev", BinaryName: "shunt-dev", ProjectDirName: ".shunt-dev"},
	}
	for _, identity := range channels {
		t.Run(identity.Channel, func(t *testing.T) {
			text := channelOnboarding(identity)
			if !strings.Contains(text, universalPrerequisitesHeading) {
				t.Errorf("%s onboarding omits universal prerequisites", identity.Channel)
			}
			if !strings.Contains(text, identity.BinaryName+" init") {
				t.Errorf("%s onboarding omits its init command", identity.Channel)
			}
		})
	}
}

func assertRenderedSkillTree(t *testing.T, dest string, identity config.Identity) {
	t.Helper()
	err := fs.WalkDir(skillfiles.FS, "files", func(source string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("files", source)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		info, err := os.Stat(target)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasPrefix(rel, "scripts"+string(os.PathSeparator)) && info.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable: mode %v", rel, info.Mode())
		}
		content, err := os.ReadFile(target)
		if err != nil {
			return err
		}
		text := string(content)
		if strings.Contains(text, "{{shunt-") {
			t.Errorf("%s retains a shunt placeholder", rel)
		}
		if identity.Channel != "nightly" {
			for _, phrase := range nightlyOnlyGuidance {
				if strings.Contains(text, phrase) {
					t.Errorf("%s retains nightly-only guidance %q", rel, phrase)
				}
			}
		}
		if identity.Channel != "release" && bareShuntCommand.MatchString(text) {
			t.Errorf("%s retains bare release executable token", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	skill, err := os.ReadFile(filepath.Join(dest, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(skill), "**`"+identity.BinaryName+"`**") {
		t.Errorf("rendered skill does not identify %q", identity.BinaryName)
	}
	assertPrerequisitesBeforeOnboardingCommands(t, string(skill), identity, "SKILL.md")
	if identity.Channel == "nightly" && !strings.Contains(string(skill), ".shunt-dev") {
		t.Error("nightly skill does not preserve literal .shunt-dev migration text")
	}
	if identity.Channel == "nightly" && !strings.Contains(string(skill), "brew install gordonbeeming/tap/shunt-nightly") {
		t.Error("nightly skill does not include Homebrew onboarding")
	}
	if identity.Channel == "nightly" && !strings.Contains(string(skill), "macOS 26 or newer on Apple silicon") {
		t.Error("nightly skill does not include its supported host guidance")
	}

	commands, err := os.ReadFile(filepath.Join(dest, "references", "commands.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(commands), "`"+identity.BinaryName+" active [--json]`") {
		t.Errorf("rendered command reference does not use %q", identity.BinaryName)
	}
	assertPrerequisitesBeforeOnboardingCommands(t, string(commands), identity, "references/commands.md")

	iterating, err := os.ReadFile(filepath.Join(dest, "references", "iterating.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(iterating), "<repos>/"+identity.ProjectDirName+"/<project>/base/images/") {
		t.Errorf("rendered iteration reference does not use channel directory %q", identity.ProjectDirName)
	}
	assertRenderedSkillLinksResolve(t, dest)
}

func assertPrerequisitesBeforeOnboardingCommands(t *testing.T, text string, identity config.Identity, file string) {
	t.Helper()
	headingIndex := strings.Index(text, universalPrerequisitesHeading)
	if headingIndex < 0 {
		t.Errorf("%s omits universal prerequisites for %s", file, identity.Channel)
		return
	}
	for _, command := range []string{
		"    " + identity.BinaryName + " init",
		"    " + identity.BinaryName + " skill install --all",
		"    " + identity.BinaryName + " version",
	} {
		commandIndex := strings.Index(text, command)
		if commandIndex < 0 {
			t.Errorf("%s omits onboarding command %q for %s", file, strings.TrimSpace(command), identity.Channel)
			continue
		}
		if headingIndex > commandIndex {
			t.Errorf("%s renders universal prerequisites after %q for %s", file, strings.TrimSpace(command), identity.Channel)
		}
	}
}

var nightlyOnlyGuidance = []string{
	"macOS 26 or newer on Apple silicon",
	"brew ",
	"gordonbeeming/tap/shunt-nightly",
	"Nightly keeps its own ports",
	"Do not copy `.shunt-dev`",
}

var markdownLink = regexp.MustCompile(`\]\(([^)#]+)`) // local Markdown link target without its fragment

var bareShuntCommand = regexp.MustCompile("(?m)(?:^|[`(])shunt\\s+(?:active|app|base|cd|cert|cleanup|config|dashboard|data|git|init|kill|logs|new|park|playwright|reapply|restart|rm|run|skill|space|status|switch|sync|trim|up|version|warm)\\b")

func assertRenderedSkillLinksResolve(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range markdownLink.FindAllStringSubmatch(string(content), -1) {
			target := match[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "#") {
				continue
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), filepath.FromSlash(target))); err != nil {
				t.Errorf("%s links to missing %q: %v", path, target, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
