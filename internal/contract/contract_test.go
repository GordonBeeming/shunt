package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeContract(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadValid(t *testing.T) {
	dir := writeContract(t, `{
      "apphost": "src/App.AppHost/App.AppHost.csproj",
      "frontDoor": [
        { "key": "frontend", "kind": "http",   "listenPort": 5000, "resource": "web" },
        { "key": "db",       "kind": "layer4", "listenPort": 15432, "resource": "postgres", "endpoint": "tcp" }
      ],
      "dataVolumes": [ "pg-data" ]
    }`)
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.AppHost == "" || len(c.FrontDoor) != 2 || len(c.Volumes) != 1 {
		t.Fatalf("unexpected contract: %+v", c)
	}
	if c.FrontDoor[1].Kind != "layer4" || c.FrontDoor[1].ListenPort != 15432 {
		t.Errorf("db route parsed wrong: %+v", c.FrontDoor[1])
	}
}

func TestLoadMissing(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("expected error when contract file is absent")
	}
}

func TestNonAspireContractNeedsNoAppHost(t *testing.T) {
	// A dotnet/node/custom app declares runner + start instead of an apphost, so
	// validation must not demand apphost for them.
	dir := writeContract(t, `{
      "runner": "node",
      "start": "pnpm dev",
      "frontDoor": [
        { "key": "web", "kind": "http", "listenPort": 5173, "resource": "web", "guestPort": 5173 }
      ]
    }`)
	if _, err := Load(dir); err != nil {
		t.Fatalf("non-aspire contract without apphost should validate, got: %v", err)
	}
}

func TestDigestPinnedPrebakeImageRejected(t *testing.T) {
	dir := writeContract(t, `{
      "runner": "node", "start": "npm start",
      "prebakeImages": ["postgres@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"],
      "frontDoor": [{"key":"web","kind":"http","listenPort":3000,"resource":"web","guestPort":3000}]
    }`)
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "Docker save/load cannot recreate a runnable repo@digest alias") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestImageAndVolumeBoundaries(t *testing.T) {
	valid := `{
      "runner": "node", "start": "npm start",
		"prebakeImages": ["postgres:16", "mcr.microsoft.com/azure-storage/azurite:3.35.0", "alpine"],
      "dataVolumes": ["pg-data", "azurite_data.1"],
      "frontDoor": [{"key":"web","kind":"http","listenPort":3000,"resource":"web","guestPort":3000}]
    }`
	if _, err := Load(writeContract(t, valid)); err != nil {
		t.Fatalf("valid image and volume contract should load: %v", err)
	}

	cases := map[string]string{
		"empty image":      `"prebakeImages":[""]`,
		"invalid image":    `"prebakeImages":["not a valid image"]`,
		"digest image":     `"prebakeImages":["postgres@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]`,
		"duplicate image":  `"prebakeImages":["postgres:16","docker.io/library/postgres:16"]`,
		"empty volume":     `"dataVolumes":[""]`,
		"dot volume":       `"dataVolumes":["."]`,
		"dot-dot volume":   `"dataVolumes":[".."]`,
		"nested volume":    `"dataVolumes":["data/postgres"]`,
		"absolute volume":  `"dataVolumes":["/data/postgres"]`,
		"windows path":     `"dataVolumes":["data\\postgres"]`,
		"duplicate volume": `"dataVolumes":["pg-data","pg-data"]`,
	}
	for name, field := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{"runner":"node","start":"npm start",` + field + `,"frontDoor":[{"key":"web","kind":"http","listenPort":3000,"resource":"web","guestPort":3000}]}`
			if _, err := Load(writeContract(t, body)); err == nil {
				t.Fatalf("Load() accepted invalid %s contract", name)
			}
		})
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"no apphost":        `{"frontDoor":[{"key":"a","kind":"http","listenPort":1,"resource":"r"}]}`,
		"no routes":         `{"apphost":"x"}`,
		"bad kind":          `{"apphost":"x","frontDoor":[{"key":"a","kind":"udp","listenPort":1,"resource":"r"}]}`,
		"dup key":           `{"apphost":"x","frontDoor":[{"key":"a","kind":"http","listenPort":1,"resource":"r"},{"key":"a","kind":"http","listenPort":2,"resource":"r"}]}`,
		"dup port":          `{"apphost":"x","frontDoor":[{"key":"a","kind":"http","listenPort":1,"resource":"r"},{"key":"b","kind":"http","listenPort":1,"resource":"r"}]}`,
		"missing resource":  `{"apphost":"x","frontDoor":[{"key":"a","kind":"http","listenPort":1}]}`,
		"port out of range": `{"apphost":"x","frontDoor":[{"key":"a","kind":"http","listenPort":99999,"resource":"r"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeContract(t, body)); err == nil {
				t.Errorf("expected validation error for %q", name)
			}
		})
	}
}
