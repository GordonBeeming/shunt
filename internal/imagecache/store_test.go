package imagecache

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLinkImmutableObjectReplacesSameSizeCorruptDestination(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "staged")
	destination := filepath.Join(dir, "immutable")
	contents := []byte("correct contents")
	if err := os.WriteFile(source, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("corrupt contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(contents)
	if err := linkImmutableObject(source, destination, int64(len(contents)), fmt.Sprintf("sha256:%x", sum)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(contents) {
		t.Fatalf("immutable destination = %q, want %q", got, contents)
	}
}
