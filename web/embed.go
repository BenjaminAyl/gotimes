// Package web embeds the single-page frontend.
package web

import "embed"

//go:embed index.html assets
var FS embed.FS
