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
	if len(args) != 8 || args[len(args)-1] != version {
		t.Fatalf("capability argv = %#v", args)
	}
}
