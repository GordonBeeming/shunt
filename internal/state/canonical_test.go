package state

import "testing"

func TestCanonicalIn(t *testing.T) {
	projects := map[string]string{
		"MyApp":           "/repos/.shunt/MyApp",
		"SampleApp": "/repos/.shunt/SampleApp",
	}
	cases := []struct {
		in       string
		wantName string
		wantOK   bool
	}{
		{"MyApp", "MyApp", true},           // exact
		{"myApp", "MyApp", true},           // case-insensitive fold onto the registered casing
		{"MYAPP", "MyApp", true},           // any casing folds
		{"sampleapp", "SampleApp", true},
		{"NotRegistered", "", false},     // genuinely new project — no fold
	}
	for _, c := range cases {
		name, dir, ok := canonicalIn(projects, c.in)
		if ok != c.wantOK || name != c.wantName {
			t.Errorf("canonicalIn(%q) = (%q,%q,%v), want name=%q ok=%v", c.in, name, dir, ok, c.wantName, c.wantOK)
		}
		if ok && dir != projects[name] {
			t.Errorf("canonicalIn(%q) dir = %q, want %q", c.in, dir, projects[name])
		}
	}
}
