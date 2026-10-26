package main

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:web/dist/web/browser
var embeddedWebFS embed.FS

// RegisterUIHandlers mounts the embedded Angular web interface and token authentication handlers.
func RegisterUIHandlers(mux *http.ServeMux, cfg ServerConfig) {
	distFS, err := fs.Sub(embeddedWebFS, "web/dist/web/browser")
	if err != nil {
		panic("failed to load embedded UI filesystem: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(distFS))

	uiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. Check if token is passed via query parameter (?token=...)
		queryToken := r.URL.Query().Get("token")
		if queryToken != "" && constantTimeCompare(queryToken, cfg.MasterToken) {
			setMasterCookie(w, cfg.MasterToken)
			http.Redirect(w, r, "/ui/", http.StatusFound)
			return
		}

		// 2. Check if accessed via /ui/<token>
		trimmed := strings.TrimPrefix(r.URL.Path, "/ui")
		trimmed = strings.TrimPrefix(trimmed, "/")
		parts := strings.Split(trimmed, "/")
		if len(parts) == 1 && parts[0] != "" && constantTimeCompare(parts[0], cfg.MasterToken) {
			setMasterCookie(w, cfg.MasterToken)
			http.Redirect(w, r, "/ui/", http.StatusFound)
			return
		}

		// 3. Check if requesting an existing static asset in distFS (e.g. *.js, *.css, *.ico)
		filePath := strings.TrimPrefix(r.URL.Path, "/ui")
		filePath = strings.TrimPrefix(filePath, "/")
		if filePath != "" {
			if f, err := distFS.Open(filePath); err == nil {
				defer f.Close()
				if fi, err := f.Stat(); err == nil && !fi.IsDir() {
					http.StripPrefix("/ui", fileServer).ServeHTTP(w, r)
					return
				}
			}
		}

		// 4. Default / SPA fallback: serve index.html
		indexFile, err := distFS.Open("index.html")
		if err != nil {
			http.Error(w, "UI index.html not found in embedded filesystem", http.StatusNotFound)
			return
		}
		defer indexFile.Close()

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.Copy(w, indexFile)
	})

	mux.Handle("GET /ui", uiHandler)
	mux.Handle("GET /ui/", uiHandler)
}

func setMasterCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "mem_master_token",
		Value:    token,
		Path:     "/",
		SameSite: http.SameSiteLaxMode,
		HttpOnly: false, // accessible to client-side scripts
		MaxAge:   86400 * 30,
	})
}
