package siding

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/imagecache"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestLoadWarmLoadsOnlyPlannedImagesThenInspectsAndRecords(t *testing.T) {
	app := state.App{ConfigDir: t.TempDir(), PrebakeImages: []string{"db:latest", "web:latest"}}
	if err := os.MkdirAll(WarmTarPath(app), 0o700); err != nil {
		t.Fatal(err)
	}
	sd := state.Siding{Container: "guest"}
	originalPlan, originalExec, originalStdin, originalStdinDigest := planCachedImages, execGuest, execGuestStdinFile, execGuestStdinFileDigest
	defer func() {
		planCachedImages, execGuest, execGuestStdinFile, execGuestStdinFileDigest = originalPlan, originalExec, originalStdin, originalStdinDigest
	}()

	var events []string
	var writtenMarker imagecache.GuestMarker
	refs, err := configuredImageRefs(app)
	if err != nil {
		t.Fatal(err)
	}
	dbRef, webRef := refs[0], refs[1]
	planCachedImages = func(_ context.Context, _ string, guest imagecache.GuestState) (imagecache.LoadPlan, error) {
		if guest.Marker.Generation != "old" || guest.Marker.Images["db:latest"] != "sha256:db" {
			t.Fatalf("guest marker = %#v", guest)
		}
		if guest.ImageIDs[dbRef] != "sha256:db" || guest.ImageIDs[webRef] != "sha256:web" {
			t.Fatalf("guest image IDs = %#v", guest.ImageIDs)
		}
		return imagecache.LoadPlan{
			Generation: "new",
			Images:     []imagecache.PlannedImage{{Ref: webRef, Path: filepath.Join(t.TempDir(), "web.tar"), Checksum: "sha256:web"}},
			Marker: imagecache.GuestMarker{
				Generation: "new",
				Images: map[string]string{
					dbRef: "sha256:cache-db", webRef: "sha256:cache-web",
				},
				Digests: map[string]string{
					dbRef: "sha256:cache-db-digest", webRef: "sha256:cache-web-digest",
				},
			},
		}, nil
	}
	execGuest = func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "sh" {
			return `{"generation":"old","images":{"db:latest":"sha256:db"}}`, nil
		}
		if reflect.DeepEqual(args, []string{"docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}"}) {
			return "db:latest\nweb:latest\n", nil
		}
		events = append(events, "inspect")
		want := []string{"docker", "image", "inspect", "--format", "{{.Id}}", dbRef, webRef}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("inspect args = %#v, want %#v", args, want)
		}
		return "sha256:db\nsha256:web\n", nil
	}
	execGuestStdinFile = func(_ context.Context, _ string, path string, args ...string) error {
		if len(args) > 0 && args[0] == "docker" {
			events = append(events, "load:"+filepath.Base(path))
		} else {
			events = append(events, "marker")
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(data, &writtenMarker); err != nil {
				return err
			}
		}
		return nil
	}
	execGuestStdinFileDigest = func(_ context.Context, _ string, path string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "docker" {
			events = append(events, "load:"+filepath.Base(path))
			return "sha256:web", nil
		}
		return "", execGuestStdinFile(context.Background(), "", path, args...)
	}

	loaded, err := LoadWarm(context.Background(), app, sd)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded || !reflect.DeepEqual(events, []string{"inspect", "load:web.tar", "inspect", "marker"}) {
		t.Fatalf("LoadWarm() loaded=%v events=%v", loaded, events)
	}
	if writtenMarker.Generation != "new" || writtenMarker.Images[dbRef] != "sha256:db" || writtenMarker.Images[webRef] != "sha256:web" {
		t.Fatalf("written marker = %#v", writtenMarker)
	}
	if writtenMarker.Digests[dbRef] != "sha256:cache-db-digest" || writtenMarker.Digests[webRef] != "sha256:cache-web-digest" {
		t.Fatalf("written marker digests = %#v", writtenMarker.Digests)
	}
}

func TestLoadWarmDoesNotRecordMarkerAfterInspectionFailure(t *testing.T) {
	app := state.App{ConfigDir: t.TempDir(), PrebakeImages: []string{"db:latest"}}
	if err := os.MkdirAll(WarmTarPath(app), 0o700); err != nil {
		t.Fatal(err)
	}
	originalPlan, originalExec, originalStdin, originalStdinDigest := planCachedImages, execGuest, execGuestStdinFile, execGuestStdinFileDigest
	defer func() {
		planCachedImages, execGuest, execGuestStdinFile, execGuestStdinFileDigest = originalPlan, originalExec, originalStdin, originalStdinDigest
	}()
	refs, err := configuredImageRefs(app)
	if err != nil {
		t.Fatal(err)
	}
	dbRef := refs[0]
	planCachedImages = func(context.Context, string, imagecache.GuestState) (imagecache.LoadPlan, error) {
		return imagecache.LoadPlan{
			Generation: "same",
			Marker:     imagecache.GuestMarker{Generation: "same", Images: map[string]string{dbRef: "sha256:db"}},
		}, nil
	}
	inspectCalls := 0
	execGuest = func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "sh" {
			return `{"generation":"same","images":{"db:latest":"sha256:db"}}`, nil
		}
		if reflect.DeepEqual(args, []string{"docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}"}) {
			return "db:latest\n", nil
		}
		inspectCalls++
		if inspectCalls == 1 {
			return "sha256:db\n", nil
		}
		return "", errors.New("inspect failed")
	}
	markerWritten := false
	execGuestStdinFile = func(context.Context, string, string, ...string) error {
		markerWritten = true
		return nil
	}

	loaded, err := LoadWarm(context.Background(), app, state.Siding{Container: "guest"})
	if err == nil || loaded {
		t.Fatalf("LoadWarm() = %v, %v; want inspection error", loaded, err)
	}
	if markerWritten {
		t.Fatal("guest marker was recorded after failed image inspection")
	}
}

func TestLoadWarmDoesNotRecordMarkerAfterStreamChecksumMismatch(t *testing.T) {
	app := state.App{ConfigDir: t.TempDir(), PrebakeImages: []string{"db:latest"}}
	if err := os.MkdirAll(WarmTarPath(app), 0o700); err != nil {
		t.Fatal(err)
	}
	originalPlan, originalExec, originalStdin, originalStdinDigest := planCachedImages, execGuest, execGuestStdinFile, execGuestStdinFileDigest
	defer func() {
		planCachedImages, execGuest, execGuestStdinFile, execGuestStdinFileDigest = originalPlan, originalExec, originalStdin, originalStdinDigest
	}()
	refs, err := configuredImageRefs(app)
	if err != nil {
		t.Fatal(err)
	}
	planCachedImages = func(context.Context, string, imagecache.GuestState) (imagecache.LoadPlan, error) {
		return imagecache.LoadPlan{
			Generation: "new",
			Images:     []imagecache.PlannedImage{{Ref: refs[0], Path: filepath.Join(t.TempDir(), "db.tar"), Checksum: "sha256:trusted"}},
			Marker:     imagecache.GuestMarker{Generation: "new", Images: map[string]string{refs[0]: "sha256:db"}},
		}, nil
	}
	execGuest = func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "sh" {
			return "", nil
		}
		if reflect.DeepEqual(args, []string{"docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}"}) {
			return "", nil
		}
		return "", errors.New("inspect must not run after checksum mismatch")
	}
	execGuestStdinFileDigest = func(context.Context, string, string, ...string) (string, error) {
		return "sha256:tampered", nil
	}
	markerWritten := false
	execGuestStdinFile = func(context.Context, string, string, ...string) error {
		markerWritten = true
		return nil
	}

	loaded, err := LoadWarm(context.Background(), app, state.Siding{Container: "guest"})
	if err == nil || loaded || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("LoadWarm() = %v, %v; want checksum error", loaded, err)
	}
	if markerWritten {
		t.Fatal("guest marker was recorded after stream checksum mismatch")
	}
}

func TestLocalBuildSourcesMapsTypedContractWithoutAliasingArgs(t *testing.T) {
	app := state.App{PrebakeBuilds: []state.PrebakeBuild{{
		Image:      "example/db:local",
		Context:    "/repo/containers/db",
		Dockerfile: "/repo/containers/Database.Dockerfile",
		Platform:   "linux/amd64",
		BuildArgs:  map[string]string{"EDITION": "dev"},
	}}}
	sources := localBuildSources(app)
	if len(sources) != 1 || sources[0].Ref != app.PrebakeBuilds[0].Image || sources[0].ContextDir != app.PrebakeBuilds[0].Context || sources[0].Dockerfile != app.PrebakeBuilds[0].Dockerfile || sources[0].Platform != "linux/amd64" {
		t.Fatalf("localBuildSources() = %#v", sources)
	}
	sources[0].BuildArgs["EDITION"] = "changed"
	if app.PrebakeBuilds[0].BuildArgs["EDITION"] != "dev" {
		t.Fatal("localBuildSources aliased persisted build arguments")
	}
}

func TestLoadWarmIgnoresOldCacheWhenNoImagesAreConfigured(t *testing.T) {
	originalExec := execGuest
	defer func() { execGuest = originalExec }()
	called := false
	execGuest = func(context.Context, string, ...string) (string, error) {
		called = true
		return "", nil
	}
	app := state.App{ConfigDir: t.TempDir()}
	if err := os.MkdirAll(WarmTarPath(app), 0o700); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadWarm(context.Background(), app, state.Siding{Container: "guest"})
	if err != nil || loaded || called {
		t.Fatalf("LoadWarm() = loaded %v, called %v, error %v", loaded, called, err)
	}
}
