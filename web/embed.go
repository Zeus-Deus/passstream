// Package web embeds the SPA; a fresh checkout serves a not-built placeholder.
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
