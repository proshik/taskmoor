// Package finding is the shape of a problem found in a project: a file, a
// line when known, and a message.
package finding

import (
	"fmt"
	"sort"
)

// Severity of a finding.
type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
)

// Finding is one problem.
type Finding struct {
	Severity Severity `json:"severity"`
	// File is relative to the repository root for project files and absolute
	// for state files (they live in the git common directory).
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

// String renders `path:line: message` (the line only when known).
func (f Finding) String() string {
	switch {
	case f.File != "" && f.Line > 0:
		return fmt.Sprintf("%s:%d: %s: %s", f.File, f.Line, f.Severity, f.Message)
	case f.File != "":
		return fmt.Sprintf("%s: %s: %s", f.File, f.Severity, f.Message)
	default:
		return fmt.Sprintf("%s: %s", f.Severity, f.Message)
	}
}

// Errorf makes an error finding.
func Errorf(file string, line int, format string, args ...any) Finding {
	return Finding{Severity: Error, File: file, Line: line, Message: fmt.Sprintf(format, args...)}
}

// Warnf makes a warning finding.
func Warnf(file string, line int, format string, args ...any) Finding {
	return Finding{Severity: Warning, File: file, Line: line, Message: fmt.Sprintf(format, args...)}
}

// Sort orders findings by file, line, then message, keeping it stable.
func Sort(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Message < b.Message
	})
}

// Count returns the number of errors and warnings.
func Count(fs []Finding) (errors, warnings int) {
	for _, f := range fs {
		if f.Severity == Error {
			errors++
		} else {
			warnings++
		}
	}
	return
}
