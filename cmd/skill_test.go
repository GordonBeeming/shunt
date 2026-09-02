package cmd

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/skillfiles"
)

func TestRenderedStatusScriptReportsWorktreeOnlyState(t *testing.T) {
	identity := config.Identity{Channel: "dev", BinaryName: "shunt-dev", ProjectDirName: ".shunt-dev"}
	dest := t.TempDir()
	if err := writeSkill(dest, identity); err != nil {
		t.Fatal(err)
	}
	fakeBinary := filepath.Join(t.TempDir(), "shunt-dev")
	fake := `#!/bin/sh
printf '%s\n' '{"active":false,"managed":true,"registered":false,"project":"sample","sidings":[{"name":"shell","live":false,"appRunning":false,"guestRunning":false,"src":"/work/shell","ip":"","dashboard":""}]}'
`
	if err := os.WriteFile(fakeBinary, []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(dest, "scripts", "status.sh"))
	cmd.Env = append(os.Environ(), "SHUNT_BIN="+fakeBinary)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status script: %v\n%s", err, out)
	}
	text := string(out)
	for _, want := range []string{
		"shunt worktree-only project: sample",
		"guest runtime is not registered",
		"shell",
		"edit: /work/shell",
		"next: edit and test in this worktree",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("status output omits %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "next: shunt-dev up shell") {
		t.Fatalf("worktree-only status suggests unavailable guest operation:\n%s", text)
	}
}

func TestRenderedStatusScriptListsWaitingOnRoutesForAGuestUpButNotServingSiding(t *testing.T) {
	identity := config.Identity{Channel: "dev", BinaryName: "shunt-dev", ProjectDirName: ".shunt-dev"}
	dest := t.TempDir()
	if err := writeSkill(dest, identity); err != nil {
		t.Fatal(err)
	}
	fakeBinary := filepath.Join(t.TempDir(), "shunt-dev")
	fake := `#!/bin/sh
printf '%s\n' '{"active":true,"managed":true,"registered":true,"project":"sample","sidings":[{"name":"one","live":false,"appRunning":false,"guestRunning":true,"src":"/work/one","ip":"10.0.0.2","dashboard":"","routes":[{"key":"storefront","guestPort":5100,"listening":false},{"key":"api","guestPort":7219,"listening":false},{"key":"gateway","guestPort":7022,"listening":true},{"key":"webapp","guestPort":5173,"optional":true,"listening":false}]}]}'
`
	if err := os.WriteFile(fakeBinary, []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(dest, "scripts", "status.sh"))
	cmd.Env = append(os.Environ(), "SHUNT_BIN="+fakeBinary)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status script: %v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "waiting on: storefront(5100), api(7219)") {
		t.Fatalf("status output omits the routes still down:\n%s", text)
	}
	for _, unwanted := range []string{"gateway(7022)", "webapp(5173)"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("status output lists a listening or optional route as waiting-on (%q):\n%s", unwanted, text)
		}
	}
}

func TestRenderedStatusScriptPrintsProbeErrorInsteadOfWaitingOnRoutes(t *testing.T) {
	identity := config.Identity{Channel: "dev", BinaryName: "shunt-dev", ProjectDirName: ".shunt-dev"}
	dest := t.TempDir()
	if err := writeSkill(dest, identity); err != nil {
		t.Fatal(err)
	}
	fakeBinary := filepath.Join(t.TempDir(), "shunt-dev")
	fake := `#!/bin/sh
printf '%s\n' '{"active":true,"managed":true,"registered":true,"project":"sample","sidings":[{"name":"one","live":false,"appRunning":false,"guestRunning":true,"src":"/work/one","ip":"10.0.0.2","dashboard":"","probeError":"probe routes for \"one\": exec into guest did not answer within 5s"}]}'
`
	if err := os.WriteFile(fakeBinary, []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(dest, "scripts", "status.sh"))
	cmd.Env = append(os.Environ(), "SHUNT_BIN="+fakeBinary)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status script: %v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "probe error: probe routes for \"one\": exec into guest did not answer within 5s") {
		t.Fatalf("status output omits the probe error:\n%s", text)
	}
	if strings.Contains(text, "waiting on:") {
		t.Fatalf("status output printed waiting-on routes alongside a probe error:\n%s", text)
	}
}

func TestRenderedStatusScriptAcceptsLegacyRegisteredJSON(t *testing.T) {
	identity := config.Identity{Channel: "dev", BinaryName: "shunt-dev", ProjectDirName: ".shunt-dev"}
	dest := t.TempDir()
	if err := writeSkill(dest, identity); err != nil {
		t.Fatal(err)
	}
	fakeBinary := filepath.Join(t.TempDir(), "shunt-dev")
	fake := `#!/bin/sh
printf '%s\n' '{"active":true,"project":"sample","sidings":[]}'
`
	if err := os.WriteFile(fakeBinary, []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(dest, "scripts", "status.sh"))
	cmd.Env = append(os.Environ(), "SHUNT_BIN="+fakeBinary)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status script: %v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "shunt app: sample") || !strings.Contains(text, "no sidings yet") {
		t.Fatalf("legacy registered JSON was not recognized:\n%s", text)
	}
}

func TestRenderedStatusScriptPropagatesActiveDiscoveryFailure(t *testing.T) {
	identity := config.Identity{Channel: "dev", BinaryName: "shunt-dev", ProjectDirName: ".shunt-dev"}
	dest := t.TempDir()
	if err := writeSkill(dest, identity); err != nil {
		t.Fatal(err)
	}
	fakeBinary := filepath.Join(t.TempDir(), "shunt-dev")
	fake := `#!/bin/sh
echo 'load Shunt state: unsupported state version' >&2
exit 23
`
	if err := os.WriteFile(fakeBinary, []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(dest, "scripts", "status.sh"))
	cmd.Env = append(os.Environ(), "SHUNT_BIN="+fakeBinary)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("status script succeeded after active discovery failure:\n%s", out)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 23 {
		t.Fatalf("status script error = %v, want active exit 23:\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "unsupported state version") {
		t.Fatalf("status script hid active diagnostic:\n%s", text)
	}
	if strings.Contains(text, "No Shunt state here") {
		t.Fatalf("status script misreported discovery failure as unmanaged:\n%s", text)
	}
}

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
			heading := universalPrerequisitesHeading
			if identity.Channel == "nightly" {
				heading = "## Nightly host prerequisites"
			}
			if !strings.Contains(text, heading) {
				t.Errorf("%s onboarding omits universal prerequisites", identity.Channel)
			}
			if !strings.Contains(text, identity.BinaryName+" init") {
				t.Errorf("%s onboarding omits its init command", identity.Channel)
			}
		})
	}
}

func TestNightlyOnboardingOperationalTokensStayInSync(t *testing.T) {
	root := repositoryRoot(t)
	surfaces := []string{
		filepath.Join(root, "README.md"),
		filepath.Join(root, "CLAUDE.md"),
		filepath.Join(root, "cmd", "skill.go"),
		filepath.Join(root, "packaging", "homebrew", "shunt-nightly.rb.tmpl"),
	}
	for _, surface := range surfaces {
		content, err := os.ReadFile(surface)
		if err != nil {
			t.Fatal(err)
		}
		text := string(content)
		for _, token := range nightlyOperationalTokens {
			if !strings.Contains(text, token) {
				t.Errorf("%s omits nightly onboarding token %q", surface, token)
			}
		}
		if strings.Contains(text, "GOPATH_BIN") {
			t.Errorf("%s retains the ambient GOPATH bin variable", surface)
		}
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
	if identity.Channel == "nightly" {
		for _, phrase := range append([]string{"## Nightly host prerequisites"}, nightlyOperationalTokens...) {
			if !strings.Contains(string(skill), phrase) {
				t.Errorf("nightly skill omits %q", phrase)
			}
		}
		if !strings.Contains(string(skill), "Go 1.25.13 or a later patch on the 1.25 line") {
			t.Error("nightly skill omits the supported Go patch policy")
		}
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
	heading := universalPrerequisitesHeading
	if identity.Channel == "nightly" {
		heading = "## Nightly host prerequisites"
	}
	headingIndex := strings.Index(text, heading)
	if headingIndex < 0 {
		t.Errorf("%s omits prerequisites for %s", file, identity.Channel)
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
	"go@1.25",
	"XCADDY_BIN=",
	`GOBIN="$XCADDY_BIN"`,
	"gordonbeeming/tap/shunt-nightly",
	"Nightly keeps its own ports",
	"Do not copy `.shunt-dev`",
}

var nightlyOperationalTokens = []string{
	`GO_BIN="$(brew --prefix go@1.25)/bin/go"`,
	`XCADDY_BIN="$("$GO_BIN" env GOPATH | cut -d: -f1)/bin"`,
	`GOBIN="$XCADDY_BIN" "$GO_BIN" install github.com/caddyserver/xcaddy/cmd/xcaddy@v0.4.6`,
	`export PATH="$(brew --prefix go@1.25)/bin:$XCADDY_BIN:$PATH"`,
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(filename))
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
