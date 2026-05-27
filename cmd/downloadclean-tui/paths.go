package main

import (
	"net/url"
	"regexp"
	"strings"
)

var shellEscapeRE = regexp.MustCompile(`\\([ \t()\[\]'"])`)

// normalizeDroppedPath cleans a string a user typed into the picker, OR that
// the terminal pasted when they dragged a file onto the window. Different
// terminals encode the drop differently — some send file://-URL with percent
// escapes, some send a raw path, some wrap it in quotes, some leak bracketed-
// paste markers. The returned path is suitable to hand to os.Stat / os.Open.
// Returns "" if input is empty or whitespace-only after cleaning.
func normalizeDroppedPath(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\x1b[200~", "")
	s = strings.ReplaceAll(s, "\x1b[201~", "")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	if n := len(s); n >= 2 {
		first, last := s[0], s[n-1]
		if (first == '\'' && last == '\'') || (first == '"' && last == '"') {
			s = s[1 : n-1]
		}
	}

	if rest, ok := strings.CutPrefix(s, "file://"); ok {
		// Standard file URLs are file://host/path. After stripping file://
		// we either have /path (host empty) or host/path. In the latter
		// case, drop the host segment but keep the path's leading /.
		if !strings.HasPrefix(rest, "/") {
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				rest = rest[i:]
			}
		}
		s = rest
	}

	if strings.Contains(s, "%") {
		if unescaped, err := url.PathUnescape(s); err == nil {
			s = unescaped
		}
	}

	s = shellEscapeRE.ReplaceAllString(s, "$1")

	return s
}
