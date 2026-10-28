package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func hasPerm(m map[string]any, p string) bool {
	l, _ := m["perms"].([]any)
	for _, x := range l {
		if x == p {
			return true
		}
	}
	return false
}

// pinged: everything c was sent before this is in (a pong comes after it).
func pinged(c *tclient) {
	c.t.Helper()
	n := float64(time.Now().UnixNano() % 1000000)
	c.send(map[string]any{"t": "ping", "n": n})
	c.wait(func(m map[string]any) bool { return m["t"] == "pong" && m["n"] == n })
}

func ownerKeyCfg(t *testing.T) (*ecdsa.PrivateKey, func(*config)) {
	k := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return k, func(c *config) { c.owners = uidOf(spki) }
}

func TestSpaceRolesInvites(t *testing.T) {
	ownerKey, own := ownerKeyCfg(t)
	s, hs := newTestServer(t, own)
	ana, _ := dialV(t, hs, ownerKey, "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	// Everyone is in the server's one space, named as the server
	srv := bob.wel["server"].(map[string]any)
	if srv["name"] != "Test" || srv["password"] != false || bob.wel["communities"] != nil {
		t.Fatal(bob.wel)
	}
	// The messages of several communities do nothing now (no error either)
	for _, m := range []map[string]any{{"t": "community-create", "name": "Mine"}, {"t": "enter", "c": 2}, {"t": "communities"},
		{"t": "community-find", "code": "AAAA-BBBB"}, {"t": "discover"}} {
		bob.send(m)
	}
	pinged(bob)
	for _, m := range bob.seen {
		if m["t"] == "error" || m["t"] == "community" || m["t"] == "communities" {
			t.Fatal(m)
		}
	}
	var n int
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM communities`).Scan(&n)
	if n != 1 {
		t.Fatal("communities:", n)
	}

	// An invite to the server (and its Lobby)
	lobby := chanID(t, ana.wel, "Lobby")
	ana.send(map[string]any{"t": "invite-create", "hours": 24, "uses": 1, "ch": lobby})
	inv := ana.wait(isT("invite"))
	code := inv["code"].(string)
	if !strings.HasSuffix(inv["url"].(string), "/i/"+code) || inv["pass"] != false {
		t.Fatal(inv)
	}
	res, _ := http.Get(hs.URL + "/api/invite/" + code)
	var info inviteInfo
	_ = json.NewDecoder(res.Body).Decode(&info)
	res.Body.Close()
	if res.StatusCode != 200 || !info.Valid || info.Server != "Test" || info.Community.Name != "Test" || info.Password {
		t.Fatal(res.StatusCode, info)
	}
	res, _ = http.Get(hs.URL + "/i/" + code)
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(page), "Test") || !strings.Contains(string(page), "spectrevoice://") {
		t.Fatal(res.StatusCode, string(page))
	}
	// Carl comes with it in his hello: counted, and into its channel
	carl, _ := dialVWith(t, hs, newKey(t), "Carl", map[string]any{"invite": code})
	if carl == nil {
		t.Fatal("refused with an invite")
	}
	if got, _ := s.store.invite(code); got.Uses != 1 {
		t.Fatal(got)
	}
	// Opened while on the server: counted, into its channel
	ana.send(map[string]any{"t": "invite-create", "ch": lobby})
	again := ana.wait(isT("invite"))["code"].(string)
	bob.send(map[string]any{"t": "invite-use", "invite": again})
	pinged(bob)
	if got, _ := s.store.invite(again); got.Uses != 1 {
		t.Fatal("invite-use not counted", got)
	}
	// Used up: Dan still gets onto the open server, the invite just doesn't count
	if dan, e := dialVWith(t, hs, newKey(t), "Dan", map[string]any{"invite": code}); dan == nil {
		t.Fatal(e)
	}

	// Bob may not make channels; with a role that may, he can
	bob.send(map[string]any{"t": "ch-save", "name": "Nope"})
	if e := bob.wait(isT("error")); e["op"] != "ch-save" {
		t.Fatal(e)
	}
	ana.send(map[string]any{"t": "role-save", "name": "Builder", "color": 0xFF00AAFF, "pos": 5, "perms": []string{"manage_channels"}})
	roles := ana.wait(isT("roles"))
	var builder int64
	for _, r := range roles["items"].([]any) {
		if r.(map[string]any)["name"] == "Builder" {
			builder = int64(r.(map[string]any)["id"].(float64))
		}
	}
	if builder == 0 {
		t.Fatal(roles)
	}
	ana.send(map[string]any{"t": "member-roles", "uid": bob.uid, "roles": []int64{builder}})
	bob.wait(func(m map[string]any) bool { return m["t"] == "role" && hasPerm(m, "manage_channels") })
	bob.send(map[string]any{"t": "ch-save", "name": "Bob's"})
	if c := bob.wait(isT("channel"))["c"].(map[string]any); int64(c["comm"].(float64)) != spaceID {
		t.Fatal(c)
	}
	// But Bob can't give himself more than he has, nor touch Ana, nor rename the server
	bob.send(map[string]any{"t": "role-save", "name": "Boss", "perms": []string{"administrator"}})
	if e := bob.wait(isT("error")); e["op"] != "role-save" {
		t.Fatal(e)
	}
	bob.send(map[string]any{"t": "kick", "sid": ana.sid})
	if e := bob.wait(isT("error")); e["op"] != "kick" {
		t.Fatal(e)
	}
	bob.send(map[string]any{"t": "server-save", "name": "Bob's server"})
	if e := bob.wait(isT("error")); e["op"] != "server-save" || e["why"] != "role" {
		t.Fatal(e)
	}
	// The server chat: everyone
	bob.send(map[string]any{"t": "say", "to": "g:1", "text": "salut server"})
	carl.wait(func(m map[string]any) bool {
		return m["t"] == "msg" && m["m"].(map[string]any)["text"] == "salut server"
	})
	if !s.mayUseSpace(mainSpace, bob.uid) || s.knownSpace("c2") {
		t.Fatal("bus spaces")
	}

	// The owner renames the server: everyone hears, the name stays
	ana.send(map[string]any{"t": "server-save", "name": "Clanul Ana", "about": "salut"})
	si := bob.wait(isT("server-info"))["server"].(map[string]any)
	if si["name"] != "Clanul Ana" || si["about"] != "salut" {
		t.Fatal(si)
	}
	if s.serverName() != "Clanul Ana" {
		t.Fatal(s.serverName())
	}
	s2 := &server{cfg: s.cfg, store: s.store}
	if err := s2.loadSettings(); err != nil || s2.serverName() != "Clanul Ana" {
		t.Fatal("name not kept", err, s2.serverName())
	}

	// Ana bans Bob: he's out, and can't come back
	ana.send(map[string]any{"t": "ban", "uid": bob.uid})
	if k := bob.wait(isT("kicked")); k["ban"] != true {
		t.Fatal(k)
	}
	if s.store.isMember(spaceID, bob.uid) {
		t.Fatal("still a member")
	}
	if _, e := dialV(t, hs, bob.key, "Bob", ""); e == nil || e["why"] != "banned" {
		t.Fatal("banned person came back:", e)
	}
}

// The server password: from the environment, then changed by an admin in the app (kept hashed, over the
// environment); an invite may skip it, and who came in that way comes back without it.
func TestServerPasswordFirstClass(t *testing.T) {
	ownerKey, own := ownerKeyCfg(t)
	s, hs := newTestServer(t, func(c *config) { own(c); c.joinPass = "s3cret" })
	if _, e := dialV(t, hs, newKey(t), "X", ""); e == nil || e["why"] != "password" {
		t.Fatal(e)
	}
	ana, _ := dialV(t, hs, ownerKey, "Ana", "") // the owner needs none
	if ana == nil || ana.wel["server"].(map[string]any)["password"] != true {
		t.Fatal("owner", ana)
	}
	bobKey := newKey(t)
	bob, _ := dialV(t, hs, bobKey, "Bob", "s3cret")
	if bob == nil {
		t.Fatal("refused with the right password")
	}
	// A member can't change it
	bob.send(map[string]any{"t": "server-save", "password": "mine"})
	if e := bob.wait(isT("error")); e["why"] != "role" {
		t.Fatal(e)
	}
	// The owner does: kept as a hash, the environment's no longer works
	ana.send(map[string]any{"t": "server-save", "password": "nou-parola"})
	if si := bob.wait(isT("server-info"))["server"].(map[string]any); si["password"] != true {
		t.Fatal(si)
	}
	if v, ok := s.store.setting("password"); !ok || !strings.HasPrefix(v, "$2") {
		t.Fatal("not hashed:", v)
	}
	if _, e := dialV(t, hs, newKey(t), "Y", "s3cret"); e == nil || e["why"] != "password" {
		t.Fatal("the old password still works", e)
	}
	if c, _ := dialV(t, hs, newKey(t), "Z", "nou-parola"); c == nil {
		t.Fatal("the new password doesn't work")
	}
	s2 := &server{cfg: s.cfg, store: s.store}
	if err := s2.loadSettings(); err != nil || !s2.passwordOK("nou-parola") || s2.passwordOK("s3cret") {
		t.Fatal("password not kept", err)
	}

	// Invites: a plain one doesn't skip the password; only who may change it makes one that does
	bob.send(map[string]any{"t": "invite-create", "pass": true})
	if e := bob.wait(isT("error")); e["op"] != "invite-create" || e["why"] != "role" {
		t.Fatal(e)
	}
	bob.send(map[string]any{"t": "invite-create"})
	plain := bob.wait(isT("invite"))["code"].(string)
	if _, e := dialVWith(t, hs, newKey(t), "P", map[string]any{"invite": plain}); e == nil || e["why"] != "password" {
		t.Fatal("a plain invite skipped the password", e)
	}
	ana.send(map[string]any{"t": "invite-create", "pass": true, "uses": 1})
	inv := ana.wait(isT("invite"))
	code := inv["code"].(string)
	if inv["pass"] != true {
		t.Fatal(inv)
	}
	res, _ := http.Get(hs.URL + "/api/invite/" + code)
	var info inviteInfo
	_ = json.NewDecoder(res.Body).Decode(&info)
	res.Body.Close()
	if info.Password || !info.Bypass {
		t.Fatal(info)
	}
	carlKey := newKey(t)
	carl, e := dialVWith(t, hs, carlKey, "Carl", map[string]any{"invite": code})
	if carl == nil {
		t.Fatal("the invite didn't skip the password", e)
	}
	_ = carl.ws.Close()
	// Back later without the invite (and no password): let in
	if c, e := dialV(t, hs, carlKey, "Carl", ""); c == nil {
		t.Fatal("not remembered", e)
	}
	// The invite was for one person
	if _, e := dialVWith(t, hs, newKey(t), "Q", map[string]any{"invite": code}); e == nil || e["why"] != "password" {
		t.Fatal("a used-up invite skipped the password", e)
	}

	// No password at all: anyone
	ana.send(map[string]any{"t": "server-save", "password": ""})
	if si := bob.wait(isT("server-info"))["server"].(map[string]any); si["password"] != false {
		t.Fatal(si)
	}
	if c, e := dialV(t, hs, newKey(t), "R", ""); c == nil {
		t.Fatal("refused on an open server", e)
	}
}

// A server from before with more communities: the first is the space; the others stay in the database untouched
// but nothing of them is reachable.
func TestOtherCommunitiesHidden(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.fileMB = 10 })
	st := s.store
	now := time.Now().UnixMilli()
	_, _ = st.db.Exec(`INSERT INTO communities(id,slug,name,owner,open,created) VALUES(2,'old','Old club','',0,?)`, now)
	_, _ = st.db.Exec(`INSERT INTO channels(parent,name,pos,is_default,comm,kind) VALUES(0,'Old lobby',0,1,2,'')`)
	var oldCh int64
	_ = st.db.QueryRow(`SELECT id FROM channels WHERE comm=2`).Scan(&oldCh)
	_, _ = st.db.Exec(`INSERT INTO messages(conv,from_uid,from_name,text,at) VALUES(?, 'U', 'U', 'old secret', ?), ('g:2','U','U','old club chat',?)`, chanConv(oldCh), now, now)
	_, _ = st.db.Exec(`INSERT INTO invites(code,comm,channel,by,created) VALUES('OldInvite1',2,0,'U',?)`, now)
	botKey := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&botKey.PublicKey)
	_ = st.saveBot(&botRec{UID: uidOf(spki), Comm: 2, Name: "Oldbot", Created: now})
	st.migrateOneSpace()

	// A fresh hub (as after the update's restart) loads only the space's channels
	h2, err := newHub(s)
	if err != nil {
		t.Fatal(err)
	}
	if h2.chans[oldCh] != nil {
		t.Fatal("hidden channel loaded")
	}
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	for _, c := range ana.wel["channels"].([]any) {
		if c.(map[string]any)["name"] == "Old lobby" {
			t.Fatal("hidden channel shown")
		}
	}
	ana.send(map[string]any{"t": "join", "ch": oldCh})
	if e := ana.wait(isT("error")); e["op"] != "join" {
		t.Fatal(e)
	}
	for _, to := range []string{chanConv(oldCh), "g:2"} {
		ana.send(map[string]any{"t": "history", "to": to})
		if e := ana.wait(isT("error")); e["op"] != "history" {
			t.Fatal(to, e)
		}
	}
	// Its invites and bots don't work; its bus space is unknown
	res, _ := http.Get(hs.URL + "/api/invite/OldInvite1")
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatal("hidden invite:", res.StatusCode)
	}
	if _, e := dialV(t, hs, botKey, "x", ""); e == nil || e["why"] != "bot" {
		t.Fatal("hidden bot came in:", e)
	}
	if s.knownSpace("c2") {
		t.Fatal("hidden bus space")
	}
	req, _ := http.NewRequest("POST", hs.URL+"/cf/upload?ch="+itoa(int(oldCh))+"&folder=0&name=a.txt", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+ana.tok)
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode < 400 {
		t.Fatal("upload into a hidden channel", res, err)
	}
	// Nothing was deleted
	var msgs, chans int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conv IN (?, 'g:2')`, chanConv(oldCh)).Scan(&msgs)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM channels WHERE comm=2`).Scan(&chans)
	if msgs != 2 || chans != 1 {
		t.Fatal("hidden data changed", msgs, chans)
	}
}

func TestMigrationToOneSpace(t *testing.T) {
	dir := t.TempDir()
	st, err := openStore(filepath.Join(dir, "spectre.db"))
	if err != nil {
		t.Fatal(err)
	}
	// A server from before communities: a mod and an admin by the old roles
	_, _ = st.db.Exec(`INSERT INTO users(uid,pub,name,role,created,seen) VALUES('M','p','Mo','mod',1,1),('A','p','Al','admin',2,2),('O','p','Ow','owner',0,0),('X','p','X','',3,3)`)
	st.roles = map[string]role{"M": roleMod, "A": roleAdmin, "O": roleOwner}
	if err := st.migrateCommunities("Vechiul"); err != nil {
		t.Fatal(err)
	}
	c, err := st.community(1)
	if err != nil || c.Name != "Vechiul" || c.Owner != "O" || !c.Open {
		t.Fatal(c, err)
	}
	for _, u := range []string{"M", "A", "O", "X"} {
		if !st.isMember(1, u) {
			t.Fatal("not a member", u)
		}
	}
	mr, _ := st.memberRoles(1)
	rs, _ := st.rolesOf(1)
	name := map[int64]string{}
	for _, r := range rs {
		name[r.ID] = r.Name
	}
	if len(mr["M"]) != 1 || name[mr["M"][0]] != "Moderator" || len(mr["A"]) != 1 || name[mr["A"][0]] != "Admin" {
		t.Fatal(mr, name)
	}
	chs, _ := st.channels()
	for _, ch := range chs {
		if ch.Comm != 1 {
			t.Fatal(ch)
		}
	}
	// Running again changes nothing
	if err := st.migrateCommunities("Vechiul"); err != nil {
		t.Fatal(err)
	}
	st.close()
}

func TestPushForOffline(t *testing.T) {
	// A fake Google: the token endpoint and FCM
	var mu sync.Mutex
	var got []map[string]any
	var fake *httptest.Server
	fake = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			_, _ = io.WriteString(w, `{"access_token":"at","expires_in":3600}`)
		case strings.HasSuffix(r.URL.Path, "/messages:send"):
			if r.Header.Get("Authorization") != "Bearer at" {
				http.Error(w, "no", 401)
				return
			}
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			mu.Lock()
			got = append(got, m)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"name":"x"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	cred, _ := json.Marshal(map[string]string{"project_id": "proj", "client_email": "sa@proj", "token_uri": fake.URL + "/token",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))})
	credFile := filepath.Join(t.TempDir(), "sa.json")
	_ = os.WriteFile(credFile, cred, 0o600)

	s, hs := newTestServer(t, func(c *config) { c.pushCreds = credFile })
	s.cfg.pushContent = false
	s.push.api = fake.URL
	// Bob's phone registers, then goes away
	bobKey := newKey(t)
	bob, _ := dialV(t, hs, bobKey, "Bob", "")
	bob.send(map[string]any{"t": "push-token", "token": "tok-bob-0123456789abcdef", "platform": "android"})
	time.Sleep(200 * time.Millisecond)
	_ = bob.ws.Close()
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	time.Sleep(300 * time.Millisecond)
	// A private message and a mention reach his phone
	ana.send(map[string]any{"t": "say", "to": "u:" + bob.uid, "text": "salut"})
	ana.send(map[string]any{"t": "say", "to": "c:1", "text": "hai @Bob pe voce"})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("pushes: %v", got)
	}
	kinds := map[string]bool{}
	for _, g := range got {
		msg := g["message"].(map[string]any)
		if msg["token"] != "tok-bob-0123456789abcdef" {
			t.Fatal(msg)
		}
		kinds[msg["data"].(map[string]any)["kind"].(string)] = true
	}
	if !kinds["dm"] || !kinds["mention"] {
		t.Fatal(kinds)
	}
}
