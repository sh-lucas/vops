// Package ui holds the embedded dashboard (plain html, css and js).
package ui

import "embed"

//go:embed static
var Files embed.FS
