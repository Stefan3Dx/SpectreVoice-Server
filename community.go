package main

// Communities: one server holds several, each with its own channels, roles, members, chat and bus space (games,
// Lei, profiles). A person is a member of the communities they joined (an open one, or through an invite) and is
// in one of them at a time on a connection.
//
// Roles are per community, Discord-like: a name, a colour, a place in the order and a set of permissions. Everyone
// has the community's "@everyone" role; the owner can do everything; the server's own owners and admins (users.role,
// SV_OWNERS) can do everything everywhere.

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

type perm uint64

const (
	pAdministrator   perm = 1 << 0  // everything in the community
	pManageCommunity perm = 1 << 1  // name, description, open or not, invites list
	pManageChannels  perm = 1 << 2  // create, edit, delete channels
	pManageRoles     perm = 1 << 3  // create, edit roles; give roles below one's own
	pMove            perm = 1 << 4  // move people between channels
	pKick            perm = 1 << 5  // kick from the server connection
	pBan             perm = 1 << 6  // ban from the community
	pMuteChat        perm = 1 << 7  // stop someone writing for a while
	pChannelBan      perm = 1 << 8  // keep someone out of a channel
	pDeleteMessages  perm = 1 << 9  // take back others' channel messages
	pInvite          perm = 1 << 10 // make invites
	pStream          perm = 1 << 11 // share the screen / camera
	pBypassLocks     perm = 1 << 12 // enter locked or full channels, read their chat
	pMentionAll      perm = 1 << 13 // @everyone notifies everyone
	pSpeak           perm = 1 << 14 // talk in voice channels
	pMuteVoice       perm = 1 << 15 // silence someone's voice for everyone; let people talk in moderated channels
	pFiles           perm = 1 << 16 // manage the channels' files (folders, others' files)
	pMusic           perm = 1 << 17 // control the music bot for everyone (skip, stop, without a vote)
	pAll             perm = 1<<18 - 1
)

// The permissions apps may show, by name (role-save takes and gives these).
var permNames = map[string]perm{
	"administrator": pAdministrator, "manage_community": pManageCommunity, "manage_channels": pManageChannels,
	"manage_roles": pManageRoles, "move": pMove, "kick": pKick, "ban": pBan, "mute_chat": pMuteChat,
	"channel_ban": pChannelBan, "delete_messages": pDeleteMessages, "invite": pInvite, "stream": pStream,
	"bypass_locks": pBypassLocks, "mention_all": pMentionAll, "speak": pSpeak,
	"mute_voice": pMuteVoice, "files": pFiles, "music": pMusic,
}

func permList(p perm) []string {
	var out []string
	for n, b := range permNames {
		if p&b != 0 {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func permOf(v any) perm {
	var p perm
	switch l := v.(type) {
	case []any:
		for _, x := range l {
			if s, ok := x.(string); ok {
				p |= permNames[s]
			}
		}
	case float64:
		p = perm(uint64(l)) & pAll
	}
	return p
}

const (
	everyonePerms = pInvite | pStream | pSpeak
	modPerms      = pMove | pKick | pMuteChat | pChannelBan | pDeleteMessages | pMuteVoice | pFiles | pMusic | everyonePerms
	adminPerms    = pAdministrator | modPerms | pManageChannels | pManageRoles | pManageCommunity | pBan
)

type community struct {
	ID      int64  `json:"id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	About   string `json:"about,omitempty"`
	Icon    string `json:"icon,omitempty"`
	Owner   string `json:"owner"`
	Open    bool   `json:"open"`
	Code    string `json:"code,omitempty"` // to find it (community_codes.go)
	Created int64  `json:"created"`
	Members int    `json:"members,omitempty"`
}

type crole struct {
	ID       int64    `json:"id"`
	Comm     int64    `json:"-"`
	Name     string   `json:"name"`
	Color    int64    `json:"color"`
	Pos      int      `json:"pos"`
	Perms    []string `json:"perms"`
	Everyone bool     `json:"everyone,omitempty"`
	p        perm
}

const communitySchema = `
CREATE TABLE IF NOT EXISTS communities(id INTEGER PRIMARY KEY, slug TEXT UNIQUE NOT NULL, name TEXT NOT NULL, about TEXT NOT NULL DEFAULT '', icon TEXT NOT NULL DEFAULT '', owner TEXT NOT NULL DEFAULT '', open INTEGER NOT NULL DEFAULT 0, created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS memberships(comm INTEGER NOT NULL, uid TEXT NOT NULL, joined INTEGER NOT NULL, PRIMARY KEY(comm, uid));
CREATE INDEX IF NOT EXISTS memberships_uid ON memberships(uid);
CREATE TABLE IF NOT EXISTS roles(id INTEGER PRIMARY KEY, comm INTEGER NOT NULL, name TEXT NOT NULL, color INTEGER NOT NULL DEFAULT 0, pos INTEGER NOT NULL DEFAULT 0, perms INTEGER NOT NULL DEFAULT 0, everyone INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS member_roles(comm INTEGER NOT NULL, uid TEXT NOT NULL, role INTEGER NOT NULL, PRIMARY KEY(comm, uid, role));
`

// migrateCommunities makes the tables, and the first community from a server that had none (its channels, people
// and roles move into it).
func (st *store) migrateCommunities(name string) error {
	if _, err := st.db.Exec(communitySchema); err != nil {
		return err
	}
	if err := st.migrateCodes(); err != nil {
		return err
	}
	if !st.hasColumn("channels", "comm") {
		if _, err := st.db.Exec(`ALTER TABLE channels ADD COLUMN comm INTEGER NOT NULL DEFAULT 1`); err != nil {
			return err
		}
	}
	// Roles from before the voice-mute, files and music permissions: moderators get them
	if !st.hasColumn("channels", "quality") {
		_, _ = st.db.Exec(`UPDATE roles SET perms = perms | ? WHERE perms & ? != 0`, int64(pMuteVoice|pFiles|pMusic), int64(pMuteChat))
	}
	var n int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM communities`).Scan(&n)
	if n > 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	owner := ""
	_ = st.db.QueryRow(`SELECT uid FROM users WHERE role='owner' ORDER BY created LIMIT 1`).Scan(&owner)
	if _, err := st.db.Exec(`INSERT INTO communities(id,slug,name,owner,open,code,created) VALUES(1,'main',?,?,1,?,?)`, name, owner, st.freeCode(), now); err != nil {
		return err
	}
	_, _ = st.db.Exec(`UPDATE channels SET comm=1`)
	_, _ = st.db.Exec(`INSERT OR IGNORE INTO memberships(comm,uid,joined) SELECT 1, uid, created FROM users WHERE pub != ''`)
	if err := st.defaultRoles(1); err != nil {
		return err
	}
	// The old server-wide mods and admins keep their powers here as roles
	var admin, mod int64
	_ = st.db.QueryRow(`SELECT id FROM roles WHERE comm=1 AND name='Admin'`).Scan(&admin)
	_ = st.db.QueryRow(`SELECT id FROM roles WHERE comm=1 AND name='Moderator'`).Scan(&mod)
	_, _ = st.db.Exec(`INSERT OR IGNORE INTO member_roles(comm,uid,role) SELECT 1, uid, ? FROM users WHERE role='admin'`, admin)
	_, _ = st.db.Exec(`INSERT OR IGNORE INTO member_roles(comm,uid,role) SELECT 1, uid, ? FROM users WHERE role='mod'`, mod)
	_, _ = st.db.Exec(`UPDATE users SET role='' WHERE role='mod'`)
	return nil
}

func (st *store) hasColumn(table, col string) bool {
	rows, err := st.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk) == nil && name == col {
			return true
		}
	}
	return false
}

func (st *store) defaultRoles(comm int64) error {
	_, err := st.db.Exec(`INSERT INTO roles(comm,name,color,pos,perms,everyone) VALUES(?,?,?,?,?,1),(?,?,?,?,?,0),(?,?,?,?,?,0)`,
		comm, "@everyone", 0, 0, int64(everyonePerms),
		comm, "Moderator", 0xFF3BA55D, 10, int64(modPerms),
		comm, "Admin", 0xFFE0A33C, 20, int64(adminPerms))
	return err
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(name string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-")
	}
	if s == "" {
		s = "c"
	}
	return s
}

func scanCommunity(row interface{ Scan(...any) error }) (*community, error) {
	c := &community{}
	var open int
	if err := row.Scan(&c.ID, &c.Slug, &c.Name, &c.About, &c.Icon, &c.Owner, &open, &c.Code, &c.Created); err != nil {
		return nil, err
	}
	c.Open = open == 1
	return c, nil
}

const commCols = `id,slug,name,about,icon,owner,open,code,created`

func (st *store) community(id int64) (*community, error) {
	return scanCommunity(st.db.QueryRow(`SELECT `+commCols+` FROM communities WHERE id=?`, id))
}

func (st *store) communityBySlug(slug string) (*community, error) {
	return scanCommunity(st.db.QueryRow(`SELECT `+commCols+` FROM communities WHERE slug=?`, slug))
}

func (st *store) queryCommunities(q string, args ...any) ([]community, error) {
	rows, err := st.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []community
	for rows.Next() {
		c, err := scanCommunity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		_ = st.db.QueryRow(`SELECT COUNT(*) FROM memberships WHERE comm=?`, out[i].ID).Scan(&out[i].Members)
	}
	return out, nil
}

// communitiesOf: those uid is a member of, in the order joined.
func (st *store) communitiesOf(uid string) ([]community, error) {
	return st.queryCommunities(`SELECT c.id,c.slug,c.name,c.about,c.icon,c.owner,c.open,c.code,c.created FROM communities c JOIN memberships m ON m.comm=c.id WHERE m.uid=? ORDER BY m.joined`, uid)
}

// openCommunities: those anyone may join, the biggest first.
func (st *store) openCommunities(limit int) ([]community, error) {
	list, err := st.queryCommunities(`SELECT ` + commCols + ` FROM communities WHERE open=1`)
	if err != nil {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Members > list[j].Members })
	if len(list) > limit {
		list = list[:limit]
	}
	return list, nil
}

func (st *store) ownedBy(uid string) int {
	var n int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM communities WHERE owner=?`, uid).Scan(&n)
	return n
}

// createCommunity makes a community with its roles, two channels and its owner as the first member.
func (st *store) createCommunity(name, owner string, open bool) (*community, error) {
	base := slugify(name)
	slug := base
	for i := 2; ; i++ {
		var n int
		_ = st.db.QueryRow(`SELECT COUNT(*) FROM communities WHERE slug=?`, slug).Scan(&n)
		if n == 0 {
			break
		}
		slug = fmt.Sprintf("%s-%d", base, i)
		if i > 500 {
			return nil, errors.New("no free name")
		}
	}
	now := time.Now().UnixMilli()
	o := 0
	if open {
		o = 1
	}
	res, err := st.db.Exec(`INSERT INTO communities(slug,name,owner,open,code,created) VALUES(?,?,?,?,?,?)`, slug, name, owner, o, st.freeCode(), now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := st.defaultRoles(id); err != nil {
		return nil, err
	}
	if _, err := st.db.Exec(`INSERT INTO channels(parent,name,pos,is_default,comm,kind) VALUES(0,'general',-10,0,?,'text'),(0,'Lobby',0,1,?,''),(0,'AFK',100,0,?,'')`, id, id, id); err != nil {
		return nil, err
	}
	if err := st.join(id, owner); err != nil {
		return nil, err
	}
	return st.community(id)
}

func (st *store) saveCommunity(c *community) error {
	o := 0
	if c.Open {
		o = 1
	}
	_, err := st.db.Exec(`UPDATE communities SET name=?, about=?, icon=?, open=?, owner=? WHERE id=?`, c.Name, c.About, c.Icon, o, c.Owner, c.ID)
	return err
}

func (st *store) deleteCommunity(id int64) error {
	for _, q := range []string{
		`DELETE FROM messages WHERE conv IN (SELECT 'c:' || id FROM channels WHERE comm=?)`,
		`DELETE FROM mods WHERE kind='ch-ban' AND ch IN (SELECT id FROM channels WHERE comm=?)`,
		`DELETE FROM channels WHERE comm=?`,
		`DELETE FROM messages WHERE conv='g:' || ?`,
		`DELETE FROM mods WHERE kind IN ('ban','mute','vmute') AND ch=?`,
		`UPDATE user_state SET last_comm=0 WHERE last_comm=?`, `DELETE FROM member_roles WHERE comm=?`, `DELETE FROM roles WHERE comm=?`,
		`DELETE FROM memberships WHERE comm=?`, `DELETE FROM bots WHERE comm=?`, `DELETE FROM invites WHERE comm=?`, `DELETE FROM comm_requests WHERE comm=?`, `DELETE FROM communities WHERE id=?`,
	} {
		if _, err := st.db.Exec(q, id); err != nil && !strings.Contains(err.Error(), "no such table") {
			return err
		}
	}
	return nil
}

// sharesCommunity: a and b are members of at least one community together.
func (st *store) sharesCommunity(a, b string) bool {
	var n int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM memberships x JOIN memberships y ON x.comm=y.comm WHERE x.uid=? AND y.uid=?`, a, b).Scan(&n)
	return n > 0
}

func (st *store) isMember(comm int64, uid string) bool {
	var n int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM memberships WHERE comm=? AND uid=?`, comm, uid).Scan(&n)
	return n > 0
}

func (st *store) join(comm int64, uid string) error {
	_, err := st.db.Exec(`INSERT OR IGNORE INTO memberships(comm,uid,joined) VALUES(?,?,?)`, comm, uid, time.Now().UnixMilli())
	return err
}

func (st *store) leave(comm int64, uid string) error {
	if _, err := st.db.Exec(`DELETE FROM member_roles WHERE comm=? AND uid=?`, comm, uid); err != nil {
		return err
	}
	_, err := st.db.Exec(`DELETE FROM memberships WHERE comm=? AND uid=?`, comm, uid)
	return err
}

func (st *store) rolesOf(comm int64) ([]crole, error) {
	rows, err := st.db.Query(`SELECT id,comm,name,color,pos,perms,everyone FROM roles WHERE comm=? ORDER BY pos DESC, id`, comm)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []crole
	for rows.Next() {
		var r crole
		var p int64
		var every int
		if err := rows.Scan(&r.ID, &r.Comm, &r.Name, &r.Color, &r.Pos, &p, &every); err != nil {
			return nil, err
		}
		r.p, r.Everyone = perm(p), every == 1
		r.Perms = permList(r.p)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (st *store) saveRole(r *crole) error {
	if r.ID == 0 {
		res, err := st.db.Exec(`INSERT INTO roles(comm,name,color,pos,perms) VALUES(?,?,?,?,?)`, r.Comm, r.Name, r.Color, r.Pos, int64(r.p))
		if err != nil {
			return err
		}
		r.ID, _ = res.LastInsertId()
		return nil
	}
	_, err := st.db.Exec(`UPDATE roles SET name=?, color=?, pos=?, perms=? WHERE id=? AND comm=?`, r.Name, r.Color, r.Pos, int64(r.p), r.ID, r.Comm)
	return err
}

func (st *store) deleteRole(comm, id int64) error {
	if _, err := st.db.Exec(`DELETE FROM member_roles WHERE comm=? AND role=?`, comm, id); err != nil {
		return err
	}
	_, err := st.db.Exec(`DELETE FROM roles WHERE comm=? AND id=? AND everyone=0`, comm, id)
	return err
}

// memberRoles: the role ids of everyone in comm who has some (uid → ids).
func (st *store) memberRoles(comm int64) (map[string][]int64, error) {
	rows, err := st.db.Query(`SELECT uid, role FROM member_roles WHERE comm=?`, comm)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]int64{}
	for rows.Next() {
		var u string
		var r int64
		if rows.Scan(&u, &r) == nil {
			out[u] = append(out[u], r)
		}
	}
	return out, rows.Err()
}

func (st *store) setMemberRoles(comm int64, uid string, ids []int64) error {
	if _, err := st.db.Exec(`DELETE FROM member_roles WHERE comm=? AND uid=?`, comm, uid); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := st.db.Exec(`INSERT OR IGNORE INTO member_roles(comm,uid,role) VALUES(?,?,?)`, comm, uid, id); err != nil {
			return err
		}
	}
	return nil
}

// ====== Who may what: kept in memory per community, worked out again when roles change ======

type commAccess struct {
	comm   *community
	roles  map[int64]crole
	every  crole
	member map[string][]int64
	bots   map[string]perm // the registered bots' permissions (bots.go)
}

// access gives (and keeps) what the hub needs to decide permissions in comm; h.mu held.
func (h *hub) access(comm int64) *commAccess {
	if a := h.acc[comm]; a != nil {
		return a
	}
	a := &commAccess{roles: map[int64]crole{}}
	a.comm, _ = h.s.store.community(comm)
	if a.comm == nil {
		a.comm = &community{ID: comm}
	}
	rs, _ := h.s.store.rolesOf(comm)
	for _, r := range rs {
		a.roles[r.ID] = r
		if r.Everyone {
			a.every = r
		}
	}
	a.member, _ = h.s.store.memberRoles(comm)
	if a.member == nil {
		a.member = map[string][]int64{}
	}
	a.bots = map[string]perm{}
	if bs, _ := h.s.store.botsIn(comm); bs != nil {
		for _, b := range bs {
			a.bots[b.UID] = b.p
		}
	}
	h.acc[comm] = a
	return a
}

// forget drops what's kept of comm (its roles or owner changed); h.mu held.
func (h *hub) forget(comm int64) { delete(h.acc, comm) }

// staff: the server's own owners and admins.
func (h *hub) staff(uid string) bool { return h.s.store.role(uid).atLeast(roleAdmin) }

// permsOf: uid's permissions in comm; h.mu held.
func (h *hub) permsOf(comm int64, uid string) perm {
	a := h.access(comm)
	if p, ok := a.bots[uid]; ok {
		return p // exactly what its manager gave it, roles or not
	}
	if h.staff(uid) || (a.comm.Owner != "" && a.comm.Owner == uid) {
		return pAll
	}
	p := a.every.p
	for _, id := range a.member[uid] {
		p |= a.roles[id].p
	}
	if p&pAdministrator != 0 {
		return pAll
	}
	return p
}

// rank: how high uid stands in comm (owner and staff above every role); h.mu held.
func (h *hub) rank(comm int64, uid string) int {
	a := h.access(comm)
	if _, ok := a.bots[uid]; ok {
		return botRank
	}
	if h.staff(uid) {
		return 1 << 30
	}
	if a.comm.Owner != "" && a.comm.Owner == uid {
		return 1 << 29
	}
	top := 0
	for _, id := range a.member[uid] {
		if r, ok := a.roles[id]; ok && r.Pos > top {
			top = r.Pos
		}
	}
	return top
}

// legacyRole is the simple role the apps showed before roles came ("owner", "admin", "mod" or ""); h.mu held.
func (h *hub) legacyRole(comm int64, uid string) role {
	a := h.access(comm)
	switch p := h.permsOf(comm, uid); {
	case h.staff(uid) && h.s.store.role(uid) == roleOwner, a.comm.Owner != "" && a.comm.Owner == uid:
		return roleOwner
	case p&pAdministrator != 0 || p == pAll:
		return roleAdmin
	case p&(pMove|pKick|pMuteChat|pChannelBan|pDeleteMessages) != 0:
		return roleMod
	}
	return roleMember
}

// may: does the session hold permission p in its community; h.mu held.
func (h *hub) may(ss *session, p perm) bool { return h.permsOf(ss.comm, ss.uid)&p == p }

// outranks: may actor act on uid (strictly higher in the community; never on oneself); h.mu held.
func (h *hub) outranks(actor *session, uid string) bool {
	return actor.uid != uid && h.rank(actor.comm, actor.uid) > h.rank(actor.comm, uid)
}

func spaceOf(comm int64) string {
	if comm == 1 {
		return mainSpace
	}
	return fmt.Sprintf("c%d", comm)
}

func commOfSpace(space string) int64 {
	if space == mainSpace {
		return 1
	}
	var id int64
	if _, err := fmt.Sscanf(space, "c%d", &id); err != nil {
		return 0
	}
	return id
}
