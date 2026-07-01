// Package skillfiles embeds the shunt agent skill so `shunt skill install` can
// deploy it to installed agents from the binary itself — no external files, and
// the repo copy under files/ is the single source of truth.
package skillfiles

import "embed"

//go:embed all:files
var FS embed.FS
