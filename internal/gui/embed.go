package gui

import "embed"

// webFiles keeps the local console self-contained in the agent executable.
//
//go:embed web/index.html web/app.js web/style.css
var webFiles embed.FS
