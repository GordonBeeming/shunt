package config

import "testing"

func TestKnownChannelsAndFallback(t *testing.T) {
	cases := []struct {
		channel string
		bin     string
		dir     string
		admin   int
		offset  int
		prefix  string
	}{
		{"release", "shunt", ".shunt", 2019, 0, "shunt"},
		{"beta", "shunt-beta", ".shunt-beta", 2119, 100, "shuntbeta"},
		{"dev", "shunt-dev", ".shunt-dev", 2219, 200, "shuntdev"},
		{"totally-bogus", "shunt-dev", ".shunt-dev", 2219, 200, "shuntdev"}, // falls back to dev
	}
	for _, c := range cases {
		id := known(c.channel)
		if id.BinaryName != c.bin || id.GlobalDirName != c.dir || id.AdminPort != c.admin ||
			id.PortOffset != c.offset || id.ContainerPrefix != c.prefix {
			t.Errorf("known(%q) = %+v, want bin=%s dir=%s admin=%d offset=%d prefix=%s",
				c.channel, id, c.bin, c.dir, c.admin, c.offset, c.prefix)
		}
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
}

func restoreChannel(prev string) { Channel = prev }
