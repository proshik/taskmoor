// Package spec embeds the taskmoor task format's schemas (format = 1).
//
// README.md beside this file is the format's definition; the schemas here are
// authoritative and are compiled into the binary so a reader validates exactly
// what the spec says.
package spec

import _ "embed"

// Format is the task format version this build reads and writes.
const Format = 1

// TaskSchema is task.schema.json: the front matter of a task file.
//
//go:embed task.schema.json
var TaskSchema []byte

// StateSchema is state.schema.json: the shared state file.
//
//go:embed state.schema.json
var StateSchema []byte
