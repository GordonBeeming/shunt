package caddy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

func TestXcaddyBuildArgsPinEveryModule(t *testing.T) {
	bin := "/tmp/shunt"
	want := []string{"build", "v2.11.4", "--output", bin, "--with", "github.com/mholt/caddy-l4@v0.1.2"}
	if got := xcaddyBuildArgs(bin); !reflect.DeepEqual(got, want) {
		t.Fatalf("xcaddy build args = %q, want %q", got, want)
	}
}

func TestPinnedCaddyDoesNotSupportForwardAuth(t *testing.T) {
	if caddyVersion != "v2.11.4" {
		t.Fatalf("Caddy version = %s, update this forward_auth boundary when changing the pin", caddyVersion)
	}
	if forwardAuthFixedInVersion != "v2.11.5" {
		t.Fatalf("forward_auth fix version = %s, want v2.11.5", forwardAuthFixedInVersion)
	}
	if forwardAuthSupported {
		t.Fatal("forward_auth must remain disabled for the pinned Caddy version")
	}
	_, generated, err := ServerForRoute("test", state.Route{
		Key:        "web",
		Kind:       state.KindHTTP,
		ListenPort: 5000,
		CaddyID:    "app_test_http_web",
	}, false)
	if err != nil {
		t.Fatalf("generate HTTP route: %v", err)
	}
	if strings.Contains(string(generated), "forward_auth") {
		t.Fatalf("generated HTTP route enables unsupported forward_auth: %s", generated)
	}
}
