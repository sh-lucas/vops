package daemon

import (
	"io/fs"
	"net/http"

	"github.com/sh-lucas/vops/internal/ui"
)

func staticUI() http.Handler {
	sub, _ := fs.Sub(ui.Files, "static")
	return http.FileServerFS(sub)
}
