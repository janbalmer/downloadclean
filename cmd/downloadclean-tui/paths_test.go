package main

import "testing"

func TestNormalizeDroppedPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"file_url_plain", "file:///home/jan/foo.dlc", "/home/jan/foo.dlc"},
		{"file_url_percent_space", "file:///home/jan/foo%20bar.dlc", "/home/jan/foo bar.dlc"},
		{"single_quoted_with_newline", "'/home/jan/foo bar.dlc'\n", "/home/jan/foo bar.dlc"},
		{"double_quoted", "\"/home/jan/quoted.dlc\"", "/home/jan/quoted.dlc"},
		{"shell_escaped_space", `/home/jan/foo\ bar.dlc`, "/home/jan/foo bar.dlc"},
		{"file_url_utf8_percent", "file:///home/jan/%C3%A4.dlc", "/home/jan/ä.dlc"},
		{"surrounding_spaces", "  /home/jan/spaced.dlc  ", "/home/jan/spaced.dlc"},
		{"empty", "", ""},
		{"whitespace_only", "   ", ""},
		{"bracketed_paste_markers", "\x1b[200~/home/jan/bracketed.dlc\x1b[201~", "/home/jan/bracketed.dlc"},
		{"file_url_with_host", "file://localhost/home/jan/host.dlc", "/home/jan/host.dlc"},
		{"already_clean", "/already/clean/path.dlc", "/already/clean/path.dlc"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeDroppedPath(tc.in)
			if got != tc.want {
				t.Errorf("normalizeDroppedPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
		t.Run(tc.name+"_idempotent", func(t *testing.T) {
			once := normalizeDroppedPath(tc.in)
			twice := normalizeDroppedPath(once)
			if once != twice {
				t.Errorf("not idempotent: f(%q)=%q, f(f(%q))=%q", tc.in, once, tc.in, twice)
			}
		})
	}
}
