package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var assets embed.FS

func Handler() http.Handler {
	dist, _ := fs.Sub(assets, "dist")
	return http.FileServer(http.FS(dist))
}
