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
	"github.com/gordonbeeming/shunt/internal/databaseline"
	"github.com/gordonbeeming/shunt/internal/image"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestAppleContainerDataPromotionLifecycle(t *testing.T) {
	if os.Getenv("SHUNT_CONTAINER_INTEGRATION") != "1" {
		t.Skip("set SHUNT_CONTAINER_INTEGRATION=1 to exercise data promotion in Apple container")
	}
	if err := exec.Command("container", "system", "status").Run(); err != nil {
		t.Skipf("Apple container runtime is unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	baseTag := image.Tag()
	baseExisted := image.Exists(ctx, baseTag)
	if err := image.EnsureBuilt(ctx, false); err != nil {
		t.Fatalf("build content-versioned base image: %v", err)
	}
	if !baseExisted {
		t.Cleanup(func() {
			output, err := exec.Command("container", "image", "delete", baseTag).CombinedOutput()
			if err != nil {
				t.Errorf("delete disposable base image %s: %v\n%s", baseTag, err, output)
			}
		})
	}

	configDir := t.TempDir()
	volume := fmt.Sprintf("shunt-data-integration-%d", time.Now().UnixNano())
	guest := fmt.Sprintf("shunt-data-integration-%d", time.Now().UnixNano())
	app := state.App{
		ConfigDir: configDir,
		Volumes:   []string{volume},
		Sidings:   map[string]state.Siding{},
	}
	sd := state.Siding{Name: "alpha", Container: guest}
	app.Sidings[sd.Name] = sd
	srcRoot, volumeRoot, err := Paths(app, sd.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(srcRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(volumeRoot, volume), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := container.Run(ctx, container.RunOpts{
		Name: guest, Image: baseTag, Init: true, CapAddAll: true, WritableProcSys: true,
		Mounts: []container.Mount{{Host: srcRoot, Guest: "/workspace"}, {Host: volumeRoot, Guest: "/mnt/dvol"}},
		Cmd:    []string{"sleep", "1200"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = container.Exec(context.Background(), guest, "sh", "-c", "docker rm -f shunt-data-consumer >/dev/null 2>&1 || true; sync")
		var output []byte
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			output, err = exec.Command("container", "delete", "--force", guest).CombinedOutput()
			if err == nil || strings.Contains(strings.ToLower(string(output)), "not found") {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Errorf("delete disposable guest %s: %v\n%s", guest, err, output)
	})
	waitForGuestDocker(t, ctx, guest)
	if err := CreateBindVolumes(ctx, app, sd); err != nil {
		t.Fatal(err)
	}
	buildDataConsumer(t, ctx, guest, volume)

	manager, err := databaseline.New(configDir, app.Volumes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.InitializeEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(volumeRoot, volume, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("promoted-one"), 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle := NewDataPromotionLifecycle(app, sd, os.Stdout)
	result, err := manager.PromoteWithLifecycle(ctx, sd.Name, lifecycle)
	if err != nil || !result.Committed || !result.Restore.Restored {
		t.Fatalf("first promotion result=%#v error=%v", result, err)
	}
	assertGuestConsumerRunning(t, ctx, guest)
	assertResetValue(t, ctx, manager, filepath.Join(configDir, "beta-vol"), volume, "promoted-one")

	if err := os.WriteFile(sentinel, []byte("promoted-two"), 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle = NewDataPromotionLifecycle(app, sd, os.Stdout)
	result, err = manager.PromoteWithLifecycle(ctx, sd.Name, lifecycle)
	if err != nil || !result.Committed || !result.Restore.Restored {
		t.Fatalf("second promotion result=%#v error=%v", result, err)
	}
	if result, err = manager.RollbackContext(ctx); err != nil || !result.Committed {
		t.Fatalf("rollback result=%#v error=%v", result, err)
	}
	assertResetValue(t, ctx, manager, filepath.Join(configDir, "gamma-vol"), volume, "promoted-one")
}

func waitForGuestDocker(t *testing.T, ctx context.Context, guest string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, err := container.Exec(ctx, guest, "sh", "-c", "docker info >/dev/null 2>&1"); err == nil {
			return
		}
		if time.Now().After(deadline) {
			logs, _ := container.Logs(ctx, guest)
			t.Fatalf("guest Docker did not become ready in 90 seconds\n%s", logs)
		}
		time.Sleep(time.Second)
	}
}

func buildDataConsumer(t *testing.T, ctx context.Context, guest, volume string) {
	t.Helper()
	script := fmt.Sprintf(`set -eu
build_dir=/tmp/shunt-data-consumer
mkdir -p "$build_dir"
cp /usr/local/bin/shunt-docker-api-admission "$build_dir/proof"
printf 'FROM scratch\nCOPY proof /proof\nENTRYPOINT ["/proof"]\n' > "$build_dir/Containerfile"
docker build -f "$build_dir/Containerfile" -t shunt-data-consumer:latest "$build_dir" >/dev/null
docker run --detach --name shunt-data-consumer --volume %s:/data shunt-data-consumer:latest --listen /tmp/proof.sock --backend /tmp/missing.sock >/dev/null
`, volume)
	if _, err := container.Exec(ctx, guest, "sh", "-c", script); err != nil {
		t.Fatal(err)
	}
}

func assertGuestConsumerRunning(t *testing.T, ctx context.Context, guest string) {
	t.Helper()
	out, err := container.Exec(ctx, guest, "docker", "inspect", "--format", "{{.State.Running}}", "shunt-data-consumer")
	if err != nil || strings.TrimSpace(out) != "true" {
		t.Fatalf("data consumer state=%q error=%v", out, err)
	}
}

func assertResetValue(t *testing.T, ctx context.Context, manager *databaseline.Manager, destination, volume, want string) {
	t.Helper()
	if _, err := manager.ResetVolumeRoot(ctx, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, volume, "sentinel.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("reset sentinel=%q want=%q", data, want)
	}
}
