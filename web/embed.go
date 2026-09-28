package web

import "embed"

//go:embed index.html app.js app.css
var FS embed.FS
