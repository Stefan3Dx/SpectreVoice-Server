package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// until reads until a message of type t (skipping others, like the puts everyone gets).
func (c *busConn) until(t string) map[string]any {
	c.t.Helper()
	for i := 0; i < 50; i++ {
		if m := c.read(); m["t"] == t {
			return m
		}
	}
	c.t.Fatalf("no %q", t)
	return nil
}

func TestModerationReports(t *testing.T) {
	bt := newBusTest(t, nil, nil)
	me, bad := newUID(t), newUID(t)
	u := bt.dial("pub", me)
	for _, m := range []map[string]any{
		{"t": "report", "uid": bad, "why": "nope"},
		{"t": "report", "uid": me, "why": "spam"},
		{"t": "report", "uid": "bad uid!", "why": "spam"},
	} {
		u.write(m)
		if r := u.until("reported"); r["ok"] != false || r["why"] != "bad" {
			t.Fatalf("%v: %v", m, r)
		}
	}
	u.write(map[string]any{"t": "report", "uid": bad, "name": "Troll", "why": "abuse", "text": strings.Repeat("x", 900), "where": "profile"})
	if r := u.until("reported"); r["ok"] != true {
		t.Fatalf("report: %v", r)
	}
	// Only the admin reads them
	u.write(map[string]any{"t": "reports"})
	if m := u.until("error"); m["why"] != "admin" {
		t.Fatalf("user reads: %v", m)
	}
	a := bt.dialAdmin("pub", "ADMIN")
	a.write(map[string]any{"t": "reports"})
	list := a.until("reports")["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list: %v", list)
	}
	r := list[0].(map[string]any)
	if r["target"] != bad || r["from"] != me || r["why"] != "abuse" || r["space"] != "pub" || len([]rune(r["text"].(string))) != reportTextLimit {
		t.Fatalf("report kept: %v", r)
	}
	a.write(map[string]any{"t": "report-done", "id": r["id"]})
	if l := a.until("reports")["list"].([]any); len(l) != 0 {
		t.Fatalf("closed: %v", l)
	}
	// At most reportsPerHour an hour
	for i := 0; i < reportsPerHour-1; i++ {
		u.write(map[string]any{"t": "report", "uid": bad, "why": "spam"})
		if r := u.until("reported"); r["ok"] != true {
			t.Fatalf("report %d: %v", i, r)
		}
	}
	u.write(map[string]any{"t": "report", "uid": bad, "why": "spam"})
	if r := u.until("reported"); r["why"] != "rate" {
		t.Fatalf("rate: %v", r)
	}
	// Kept across restarts
	if len(newReports(bt.s.bus.dir).all()) != reportsPerHour-1 {
		t.Fatal("reports not on disk")
	}
}

func TestModerationWipe(t *testing.T) {
	bt := newBusTest(t, nil, nil)
	dir := bt.s.bus.dir
	me, other := newUID(t), newUID(t)
	// Chat kept by the server: theirs (channel and private) goes, the others' stays
	lobby := int64(1)
	for _, m := range []*chatMsg{
		{To: chanConv(lobby), From: me, Name: "Me", Text: "mine", At: 1},
		{To: chanConv(lobby), From: other, Name: "O", Text: "theirs", At: 2},
		{To: dmConv(me, other), From: other, Name: "O", Text: "to me", At: 3},
	} {
		if err := bt.s.store.addMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	bt.s.store.seen(me, "pub", "Me")
	// Another space, closed by the time of the wipe (on disk)
	c := bt.dial("old", me)
	c.write(map[string]any{"t": "put", "kind": "profiles", "id": me, "v": map[string]any{"bio": "old"}})
	c.until("put")
	c.ws.Close()
	bt.s.bus.conns.Wait()
	// The space open now
	u := bt.dial("now", me)
	o := bt.dial("now", other)
	u.write(map[string]any{"t": "put", "kind": "profiles", "id": me, "v": map[string]any{"bio": "hi"}})
	u.until("put")
	u.write(map[string]any{"t": "put", "kind": "scores", "id": busFeedPrefix + "1", "v": map[string]any{"e": "🎉", "x": "mine"}})
	u.until("put")
	o.write(map[string]any{"t": "put", "kind": "scores", "id": busFeedPrefix + "2", "v": map[string]any{"e": "🎉", "x": "theirs"}})
	u.until("put")
	o.write(map[string]any{"t": "put", "kind": "events", "id": "plan", "v": map[string]any{"title": "cs", "r": map[string]any{me: "yes", other: "no"}}})
	u.until("put")
	bt.s.bus.mu.Lock()
	now := bt.s.bus.spaces["now"]
	bt.s.bus.mu.Unlock()
	bt.s.bus.addDonation(now, me, "Me", 25, 1, "x")
	u.until("put")
	// Not someone else's
	o.write(map[string]any{"t": "wipe", "uid": me})
	if m := o.until("error"); m["why"] != "admin" {
		t.Fatalf("wipe by a user: %v", m)
	}
	u.write(map[string]any{"t": "wipe-me"})
	w := u.until("wiped")
	// 4 records in the open space, 1 on disk, 2 chat messages
	if w["uid"] != me || w["n"].(float64) < 7 {
		t.Fatalf("wiped: %v", w)
	}
	bt.s.bus.mu.Lock()
	_, prof := now.state["profiles"][me]
	_, mine := now.state["scores"][busFeedPrefix+"1"]
	_, theirs := now.state["scores"][busFeedPrefix+"2"]
	plan := decodeRecord(now.state["events"]["plan"])
	donor := decodeRecord(now.state["donors"][me])
	bt.s.bus.mu.Unlock()
	if prof || mine || !theirs {
		t.Fatalf("profile %v, my feed line %v, theirs %v", prof, mine, theirs)
	}
	if r := plan["r"].(map[string]any); r[me] != nil || r[other] != "no" {
		t.Fatalf("plan: %v", plan)
	}
	if donor["n"] != "Anonim" || len(donor["d"].([]any)) != 1 {
		t.Fatalf("donation: %v", donor)
	}
	// The chat
	if h, _ := bt.s.store.history(chanConv(lobby), 0, 10); len(h) != 1 || h[0].From != other {
		t.Fatalf("channel chat after the wipe: %v", h)
	}
	if h, _ := bt.s.store.history(dmConv(me, other), 0, 10); len(h) != 0 {
		t.Fatalf("private chat after the wipe: %v", h)
	}
	if n := bt.s.store.userName(me); n != "" {
		t.Fatalf("user row kept: %q", n)
	}
	// The closed space on disk too
	data, err := os.ReadFile(spaceFile(dir, "old"))
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]map[string]json.RawMessage
	_ = json.Unmarshal(data, &st)
	if _, has := st["profiles"][me]; has {
		t.Fatal("old space not wiped")
	}
	// An admin wipes someone else
	a := bt.dialAdmin("now", "ADMIN")
	a.write(map[string]any{"t": "wipe", "uid": other})
	if w := a.until("wiped"); w["uid"] != other || w["n"].(float64) < 2 {
		t.Fatalf("admin wipe: %v", w)
	}
	a.write(map[string]any{"t": "wipe", "uid": "bad uid!"})
	if m := a.until("error"); m["why"] != "bad" {
		t.Fatalf("bad uid: %v", m)
	}
}

func TestDeletePageAndFiles(t *testing.T) {
	bt := newBusTest(t, nil, func(s *server) {
		s.files = &fileStore{dir: filepath.Join(s.cfg.data, "files"), maxSize: 1 << 20, quota: 10 << 20, spaceQuota: 100}
		_ = os.MkdirAll(s.files.dir, 0o755)
	})
	s, srv := bt.s, bt.srv.URL
	r, _ := http.Get(srv + "/delete")
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(body), `action="/delete"`) || strings.Contains(string(body), "{{MESSAGE}}") {
		t.Fatalf("page: %d", r.StatusCode)
	}
	r, _ = http.PostForm(srv+"/delete", url.Values{"uid": {"bad uid"}})
	r.Body.Close()
	if r.StatusCode != 400 {
		t.Fatalf("bad uid: %d", r.StatusCode)
	}
	for i := 0; i < deletesPerHour; i++ {
		r, _ = http.PostForm(srv+"/delete", url.Values{"uid": {"Abc+/="}, "name": {"<b>x</b>"}, "contact": {"a@b.ro"}})
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != 200 || strings.Contains(string(body), "<b>x</b>") {
			t.Fatalf("request %d: %d", i, r.StatusCode)
		}
	}
	r, _ = http.PostForm(srv+"/delete", url.Values{"uid": {"Abc+/="}})
	r.Body.Close()
	if r.StatusCode != 429 {
		t.Fatalf("rate: %d", r.StatusCode)
	}
	if l := s.bus.reports.all(); len(l) != deletesPerHour || l[0].Kind != "delete" || l[0].Contact != "a@b.ro" || l[0].Target != "Abc+/=" {
		t.Fatalf("requests: %v", l)
	}
	// A space's quota for files, and an admin removing one
	userTok := url.QueryEscape(bt.token("U", false))
	up := func(space string, n int) (int, string) {
		req, _ := http.NewRequest("POST", srv+"/files?token="+userTok+"&name=a.txt&space="+url.QueryEscape(space), strings.NewReader(strings.Repeat("x", n)))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if code, _ := up("a", 80); code != 200 {
		t.Fatalf("first: %d", code)
	}
	if code, _ := up("a", 80); code != http.StatusInsufficientStorage {
		t.Fatalf("over the space's quota: %d", code)
	}
	code, out := up("b", 80)
	if code != 200 {
		t.Fatalf("another space: %d", code)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(out), &m)
	link := m["url"].(string)
	if g, _ := http.Get(srv + link); g.StatusCode != 200 {
		t.Fatalf("get: %d", g.StatusCode)
	}
	del := func(q string) int {
		req, _ := http.NewRequest("DELETE", srv+link+q, nil)
		res, _ := http.DefaultClient.Do(req)
		res.Body.Close()
		return res.StatusCode
	}
	if c := del(""); c != 401 {
		t.Fatalf("delete without a token: %d", c)
	}
	if c := del("?token=" + userTok); c != 401 {
		t.Fatalf("delete by a member: %d", c)
	}
	if c := del("?token=" + url.QueryEscape(bt.token("BOSS", true))); c != 204 {
		t.Fatalf("delete: %d", c)
	}
	if g, _ := http.Get(srv + link); g.StatusCode != 404 {
		t.Fatalf("after delete: %d", g.StatusCode)
	}
	// The mark that says the space isn't served
	parts := strings.Split(link, "/")
	if g, _ := http.Get(srv + "/f/" + parts[2] + "/.space"); g.StatusCode != 404 {
		t.Fatalf(".space served: %d", g.StatusCode)
	}
}

func TestBusRateLimit(t *testing.T) {
	bt := newBusTest(t, nil, nil)
	a := bt.dial("rate", "A")
	for i := 0; i < int(busRateBurst)+20; i++ {
		a.write(map[string]any{"t": "wave"})
	}
	if m := a.until("error"); m["why"] != "rate" {
		t.Fatalf("rate: %v", m)
	}
}
