package contract

import (
	"os"
	"path/filepath"
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

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"no apphost":      `{"frontDoor":[{"key":"a","kind":"http","listenPort":1,"resource":"r"}]}`,
		"no routes":       `{"apphost":"x"}`,
		"bad kind":        `{"apphost":"x","frontDoor":[{"key":"a","kind":"udp","listenPort":1,"resource":"r"}]}`,
		"dup key":         `{"apphost":"x","frontDoor":[{"key":"a","kind":"http","listenPort":1,"resource":"r"},{"key":"a","kind":"http","listenPort":2,"resource":"r"}]}`,
		"dup port":        `{"apphost":"x","frontDoor":[{"key":"a","kind":"http","listenPort":1,"resource":"r"},{"key":"b","kind":"http","listenPort":1,"resource":"r"}]}`,
		"missing resource": `{"apphost":"x","frontDoor":[{"key":"a","kind":"http","listenPort":1}]}`,
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
