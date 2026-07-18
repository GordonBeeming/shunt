package launchagent

import (
	"strings"
	"testing"
)

func TestRenderDashboardUsesServeMode(t *testing.T) {
	plist, err := renderDashboard("/tmp/shunt-dev")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plist, "<string>dashboard</string>\n    <string>--serve</string>") {
		t.Fatalf("dashboard plist does not use --serve mode:\n%s", plist)
	}
}
