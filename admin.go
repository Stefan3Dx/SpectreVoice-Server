package main

// The admin panel (Pro servers): a web page at /admin with the server's numbers over time, its communities,
// who's online, bans, announcements and the log. The app opens it with a one-time link:
//
//	<- {"t":"admin-link"}  (staff, Pro)  -> {"t":"admin-link","url":"https://…/admin/login?code=…"}
//
// The code works once, for two minutes, and becomes a 12-hour cookie for that browser.

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed admin.html
var adminPage []byte

type adminAuth struct {
	mu       sync.Mutex
	codes    map[string]adminGrant // one-time
	sessions map[string]adminGrant
}

type adminGrant struct {
	uid string
	exp time.Time
}

func newAdminAuth() *adminAuth {
	return &adminAuth{codes: map[string]adminGrant{}, sessions: map[string]adminGrant{}}
}

func randToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *adminAuth) newCode(uid string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, g := range a.codes {
		if now.After(g.exp) {
			delete(a.codes, k)
		}
	}
	c := randToken()
	a.codes[c] = adminGrant{uid, now.Add(2 * time.Minute)}
	return c
}

func (a *adminAuth) redeem(code string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.codes[code]
	delete(a.codes, code)
	if !ok || time.Now().After(g.exp) {
		return "", false
	}
	tok := randToken()
	a.sessions[tok] = adminGrant{g.uid, time.Now().Add(12 * time.Hour)}
	return tok, true
}

func (a *adminAuth) check(r *http.Request) (string, bool) {
	c, err := r.Cookie("sv_admin")
	if err != nil {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.sessions[c.Value]
	if !ok || time.Now().After(g.exp) {
		delete(a.sessions, c.Value)
		return "", false
	}
	return g.uid, true
}

func (s *server) adminOp(ss *session, t string, _ map[string]any) bool {
	if t != "admin-link" {
		return false
	}
	h := s.hub
	h.mu.Lock()
	staff := h.staff(ss.uid)
	h.mu.Unlock()
	switch {
	case !staff:
		ss.refuse(t, "role", "only the server's admins")
	case !s.limits().Pro:
		ss.refuse(t, "pro", "the admin panel comes with a Pro licence")
	default:
		ss.send(map[string]any{"t": "admin-link", "url": ss.base + "/admin/login?code=" + s.admin.newCode(ss.uid)})
	}
	return true
}

func (s *server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:")
	path := strings.TrimPrefix(r.URL.Path, "/admin")
	if path == "/login" {
		tok, ok := s.admin.redeem(r.URL.Query().Get("code"))
		if !ok {
			http.Error(w, "This link was used or is too old: open the panel again from the app.", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "sv_admin", Value: tok, Path: "/admin", HttpOnly: true, SameSite: http.SameSiteStrictMode,
			Secure: strings.HasPrefix(requestBase(r), "https://"), MaxAge: 12 * 3600})
		http.Redirect(w, r, "/admin/", http.StatusSeeOther)
		return
	}
	uid, ok := s.admin.check(r)
	if !ok || !s.store.role(uid).atLeast(roleAdmin) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "Open the admin panel from the app (Server › Admin panel).")
		return
	}
	if !s.limits().Pro {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, "The admin panel comes with a Pro licence.")
		return
	}
	if path == "" || path == "/" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(adminPage)
		return
	}
	if !strings.HasPrefix(path, "/api/") {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPost && r.Header.Get("X-Admin") != "1" { // a page elsewhere can't post here (CSRF)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var out any
	switch strings.TrimPrefix(path, "/api/") {
	case "overview":
		out = s.adminOverview()
	case "metrics":
		hours, _ := strconv.Atoi(r.URL.Query().Get("h"))
		if hours <= 0 || hours > 24*30 {
			hours = 24
		}
		out = s.metrics.since(time.Now().Add(-time.Duration(hours) * time.Hour))
	case "communities":
		out = s.adminCommunities()
	case "sessions":
		out = s.adminSessions()
	case "top":
		out = s.adminTop()
	case "bans":
		s.mods.mu.Lock()
		list := []ban{}
		for _, b := range s.mods.list {
			if b.Kind == "sban" {
				list = append(list, b)
			}
		}
		s.mods.mu.Unlock()
		for i := range list {
			list[i].Name = s.store.userName(list[i].UID)
		}
		out = list
	case "logs":
		out = s.logs.last()
	case "kick", "announce", "unban":
		if r.Method != http.MethodPost {
			http.Error(w, "POST", http.StatusMethodNotAllowed)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req)
		out = s.adminAction(uid, strings.TrimPrefix(path, "/api/"), req)
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if i, err := d.Info(); err == nil {
				n += i.Size()
			}
		}
		return nil
	})
	return n
}

func (s *server) adminOverview() map[string]any {
	var users, msgs, comms int64
	db := s.store.db
	_ = db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users)
	_ = db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgs)
	_ = db.QueryRow(`SELECT COUNT(*) FROM communities`).Scan(&comms)
	var active7 int64
	_ = db.QueryRow(`SELECT COUNT(*) FROM users WHERE seen > ?`, time.Now().Add(-7*24*time.Hour).UnixMilli()).Scan(&active7)
	s.metrics.mu.Lock()
	now := s.metrics.now
	s.metrics.mu.Unlock()
	if now.T == 0 {
		now = s.metrics.take()
	}
	return map[string]any{
		"name": s.cfg.name, "version": version, "uptime": int64(time.Since(started).Seconds()), "license": s.lic.statusMsg(),
		"now": now, "users": users, "active7d": active7, "messages": msgs, "communities": comms,
		"disk": dirSize(s.cfg.data), "maxUsers": s.maxUsers(), "music": s.music.enabled(),
	}
}

func (s *server) adminCommunities() []map[string]any {
	// Count under h.mu before the query: rows hold the only DB connection, and h.mu holders query the DB
	online := map[int64]int{}
	s.hub.mu.Lock()
	for _, ss := range s.hub.sessions {
		if !ss.bot {
			online[ss.comm]++
		}
	}
	s.hub.mu.Unlock()
	rows, err := s.store.db.Query(`SELECT c.id, c.name, c.owner, c.open, c.created,
 (SELECT COUNT(*) FROM memberships m WHERE m.comm=c.id), (SELECT COUNT(*) FROM channels ch WHERE ch.comm=c.id) FROM communities c ORDER BY c.id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, created, members, chans int64
		var name, owner string
		var open int
		if rows.Scan(&id, &name, &owner, &open, &created, &members, &chans) != nil {
			continue
		}
		out = append(out, map[string]any{"id": id, "name": name, "owner": owner, "open": open == 1,
			"created": created, "members": members, "channels": chans, "online": online[id]})
	}
	// The store has one connection: other queries only after these rows are closed
	rows.Close()
	for _, c := range out {
		c["ownerName"] = s.store.userName(c["owner"].(string))
	}
	return out
}

func (s *server) adminSessions() []map[string]any {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []map[string]any{}
	for _, ss := range h.sessions {
		ch := ""
		if c := h.chans[ss.ch]; c != nil {
			ch = c.Name
		}
		out = append(out, map[string]any{"sid": ss.sid, "uid": ss.uid, "name": ss.name, "client": ss.client, "comm": ss.comm,
			"channel": ch, "rtt": ss.rtt, "loss": ss.loss, "since": ss.since, "bot": ss.bot, "away": ss.away})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["since"].(int64) < out[j]["since"].(int64) })
	return out
}

// adminTop: the busiest channels and people of the last week (messages).
func (s *server) adminTop() map[string]any {
	from := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
	q := func(sql string) []map[string]any {
		rows, err := s.store.db.Query(sql, from)
		if err != nil {
			return nil
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var k, name string
			var n int64
			if rows.Scan(&k, &name, &n) == nil {
				out = append(out, map[string]any{"key": k, "name": name, "n": n})
			}
		}
		return out
	}
	channels := q(`SELECT m.conv, COALESCE(c.name, m.conv), COUNT(*) n FROM messages m LEFT JOIN channels c ON m.conv = 'c:' || c.id
 WHERE m.at > ? AND m.conv NOT LIKE 'd:%' GROUP BY m.conv ORDER BY n DESC LIMIT 10`)
	people := q(`SELECT from_uid, MAX(from_name), COUNT(*) n FROM messages WHERE at > ? GROUP BY from_uid ORDER BY n DESC LIMIT 10`)
	// Messages per hour of the day, the last week
	hours := make([]int64, 24)
	if rows, err := s.store.db.Query(`SELECT at FROM messages WHERE at > ?`, from); err == nil {
		for rows.Next() {
			var at int64
			if rows.Scan(&at) == nil {
				hours[time.UnixMilli(at).Hour()]++
			}
		}
		rows.Close()
	}
	return map[string]any{"channels": channels, "people": people, "hours": hours}
}

func (s *server) adminAction(by, what string, req map[string]any) map[string]any {
	h := s.hub
	switch what {
	case "kick":
		h.mu.Lock()
		o := h.sessions[int(i64(req, "sid"))]
		h.mu.Unlock()
		if o == nil || o.bot {
			return map[string]any{"ok": false}
		}
		o.send(map[string]any{"t": "kicked", "by": "admin", "why": clip(str(req, "why"), 200)})
		time.AfterFunc(300*time.Millisecond, o.close)
	case "announce":
		text := clip(strings.TrimSpace(str(req, "text")), 500)
		if text == "" {
			return map[string]any{"ok": false}
		}
		h.mu.Lock()
		h.broadcast(map[string]any{"t": "announce", "text": text, "by": s.store.userName(by)}, func(o *session) bool { return !o.bot })
		h.mu.Unlock()
	case "unban":
		s.mods.remove(str(req, "uid"), "sban", 0)
	}
	return map[string]any{"ok": true}
}
