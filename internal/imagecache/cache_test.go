package imagecache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

func TestAssureExactHitDoesNotFetchAndRewritesExactSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images.tar")
	ref := mustRef(t, "example.test/one:latest")
	img := mustImage(t, 1)
	if err := write(path, map[string]cachedImage{ref.Name(): {ref: ref, logical: ref.Name(), img: img}}); err != nil {
		t.Fatal(err)
	}
	old := fetchImage
	t.Cleanup(func() { fetchImage = old })
	fetchImage = func(context.Context, name.Reference) (v1.Image, error) { t.Fatal("exact hit fetched"); return nil, nil }
	changes, err := Assure(context.Background(), path, []string{ref.Name()})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Action != "unchanged" {
		t.Fatalf("changes = %#v", changes)
	}
}

func TestAssureFetchesOnlyMissingAndRefreshesMutableTag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images.tar")
	one, two := mustRef(t, "example.test/one:latest"), mustRef(t, "example.test/two:latest")
	first, second := mustImage(t, 1), mustImage(t, 2)
	if err := write(path, map[string]cachedImage{one.Name(): {ref: one, logical: one.Name(), img: first}}); err != nil {
		t.Fatal(err)
	}
	old := fetchImage
	t.Cleanup(func() { fetchImage = old })
	calls := 0
	fetchImage = func(_ context.Context, r name.Reference) (v1.Image, error) {
		calls++
		if r.Name() == two.Name() {
			return second, nil
		}
		return second, nil
	}
	changes, err := Assure(context.Background(), path, []string{one.Name(), two.Name()})
	if err != nil || calls != 1 {
		t.Fatalf("assure: calls=%d err=%v", calls, err)
	}
	if changes[0].Action != "unchanged" || changes[1].Action != "added" {
		t.Fatalf("changes=%#v", changes)
	}
	changes, err = Refresh(context.Background(), path, []string{one.Name(), two.Name()})
	if err != nil || calls != 3 {
		t.Fatalf("refresh: calls=%d err=%v", calls, err)
	}
	if changes[0].Action != "updated" || changes[1].Action != "unchanged" {
		t.Fatalf("changes=%#v", changes)
	}
	if got, err := Read(path); err != nil || len(got) != 2 {
		t.Fatalf("archive = %d, %v", len(got), err)
	}
}

func TestFailurePreservesArchiveAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "images.tar")
	ref := mustRef(t, "example.test/one:latest")
	if err := write(path, map[string]cachedImage{ref.Name(): {ref: ref, logical: ref.Name(), img: mustImage(t, 1)}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := fetchImage
	t.Cleanup(func() { fetchImage = old })
	fetchImage = func(context.Context, name.Reference) (v1.Image, error) { return nil, errors.New("registry down") }
	if _, err := Refresh(context.Background(), path, []string{ref.Name()}); err == nil {
		t.Fatal("Refresh succeeded")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("existing archive changed")
	}
	files, _ := filepath.Glob(filepath.Join(dir, ".images-*.tar"))
	if len(files) != 0 {
		t.Fatalf("temporary files left: %v", files)
	}
}

func TestValidateRejectsInvalidAndCaptureIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "images.tar")
	ref := mustRef(t, "example.test/one:latest")
	if err := write(path, map[string]cachedImage{ref.Name(): {ref: ref, logical: ref.Name(), img: mustImage(t, 1)}}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := Capture(path, func(tmp string) error { return os.WriteFile(tmp, []byte("not a tar"), 0o644) }); err == nil {
		t.Fatal("invalid capture succeeded")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("invalid capture replaced archive")
	}
	if err := Validate(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "invalid.tar"), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Validate(filepath.Join(dir, "invalid.tar")); err == nil {
		t.Fatal("invalid archive validated")
	}
}

func TestFetchForPlatformFallsBackOnlyForMissingNativePlatform(t *testing.T) {
	ref := mustRef(t, "example.test/one:latest")
	img := mustImage(t, 1)
	for _, test := range []struct {
		name    string
		first   error
		calls   int
		wantErr bool
	}{
		{"missing arm64 falls back", errors.New("no child with platform {Architecture:arm64 OS:linux}"), 2, false},
		{"auth failure does not fall back", errors.New("unauthorized: password=secret"), 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var platforms []v1.Platform
			got, err := fetchForPlatform(context.Background(), ref, "arm64", func(_ context.Context, _ name.Reference, p v1.Platform) (v1.Image, error) {
				platforms = append(platforms, p)
				if len(platforms) == 1 && test.first != nil {
					return nil, test.first
				}
				return img, nil
			})
			if (err != nil) != test.wantErr || len(platforms) != test.calls {
				t.Fatalf("err=%v platforms=%v", err, platforms)
			}
			if !test.wantErr && got == nil {
				t.Fatal("fallback did not return image")
			}
			if test.calls == 2 && platforms[1].Architecture != "amd64" {
				t.Fatalf("fallback platform = %+v", platforms[1])
			}
		})
	}
}

func TestPinnedDigestRefRemainsFixed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images.tar")
	img := mustImage(t, 1)
	d := digest(img)
	ref, err := name.NewDigest("example.test/one@" + d)
	if err != nil {
		t.Fatal(err)
	}
	old := fetchImage
	t.Cleanup(func() { fetchImage = old })
	calls := 0
	fetchImage = func(context.Context, name.Reference) (v1.Image, error) { calls++; return img, nil }
	if _, err := Refresh(context.Background(), path, []string{ref.Name()}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache missing after refresh: %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[ref.Name()]; !ok {
		t.Fatalf("pinned ref %q not recovered from archive: %#v", ref.Name(), got)
	}
	if _, err := Assure(context.Background(), path, []string{ref.Name()}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("pinned exact hit fetched %d times", calls)
	}
}

func TestRedactDoesNotExposeCredentialValues(t *testing.T) {
	err := redact(errors.New("unauthorized password=hunter2 token: abc123 https://user:pass@example.test/v2"))
	if got := err.Error(); strings.Contains(got, "hunter2") || strings.Contains(got, "abc123") || strings.Contains(got, "pass@") {
		t.Fatalf("credential leaked: %s", got)
	}
}

func mustRef(t *testing.T, s string) name.Tag {
	t.Helper()
	r, err := name.NewTag(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func mustImage(t *testing.T, seed int64) v1.Image {
	t.Helper()
	i, err := random.Image(1024+seed, 1)
	if err != nil {
		t.Fatal(err)
	}
	return i
}
