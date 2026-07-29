package imagecache

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
)

func TestAppleContainerLocalBuildBootstrap(t *testing.T) {
	if os.Getenv("SHUNT_CONTAINER_INTEGRATION") != "1" {
		t.Skip("set SHUNT_CONTAINER_INTEGRATION=1 to use the Apple container image store")
	}
	if err := exec.Command("container", "system", "status").Run(); err != nil {
		t.Skipf("Apple container runtime is unavailable: %v", err)
	}

	ref := fmt.Sprintf("shunt-cache-integration:%d", time.Now().UnixNano())
	canonicalRef, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("container", "image", "delete", canonicalRef.Name()).Run()
	})
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "Containerfile"), []byte("FROM scratch\nLABEL shunt.integration=local-cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := LocalBuildSource{
		Ref:        ref,
		ContextDir: contextDir,
		Dockerfile: "Containerfile",
		Platform:   "linux/arm64",
	}

	oldRunner := runContainer
	var calls atomic.Int32
	runContainer = func(ctx context.Context, args ...string) error {
		calls.Add(1)
		return oldRunner(ctx, args...)
	}
	t.Cleanup(func() { runContainer = oldRunner })
	path := filepath.Join(t.TempDir(), "images")
	changes, err := AssureSources(context.Background(), path, nil, []LocalBuildSource{source})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Action != "added" || calls.Load() != 3 {
		t.Fatalf("bootstrap calls=%d changes=%#v", calls.Load(), changes)
	}
	if err := Validate(path); err != nil {
		t.Fatalf("validate imported Apple container archive: %v", err)
	}

	calls.Store(0)
	changes, err = AssureSources(context.Background(), path, nil, []LocalBuildSource{source})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Action != "unchanged" || calls.Load() != 0 {
		t.Fatalf("second assure calls=%d changes=%#v", calls.Load(), changes)
	}
}
