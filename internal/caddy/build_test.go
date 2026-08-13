package caddy

import (
	"reflect"
	"testing"
)

func TestXcaddyBuildArgsPinEveryModule(t *testing.T) {
	bin := "/tmp/shunt"
	want := []string{"build", "v2.11.4", "--output", bin, "--with", "github.com/mholt/caddy-l4@v0.1.2"}
	if got := xcaddyBuildArgs(bin); !reflect.DeepEqual(got, want) {
		t.Fatalf("xcaddy build args = %q, want %q", got, want)
	}
}
