// Package web embeds the built React dashboard (web/dist) into the binary so
// production runs without Node.js or any static file server: the Go process
// serves the API and the panel from the same origin.
//
// The embed requires web/dist to exist at compile time. The committed
// placeholder (index.html + .gitkeep) keeps `go build` working on machines
// without a Node toolchain; `make web` or the Docker build replace it with
// the real Vite output before compiling.
package web

import "embed"

//go:embed all:dist
var DistFS embed.FS
