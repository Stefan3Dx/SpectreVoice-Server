package main

// Bots: programs other people write against this server (docs/BOTS.md), each with its own key like an app's. A
// server manager registers the key's uid, and from then on a connection with that key is a bot, with the
// permissions it was given (never more than the manager's own, never the ones that decide who may what), shown
// with the robot icon, outside the places count, on a slower message budget, heard in voice only when it may
// speak, never asked for the server password, and never the cause of a push notification.
//
//	<- {"t":"bot-add","uid":…,"name":…,"perms":[…]}   (manage_community) -> {"t":"bot-added","bot":{…}} to everyone
//	<- {"t":"bot-del","uid":…}                          (manage_community) -> {"t":"bot-deleted","uid":…} to everyone
//	<- {"t":"bots"}                                     (manage_community) -> {"t":"bots","items":[{uid,name,perms,kind,by,created}]}
//
// Kept in the store's "bots" table (comm, uid, name, perms, added_by, created; comm is the space, 1); a bot is a
// member of the server like anyone, so the chat, the bus and the files see it as one. Its bus kind is
// "bot:<slug of its name>", which only it (and the server's managers) may write and everyone may read.

import (
	"log"
	"regexp"
	"strings"
	"time"
)

// Vars so tests can change them.
var (
	botRatePerSec  float64 = 10 // messages a second a bot may send (people: sessRatePerSec)
	botRateBurst   float64 = 30
	maxBotsPerComm         = 10 // registered on the server
	maxBotSessions         = 64 // bot connections on the whole server at once
)

// botNever: what a bot is never given, whoever registers it: the permissions that change who may what, and the
// administrator bit (it expands to everything).
const botNever = pAdministrator | pManageCommunity | pManageRoles

// botRank: where a bot stands for kicks and moves: above people with no role, below every role.
const botRank = 1

type botRec struct {
	UID     string   `json:"uid"`
	Comm    int64    `json:"-"`
	Name    string   `json:"name"`
	Perms   []string `json:"perms"`
	Kind    string   `json:"kind"` // its bus kind, "bot:<slug>"
	By      string   `json:"by"`
	Created int64    `json:"created"`
	p       perm
}

func (st *store) migrateBots() error {
	_, err := st.db.Exec(`CREATE TABLE IF NOT EXISTS bots(uid TEXT PRIMARY KEY, comm INTEGER NOT NULL, name TEXT NOT NULL, perms INTEGER NOT NULL DEFAULT 0, added_by TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS bots_comm ON bots(comm)`)
	return err
}

func scanBot(row interface{ Scan(...any) error }) (*botRec, error) {
	b := &botRec{}
	var p int64
	if err := row.Scan(&b.UID, &b.Comm, &b.Name, &p, &b.By, &b.Created); err != nil {
		return nil, err
	}
	b.p = perm(p)
	b.Perms = permList(b.p)
	b.Kind = botKind(b.Name)
	return b, nil
}

const botCols = `uid,comm,name,perms,added_by,created`

// botOf: the registration of uid, nil when it isn't a bot.
func (st *store) botOf(uid string) *botRec {
	b, err := scanBot(st.db.QueryRow(`SELECT `+botCols+` FROM bots WHERE uid=?`, uid))
	if err != nil {
		return nil
	}
	return b
}

// botsIn: comm's bots, the oldest first.
func (st *store) botsIn(comm int64) ([]botRec, error) {
	rows, err := st.db.Query(`SELECT `+botCols+` FROM bots WHERE comm=? ORDER BY created, uid`, comm)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []botRec{}
	for rows.Next() {
		b, err := scanBot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

func (st *store) saveBot(b *botRec) error {
	_, err := st.db.Exec(`INSERT INTO bots(uid,comm,name,perms,added_by,created) VALUES(?,?,?,?,?,?) ON CONFLICT(uid) DO UPDATE SET comm=excluded.comm, name=excluded.name, perms=excluded.perms, added_by=excluded.added_by`,
		b.UID, b.Comm, b.Name, int64(b.p), b.By, b.Created)
	return err
}

func (st *store) deleteBot(uid string) error {
	_, err := st.db.Exec(`DELETE FROM bots WHERE uid=?`, uid)
	return err
}

// isPerson: uid has been on this server as a person (connected with its key, or a member somewhere): such a key
// can't be turned into a bot, which would shut that person into one community.
func (st *store) isPerson(uid string) bool {
	var n int
	_ = st.db.QueryRow(`SELECT (SELECT COUNT(*) FROM users WHERE uid=? AND pub != '') + (SELECT COUNT(*) FROM memberships WHERE uid=?1) + (SELECT COUNT(*) FROM communities WHERE owner=?1)`, uid).Scan(&n)
	return n > 0 || st.role(uid) != roleMember
}

var botSlugRe = regexp.MustCompile(`^bot:[a-z0-9-]{1,32}$`)

// botKind: the bus kind a bot named name writes.
func botKind(name string) string { return "bot:" + slugify(name) }

// botOps: the bot messages; false when t isn't one.
func (h *hub) botOp(ss *session, t string, m map[string]any) bool {
	if t != "bot-add" && t != "bot-del" && t != "bots" {
		return false
	}
	st := h.s.store
	h.mu.Lock()
	comm := ss.comm // read once, under the lock (a ban may move this session meanwhile)
	allowed := comm != 0 && !ss.api && h.may(ss, pManageCommunity)
	mine := h.permsOf(comm, ss.uid)
	h.mu.Unlock()
	if !allowed {
		ss.refuse(t, "role", "not allowed")
		return true
	}
	switch t {
	case "bots":
		items, err := st.botsIn(comm)
		if err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		ss.send(map[string]any{"t": "bots", "items": items})
	case "bot-add":
		uid := str(m, "uid")
		name := cleanName(str(m, "name"))
		switch {
		case !uidRe.MatchString(uid) || uid == ss.uid:
			ss.refuse(t, "bad", "bad uid")
			return true
		case name == "" || !botSlugRe.MatchString(botKind(name)):
			ss.refuse(t, "bad", "a bot needs a name (letters or digits in it)")
			return true
		}
		// A bot of a community this server no longer has (hidden, space.go) is registered here anew
		old := st.botOf(uid)
		if old == nil && st.isPerson(uid) {
			ss.refuse(t, "taken", "that key belongs to someone on this server")
			return true
		}
		others, _ := st.botsIn(comm)
		for _, o := range others {
			if o.UID != uid && o.Kind == botKind(name) {
				ss.refuse(t, "taken", "a bot here has that name")
				return true
			}
		}
		if (old == nil || old.Comm != comm) && len(others) >= maxBotsPerComm {
			ss.refuse(t, "full", "too many bots here")
			return true
		}
		// Nobody gives what they don't have, and nothing that decides who may what
		b := &botRec{UID: uid, Comm: comm, Name: name, By: ss.uid, Created: time.Now().UnixMilli(), p: permOf(m["perms"]) & mine &^ botNever}
		if old != nil {
			b.Created = old.Created
		}
		if err := st.saveBot(b); err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		if err := st.join(comm, uid); err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		b = st.botOf(uid)
		if b == nil {
			ss.refuse(t, "server", "not saved")
			return true
		}
		log.Printf("bot %s %q (%v) by %s", uid, name, b.Perms, ss.uid)
		h.mu.Lock()
		// Its sessions, if it's connected already, get the new name and permissions
		for _, o := range h.sessions {
			if o.uid == uid && o.api {
				o.name = name
			}
		}
		h.refreshRoles(comm)
		h.toComm(comm, map[string]any{"t": "bot-added", "bot": b})
		h.mu.Unlock()
	case "bot-del":
		uid := str(m, "uid")
		b := st.botOf(uid)
		if b == nil || b.Comm != comm {
			ss.refuse(t, "gone", "no such bot here")
			return true
		}
		if err := st.deleteBot(uid); err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		_ = st.leave(comm, uid)
		for _, o := range h.sessionsOf(uid) {
			o.send(map[string]any{"t": "kicked", "by": ss.name, "why": "bot removed"})
			time.AfterFunc(300*time.Millisecond, o.close)
		}
		log.Printf("bot %s removed by %s", uid, ss.uid)
		h.mu.Lock()
		h.refreshRoles(comm)
		h.toComm(comm, map[string]any{"t": "bot-deleted", "uid": uid})
		h.mu.Unlock()
	}
	return true
}

// botDenied: what a bot may never ask, whatever its permissions: moving between communities, changing who may
// what, its own name (the manager named it), the server's own things, and the apps' notifications.
var botDenied = map[string]bool{
	"name": true, "community-join": true, "community-save": true, "server-save": true,
	"role-save": true, "role-del": true, "member-roles": true, "role": true, "bot-add": true, "bot-del": true, "bots": true,
	"claim": true, "push-token": true, "push-off": true, "server-ban": true, "server-unban": true, "admin-link": true,
	"slots": true, "slots-redeem": true, "invite-create": true, "invite-del": true, "invites": true, "whisper": true,
}

// botAllowed: may a bot session send t now ("" when it may, else why not).
func (h *hub) botAllowed(ss *session, t string) string {
	if !ss.api {
		return ""
	}
	if botDenied[t] {
		return "not for bots"
	}
	if t == "rtc" || t == "ice" {
		// Voice only for a bot allowed to speak (listening alone would be a tap on the channel)
		h.mu.Lock()
		ok := h.may(ss, pSpeak)
		h.mu.Unlock()
		if !ok {
			return "this bot may not speak"
		}
	}
	return ""
}

// botSessions: bot connections now; h.mu held.
func (h *hub) botSessions() int {
	n := 0
	for _, o := range h.sessions {
		if o.api {
			n++
		}
	}
	return n
}

// botKindOwner: in the space's community, uid is the bot whose kind this is (or the bus kind isn't a bot's).
func (b *bus) ownsBotKind(space, kind, uid string) bool {
	if b.store == nil || !botSlugRe.MatchString(kind) {
		return false
	}
	bot := b.store.botOf(uid)
	return bot != nil && bot.Comm == commOfSpace(space) && bot.Kind == kind
}

// isBotKind: a "bot:<slug>" bus kind (everyone reads it, its bot writes it).
func isBotKind(kind string) bool {
	return strings.HasPrefix(kind, "bot:") && botSlugRe.MatchString(kind)
}
