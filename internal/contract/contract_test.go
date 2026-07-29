package contract

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
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
		"one character":    `"dataVolumes":["x"]`,
		"colon":            `"dataVolumes":["pg:data"]`,
		"space":            `"dataVolumes":["pg data"]`,
		"leading hyphen":   `"dataVolumes":["-pg"]`,
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

func TestDockerVolumeNameGrammar(t *testing.T) {
	valid := []string{"a1", "A_", "pg-data", "azurite_data.1"}
	for _, volume := range valid {
		if err := ValidateVolumeName(volume); err != nil {
			t.Errorf("ValidateVolumeName(%q) error = %v", volume, err)
		}
	}
	invalid := []string{"", "a", ".", "..", "-a", "_a", "pg:data", "pg data", "pg/data", `pg\data`}
	for _, volume := range invalid {
		if err := ValidateVolumeName(volume); err == nil {
			t.Errorf("ValidateVolumeName(%q) succeeded", volume)
		}
	}
}

func TestPrebakeBuildResolvesRepositoryPaths(t *testing.T) {
	dir := t.TempDir()
	contextDir := filepath.Join(dir, "containers", "db")
	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dockerfile := filepath.Join(dir, "containers", "Database.Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := `{
      "runner":"node", "start":"npm start",
      "prebakeBuilds":[{
        "image":"example/db:local",
        "context":"containers/db",
        "dockerfile":"containers/Database.Dockerfile",
        "platform":"linux/amd64",
        "buildArgs":{"EDITION":"dev"}
      }],
      "frontDoor":[{"key":"web","kind":"http","listenPort":3000,"resource":"web","guestPort":3000}]
    }`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	contextDir, err = filepath.EvalSymlinks(contextDir)
	if err != nil {
		t.Fatal(err)
	}
	dockerfile, err = filepath.EvalSymlinks(dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	build := loaded.PrebakeBuilds[0]
	if build.Context != contextDir || build.Dockerfile != dockerfile || build.BuildArgs["EDITION"] != "dev" {
		t.Fatalf("resolved prebake build = %#v", build)
	}
}

func TestPrebakeBuildRejectsDuplicateRegistryOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := `{
      "runner":"node", "start":"npm start",
      "prebakeImages":["example/db:local"],
      "prebakeBuilds":[{"image":"example/db:local","context":".","dockerfile":"Dockerfile"}],
      "frontDoor":[{"key":"web","kind":"http","listenPort":3000,"resource":"web","guestPort":3000}]
    }`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "duplicate prebake image output") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestPrebakeBuildRejectsInvalidBuildArguments(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "blank key", key: "  ", value: "value"},
		{name: "delimiter in key", key: "BAD=KEY", value: "value"},
		{name: "newline in key", key: "BAD\nKEY", value: "value"},
		{name: "nul in value", key: "KEY", value: "bad\x00value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contract := Contract{
				Runner: "node",
				Start:  "npm start",
				PrebakeBuilds: []state.PrebakeBuild{{
					Image:      "example/build:local",
					Context:    ".",
					Dockerfile: "Dockerfile",
					BuildArgs:  map[string]string{test.key: test.value},
				}},
				FrontDoor: []FrontDoorRoute{{Key: "web", Kind: "http", ListenPort: 3000, Resource: "web", GuestPort: 3000}},
			}
			if err := contract.validate(dir); err == nil || !strings.Contains(err.Error(), "buildArgs") {
				t.Fatalf("validate() error = %v", err)
			}
		})
	}
}

func TestPrebakeBuildRejectsPathsOutsideRepository(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideDockerfile := filepath.Join(outside, "Dockerfile")
	if err := os.WriteFile(outsideDockerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		context    string
		dockerfile string
	}{
		{name: "relative traversal context", context: "../outside", dockerfile: "../outside/Dockerfile"},
		{name: "absolute context", context: outside, dockerfile: outsideDockerfile},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `{
          "runner":"node", "start":"npm start",
          "prebakeBuilds":[{"image":"example/db:local","context":` + strconv.Quote(test.context) + `,"dockerfile":` + strconv.Quote(test.dockerfile) + `}],
          "frontDoor":[{"key":"web","kind":"http","listenPort":3000,"resource":"web","guestPort":3000}]
        }`
			if err := os.WriteFile(filepath.Join(repo, FileName), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(repo); err == nil || !strings.Contains(err.Error(), "outside app repository") {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestPrebakeBuildRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "linked")); err != nil {
		t.Fatal(err)
	}
	body := `{
      "runner":"node", "start":"npm start",
      "prebakeBuilds":[{"image":"example/db:local","context":"linked","dockerfile":"linked/Dockerfile"}],
      "frontDoor":[{"key":"web","kind":"http","listenPort":3000,"resource":"web","guestPort":3000}]
    }`
	if err := os.WriteFile(filepath.Join(repo, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(repo); err == nil || !strings.Contains(err.Error(), "outside app repository") {
		t.Fatalf("Load() error = %v", err)
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
