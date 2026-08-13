package resolve

import (
	"testing"

	"github.com/gordonbeeming/shunt/internal/config"
)

func TestFromRepoDir(t *testing.T) {
	prev := config.Channel
	defer func() { config.Channel = prev }()
	config.Channel = "dev"

	loc, err := From("/Users/x/repos/myapp")
	if err != nil {
		t.Fatal(err)
	}
	if loc.Project != "myapp" {
		t.Errorf("Project = %q, want myapp", loc.Project)
	}
	if loc.ConfigDir != "/Users/x/repos/.shunt-dev/myapp" {
		t.Errorf("ConfigDir = %q", loc.ConfigDir)
	}
	if loc.Siding != "" {
		t.Errorf("Siding = %q, want empty", loc.Siding)
	}
}

func TestFromInsideSiding(t *testing.T) {
	prev := config.Channel
	defer func() { config.Channel = prev }()
	config.Channel = "dev"

	loc, err := From("/Users/x/repos/.shunt-dev/myapp/exp1/src")
	if err != nil {
		t.Fatal(err)
	}
	if loc.Project != "myapp" {
		t.Errorf("Project = %q, want myapp", loc.Project)
	}
	if loc.ConfigDir != "/Users/x/repos/.shunt-dev/myapp" {
		t.Errorf("ConfigDir = %q", loc.ConfigDir)
	}
	if loc.Siding != "exp1" {
		t.Errorf("Siding = %q, want exp1", loc.Siding)
	}
}

func TestFromReleaseChannelDirName(t *testing.T) {
	prev := config.Channel
	defer func() { config.Channel = prev }()
	config.Channel = "release"

	loc, err := From("/Users/x/repos/.shunt/myapp/exp2/src")
	if err != nil {
		t.Fatal(err)
	}
	if loc.Project != "myapp" || loc.Siding != "exp2" {
		t.Errorf("got project=%q siding=%q", loc.Project, loc.Siding)
	}
}

func TestFromNightlyChannelDirName(t *testing.T) {
	prev := config.Channel
	defer func() { config.Channel = prev }()
	config.Channel = "nightly"

	loc, err := From("/Users/x/repos/.shunt-nightly/myapp/exp3/src")
	if err != nil {
		t.Fatal(err)
	}
	if loc.Project != "myapp" || loc.ConfigDir != "/Users/x/repos/.shunt-nightly/myapp" || loc.Siding != "exp3" {
		t.Errorf("got project=%q configDir=%q siding=%q", loc.Project, loc.ConfigDir, loc.Siding)
	}
}
