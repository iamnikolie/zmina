// Package ui holds the files `zmina ui` serves: one page (index.html, app.css, app.js; no build step) and its
// line art (drawn for zmina: generated, traced and recoloured to currentColor).
package ui

import "embed"

// Files is the page and its assets under static/.
//
//go:embed static
var Files embed.FS
