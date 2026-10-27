package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"
)

func TestAdminPanel(t *testing.T) {
	ls, vendor := fakeLicenseService(t)
	s, hs := newTestServer(t, func(c *config) { c.licenseURL = ls.URL; c.vendorKey = vendor })
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	_ = s.store.setRole(ana.uid, roleOwner)
	s.hub.mu.Lock()
	s.hub.refreshRoles(1)
	s.hub.mu.Unlock()
	// Every server has the panel
	ana.send(map[string]any{"t": "say", "to": "c:" + itoa(int(chanID(t, ana.wel, "Lobby"))), "text": "salut"})
	ana.wait(isT("msg"))
	s.metrics.take()
	time.Sleep(10 * time.Millisecond)
	sm := s.metrics.take()
	b, _ := json.Marshal(sm)
	_, _ = s.store.db.Exec(`INSERT INTO metrics(t,data) VALUES(?,?)`, sm.T, string(b))
	ana.send(map[string]any{"t": "admin-link"})
	link := ana.wait(isT("admin-link"))["url"].(string)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	// Without the link: nothing
	if r, _ := c.Get(hs.URL + "/admin/api/overview"); r.StatusCode != 401 {
		t.Fatal(r.StatusCode)
	}
	url := hs.URL + link[strings.Index(link, "/admin/login"):]
	r, err := c.Get(url)
	if err != nil || r.StatusCode != 200 {
		t.Fatal(err, r.StatusCode)
	}
	page, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(page), "Spectre Voice · Admin") {
		t.Fatal("page")
	}
	// The code worked once
	if r, _ := http.Get(url); r.StatusCode != 403 {
		t.Fatal("code used twice", r.StatusCode)
	}
	get := func(p string) any {
		r, err := c.Get(hs.URL + "/admin/api/" + p)
		if err != nil || r.StatusCode != 200 {
			t.Fatal(p, err, r.StatusCode)
		}
		defer r.Body.Close()
		var v any
		_ = json.NewDecoder(r.Body).Decode(&v)
		return v
	}
	o := get("overview").(map[string]any)
	if o["users"].(float64) < 1 || o["communities"].(float64) != 1 {
		t.Fatal(o)
	}
	if m := get("metrics?h=24").([]any); len(m) == 0 {
		t.Fatal("no metrics")
	}
	if l := get("sessions").([]any); len(l) != 1 {
		t.Fatal(l)
	}
	top := get("top").(map[string]any)
	if len(top["people"].([]any)) != 1 {
		t.Fatal(top)
	}
	_ = get("communities")
	_ = get("logs")
	// Posting needs the panel's header (no cross-site posts)
	req, _ := http.NewRequest("POST", hs.URL+"/admin/api/announce", strings.NewReader(`{"text":"repornesc"}`))
	if r, _ := c.Do(req); r.StatusCode != 400 {
		t.Fatal("csrf", r.StatusCode)
	}
	req, _ = http.NewRequest("POST", hs.URL+"/admin/api/announce", strings.NewReader(`{"text":"repornesc"}`))
	req.Header.Set("X-Admin", "1")
	if r, _ := c.Do(req); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	if a := ana.wait(isT("announce")); a["text"] != "repornesc" {
		t.Fatal(a)
	}
}
