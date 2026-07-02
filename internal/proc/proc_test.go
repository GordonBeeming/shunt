package proc

import "testing"

func TestAugmentPath(t *testing.T) {
	exists := func(d string) bool { return d == "/usr/local/bin" || d == "/opt/homebrew/bin" }

	cases := []struct {
		desc string
		path string
		dirs []string
		want string
	}{
		{
			desc: "appends missing install dirs that exist",
			path: "/usr/bin:/bin",
			dirs: []string{"/usr/local/bin", "/opt/homebrew/bin"},
			want: "/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin",
		},
		{
			desc: "skips dirs already present",
			path: "/usr/local/bin:/usr/bin",
			dirs: []string{"/usr/local/bin", "/opt/homebrew/bin"},
			want: "/usr/local/bin:/usr/bin:/opt/homebrew/bin",
		},
		{
			desc: "skips dirs that don't exist",
			path: "/usr/bin",
			dirs: []string{"/nope/bin"},
			want: "/usr/bin",
		},
		{
			desc: "empty PATH gets no leading separator (no empty cwd element)",
			path: "",
			dirs: []string{"/usr/local/bin", "/opt/homebrew/bin"},
			want: "/usr/local/bin:/opt/homebrew/bin",
		},
		{
			desc: "no change when all present",
			path: "/usr/local/bin:/opt/homebrew/bin",
			dirs: []string{"/usr/local/bin", "/opt/homebrew/bin"},
			want: "/usr/local/bin:/opt/homebrew/bin",
		},
	}
	for _, c := range cases {
		if got := augmentPath(c.path, c.dirs, exists); got != c.want {
			t.Errorf("%s: augmentPath = %q, want %q", c.desc, got, c.want)
		}
	}
}
