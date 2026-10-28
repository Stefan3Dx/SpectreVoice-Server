package main

// Invites: a link to this server (and a channel of it). One made with "pass" also lets its people in without the
// server password (only who may change the password makes those); they're remembered as admitted (settings.go).
//
//	<- {"t":"invite-create","ch":0,"hours":24,"uses":0,"pass":false}  (invite; pass: administrator)
//	                                                      -> {"t":"invite","code","url","expires","uses","pass"}
//	<- {"t":"invites"}                                    (manage_community) -> {"t":"invites","items":[…]}
//	<- {"t":"invite-del","code"}                          (manage_community, or who made it)
//	hello {"invite":CODE}                                 (hub.go) uses one: its channel, and the password skipped
//
//	GET /i/<code>           a page: the server, "open in the app", "open in the browser" (/app/), the downloads
//	GET /api/invite/<code>  the same for the apps, as JSON
//	GET /download/<android|windows>   the newest installer the server keeps (updates.go), for anyone

import (
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type invite struct {
	Code    string `json:"code"`
	Comm    int64  `json:"comm"`
	Channel int64  `json:"ch,omitempty"`
	By      string `json:"by"`
	ByName  string `json:"byName,omitempty"`
	Created int64  `json:"created"`
	Expires int64  `json:"expires,omitempty"` // ms; 0: never
	MaxUses int    `json:"maxUses,omitempty"` // 0: no limit
	Uses    int    `json:"uses"`
	Bypass  bool   `json:"pass,omitempty"` // lets its people in without the server password
	URL     string `json:"url,omitempty"`
}

var inviteCodeRe = regexp.MustCompile(`^[A-Za-z0-9]{6,16}$`)

func (st *store) migrateInvites() error {
	_, err := st.db.Exec(`CREATE TABLE IF NOT EXISTS invites(code TEXT PRIMARY KEY, comm INTEGER NOT NULL, channel INTEGER NOT NULL DEFAULT 0, by TEXT NOT NULL, created INTEGER NOT NULL, expires INTEGER NOT NULL DEFAULT 0, max_uses INTEGER NOT NULL DEFAULT 0, uses INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS invites_comm ON invites(comm);`)
	if err == nil && !st.hasColumn("invites", "bypass") {
		_, err = st.db.Exec(`ALTER TABLE invites ADD COLUMN bypass INTEGER NOT NULL DEFAULT 0`)
	}
	return err
}

func newInviteCode() string {
	const abc = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 8)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(abc))))
		b[i] = abc[n.Int64()]
	}
	return string(b)
}

func (st *store) addInvite(in *invite) error {
	for range 5 {
		in.Code = newInviteCode()
		_, err := st.db.Exec(`INSERT INTO invites(code,comm,channel,by,created,expires,max_uses,bypass) VALUES(?,?,?,?,?,?,?,?)`, in.Code, in.Comm, in.Channel, in.By, in.Created, in.Expires, in.MaxUses, b2i(in.Bypass))
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "UNIQUE") {
			return err
		}
	}
	return errors.New("no free code")
}

func (st *store) invite(code string) (*invite, error) {
	in := &invite{}
	var bypass int
	err := st.db.QueryRow(`SELECT code,comm,channel,by,created,expires,max_uses,uses,bypass FROM invites WHERE code=?`, code).
		Scan(&in.Code, &in.Comm, &in.Channel, &in.By, &in.Created, &in.Expires, &in.MaxUses, &in.Uses, &bypass)
	in.Bypass = bypass == 1
	return in, err
}

func (st *store) invitesOf(comm int64) ([]invite, error) {
	rows, err := st.db.Query(`SELECT code,comm,channel,by,created,expires,max_uses,uses,bypass FROM invites WHERE comm=? ORDER BY created DESC`, comm)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []invite{}
	for rows.Next() {
		var in invite
		var bypass int
		if rows.Scan(&in.Code, &in.Comm, &in.Channel, &in.By, &in.Created, &in.Expires, &in.MaxUses, &in.Uses, &bypass) == nil {
			in.Bypass = bypass == 1
			out = append(out, in)
		}
	}
	return out, rows.Err()
}

func (in *invite) valid() bool {
	return (in.Expires == 0 || in.Expires > time.Now().UnixMilli()) && (in.MaxUses == 0 || in.Uses < in.MaxUses)
}

// useInvite counts one use of code; the invite, or why it can't be used.
func (s *server) useInvite(code string) (*invite, error) {
	if !inviteCodeRe.MatchString(code) {
		return nil, errors.New("not an invite")
	}
	in, err := s.store.invite(code)
	if err != nil || in.Comm != spaceID {
		return nil, errors.New("this invite doesn't exist (any more)")
	}
	if !in.valid() {
		return nil, errors.New("this invite has expired")
	}
	// One step: two people can't both take the last use
	res, err := s.store.db.Exec(`UPDATE invites SET uses=uses+1 WHERE code=? AND (max_uses=0 OR uses<max_uses)`, code)
	if err != nil {
		return nil, err
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return nil, errors.New("this invite has expired")
	}
	in.Uses++
	return in, nil
}

// inviteURL: the link for code, from the server's setting or the address the app used.
func (s *server) inviteURL(base, code string) string {
	if s.cfg.inviteBase != "" {
		return strings.TrimRight(s.cfg.inviteBase, "/") + "/" + code
	}
	return strings.TrimRight(base, "/") + "/i/" + code
}

// requestBase: how the app reached the server (https://sfu.example.com), behind the proxy too.
func requestBase(r *http.Request) string {
	// The proxy's headers count only from the proxy (on this machine or the LAN), not from anyone
	viaProxy := false
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if ip := net.ParseIP(h); ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
			viaProxy = true
		}
	}
	scheme := "http"
	if r.TLS != nil || (viaProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")) {
		scheme = "https"
	}
	host := ""
	if viaProxy {
		host = r.Header.Get("X-Forwarded-Host")
	}
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

func (s *server) inviteOp(ss *session, t string, m map[string]any) bool {
	h := s.hub
	switch t {
	case "invite-create":
		bypass, _ := boolOf(m, "pass")
		h.mu.Lock()
		ok := ss.comm != 0 && h.may(ss, pInvite) && (!bypass || h.may(ss, pAdministrator))
		ch := i64(m, "ch")
		if c := h.chans[ch]; ch != 0 && (c == nil || c.Comm != ss.comm) {
			ch = 0
		}
		h.mu.Unlock()
		if !ok {
			ss.refuse(t, "role", "not allowed")
			return true
		}
		in := &invite{Comm: ss.comm, Channel: ch, By: ss.uid, Created: time.Now().UnixMilli(), MaxUses: int(max(0, min(i64(m, "uses"), 10000))), Bypass: bypass}
		if hrs := i64(m, "hours"); hrs > 0 {
			in.Expires = time.Now().Add(time.Duration(min(hrs, 24*365)) * time.Hour).UnixMilli()
		}
		if err := s.store.addInvite(in); err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		in.URL = s.inviteURL(ss.base, in.Code)
		ss.send(map[string]any{"t": "invite", "code": in.Code, "url": in.URL, "expires": in.Expires, "uses": in.MaxUses, "ch": in.Channel, "pass": in.Bypass})
	case "invites":
		h.mu.Lock()
		ok := ss.comm != 0 && h.may(ss, pManageCommunity)
		h.mu.Unlock()
		if !ok {
			ss.refuse(t, "role", "not allowed")
			return true
		}
		list, _ := s.store.invitesOf(ss.comm)
		for i := range list {
			list[i].URL = s.inviteURL(ss.base, list[i].Code)
			list[i].ByName = s.store.userName(list[i].By)
		}
		ss.send(map[string]any{"t": "invites", "items": list})
	case "invite-del":
		code := str(m, "code")
		in, err := s.store.invite(code)
		if err != nil || in.Comm != ss.comm {
			return true
		}
		h.mu.Lock()
		ok := in.By == ss.uid || h.may(ss, pManageCommunity)
		h.mu.Unlock()
		if !ok {
			ss.refuse(t, "role", "not allowed")
			return true
		}
		_, _ = s.store.db.Exec(`DELETE FROM invites WHERE code=?`, code)
		ss.send(map[string]any{"t": "done", "op": t, "code": code})
	default:
		return false
	}
	return true
}

type inviteInfo struct {
	Code      string     `json:"code"`
	Valid     bool       `json:"valid"`
	Server    string     `json:"server"`
	Address   string     `json:"address"`
	Community *community `json:"community,omitempty"` // the server's space (name, about, icon, members): older apps
	Channel   string     `json:"channel,omitempty"`
	By        string     `json:"by,omitempty"`
	Password  bool       `json:"password"`       // the server asks for its password (this invite doesn't skip it)
	Bypass    bool       `json:"pass,omitempty"` // this invite skips the password
}

func (s *server) inviteInfo(r *http.Request, code string) (*inviteInfo, int) {
	info := &inviteInfo{Code: code, Server: s.serverName(), Address: requestBase(r), Password: s.hasPassword()}
	if !inviteCodeRe.MatchString(code) {
		return info, http.StatusNotFound
	}
	in, err := s.store.invite(code)
	if err != nil || in.Comm != spaceID {
		return info, http.StatusNotFound
	}
	info.Bypass = in.Bypass
	info.Password = info.Password && !in.Bypass
	c, err := s.store.community(in.Comm)
	if err != nil {
		return info, http.StatusNotFound
	}
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM memberships WHERE comm=?`, c.ID).Scan(&c.Members)
	info.Community, info.Valid, info.By = c, in.valid(), s.store.userName(in.By)
	if in.Channel != 0 {
		s.hub.mu.Lock()
		if ch := s.hub.chans[in.Channel]; ch != nil {
			info.Channel = ch.Name
		}
		s.hub.mu.Unlock()
	}
	if !info.Valid {
		return info, http.StatusGone
	}
	return info, http.StatusOK
}

// GET /api/invite/<code>
func (s *server) handleInviteInfo(w http.ResponseWriter, r *http.Request) {
	info, code := s.inviteInfo(r, strings.TrimPrefix(r.URL.Path, "/api/invite/"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(info)
}

//go:embed invite.html
var invitePage string

// GET /i/<code>: the invite's page.
func (s *server) handleInvitePage(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimPrefix(r.URL.Path, "/i/")
	info, status := s.inviteInfo(r, code)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src https: data:")
	w.Header().Set("Cache-Control", "no-store")
	title, about, meta := "Invitația nu mai e valabilă", "Cere-i celui care ți-a trimis-o una nouă.", ""
	if info.Community != nil && info.Valid {
		title = "Ai fost invitat pe " + html.EscapeString(info.Community.Name)
		about = html.EscapeString(info.Community.About)
		meta = fmt.Sprintf("%d membri", info.Community.Members)
		if info.Channel != "" {
			meta += " · canalul #" + html.EscapeString(info.Channel)
		}
		if info.By != "" {
			meta += " · de la " + html.EscapeString(info.By)
		}
		if info.Password {
			meta += " · cere parola serverului"
		}
	}
	host := strings.TrimPrefix(strings.TrimPrefix(info.Address, "https://"), "http://")
	open := "spectrevoice://" + host + "/i/" + code
	page := strings.NewReplacer(
		"{{TITLE}}", title, "{{ABOUT}}", about, "{{META}}", meta, "{{SERVER}}", html.EscapeString(s.serverName()),
		"{{OPEN}}", html.EscapeString(open), "{{CODE}}", html.EscapeString(code), "{{ADDRESS}}", html.EscapeString(host),
		"{{PLAY}}", "https://play.google.com/store/apps/details?id="+s.cfg.playPkg,
		"{{SHOW}}", map[bool]string{true: "", false: "hidden"}[info.Community != nil && info.Valid],
	).Replace(invitePage)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, page)
}

// GET /download/android | /download/windows: the newest installer kept for updates.
func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	idx := map[string]string{"android": "android.json", "windows": "windows.json"}[strings.TrimPrefix(r.URL.Path, "/download/")]
	if idx == "" || len(s.updates) == 0 {
		http.NotFound(w, r)
		return
	}
	dir := s.updates[0].dir
	b, err := os.ReadFile(filepath.Join(dir, idx))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var info updateInfo
	if json.Unmarshal(b, &info) != nil || info.File == "" || filepath.Base(info.File) != info.File {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+info.File+`"`)
	if strings.HasSuffix(info.File, ".apk") {
		w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	}
	http.ServeFile(w, r, filepath.Join(dir, info.File))
}
