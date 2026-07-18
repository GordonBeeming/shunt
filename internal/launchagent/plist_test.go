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
	dashboardIndex := strings.Index(plist, "<string>dashboard</string>")
	serveIndex := strings.Index(plist, "<string>--serve</string>")
	if dashboardIndex == -1 || serveIndex <= dashboardIndex {
		t.Fatalf("dashboard plist does not use --serve mode:\n%s", plist)
	}
}
