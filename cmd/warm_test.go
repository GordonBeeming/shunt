package cmd

import (
	"strings"
	"testing"
)

func TestWarmHelpDescribesImmutableCacheAndGC(t *testing.T) {
	command := newWarmCmd()
	if !strings.Contains(command.Long, "immutable per-image cache") {
		t.Fatalf("warm help = %q", command.Long)
	}
	gc, _, err := command.Find([]string{"gc"})
	if err != nil || gc == command || gc.Name() != "gc" {
		t.Fatalf("warm gc command = %v, %v", gc, err)
	}
	for _, flag := range []string{"dry-run", "max-bytes"} {
		if gc.Flags().Lookup(flag) == nil {
			t.Fatalf("warm gc is missing --%s", flag)
		}
	}
}
