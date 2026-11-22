// Package text makes repository text safe to print to a terminal: task
// titles and bodies come from files anyone with push access can write.
package text

import (
	"strings"
	"unicode"
)

// Line replaces every control character (C0, DEL, C1, including ESC and
// newlines) with U+FFFD, for one-line fields such as titles.
func Line(s string) string { return clean(s, false) }

// Block is Line but keeps newlines and tabs, for a task body; a CRLF line
// end becomes a newline.
func Block(s string) string { return clean(strings.ReplaceAll(s, "\r\n", "\n"), true) }

func clean(s string, keepLines bool) string {
	if strings.IndexFunc(s, func(r rune) bool { return unsafe(r, keepLines) }) < 0 {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if unsafe(r, keepLines) {
			b.WriteRune('�')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func unsafe(r rune, keepLines bool) bool {
	if keepLines && (r == '\n' || r == '\t') {
		return false
	}
	return unicode.IsControl(r)
}
