package runner

import "testing"

func TestSkipDirIncludesEveryShuntChannel(t *testing.T) {
	for _, name := range []string{".shunt", ".shunt-beta", ".shunt-nightly", ".shunt-dev"} {
		if !skipDir(name) {
			t.Errorf("skipDir(%q) = false, want true", name)
		}
	}
}
