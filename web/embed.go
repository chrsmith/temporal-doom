// Package web holds the browser client.
package web

import "embed"

//go:embed index.html style.css app.js engine.js audio.js
var FS embed.FS
