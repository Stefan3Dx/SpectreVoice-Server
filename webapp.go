package main

// The web client: a page the server itself serves at /app/, so someone invited can come in without installing
// anything: voice and chat, streams (watching, sharing the screen), the server's TV, channel files. Plain files
// embedded in the binary (ES modules, no build step; texts in i18n-<lang>.json); the page talks to /v1/ws, /streams,
// /tv/…, /cf/… and /files on its own origin with the session's token, so nothing else server-side is needed: no CORS,
// no cookies.

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web/index.html web/*.js web/*.css web/*.json
var webFiles embed.FS

// webCSP: scripts and styles only from this server, connections only to it (its own WebSocket), nothing else.
const webCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; " +
	"media-src 'self' blob:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// GET /app/ (index.html), /app/<module>.js, /app/app.css, /app/i18n-<lang>.json.
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
		h.Set("Permissions-Policy", "microphone=(self), display-capture=(self), camera=(), geolocation=()")
		h.Set("Cache-Control", "no-store")
	case strings.HasSuffix(name, ".js"):
		h.Set("Content-Type", "text/javascript; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
	case strings.HasSuffix(name, ".css"):
		h.Set("Content-Type", "text/css; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
	case strings.HasSuffix(name, ".json"):
		h.Set("Content-Type", "application/json; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
	}
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}
