package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html app.js lib.js style.css
var files embed.FS

func FS() fs.FS { return files }
