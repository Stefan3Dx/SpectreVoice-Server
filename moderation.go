package main

// Moderation for a public app: anyone may report someone (it reaches the admin, nobody else); the admin reads the
// reports, closes them and can wipe a person's content from every space; anyone may wipe their own (in the app)
// or ask for it on the web (/delete, for Google Play's data deletion link).
//
//	<- {"t":"report","uid":U,"name":…,"why":"spam|abuse|sexual|hate|other","text":…,"where":…}   (signed in)
//	-> {"t":"reported","ok":true} or {"t":"reported","ok":false,"why":"auth|bad|rate"}
//	<- {"t":"reports"}                    (admin) -> {"t":"reports","list":[…]}
//	<- {"t":"report-done","id":…}         (admin) -> {"t":"reports","list":[…]}
//	<- {"t":"wipe","uid":U}               (admin) -> {"t":"wiped","uid":U,"n":records}
//	<- {"t":"wipe-me"}                    (signed in) -> {"t":"wiped","uid":own,"n":records}
//
//	GET  /delete                          the deletion request page
//	POST /delete  uid, name, contact      kept as a request for the admin (also listed with the reports)
//	DELETE /f/<id>?token=…                an owner or admin removes a shared file

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type report struct {
	ID         string `json:"id"`
	At         int64  `json:"at"`
	Kind       string `json:"kind"` // "report" or "delete"
	From       string `json:"from,omitempty"`
	FromName   string `json:"fromName,omitempty"`
	Target     string `json:"target"`
	TargetName string `json:"targetName,omitempty"`
	Space      string `json:"space,omitempty"`
	Why        string `json:"why,omitempty"`
	Text       string `json:"text,omitempty"`
	Where      string `json:"where,omitempty"`
	Contact    string `json:"contact,omitempty"`
}

var reportWhy = map[string]bool{"spam": true, "abuse": true, "sexual": true, "hate": true, "other": true}

const (
	reportsKept     = 500
	reportsPerHour  = 10 // per reporter
	deletesPerHour  = 5  // per address on the web form
	reportTextLimit = 500
)

type reports struct {
	mu   sync.Mutex
	file string // "" in memory only
	list []report
	rate map[string][]time.Time
}

func newReports(dir string) *reports {
	r := &reports{rate: map[string][]time.Time{}}
	if dir != "" {
		r.file = filepath.Join(dir, "reports.json")
		if data, err := os.ReadFile(r.file); err == nil {
			_ = json.Unmarshal(data, &r.list)
		}
	}
	return r
}

// allow says whether who may file another one now (at most per in an hour).
func (r *reports) allow(who string, per int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	recent := r.rate[who][:0]
	for _, t := range r.rate[who] {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	if len(recent) >= per {
		r.rate[who] = recent
		return false
	}
	r.rate[who] = append(recent, now)
	return true
}

func (r *reports) add(x report) {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	x.ID = hex.EncodeToString(b)
	x.At = time.Now().UnixMilli()
	r.mu.Lock()
	r.list = append(r.list, x)
	if len(r.list) > reportsKept {
		r.list = r.list[len(r.list)-reportsKept:]
	}
	r.saveLocked()
	r.mu.Unlock()
	log.Printf("moderation: %s about %s (%s)", x.Kind, x.Target, x.Why)
}

func (r *reports) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.list[:0]
	for _, x := range r.list {
		if x.ID != id {
			kept = append(kept, x)
		}
	}
	r.list = kept
	r.saveLocked()
}

func (r *reports) all() []report {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]report{}, r.list...)
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out
}

func (r *reports) saveLocked() {
	if r.file == "" {
		return
	}
	data, _ := json.Marshal(r.list)
	if err := writeFileAtomic(r.file, data, 0o600); err != nil {
		log.Printf("moderation: reports not saved: %v", err)
	}
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// moderate handles the bus's moderation messages (see the top of the file); false when t isn't one of them.
func (b *bus) moderate(sp *busSpace, c *busClient, t string, m map[string]any) bool {
	switch t {
	case "report":
		answer := func(ok bool, why string) { c.send(map[string]any{"t": "reported", "ok": ok, "why": why}) }
		target, _ := m["uid"].(string)
		why, _ := m["why"].(string)
		switch {
		case !c.authed:
			answer(false, "auth")
		case !safeID.MatchString(target) || target == c.uid || !reportWhy[why]:
			answer(false, "bad")
		case !b.reports.allow("r:"+c.uid, reportsPerHour):
			answer(false, "rate")
		default:
			name, _ := m["name"].(string)
			from, _ := m["from"].(string)
			text, _ := m["text"].(string)
			where, _ := m["where"].(string)
			b.reports.add(report{Kind: "report", From: c.uid, FromName: clip(from, 40), Target: target, TargetName: clip(name, 40),
				Space: sp.name, Why: why, Text: clip(text, reportTextLimit), Where: clip(where, 40)})
			answer(true, "")
		}
	case "reports", "report-done":
		if !c.staff {
			c.refuse(m, t, "admin", "only the admin")
			return true
		}
		if id, _ := m["id"].(string); t == "report-done" && id != "" {
			b.reports.remove(id)
		}
		c.send(map[string]any{"t": "reports", "list": b.reports.all()})
	case "wipe", "wipe-me":
		uid, _ := m["uid"].(string)
		if t == "wipe-me" {
			if !c.authed {
				c.refuse(m, t, "auth", "sign in first")
				return true
			}
			uid = c.uid
		} else if !c.staff {
			c.refuse(m, t, "admin", "only the admin")
			return true
		}
		if !safeID.MatchString(uid) {
			c.refuse(m, t, "bad", "bad uid")
			return true
		}
		n := b.wipe(uid)
		if b.store != nil {
			if k, err := b.store.wipeUser(uid); err == nil {
				n += k
			} else {
				log.Printf("moderation: chat of %s not wiped: %v", uid, err)
			}
		}
		if cf := b.chfiles; cf != nil {
			n += cf.dropBy(uid)
		}
		log.Printf("moderation: %s wiped (%d records, asked by %s)", uid, n, c.uid)
		c.send(map[string]any{"t": "wiped", "uid": uid, "n": n})
	default:
		return false
	}
	return true
}

// wipeState takes uid's records out of one space's state; how many changed, and the changes to tell everyone.
func wipeState(state map[string]map[string]json.RawMessage, uid string) (int, []map[string]any) {
	n := 0
	var ops []map[string]any
	del := func(kind, id string) {
		delete(state[kind], id)
		ops = append(ops, map[string]any{"t": "del", "kind": kind, "id": id})
		n++
	}
	put := func(kind, id string, v map[string]any) {
		raw, _ := json.Marshal(v)
		state[kind][id] = raw
		ops = append(ops, map[string]any{"t": "put", "kind": kind, "id": id, "v": json.RawMessage(raw)})
		n++
	}
	// What's theirs by its id
	for _, kind := range []string{"profiles", "intros", "lei", "scores"} {
		if _, ok := state[kind][uid]; ok {
			del(kind, uid)
		}
	}
	// What they wrote: feed lines, shared sounds
	for id, raw := range state["scores"] {
		if strings.HasPrefix(id, busFeedPrefix) && decodeRecord(raw)["_by"] == uid {
			del("scores", id)
		}
	}
	for id, raw := range state["sounds"] {
		if decodeRecord(raw)["_by"] == uid {
			del("sounds", id)
		}
	}
	// Their answers and votes in plans and polls
	for id, raw := range state["events"] {
		rec := decodeRecord(raw)
		changed := false
		for _, k := range []string{"r", "n", "v"} {
			if mm, ok := rec[k].(map[string]any); ok {
				if _, has := mm[uid]; has {
					delete(mm, uid)
					changed = true
				}
			}
		}
		if changed {
			put("events", id, rec)
		}
	}
	// Donations stay (they're payments), without their name or note; a rank given by hand goes
	if raw, ok := state["donors"][uid]; ok {
		rec := decodeRecord(raw)
		if _, note := rec["m"]; rec["n"] != "Anonim" || note {
			rec["n"] = "Anonim"
			delete(rec, "m")
			put("donors", uid, rec)
		}
	}
	// Everything else that names them by uid: daily time and talk counters, weekly winners, tournaments, warnings
	for kind, recs := range state {
		for id, raw := range recs {
			if id == uid {
				continue
			}
			var v any
			if json.Unmarshal(raw, &v) != nil {
				continue
			}
			if scrubUID(v, uid) {
				if m, ok := v.(map[string]any); ok {
					put(kind, id, m)
				}
			}
		}
	}
	if raw, ok := state["donors"]["_ranks"]; ok {
		rec := decodeRecord(raw)
		if _, has := rec[uid]; has {
			delete(rec, uid)
			put("donors", "_ranks", rec)
		}
	}
	return n, ops
}

// scrubUID drops, anywhere inside v, map keys that are uid and entries that name uid ("u"/"uid"); true if it changed.
func scrubUID(v any, uid string) bool {
	changed := false
	names := func(x any) bool {
		m, ok := x.(map[string]any)
		return ok && (m["u"] == uid || m["uid"] == uid)
	}
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if k == uid || names(x) {
				delete(t, k)
				changed = true
			} else if scrubUID(x, uid) {
				changed = true
			}
		}
		for k, x := range t { // lists are values: filtered in place through the map
			if l, ok := x.([]any); ok {
				kept := l[:0]
				for _, e := range l {
					if names(e) {
						changed = true
						continue
					}
					kept = append(kept, e)
				}
				t[k] = kept
			}
		}
	case []any:
		for _, x := range t {
			if scrubUID(x, uid) {
				changed = true
			}
		}
	}
	return changed
}

// wipe removes uid's content from every space: those open now (everyone in them is told) and those on disk.
func (b *bus) wipe(uid string) int {
	b.fileMu.Lock()
	defer b.fileMu.Unlock()
	total := 0
	open := map[string]bool{}
	b.mu.Lock()
	for name, sp := range b.spaces {
		if b.dir != "" {
			open[spaceFile(b.dir, name)] = true
		}
		for k := range busKinds {
			if sp.state[k] == nil {
				sp.state[k] = map[string]json.RawMessage{}
			}
		}
		n, ops := wipeState(sp.state, uid)
		if n > 0 {
			sp.size = 0
			for _, items := range sp.state {
				for _, raw := range items {
					sp.size += len(raw)
				}
			}
			sp.dirty = true
			for _, op := range ops {
				op["from"] = "moderation"
				sp.broadcast(op, nil)
			}
			total += n
		}
	}
	b.mu.Unlock()
	if b.dir == "" {
		return total
	}
	files, _ := filepath.Glob(filepath.Join(b.dir, "*.json"))
	for _, f := range files {
		base := filepath.Base(f)
		if open[f] || len(base) != 29 || !isHex(strings.TrimSuffix(base, ".json")) {
			continue // a space open now (done above), or not a space (orders, reports)
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		state := map[string]map[string]json.RawMessage{}
		if json.Unmarshal(data, &state) != nil {
			continue
		}
		for k := range busKinds {
			if state[k] == nil {
				state[k] = map[string]json.RawMessage{}
			}
		}
		n, _ := wipeState(state, uid)
		if n == 0 {
			continue
		}
		out, _ := json.Marshal(state)
		if err := writeFileAtomic(f, out, 0o644); err != nil {
			log.Printf("moderation: %s not saved: %v", f, err)
			continue
		}
		total += n
	}
	return total
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// The address a web request came from (behind the reverse proxy: the one it says, when it's on our network).
func requestIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || ip.IsLoopback()) {
		if x := strings.TrimSpace(r.Header.Get("X-Real-IP")); x != "" {
			return x
		}
		if x := r.Header.Get("X-Forwarded-For"); x != "" {
			return strings.TrimSpace(strings.Split(x, ",")[0])
		}
	}
	return host
}

//go:embed delete.html
var deletePage string

// handleDelete: the web page for asking that your data be deleted (Google Play's link), and its form.
func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	show := func(status int, msg string) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, strings.ReplaceAll(deletePage, "{{MESSAGE}}", msg))
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		show(http.StatusOK, "")
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		if err := r.ParseForm(); err != nil {
			show(http.StatusBadRequest, `<p class="err">Cererea nu a putut fi citită.</p>`)
			return
		}
		uid := strings.TrimSpace(r.PostForm.Get("uid"))
		name := clip(r.PostForm.Get("name"), 40)
		contact := clip(r.PostForm.Get("contact"), 120)
		if !safeID.MatchString(uid) {
			show(http.StatusBadRequest, `<p class="err">Identificatorul (uid) nu e valid. Îl găsești în aplicație: Setări › Identitate.</p>`)
			return
		}
		if s.bus == nil || !s.bus.reports.allow("d:"+requestIP(r), deletesPerHour) {
			show(http.StatusTooManyRequests, `<p class="err">Prea multe cereri. Încearcă din nou peste o oră.</p>`)
			return
		}
		s.bus.reports.add(report{Kind: "delete", Target: uid, TargetName: name, Contact: contact})
		show(http.StatusOK, fmt.Sprintf(`<p class="ok">Cererea pentru <b>%s</b> a fost primită. Datele se șterg în cel mult 30 de zile.</p>`, html.EscapeString(uid)))
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

// deleteFile: DELETE /f/<id>?token=… (owner or admin) removes a shared file (a reported picture, say).
func (s *server) deleteFile(w http.ResponseWriter, r *http.Request, id string) {
	if !s.isAdmin(r) {
		http.Error(w, "owners and admins only", http.StatusUnauthorized)
		return
	}
	if !hexID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	dir := filepath.Join(s.files.dir, id)
	if _, err := os.Stat(dir); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.files.recount()
	log.Printf("moderation: file %s removed", id)
	w.WriteHeader(http.StatusNoContent)
}
