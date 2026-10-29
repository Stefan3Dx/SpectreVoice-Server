package main

// What the server keeps in SQLite (<data>/spectre.db): people (by uid), the channel tree, chat history.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

type role string

const (
	roleMember role = ""
	roleMod    role = "mod"
	roleAdmin  role = "admin"
	roleOwner  role = "owner"
)

var roleRank = map[role]int{roleMember: 0, roleMod: 1, roleAdmin: 2, roleOwner: 3}

func (r role) atLeast(o role) bool { return roleRank[r] >= roleRank[o] }

func validRole(r string) bool { _, ok := roleRank[role(r)]; return ok }

type channel struct {
	ID       int64  `json:"id"`
	Comm     int64  `json:"comm"`
	Parent   int64  `json:"parent"`
	Name     string `json:"name"`
	Topic    string `json:"topic,omitempty"`
	Pos      int    `json:"pos"`
	MaxUsers int    `json:"max,omitempty"`
	Default  bool   `json:"default,omitempty"`
	HasPass  bool   `json:"locked,omitempty"`
	// The voice's quality level here (0: the server's default; see quality.go), capped by the server's tier
	Quality int `json:"quality,omitempty"`
	// An emoji before the name, a picture (https URL) for its page
	Icon string `json:"icon,omitempty"`
	Pic  string `json:"pic,omitempty"`
	// "": a voice channel (with its own chat); "text": a chat channel, nobody joins; "spacer": a line between channels
	Kind string `json:"kind,omitempty"`
	// Moderated: only those allowed (mute_voice) and those they let talk are heard
	Moderated bool `json:"moderated,omitempty"`
	// A temporary channel (tempch.go): only in memory, made by Owner (its channel admin), gone once empty a while
	Temp       bool   `json:"temp,omitempty"`
	Owner      string `json:"owner,omitempty"`
	pass       string // bcrypt
	emptySince time.Time
}

type chatMsg struct {
	ID   int64  `json:"id"`
	To   string `json:"to"`
	From string `json:"from"`
	Name string `json:"name"`
	Text string `json:"text"`
	At   int64  `json:"at"`
	Edit int64  `json:"edit,omitempty"`
	Del  bool   `json:"del,omitempty"`
	// An end-to-end encrypted private message (e2e.go): the sender's envelope, opaque here; Text is "" then
	Enc string `json:"e2e,omitempty"`
}

// msgCols: a message row as chatMsg scans it.
const msgCols = "id,conv,from_uid,from_name,text,at,edited,deleted,enc"

type store struct {
	db    *sql.DB
	mu    sync.Mutex
	roles map[string]role // cached
}

const schema = `
CREATE TABLE IF NOT EXISTS users(uid TEXT PRIMARY KEY, pub TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', role TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL, seen INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS user_state(uid TEXT PRIMARY KEY, last_comm INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS channels(id INTEGER PRIMARY KEY, parent INTEGER NOT NULL DEFAULT 0, name TEXT NOT NULL, topic TEXT NOT NULL DEFAULT '', pos INTEGER NOT NULL DEFAULT 0, pass TEXT NOT NULL DEFAULT '', max_users INTEGER NOT NULL DEFAULT 0, is_default INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS messages(id INTEGER PRIMARY KEY, conv TEXT NOT NULL, from_uid TEXT NOT NULL, from_name TEXT NOT NULL, text TEXT NOT NULL, at INTEGER NOT NULL, edited INTEGER NOT NULL DEFAULT 0, deleted INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS messages_conv ON messages(conv, id);
CREATE INDEX IF NOT EXISTS messages_from ON messages(from_uid);
CREATE TABLE IF NOT EXISTS e2e_devices(uid TEXT NOT NULL, dev TEXT NOT NULL, rec TEXT NOT NULL, ts INTEGER NOT NULL, seen INTEGER NOT NULL, PRIMARY KEY(uid, dev));
`

func openStore(path string) (*store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; SQLite serialises anyway
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	st := &store{db: db, roles: map[string]role{}}
	// End-to-end encrypted private messages (e2e.go): their envelope, in a column of its own
	if !st.hasColumn("messages", "enc") {
		if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN enc TEXT NOT NULL DEFAULT ''`); err != nil {
			return nil, fmt.Errorf("schema: %w", err)
		}
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM channels`).Scan(&n)
	if n == 0 {
		// A new server: one channel everyone lands in, and an AFK one
		_, _ = db.Exec(`INSERT INTO channels(parent,name,pos,is_default) VALUES(0,'Lobby',0,1),(0,'AFK',100,0)`)
	}
	rows, err := db.Query(`SELECT uid, role FROM users WHERE role != ''`)
	if err == nil {
		for rows.Next() {
			var u, r string
			if rows.Scan(&u, &r) == nil {
				st.roles[u] = role(r)
			}
		}
		rows.Close()
	}
	return st, nil
}

func (st *store) close() { _ = st.db.Close() }

// seen records a proven uid (its key and name) and gives its role.
func (st *store) seen(uid, pub, name string) role {
	now := time.Now().UnixMilli()
	_, _ = st.db.Exec(`INSERT INTO users(uid,pub,name,created,seen) VALUES(?,?,?,?,?) ON CONFLICT(uid) DO UPDATE SET name=excluded.name, seen=excluded.seen`, uid, pub, name, now, now)
	return st.role(uid)
}

func (st *store) role(uid string) role {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.roles[uid]
}

func (st *store) setRole(uid string, r role) error {
	res, err := st.db.Exec(`UPDATE users SET role=? WHERE uid=?`, string(r), uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Not seen yet: kept anyway, so an owner can be named before they first connect
		if _, err := st.db.Exec(`INSERT INTO users(uid,pub,role,created,seen) VALUES(?, '', ?, ?, 0)`, uid, string(r), time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	st.mu.Lock()
	if r == roleMember {
		delete(st.roles, uid)
	} else {
		st.roles[uid] = r
	}
	st.mu.Unlock()
	return nil
}

func (st *store) userName(uid string) string {
	var n string
	_ = st.db.QueryRow(`SELECT name FROM users WHERE uid=?`, uid).Scan(&n)
	return n
}

func (st *store) channels() ([]*channel, error) {
	rows, err := st.db.Query(`SELECT id,comm,parent,name,topic,pos,pass,max_users,is_default,quality,icon,pic,kind,moderated FROM channels ORDER BY parent,pos,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*channel
	for rows.Next() {
		c := &channel{}
		var def, mod int
		if err := rows.Scan(&c.ID, &c.Comm, &c.Parent, &c.Name, &c.Topic, &c.Pos, &c.pass, &c.MaxUsers, &def, &c.Quality, &c.Icon, &c.Pic, &c.Kind, &mod); err != nil {
			return nil, err
		}
		c.Default = def == 1
		c.Moderated = mod == 1
		c.HasPass = c.pass != ""
		out = append(out, c)
	}
	return out, rows.Err()
}

func hashPass(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if len(p) > 72 {
		return "", errors.New("password too long")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	return string(h), err
}

func (c *channel) passOK(p string) bool {
	return c.pass == "" || bcrypt.CompareHashAndPassword([]byte(c.pass), []byte(p)) == nil
}

func (st *store) saveChannel(c *channel) error {
	def := 0
	if c.Default {
		def = 1
	}
	if c.Default {
		if _, err := st.db.Exec(`UPDATE channels SET is_default=0 WHERE comm=?`, c.Comm); err != nil {
			return err
		}
	}
	if c.ID == 0 {
		res, err := st.db.Exec(`INSERT INTO channels(parent,name,topic,pos,pass,max_users,is_default,comm,quality,icon,pic,kind,moderated) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, c.Parent, c.Name, c.Topic, c.Pos, c.pass, c.MaxUsers, def, c.Comm, c.Quality, c.Icon, c.Pic, c.Kind, b2i(c.Moderated))
		if err != nil {
			return err
		}
		c.ID, _ = res.LastInsertId()
		return nil
	}
	_, err := st.db.Exec(`UPDATE channels SET parent=?,name=?,topic=?,pos=?,pass=?,max_users=?,is_default=?,quality=?,icon=?,pic=?,kind=?,moderated=? WHERE id=?`, c.Parent, c.Name, c.Topic, c.Pos, c.pass, c.MaxUsers, def, c.Quality, c.Icon, c.Pic, c.Kind, b2i(c.Moderated), c.ID)
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// migrateChannels adds the columns newer servers keep for a channel.
func (st *store) migrateChannels() error {
	for _, c := range []struct{ name, def string }{
		{"quality", "INTEGER NOT NULL DEFAULT 0"}, {"icon", "TEXT NOT NULL DEFAULT ''"}, {"pic", "TEXT NOT NULL DEFAULT ''"},
		{"kind", "TEXT NOT NULL DEFAULT ''"}, {"moderated", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if !st.hasColumn("channels", c.name) {
			if _, err := st.db.Exec(`ALTER TABLE channels ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
				return err
			}
		}
	}
	return nil
}

func (st *store) deleteChannel(id int64) error {
	if _, err := st.db.Exec(`DELETE FROM channels WHERE id=?`, id); err != nil {
		return err
	}
	if _, err := st.db.Exec(`DELETE FROM messages WHERE conv=?`, chanConv(id)); err != nil {
		return err
	}
	_, err := st.db.Exec(`DELETE FROM mods WHERE kind='ch-ban' AND ch=?`, id)
	if err != nil && strings.Contains(err.Error(), "no such table") {
		err = nil
	}
	return err
}

func chanConv(id int64) string { return fmt.Sprintf("c:%d", id) }

// dmConv names the private conversation of two uids, the same whoever writes.
func dmConv(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return "d:" + a + ":" + b
}

// dmOther gives the other uid of a private conversation, or "" when uid isn't in it.
func dmOther(conv, uid string) string {
	rest, ok := strings.CutPrefix(conv, "d:")
	if !ok {
		return ""
	}
	a, b, ok := strings.Cut(rest, ":")
	switch {
	case !ok:
		return ""
	case a == uid:
		return b
	case b == uid:
		return a
	}
	return ""
}

func (st *store) addMessage(m *chatMsg) error {
	res, err := st.db.Exec(`INSERT INTO messages(conv,from_uid,from_name,text,at,enc) VALUES(?,?,?,?,?,?)`, m.To, m.From, m.Name, m.Text, m.At, m.Enc)
	if err != nil {
		return err
	}
	m.ID, _ = res.LastInsertId()
	return nil
}

// history gives up to limit messages of conv older than before (0: the newest), oldest first.
func (st *store) history(conv string, before int64, limit int) ([]chatMsg, error) {
	if before <= 0 {
		before = 1 << 62
	}
	rows, err := st.db.Query(`SELECT `+msgCols+` FROM messages WHERE conv=? AND id<? ORDER BY id DESC LIMIT ?`, conv, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chatMsg
	for rows.Next() {
		var m chatMsg
		var del int
		if err := rows.Scan(&m.ID, &m.To, &m.From, &m.Name, &m.Text, &m.At, &m.Edit, &del, &m.Enc); err != nil {
			return nil, err
		}
		if del == 1 {
			m.Del, m.Text, m.Enc = true, "", ""
		}
		out = append(out, m)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

func (st *store) scanMessages(rows *sql.Rows) ([]chatMsg, error) {
	defer rows.Close()
	var out []chatMsg
	for rows.Next() {
		var m chatMsg
		var del int
		if err := rows.Scan(&m.ID, &m.To, &m.From, &m.Name, &m.Text, &m.At, &m.Edit, &del, &m.Enc); err != nil {
			return nil, err
		}
		if del == 1 {
			m.Del, m.Text, m.Enc = true, "", ""
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// historyAfter gives up to limit messages of conv newer than after, oldest first (an app catching up).
func (st *store) historyAfter(conv string, after int64, limit int) ([]chatMsg, error) {
	rows, err := st.db.Query(`SELECT `+msgCols+` FROM messages WHERE conv=? AND id>? ORDER BY id ASC LIMIT ?`, conv, after, limit)
	if err != nil {
		return nil, err
	}
	return st.scanMessages(rows)
}

// historyAround gives up to half messages up to and including id and half after it, oldest first, and whether
// older and newer ones exist beyond them.
func (st *store) historyAround(conv string, id int64, half int) (items []chatMsg, older, newer bool, err error) {
	rows, err := st.db.Query(`SELECT `+msgCols+` FROM messages WHERE conv=? AND id<=? ORDER BY id DESC LIMIT ?`, conv, id, half+1)
	if err != nil {
		return nil, false, false, err
	}
	before, err := st.scanMessages(rows)
	if err != nil {
		return nil, false, false, err
	}
	if older = len(before) > half; older {
		before = before[:half]
	}
	for i, j := 0, len(before)-1; i < j; i, j = i+1, j-1 {
		before[i], before[j] = before[j], before[i]
	}
	rows, err = st.db.Query(`SELECT `+msgCols+` FROM messages WHERE conv=? AND id>? ORDER BY id ASC LIMIT ?`, conv, id, half+1)
	if err != nil {
		return nil, false, false, err
	}
	after, err := st.scanMessages(rows)
	if err != nil {
		return nil, false, false, err
	}
	if newer = len(after) > half; newer {
		after = after[:half]
	}
	return append(before, after...), older, newer, nil
}

func (st *store) message(id int64) (*chatMsg, error) {
	var m chatMsg
	var del int
	err := st.db.QueryRow(`SELECT `+msgCols+` FROM messages WHERE id=?`, id).Scan(&m.ID, &m.To, &m.From, &m.Name, &m.Text, &m.At, &m.Edit, &del, &m.Enc)
	if err != nil {
		return nil, err
	}
	m.Del = del == 1
	return &m, nil
}

func (st *store) editMessage(id int64, text string, at int64) error {
	_, err := st.db.Exec(`UPDATE messages SET text=?, edited=? WHERE id=?`, text, at, id)
	return err
}

func (st *store) deleteMessage(id int64) error {
	_, err := st.db.Exec(`UPDATE messages SET text='', enc='', deleted=1 WHERE id=?`, id)
	return err
}

type dmSummary struct {
	With string  `json:"with"`
	Name string  `json:"name"`
	Last chatMsg `json:"last"`
}

// dms lists uid's private conversations, the most recent first.
func (st *store) dms(uid string, limit int) ([]dmSummary, error) {
	rows, err := st.db.Query(`SELECT m.id,m.conv,m.from_uid,m.from_name,m.text,m.at,m.edited,m.deleted,m.enc FROM messages m
		JOIN (SELECT conv, MAX(id) AS id FROM messages WHERE conv LIKE ? OR conv LIKE ? GROUP BY conv) l ON l.id=m.id
		ORDER BY m.id DESC LIMIT ?`, "d:"+uid+":%", "d:%:"+uid, limit)
	if err != nil {
		return nil, err
	}
	var out []dmSummary
	for rows.Next() {
		var m chatMsg
		var del int
		if err := rows.Scan(&m.ID, &m.To, &m.From, &m.Name, &m.Text, &m.At, &m.Edit, &del, &m.Enc); err != nil {
			rows.Close()
			return nil, err
		}
		if del == 1 {
			m.Del, m.Text, m.Enc = true, "", ""
		}
		out = append(out, dmSummary{With: dmOther(m.To, uid), Last: m})
	}
	rows.Close()
	for i := range out {
		out[i].Name = st.userName(out[i].With)
	}
	return out, nil
}

// wipeUser forgets what the server keeps about uid (its account deletion): its messages and its row.
func (st *store) wipeUser(uid string) (int, error) {
	res, err := st.db.Exec(`DELETE FROM messages WHERE from_uid=? OR conv LIKE ? OR conv LIKE ?`, uid, "d:"+uid+":%", "d:%:"+uid)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	// What else the server keeps about them: devices for push, where they're a member (not what they own),
	// their roles there, where they were last. Bans stay (they're the community's, not the person's).
	for _, q := range []string{
		`DELETE FROM push_tokens WHERE uid=?`,
		`DELETE FROM e2e_devices WHERE uid=?`,
		`DELETE FROM member_roles WHERE uid=?`,
		`DELETE FROM memberships WHERE uid=? AND comm NOT IN (SELECT id FROM communities WHERE owner=?1)`,
		`DELETE FROM user_state WHERE uid=?`,
		`DELETE FROM comm_requests WHERE uid=?`,
		`DELETE FROM admitted WHERE uid=?`,
		`UPDATE sounds SET by_uid='', by_name='' WHERE by_uid=?`,
		`DELETE FROM users WHERE uid=? AND role=''`,
	} {
		if _, err := st.db.Exec(q, uid); err != nil && !strings.Contains(err.Error(), "no such table") {
			return int(n), err
		}
	}
	return int(n), nil
}

// namesIn: the members of comm by the names they go by (lower case), for mentions.
func (st *store) namesIn(comm int64) (map[string][]string, error) {
	rows, err := st.db.Query(`SELECT u.uid, u.name FROM users u JOIN memberships m ON m.uid=u.uid WHERE m.comm=?`, comm)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var u, n string
		if rows.Scan(&u, &n) == nil && n != "" {
			k := strings.ToLower(n)
			out[k] = append(out[k], u)
		}
	}
	return out, rows.Err()
}

// hasColumnValue: some row of table has col = v.
func (st *store) hasColumnValue(table, col, v string) bool {
	var n int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+col+` = ?`, v).Scan(&n)
	return n > 0
}
