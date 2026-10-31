package main

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web
var webAssets embed.FS

func (s *Server) webHandler() http.Handler {
	sub, err := fs.Sub(webAssets, "web")
	if err != nil {
		return http.NotFoundHandler()
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/v1/") {
			http.NotFound(w, r)
			return
		}
		// 静态资源让 CDN/浏览器每次都回源校验：面板改版后不该被缓存挡在旧版本
		w.Header().Set("Cache-Control", "no-cache")
		if !s.adminAuthed(r) {
			// Only the login page and its minimal assets are reachable
			// before authentication.
			if path == "/login.js" || path == "/style.css" {
				files.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			r.URL.Path = "/login.html"
		}
		if path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
