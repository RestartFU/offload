// Package profiles embeds the default runner profiles that `offload runner install` writes.
package profiles

import "embed"

// Defaults holds the built-in profiles (*.toml in this directory).
//
//go:embed *.toml
var Defaults embed.FS
