package cmd

import "testing"

func TestValidateBranchPrefix(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"gb/shunt/", true},  // trailing slash — the intended form
		{"gb/shunt-", true},  // trailing dash is also a valid separator
		{"", true},           // empty clears the override back to the default
		{"gb/shunt", false},  // no separator — would mash into gb/shuntmy-siding
		{"shunt", false},     // the default's stem without its slash
		{"gb/shunt_", false}, // underscore isn't one of the accepted separators
	}
	for _, c := range cases {
		err := validateBranchPrefix(c.in)
		if (err == nil) != c.ok {
			t.Errorf("validateBranchPrefix(%q): got err=%v, want ok=%v", c.in, err, c.ok)
		}
	}
}
