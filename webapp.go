package main

// The web client: a page the server itself serves at /app/, so someone invited can come in without installing
// anything (voice and chat; the rest is in the apps). Plain files embedded in the binary; the page talks to /v1/ws on
// its own origin (hello v2, client "web/<version>"), so nothing else server-side is needed: no CORS, no cookies.

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web/index.html web/app.js web/app.css
var webFiles embed.FS

// webCSP: scripts and styles only from this server, connections only to it (its own WebSocket), nothing else.
const webCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; " +
	"media-src 'self' blob:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// GET /app/ (index.html), /app/app.js, /app/app.css.
func (s *server) handleWebApp(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/app" {
		http.Redirect(w, r, "/app/"+map[bool]string{true: "?" + r.URL.RawQuery, false: ""}[r.URL.RawQuery != ""], http.StatusMovedPermanently)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/app/")
	if name == "" {
		name = "index.html"
	}
	sub, _ := fs.Sub(webFiles, "web")
	data, err := fs.ReadFile(sub, name)
	if err != nil || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	switch {
	case strings.HasSuffix(name, ".html"):
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Security-Policy", webCSP)
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Permissions-Policy", "microphone=(self), camera=(), geolocation=()")
		h.Set("Cache-Control", "no-store")
	case strings.HasSuffix(name, ".js"):
		h.Set("Content-Type", "text/javascript; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
	case strings.HasSuffix(name, ".css"):
		h.Set("Content-Type", "text/css; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
	}
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}
