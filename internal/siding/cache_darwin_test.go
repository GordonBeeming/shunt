package siding

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/image"
	"github.com/gordonbeeming/shunt/internal/imagecache"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestAppleContainerCachedLocalImageLoadUsesObservedGuestIdentity(t *testing.T) {
	if os.Getenv("SHUNT_CONTAINER_INTEGRATION") != "1" {
		t.Skip("set SHUNT_CONTAINER_INTEGRATION=1 to exercise cached local-image loading in Apple container")
	}
	if err := exec.Command("container", "system", "status").Run(); err != nil {
		t.Skipf("Apple container runtime is unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := image.EnsureBuilt(ctx, false); err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	buildDir := t.TempDir()
	dockerfile := filepath.Join(buildDir, "Containerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\nLABEL shunt.integration=observed-identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := fmt.Sprintf("shunt-observed-identity-%d:latest", time.Now().UnixNano())
	app := state.App{
		ConfigDir: configDir,
		PrebakeBuilds: []state.PrebakeBuild{{
			Image: ref,
		}},
	}
	if _, err := imagecache.RefreshSources(ctx, WarmTarPath(app), nil, []imagecache.LocalBuildSource{{
		Ref: ref, ContextDir: buildDir, Dockerfile: dockerfile, Platform: "linux/arm64",
	}}); err != nil {
		t.Fatal(err)
	}

	guest := fmt.Sprintf("shunt-observed-identity-%d", time.Now().UnixNano())
	sd := state.Siding{Name: "identity", Container: guest}
	if err := container.Run(ctx, container.RunOpts{
		Name: guest, Image: image.Tag(), Init: true, CapAddAll: true, WritableProcSys: true,
		Cmd: []string{"sleep", "600"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		output, err := exec.Command("container", "delete", "--force", guest).CombinedOutput()
		if err != nil && !strings.Contains(strings.ToLower(string(output)), "not found") {
			t.Errorf("delete disposable guest %s: %v\n%s", guest, err, output)
		}
	})
	waitForGuestDocker(t, ctx, guest)

	loaded, err := LoadWarm(ctx, app, sd)
	if err != nil || !loaded {
		t.Fatalf("first LoadWarm() = %v, %v", loaded, err)
	}
	loaded, err = LoadWarm(ctx, app, sd)
	if err != nil || loaded {
		t.Fatalf("second LoadWarm() = %v, %v; want verified no-op", loaded, err)
	}
	if _, err := container.Exec(ctx, guest, "docker", "image", "inspect", ref); err != nil {
		t.Fatalf("inspect loaded local image: %v", err)
	}
}
