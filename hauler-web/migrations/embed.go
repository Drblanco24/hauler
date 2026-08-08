// Package migrations holds hauler-web's SQL schema migrations and embeds them
// into the binary.
//
// They live at the repository root rather than under internal/db so the schema
// is the first thing a reader finds, and so an operator can apply them with
// psql or their own tooling without extracting them from a Go package.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

// Dir is the path within FS that goose should scan.
const Dir = "."
