// Package web embeds the static front-end so the whole app ships in one binary.
package web

import "embed"

//go:embed index.html admin.html css/*.css js/*.js fonts/*.woff2 vendor/**/*
var FS embed.FS
