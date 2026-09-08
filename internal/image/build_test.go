package image

import (
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/config"
)

func TestBaseImageTagAndCapabilityAreContentVersioned(t *testing.T) {
	oldChannel := config.Channel
	t.Cleanup(func() { config.Channel = oldChannel })
	config.Channel = "dev"
	version := ContentVersion()
	if !strings.HasPrefix(version, "sha256:") || len(version) != len("sha256:")+64 {
		t.Fatalf("content version = %q", version)
	}
	wantTagPrefix := "shunt-base-dev:content-"
	if tag := Tag(); !strings.HasPrefix(tag, wantTagPrefix) || len(tag) != len(wantTagPrefix)+16 {
		t.Fatalf("tag = %q", tag)
	}
	args := GuestCapabilityCheck()
	if len(args) != 9 || args[len(args)-1] != version {
		t.Fatalf("capability argv = %#v", args)
	}
}

func TestGuestCapabilityCheckNamesEveryInstalledProgram(t *testing.T) {
	// The version marker proves which assets built the image, not that the build
	// installed what they describe. A relay binary went missing from a rebuilt
	// image while the marker stayed correct, and nothing noticed until someone
	// looked in /usr/local/bin by hand.
	args := GuestCapabilityCheck()
	joined := strings.Join(args, " ")
	for _, program := range []string{
		"/usr/local/bin/shunt-docker-api-admission",
		"/usr/local/bin/shunt-host-reach-relay",
	} {
		if !strings.Contains(joined, program) {
			t.Errorf("capability check does not prove %s is installed", program)
		}
	}
}

func TestContainerfileInstallsEveryProgramItBuilds(t *testing.T) {
	// Building a binary and never copying it out of the build stage is a silent
	// failure: the image builds, the version changes, and the program is absent.
	data, err := assets.ReadFile("assets/Containerfile")
	if err != nil {
		t.Fatal(err)
	}
	containerfile := string(data)
	for _, binary := range []string{"shunt-docker-api-admission", "shunt-host-reach-relay"} {
		if !strings.Contains(containerfile, "-o /out/"+binary) {
			t.Errorf("%s is not built", binary)
		}
		if !strings.Contains(containerfile, "COPY --from=shunt-tools /out/"+binary) {
			t.Errorf("%s is built but never copied into the image", binary)
		}
	}
}
