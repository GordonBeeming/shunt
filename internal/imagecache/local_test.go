package imagecache

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractOCIArchiveRejectsUnsafeEntries(t *testing.T) {
	tests := []struct {
		name   string
		header tar.Header
	}{
		{name: "traversal", header: tar.Header{Name: "../outside", Typeflag: tar.TypeReg, Size: 1, Mode: 0o600}},
		{name: "absolute", header: tar.Header{Name: "/tmp/outside", Typeflag: tar.TypeReg, Size: 1, Mode: 0o600}},
		{name: "symlink", header: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../outside", Mode: 0o777}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			archivePath := filepath.Join(t.TempDir(), "image.tar")
			file, err := os.OpenFile(archivePath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			writer := tar.NewWriter(file)
			if err := writer.WriteHeader(&tc.header); err != nil {
				t.Fatal(err)
			}
			if tc.header.Size > 0 {
				if _, err := writer.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			err = extractOCIArchive(archivePath, t.TempDir())
			if err == nil || (!strings.Contains(err.Error(), "unsafe path") && !strings.Contains(err.Error(), "unsupported entry type")) {
				t.Fatalf("unsafe OCI archive error = %v", err)
			}
		})
	}
}
