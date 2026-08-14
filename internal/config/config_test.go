package config

import (
	"reflect"
	"testing"
)

func TestKnownChannelsAndFallback(t *testing.T) {
	cases := map[string]Identity{
		"release":       {Channel: "release", BinaryName: "shunt", GlobalDirName: ".shunt", ProjectDirName: ".shunt", AdminPort: 2019, DashboardPort: 2020, PortOffset: 0, LaunchAgentID: "com.gordonbeeming.shunt.caddy", DashboardLaunchAgentID: "com.gordonbeeming.shunt.dashboard", ContainerPrefix: "shunt"},
		"beta":          {Channel: "beta", BinaryName: "shunt-beta", GlobalDirName: ".shunt-beta", ProjectDirName: ".shunt-beta", AdminPort: 2119, DashboardPort: 2120, PortOffset: 100, LaunchAgentID: "com.gordonbeeming.shunt-beta.caddy", DashboardLaunchAgentID: "com.gordonbeeming.shunt-beta.dashboard", ContainerPrefix: "shuntbeta"},
		"nightly":       {Channel: "nightly", BinaryName: "shunt-nightly", GlobalDirName: ".shunt-nightly", ProjectDirName: ".shunt-nightly", AdminPort: 2319, DashboardPort: 2320, PortOffset: 300, LaunchAgentID: "com.gordonbeeming.shunt-nightly.caddy", DashboardLaunchAgentID: "com.gordonbeeming.shunt-nightly.dashboard", ContainerPrefix: "shuntnightly"},
		"dev":           {Channel: "dev", BinaryName: "shunt-dev", GlobalDirName: ".shunt-dev", ProjectDirName: ".shunt-dev", AdminPort: 2219, DashboardPort: 2220, PortOffset: 200, LaunchAgentID: "com.gordonbeeming.shunt-dev.caddy", DashboardLaunchAgentID: "com.gordonbeeming.shunt-dev.dashboard", ContainerPrefix: "shuntdev"},
		"totally-bogus": {Channel: "dev", BinaryName: "shunt-dev", GlobalDirName: ".shunt-dev", ProjectDirName: ".shunt-dev", AdminPort: 2219, DashboardPort: 2220, PortOffset: 200, LaunchAgentID: "com.gordonbeeming.shunt-dev.caddy", DashboardLaunchAgentID: "com.gordonbeeming.shunt-dev.dashboard", ContainerPrefix: "shuntdev"},
	}
	for channel, want := range cases {
		if got := known(channel); !reflect.DeepEqual(got, want) {
			t.Errorf("known(%q) = %+v, want %+v", channel, got, want)
		}
	}
}

func TestBuildVersionDefaultsToSource(t *testing.T) {
	if BuildVersion != "source" {
		t.Fatalf("BuildVersion = %q, want source", BuildVersion)
	}
}

func TestContainerName(t *testing.T) {
	defer restoreChannel(Channel)
	Channel = "dev"
	if got, want := ContainerName("myapp", "exp1"), "shuntdev_myapp_exp1"; got != want {
		t.Errorf("ContainerName = %q, want %q", got, want)
	}
}

func TestProjectConfigDir(t *testing.T) {
	defer restoreChannel(Channel)
	Channel = "dev"
	got, err := ProjectConfigDir("/Users/x/repos/myapp")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/Users/x/repos/.shunt-dev/myapp"; got != want {
		t.Errorf("ProjectConfigDir = %q, want %q", got, want)
	}
}

func TestAdminAddrAndBaseImageTag(t *testing.T) {
	defer restoreChannel(Channel)
	Channel = "release"
	if got, want := AdminAddr(), "127.0.0.1:2019"; got != want {
		t.Errorf("AdminAddr = %q, want %q", got, want)
	}
	if got, want := BaseImageTag(), "shunt-base:latest"; got != want {
		t.Errorf("BaseImageTag(release) = %q, want %q", got, want)
	}
	Channel = "dev"
	if got, want := BaseImageTag(), "shunt-base-dev:latest"; got != want {
		t.Errorf("BaseImageTag(dev) = %q, want %q", got, want)
	}
	Channel = "nightly"
	if got, want := BaseImageTag(), "shunt-base-nightly:latest"; got != want {
		t.Errorf("BaseImageTag(nightly) = %q, want %q", got, want)
	}
}

func restoreChannel(prev string) { Channel = prev }
