// Package webui mounts the embedded React dashboard as an SPA handler: real
// static files from web/dist are served verbatim, unknown extension-less GET
// requests fall back to index.html (so client-side routes survive a reload)
// and anything under /api/ is never swallowed (the API mux registers its
// routes before this handler).
package webui

import (
	"io/fs"
	"net/http"
	"strings"

	"carteiro/web"
)

// Handler returns the SPA handler over the embedded web/dist files.
func Handler() http.Handler {
	dist, err := fs.Sub(web.DistFS, "dist")
	if err != nil {
		panic("web embed: " + err.Error())
	}
	files := http.FileServer(http.FS(dist))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")

		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}

		if _, err := fs.Stat(dist, p); err == nil {
			if strings.HasSuffix(p, ".html") {
				// index.html is version-agnostic (the assets carry hashes);
				// never serve a stale shell.
				w.Header().Set("Cache-Control", "no-cache")
			}
			files.ServeHTTP(w, r)
			return
		}

		if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
			!strings.Contains(p, ".") &&
			!strings.HasPrefix(p, "api/") {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			files.ServeHTTP(w, r2)
			return
		}

		http.NotFound(w, r)
	})
}
