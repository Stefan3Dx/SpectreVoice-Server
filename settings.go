package main

// The server's own settings, changed from the apps by its admins and kept in the database: they win over the
// environment (SV_NAME, SV_PASSWORD), which still gives the first value.
//
//	<- {"t":"server-save","name"?,"about"?,"icon"?}   (manage_community) the server's name, description, icon
//	<- {"t":"server-save","password":"…"}             (administrator) the server password; "" opens the server
//	-> {"t":"server-info","server":{name,about,icon,password}} to everyone ("password": whether there is one)
//
// The password is kept as a bcrypt hash (the environment's stays plain, as the operator wrote it). People let in
// by an invite that skips the password (invites.go) are remembered in "admitted": they come back without it until
// they're banned or their data is wiped.

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const settingsSchema = `
CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS admitted(uid TEXT PRIMARY KEY, at INTEGER NOT NULL, invite TEXT NOT NULL DEFAULT '');
`

func (st *store) migrateSettings() error {
	_, err := st.db.Exec(settingsSchema)
	return err
}

// setting: the value kept for key, and whether there is one.
func (st *store) setting(key string) (string, bool) {
	var v string
	err := st.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v, err == nil
}

func (st *store) setSetting(key, v string) error {
	_, err := st.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, v)
	return err
}

func (st *store) admit(uid, invite string) {
	_, _ = st.db.Exec(`INSERT INTO admitted(uid,at,invite) VALUES(?,?,?) ON CONFLICT(uid) DO NOTHING`, uid, time.Now().UnixMilli(), invite)
}

func (st *store) admitted(uid string) bool {
	var n int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM admitted WHERE uid=?`, uid).Scan(&n)
	return n > 0
}

func (st *store) unadmit(uid string) { _, _ = st.db.Exec(`DELETE FROM admitted WHERE uid=?`, uid) }

// serverConf: the name and password as they are now (settings over the environment).
type serverConf struct {
	mu       sync.Mutex
	name     string
	plain    string   // the environment's password (when no hash is kept)
	hash     string   // bcrypt, when an admin set one
	set      bool     // an admin set the password (hash, or "" for none): the environment's no longer counts
	lastGood [32]byte // sha256 of the last password that matched the hash (bcrypt once, not on every hello)
}

// loadSettings reads the kept settings over cfg, and keeps the space's row (community 1) named as the server.
func (s *server) loadSettings() error {
	st := s.store
	if err := st.migrateSettings(); err != nil {
		return err
	}
	c := &s.conf
	c.name = s.cfg.name
	if v, ok := st.setting("name"); ok && v != "" {
		c.name = v
	} else if old, err := st.community(spaceID); err == nil && old.Name != "" && old.Name != s.cfg.name {
		// From before: the main community was renamed in the apps; that name stays the server's
		c.name = old.Name
		_ = st.setSetting("name", old.Name)
	}
	_, _ = st.db.Exec(`UPDATE communities SET name=? WHERE id=?`, c.name, spaceID)
	c.plain = s.cfg.joinPass
	if v, ok := st.setting("password"); ok {
		c.set, c.hash = true, v
	}
	return nil
}

// serverName: the server's name in the apps, on the invite and privacy pages.
func (s *server) serverName() string {
	s.conf.mu.Lock()
	defer s.conf.mu.Unlock()
	return s.conf.name
}

// hasPassword: does joining need the server password.
func (s *server) hasPassword() bool {
	c := &s.conf
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set {
		return c.hash != ""
	}
	return c.plain != ""
}

// passwordOK: p is the server password (or there is none).
func (s *server) passwordOK(p string) bool {
	c := &s.conf
	c.mu.Lock()
	set, hash, plain, last := c.set, c.hash, c.plain, c.lastGood
	c.mu.Unlock()
	if !set {
		return plain == "" || constEq(p, plain)
	}
	if hash == "" {
		return true
	}
	sum := sha256.Sum256([]byte(hash + "\x00" + p))
	if subtle.ConstantTimeCompare(sum[:], last[:]) == 1 {
		return true
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(p)) != nil {
		return false
	}
	c.mu.Lock()
	if c.hash == hash {
		c.lastGood = sum
	}
	c.mu.Unlock()
	return true
}

// setPassword: an admin's new password ("": an open server), kept hashed.
func (s *server) setPassword(p string) error {
	if len(p) > 72 {
		return errors.New("password too long (72 at most)")
	}
	h := ""
	if p != "" {
		b, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		h = string(b)
	}
	if err := s.store.setSetting("password", h); err != nil {
		return err
	}
	c := &s.conf
	c.mu.Lock()
	c.set, c.hash, c.lastGood = true, h, [32]byte{}
	c.mu.Unlock()
	return nil
}

// serverInfo: what the apps show of the server (welcome, server-info).
func (s *server) serverInfo() map[string]any {
	out := map[string]any{"name": s.serverName(), "password": s.hasPassword()}
	if c, err := s.store.community(spaceID); err == nil {
		out["about"], out["icon"] = c.About, c.Icon
	}
	return out
}

// serverSave: server-save from an app.
func (h *hub) serverSave(ss *session, t string, m map[string]any) {
	st := h.s.store
	h.mu.Lock()
	mayInfo := !ss.api && h.may(ss, pManageCommunity)
	mayPass := !ss.api && h.may(ss, pAdministrator)
	c := *h.access(spaceID).comm
	h.mu.Unlock()
	pw, hasPw := m["password"].(string)
	_, hasName := m["name"]
	_, hasAbout := m["about"]
	_, hasIcon := m["icon"]
	_, hasOpen := m["open"]
	if (hasName || hasAbout || hasIcon || hasOpen) && !mayInfo || hasPw && !mayPass {
		ss.refuse(t, "role", "not allowed")
		return
	}
	if hasPw {
		if err := h.s.setPassword(pw); err != nil {
			ss.refuse(t, "bad", err.Error())
			return
		}
		log.Printf("server password %s by %s", map[bool]string{true: "set", false: "removed"}[pw != ""], ss.uid)
	}
	if hasName || hasAbout || hasIcon {
		if n := cleanName(str(m, "name")); n != "" {
			c.Name = n
		}
		if v, ok := m["about"].(string); ok {
			c.About = clip(strings.TrimSpace(v), 1000)
		}
		if v, ok := m["icon"].(string); ok && (v == "" || strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "/f/")) {
			c.Icon = clip(v, 500)
		}
		c.Open = true
		if err := st.saveCommunity(&c); err != nil {
			ss.refuse(t, "server", err.Error())
			return
		}
		if err := st.setSetting("name", c.Name); err != nil {
			ss.refuse(t, "server", err.Error())
			return
		}
		h.s.conf.mu.Lock()
		h.s.conf.name = c.Name
		h.s.conf.mu.Unlock()
	}
	info := h.s.serverInfo()
	h.mu.Lock()
	h.forget(spaceID)
	h.toComm(spaceID, map[string]any{"t": "server-info", "server": info})
	h.mu.Unlock()
	ss.send(map[string]any{"t": "done", "op": t})
}
