package main

import (
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

func commID(m map[string]any) int64 {
	c, _ := m["community"].(map[string]any)
	if c == nil {
		return 0
	}
	return int64(c["id"].(float64))
}

func hasPerm(m map[string]any, p string) bool {
	l, _ := m["perms"].([]any)
	for _, x := range l {
		if x == p {
			return true
		}
	}
	return false
}

func TestCommunitiesRolesInvites(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.createCommunities = "anyone" })
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	// Both land in the first, open community
	if commID(ana.wel) != 1 || commID(bob.wel) != 1 {
		t.Fatal(ana.wel["community"], bob.wel["community"])
	}

	// Ana makes her own community: she's its owner, with all permissions, in its Lobby
	ana.send(map[string]any{"t": "community-create", "name": "Clanul Ana", "open": false})
	cm := ana.wait(isT("community"))
	mine := commID(cm)
	if mine == 0 || !hasPerm(cm, "administrator") || len(cm["channels"].([]any)) != 2 {
		t.Fatal(cm)
	}
	// Bob, still in the first community, doesn't see her (nor she him, there)
	bob.wait(func(m map[string]any) bool { return m["t"] == "member-left" })
	// Bob can't join a closed community without an invite
	bob.send(map[string]any{"t": "community-join", "c": mine})
	if e := bob.wait(isT("error")); e["why"] != "closed" {
		t.Fatal(e)
	}
	// An invite to it
	ana.send(map[string]any{"t": "invite-create", "hours": 24, "uses": 1})
	inv := ana.wait(isT("invite"))
	code := inv["code"].(string)
	if !strings.HasSuffix(inv["url"].(string), "/i/"+code) {
		t.Fatal(inv)
	}
	// Its page and its JSON, for anyone
	res, _ := http.Get(hs.URL + "/api/invite/" + code)
	var info inviteInfo
	_ = json.NewDecoder(res.Body).Decode(&info)
	res.Body.Close()
	if res.StatusCode != 200 || !info.Valid || info.Community.Name != "Clanul Ana" || info.Community.Members != 1 {
		t.Fatal(res.StatusCode, info)
	}
	res, _ = http.Get(hs.URL + "/i/" + code)
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(page), "Clanul Ana") || !strings.Contains(string(page), "spectrevoice://") {
		t.Fatal(res.StatusCode, string(page))
	}
	// Bob uses it: he's in, as a plain member
	bob.send(map[string]any{"t": "community-join", "invite": code})
	bc := bob.wait(isT("community"))
	if commID(bc) != mine || hasPerm(bc, "manage_channels") {
		t.Fatal(bc)
	}
	ana.wait(func(m map[string]any) bool { return m["t"] == "member" && m["m"].(map[string]any)["uid"] == bob.uid })
	// The invite was for one use
	carl, _ := dialV(t, hs, newKey(t), "Carl", "")
	carl.send(map[string]any{"t": "community-join", "invite": code})
	if e := carl.wait(isT("error")); e["why"] != "invite" {
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
	if c := bob.wait(isT("channel"))["c"].(map[string]any); int64(c["comm"].(float64)) != mine {
		t.Fatal(c)
	}
	// But Bob can't give himself more than he has, nor touch Ana
	bob.send(map[string]any{"t": "role-save", "name": "Boss", "perms": []string{"administrator"}})
	if e := bob.wait(isT("error")); e["op"] != "role-save" {
		t.Fatal(e)
	}
	bob.send(map[string]any{"t": "kick", "sid": ana.sid})
	if e := bob.wait(isT("error")); e["op"] != "kick" {
		t.Fatal(e)
	}

	// The first community's chat isn't readable from Ana's
	bob.send(map[string]any{"t": "history", "to": "c:1"})
	if e := bob.wait(isT("error")); e["op"] != "history" {
		t.Fatal(e)
	}
	// Switching back: Bob is in two communities
	bob.send(map[string]any{"t": "communities"})
	if l := bob.wait(isT("communities"))["items"].([]any); len(l) != 2 {
		t.Fatal(l)
	}
	bob.send(map[string]any{"t": "enter", "c": 1})
	if commID(bob.wait(isT("community"))) != 1 {
		t.Fatal("enter")
	}
	// The bus: members only
	if !s.mayUseSpace(spaceOf(mine), bob.uid) || s.mayUseSpace(spaceOf(mine), carl.uid) {
		t.Fatal("bus membership")
	}
	// Ana bans Bob from her community: he's out of it
	ana.send(map[string]any{"t": "ban", "uid": bob.uid})
	ana.wait(func(m map[string]any) bool { return m["t"] == "done" && m["op"] == "ban" })
	if s.store.isMember(mine, bob.uid) {
		t.Fatal("still a member")
	}
	bob.send(map[string]any{"t": "enter", "c": mine})
	if e := bob.wait(isT("error")); e["op"] != "enter" {
		t.Fatal(e)
	}
}

func TestMigrationToCommunities(t *testing.T) {
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
