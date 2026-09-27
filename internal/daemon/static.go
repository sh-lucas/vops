package daemon

import (
	"io/fs"
	"net/http"

	"github.com/sh-lucas/vops/internal/ui"
)

func staticUI() http.Handler {
	sub, _ := fs.Sub(ui.Files, "static")
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache") // embedded files have no mtime: without this an upgrade can show a stale dashboard
		files.ServeHTTP(w, r)
	})
}
