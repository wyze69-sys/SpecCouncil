package migrations

import "embed"

// FS embeds the complete migrations directory.
//
//go:embed *
var FS embed.FS
