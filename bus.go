package main

// The bus: Spectre Voice apps in the same community talk to each other through it (reactions on streams,
// watch parties, intros, the shared soundboard, games, Lei, profiles, donors), and it keeps what should last.
//
//	GET /bus?token=T&space=S   (WebSocket; the token from the voice connection's welcome says who you are)
//	-> {"t":"state","state":{kind:{id:value}},"ops":[…],"admin":bool,"version":…}   on connect: what's kept
//	<- {"t":"put","kind":"events","id":"x","v":{...}}    kept and sent to everyone (sender too), stamped "_by": U
//	<- {"t":"del","kind":"events","id":"x"}              forgotten, and sent to everyone
//	<- {"t":"add"|"incr",…}                              changes the server works out itself (see applyAdd/applyIncr)
//	<- {"t":"buy","item":…}                              a shop purchase (shop.go)
//	<- {"t":"anything else", ...}                       passed on to the others, with "from": U and "relay": true
//	-> {"t":"error","op":…,"why":…,"message":…}          a change that was refused (and why)
//
//	PUT/GET /backup/<id>?token=T                          an app's encrypted settings backup (up to 1 MB)
//
// No client is trusted: the server decides who may change what.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// busBotKindLimit: records in one bot's kind ("bot:<slug>", bots.go).
const busBotKindLimit = 500

var busKinds = map[string]int{"events": 200, "sounds": 300, "intros": 500, "lei": 2000, "scores": 2000, "profiles": 2000, "donors": 2000}

// Kinds only the server's owner and admins may change; everyone reads them.
var busAdminKinds = map[string]bool{"donors": true}

// Messages only the server sends: one from a client with these types is never passed on (a forged "state" or
// "put" would make the others believe something the server never said).
var busServerOnly = map[string]bool{"state": true, "put": true, "del": true, "error": true, "challenge": true, "authed": true, "add": true, "incr": true, "reported": true, "wiped": true}

// Backups kept at most, by count and by bytes (vars so tests can make them small).
var (
	busMaxBackups        = 2000
	busBackupBytes int64 = 1 << 30
)

// What the state message tells the apps this server can do.
var busOps = []string{"add", "incr", "buy", "play", "report", "wipe-me"}

// Kinds only the server itself writes (none yet).
var busServerKinds = map[string]bool{}

// How fast one app may send (drawings, games and reactions stay well under it); much faster is a flood.
// Vars so tests can change them.
var (
	busRatePerSec float64 = 40
	busRateBurst  float64 = 200
	busRateDrops          = 400 // refused messages before the connection is let go
)

// allow takes one message from c's allowance; false when it's sending too fast.
func (c *busClient) allow() bool {
	now := time.Now()
	if c.refill.IsZero() {
		c.tokens, c.refill = busRateBurst, now
	}
	c.tokens = min(busRateBurst, c.tokens+now.Sub(c.refill).Seconds()*busRatePerSec)
	c.refill = now
	if c.tokens < 1 {
		c.dropped++
		return false
	}
	c.tokens--
	return true
}

const (
	busMaxValue    = 16 << 10
	busMaxSpace    = 32 << 20 // all of a space's values together, so one space can't eat the memory
	busQueue       = 256      // messages waiting for a client; one that falls this far behind is let go
	busFeedPrefix  = "_feed:"
	busFeedKeep    = 3 * 24 * time.Hour // feed entries anyone may clear once they're this old
	busReadTimeout = 75 * time.Second   // pings go every 25 s: three missed pongs and the connection is gone
)

type busClient struct {
	ws        *websocket.Conn
	uid       string
	admin     bool
	staff     bool        // a server admin (not just a community manager): server-wide wipe and reports
	authed    bool        // proved it is uid (always, now: the token says so)
	out       chan []byte // waiting to be written by writer, so one slow client doesn't hold up the others
	tokens    float64     // messages it may still send now (the read loop's rate limit)
	refill    time.Time   // when tokens was last topped up
	dropped   int         // messages refused for going too fast
	relayLeft float64     // bytes it may still pass on to others (its read loop only)
	relayAt   time.Time
	done      chan struct{}
	once      sync.Once
}

func newBusClient(ws *websocket.Conn, uid string) *busClient {
	return &busClient{ws: ws, uid: uid, out: make(chan []byte, busQueue), done: make(chan struct{})}
}

// close lets the client go: its writer ends and its read loop gets an error.
func (c *busClient) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.ws.Close()
	})
}

// queue hands a message to the client's writer without waiting; a client that can't keep up is dropped.
func (c *busClient) queue(data []byte) {
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.out <- data:
	default:
		log.Printf("bus: %s can't keep up, let go", c.uid)
		c.close()
	}
}

func (c *busClient) send(v any) {
	data, err := json.Marshal(v)
	if err == nil {
		c.queue(data)
	}
}

// refuse tells the client a change of its wasn't made.
func (c *busClient) refuse(m map[string]any, op, why, message string) {
	e := map[string]any{"t": "error", "op": op, "why": why, "message": message}
	for _, k := range []string{"kind", "id"} {
		if v, ok := m[k].(string); ok {
			e[k] = v
		}
	}
	c.send(e)
}

// writer is the only one writing to the WebSocket: queued messages and the keepalive pings.
func (c *busClient) writer() {
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-c.done:
			return
		case data := <-c.out:
			_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.ws.WriteMessage(websocket.TextMessage, data); err != nil {
				c.close()
				return
			}
		case <-ping.C:
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				c.close()
				return
			}
		}
	}
}

type busSpace struct {
	name    string
	clients map[*busClient]bool
	state   map[string]map[string]json.RawMessage
	size    int // bytes of all values
	dirty   bool
}

func (sp *busSpace) set(kind, id string, raw json.RawMessage) {
	if sp.state[kind] == nil {
		sp.state[kind] = map[string]json.RawMessage{} // a bot's kind, made on its first write
	}
	sp.size += len(raw) - len(sp.state[kind][id])
	sp.state[kind][id] = raw
}

func (sp *busSpace) del(kind, id string) {
	sp.size -= len(sp.state[kind][id])
	delete(sp.state[kind], id)
}

// broadcast hands msg to everyone in the space but except. Called with b.mu held, so everyone gets the changes
// in the order they were made; queueing never waits, so holding the lock is cheap.
func (sp *busSpace) broadcast(msg any, except *busClient) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	for c := range sp.clients {
		if c != except {
			c.queue(data)
		}
	}
}

type bus struct {
	fileMu    sync.Mutex // held while a space is read from or written to disk (taken before mu)
	mu        sync.Mutex
	dir       string // "" keeps nothing on disk
	spaces    map[string]*busSpace
	maxSpaces int
	backupMu  sync.Mutex     // one backup written at a time, so the limits hold
	conns     sync.WaitGroup // connections still being handled (tests wait for their last save)
	playV     *playVerifier  // Google Play donations (play.go); nil: not taken
	store     *store         // the server's database (account deletion wipes chat too)
	chfiles   *channelFiles  // and the files they put in channels
	reports   *reports       // reports and deletion requests (moderation.go)
}

func newBus(dir string) *bus {
	b := &bus{dir: dir, spaces: map[string]*busSpace{}, maxSpaces: 64}
	if dir != "" {
		if err := os.MkdirAll(filepath.Join(dir, "backups"), 0o755); err != nil {
			log.Printf("bus: %v (nothing kept on disk)", err)
			b.dir = ""
		}
		go b.saver()
	}
	if b.dir != "" {
		log.Printf("bus: kept in %s", b.dir)
	} else {
		log.Printf("bus: nothing kept on disk (no data folder)")
	}
	b.reports = newReports(b.dir)
	return b
}

func spaceFile(dir, name string) string {
	h := sha256.Sum256([]byte(name))
	return filepath.Join(dir, hex.EncodeToString(h[:12])+".json")
}

var errTooManySpaces = errors.New("too many spaces on this server")

// load returns the named space, reading what was kept (b.fileMu and b.mu held).
func (b *bus) load(name string) (*busSpace, error) {
	if sp := b.spaces[name]; sp != nil {
		return sp, nil
	}
	if len(b.spaces) >= b.maxSpaces {
		return nil, errTooManySpaces
	}
	sp := &busSpace{name: name, clients: map[*busClient]bool{}, state: map[string]map[string]json.RawMessage{}}
	if b.dir != "" {
		f := spaceFile(b.dir, name)
		data, err := os.ReadFile(f)
		switch {
		case err == nil:
			if err := json.Unmarshal(data, &sp.state); err != nil {
				// Starting empty would overwrite it on the next save: it's put aside for a human to look at
				bad := fmt.Sprintf("%s.bad-%s", f, time.Now().Format("20060102-150405"))
				log.Printf("bus: !!! space %q: %s is damaged (%v); moved to %s, the space starts empty", name, f, err, bad)
				if rerr := os.Rename(f, bad); rerr != nil {
					log.Printf("bus: !!! couldn't move %s aside (%v); space %q not opened", f, rerr, name)
					return nil, errors.New("this space's file is damaged")
				}
				sp.state = map[string]map[string]json.RawMessage{}
			}
		case errors.Is(err, fs.ErrNotExist):
		default:
			log.Printf("bus: !!! space %q: %v; not opened", name, err)
			return nil, errors.New("this space can't be read")
		}
	}
	if sp.state == nil {
		sp.state = map[string]map[string]json.RawMessage{}
	}
	for k := range busKinds {
		if sp.state[k] == nil {
			sp.state[k] = map[string]json.RawMessage{}
		}
	}
	for _, items := range sp.state {
		for _, raw := range items {
			sp.size += len(raw)
		}
	}
	b.spaces[name] = sp
	return sp, nil
}

// leave takes c out of its space; the last one out unloads the space (saved first), so spaces nobody is in
// don't stay in memory.
func (b *bus) leave(sp *busSpace, c *busClient) {
	b.fileMu.Lock()
	defer b.fileMu.Unlock()
	b.mu.Lock()
	delete(sp.clients, c)
	// Without a folder the space only lives in memory: unloading it would forget everything
	if len(sp.clients) > 0 || b.dir == "" || b.spaces[sp.name] != sp {
		b.mu.Unlock()
		return
	}
	delete(b.spaces, sp.name)
	var data []byte
	if sp.dirty {
		data, _ = json.Marshal(sp.state)
		sp.dirty = false
	}
	b.mu.Unlock()
	if data != nil {
		if err := writeFileAtomic(spaceFile(b.dir, sp.name), data, 0o644); err != nil {
			log.Printf("bus: !!! space %q not saved: %v", sp.name, err)
		}
	}
}

// drop forgets a space (its community was deleted): its file goes and those in it are let go.
func (b *bus) drop(name string) {
	if b == nil {
		return
	}
	b.fileMu.Lock()
	defer b.fileMu.Unlock()
	b.mu.Lock()
	if sp := b.spaces[name]; sp != nil {
		delete(b.spaces, name)
		sp.dirty = false
		for c := range sp.clients {
			_ = c.ws.Close()
		}
	}
	b.mu.Unlock()
	if b.dir != "" {
		_ = os.Remove(spaceFile(b.dir, name))
	}
}

// saver writes changed spaces every few seconds.
func (b *bus) saver() {
	for range time.Tick(3 * time.Second) {
		b.saveDirty()
	}
}

func (b *bus) saveDirty() {
	// fileMu: a space being unloaded meanwhile isn't overwritten by this older copy, nor read back half-written
	b.fileMu.Lock()
	defer b.fileMu.Unlock()
	b.mu.Lock()
	snaps := map[*busSpace][]byte{}
	for _, sp := range b.spaces {
		if sp.dirty {
			sp.dirty = false
			snaps[sp], _ = json.Marshal(sp.state)
		}
	}
	b.mu.Unlock()
	for sp, data := range snaps {
		if err := writeFileAtomic(spaceFile(b.dir, sp.name), data, 0o644); err != nil {
			log.Printf("bus: space %q not saved: %v (trying again)", sp.name, err)
			b.mu.Lock()
			sp.dirty = true
			b.mu.Unlock()
		}
	}
}

// writeFileAtomic writes data so that a crash leaves either the old file or the new one, never half of one.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, perm)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// The rename itself is only lasting once the folder is synced
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9_.:+/=~-]{1,128}$`)

// stamp marks a value with who wrote it ("_a": they proved it), for the ownership checks.
func stamp(v map[string]any, c *busClient) {
	v["_by"] = c.uid
	if c.authed {
		v["_a"] = true
	} else {
		delete(v, "_a")
	}
}

// forbidden says whether a client may not write this field (or "group.key" path) of a kind's records itself:
// the server's stamps, and what was bought in the shop (only "buy" adds to a profile's "own").
func forbidden(kind, path string) bool {
	top, _, _ := strings.Cut(path, ".")
	return top == "_by" || top == "_a" || (kind == "profiles" && top == "own")
}

// sanitize drops the fields a client may not write from a put's "v" and an add/incr's arguments.
func sanitize(kind string, m map[string]any) {
	if v, ok := m["v"].(map[string]any); ok {
		for k := range v {
			if forbidden(kind, k) {
				delete(v, k)
			}
		}
	}
	for _, arg := range []string{"set", "inc", "max"} {
		if fields, ok := m[arg].(map[string]any); ok {
			for k := range fields {
				if forbidden(kind, k) {
					delete(fields, k)
				}
			}
		}
	}
	if groups, ok := m["weekly"].([]any); ok {
		kept := groups[:0]
		for _, g := range groups {
			if name, ok := g.(string); ok && !forbidden(kind, name) {
				kept = append(kept, g)
			}
		}
		m["weekly"] = kept
	}
}

// mayChange says whether c may change kind/id with op ("" when it may, else why not); b.mu held.
func (sp *busSpace) mayChange(c *busClient, op, kind, id string) string {
	if busAdminKinds[kind] && !c.admin {
		return "admin"
	}
	if c.admin {
		return ""
	}
	raw := sp.state[kind][id]
	rec := decodeRecord(raw)
	by, hasBy := rec["_by"].(string)
	mine := hasBy && by == c.uid
	switch {
	case kind == "lei":
		// Purses change only by a delta the server applies (the tables' keepers pay out and take in); nobody writes
		// or deletes a purse outright, not even their own (the shop trusts it)
		if op != "add" {
			return "owner"
		}
	case kind == "profiles" || kind == "intros":
		if id != c.uid {
			return "owner"
		}
	case kind == "sounds":
		// A sound is its uploader's: others may not replace it; old ones from before "_by" anyone may clear
		if op == "del" {
			if raw != nil && hasBy && !mine {
				return "owner"
			}
		} else if raw != nil && !mine {
			return "owner"
		}
	case kind == "scores" && strings.HasPrefix(id, busFeedPrefix):
		// Feed entries are written once; cleared by who wrote them, or by anyone once they're old
		if op == "del" {
			at, _ := rec["at"].(float64)
			old := time.Since(time.UnixMilli(int64(at))) > busFeedKeep
			if raw != nil && !mine && !old {
				return "owner"
			}
		} else if raw != nil {
			return "owner"
		}
	}
	return ""
}

func (s *server) handleBus(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.tokenUID(r)
	if !ok {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	name := r.URL.Query().Get("space")
	if !s.knownSpace(name) {
		http.Error(w, "no such space", http.StatusNotFound)
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(256 << 10) // party games send drawings
	c := newBusClient(ws, uid)
	c.authed = true
	if !s.mayUseSpace(name, uid) {
		_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = ws.WriteJSON(map[string]any{"t": "error", "op": "join", "why": "member", "message": "not on this server"})
		_ = ws.Close()
		return
	}
	c.staff = s.store.role(uid).atLeast(roleAdmin)
	c.admin = c.staff
	if comm := commOfSpace(name); comm != 0 && s.hub != nil {
		s.hub.mu.Lock()
		c.admin = c.admin || s.hub.permsOf(comm, uid)&pManageCommunity != 0
		s.hub.mu.Unlock()
	}
	b := s.bus
	b.conns.Add(1)
	defer b.conns.Done()
	b.fileMu.Lock()
	b.mu.Lock()
	// One person holds a few spaces at most (spaces are shared by the whole server: they can't all be theirs)
	held := 0
	for _, o := range b.spaces {
		for oc := range o.clients {
			if oc.uid == uid {
				held++
			}
		}
	}
	sp, err := (*busSpace)(nil), error(nil)
	if held >= busSpacesPerUID && !c.staff {
		err = errors.New("you're connected to too many spaces at once")
	} else {
		sp, err = b.load(name)
	}
	if err != nil {
		b.mu.Unlock()
		b.fileMu.Unlock()
		_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = ws.WriteJSON(map[string]any{"t": "error", "op": "join", "why": "space", "message": err.Error()})
		_ = ws.Close()
		return
	}
	sp.clients[c] = true
	snapshot, _ := json.Marshal(sp.state)
	// Queued under the lock: nothing changed after the snapshot can reach this client before it
	c.send(map[string]any{"t": "state", "state": json.RawMessage(snapshot), "version": version, "ops": busOps, "admin": c.admin})
	b.mu.Unlock()
	b.fileMu.Unlock()
	go c.writer()
	defer func() {
		c.close()
		b.leave(sp, c)
	}()
	_ = ws.SetReadDeadline(time.Now().Add(busReadTimeout))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(busReadTimeout)) })
	for {
		var m map[string]any
		if err := ws.ReadJSON(&m); err != nil {
			return
		}
		_ = ws.SetReadDeadline(time.Now().Add(busReadTimeout))
		t, _ := m["t"].(string)
		if t == "" {
			continue
		}
		if !c.admin && !c.allow() {
			if c.dropped == 1 || c.dropped%100 == 0 {
				log.Printf("bus: %s sends too fast (%d refused)", c.uid, c.dropped)
			}
			if c.dropped >= busRateDrops {
				return
			}
			if c.dropped%50 == 1 {
				c.refuse(m, t, "rate", "too many messages, slow down")
			}
			continue
		}
		if busServerOnly[t] && t != "put" && t != "del" && t != "add" && t != "incr" {
			continue // a client posing as the server
		}
		switch t {
		case "put", "del", "add", "incr":
			b.change(sp, c, t, m)
		case "buy":
			item, _ := m["item"].(string)
			b.buy(sp, c, item)
		case "report", "reports", "report-done", "wipe", "wipe-me":
			b.moderate(sp, c, t, m)
		case "play":
			b.conns.Add(1)
			go func() { defer b.conns.Done(); b.play(sp, c, m) }()
		default:
			m["from"] = c.uid
			m["relay"] = true
			// A byte budget, as the hub's relays (128 KB/s, bursts of 1 MB: drawings): one app can't flood the space
			raw, _ := json.Marshal(m)
			now := time.Now()
			c.relayLeft = min(busRelayBurst, c.relayLeft+now.Sub(c.relayAt).Seconds()*busRelayRate)
			c.relayAt = now
			if c.relayLeft < float64(len(raw)) {
				c.refuse(m, t, "rate", "too much, slow down")
				continue
			}
			c.relayLeft -= float64(len(raw))
			b.mu.Lock()
			sp.broadcast(m, c)
			b.mu.Unlock()
		}
	}
}

// change does a client's put, del, add or incr, if it may.
func (b *bus) change(sp *busSpace, c *busClient, op string, m map[string]any) {
	kind, _ := m["kind"].(string)
	id, _ := m["id"].(string)
	limit, ok := busKinds[kind]
	if isBotKind(kind) {
		// A bot's own records (bots.go): the bot writes them, the community's managers may clear them
		limit, ok = busBotKindLimit, true
		if !c.admin && !b.ownsBotKind(sp.name, kind, c.uid) {
			c.refuse(m, op, "owner", "not yours to change")
			return
		}
	}
	if !ok || !safeID.MatchString(id) {
		c.refuse(m, op, "bad", "unknown kind or bad id")
		return
	}
	if busServerKinds[kind] {
		c.refuse(m, op, "server", "only the server writes this")
		return
	}
	sanitize(kind, m)
	b.mu.Lock()
	defer b.mu.Unlock()
	if why := sp.mayChange(c, op, kind, id); why != "" {
		c.refuse(m, op, why, "not yours to change")
		return
	}
	items := sp.state[kind]
	if op == "del" {
		sp.del(kind, id)
		sp.dirty = true
		sp.broadcast(map[string]any{"t": "del", "kind": kind, "id": id, "from": c.uid}, nil)
		return
	}
	var next map[string]any
	switch op {
	case "put":
		v, ok := m["v"].(map[string]any)
		if !ok {
			c.refuse(m, op, "bad", "the value must be an object")
			return
		}
		// What was bought stays when the profile is written again
		if own := decodeRecord(items[id])["own"]; kind == "profiles" && own != nil {
			v["own"] = own
		}
		next = v
	case "add", "incr":
		// Changes the server makes itself, so two at once don't undo each other: a purse's "v" by a delta
		// ("add"), counters in a record ("incr"); everyone then gets the result as a put
		cur := decodeRecord(items[id])
		if op == "add" {
			if kind == "lei" {
				// A game's win or loss, not a fortune: a bounded delta, the apps' own start and floor
				m["d"] = max(-busLeiStep, min(busLeiStep, num(m["d"])))
				m["start"] = max(0, min(busLeiStart, num(m["start"])))
				m["min"] = 0.0
			}
			next = applyAdd(cur, m)
		} else {
			next = applyIncr(cur, m)
		}
		if next == nil {
			return
		}
	}
	stamp(next, c)
	raw, _ := json.Marshal(next)
	if len(raw) > busMaxValue || (items[id] == nil && len(items) >= limit) || sp.size+len(raw)-len(items[id]) > busMaxSpace {
		c.refuse(m, op, "full", "too much")
		return
	}
	sp.set(kind, id, raw)
	sp.dirty = true
	sp.broadcast(map[string]any{"t": "put", "kind": kind, "id": id, "v": json.RawMessage(raw), "from": c.uid}, nil)
}

var backupID = regexp.MustCompile(`^[0-9a-f]{32,64}$`)

// backupRoom says whether a backup of size bytes fits beside the others (replacing f's old one); backupMu held.
func backupRoom(dir, f string, size int64) bool {
	entries, _ := os.ReadDir(dir)
	count, total := 0, int64(0)
	for _, e := range entries {
		if !backupID.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if filepath.Join(dir, e.Name()) == f {
			continue // being replaced
		}
		count++
		total += info.Size()
	}
	return count < busMaxBackups && total+size <= busBackupBytes
}

// pruneBackups drops backups not saved again for a year (forgotten ones don't stay for ever).
func pruneBackups(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 365*24*time.Hour {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func (s *server) handleBackup(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	id := filepath.Base(r.URL.Path)
	if !backupID.MatchString(id) || s.bus.dir == "" {
		http.Error(w, "no", http.StatusBadRequest)
		return
	}
	dir := filepath.Join(s.bus.dir, "backups")
	f := filepath.Join(dir, id)
	switch r.Method {
	case http.MethodPut, http.MethodPost:
		data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
		if err != nil || len(data) > 1<<20 || len(data) == 0 {
			http.Error(w, "too big", http.StatusRequestEntityTooLarge)
			return
		}
		s.bus.backupMu.Lock()
		defer s.bus.backupMu.Unlock()
		pruneBackups(dir)
		if !backupRoom(dir, f, int64(len(data))) {
			http.Error(w, "no room for more backups", http.StatusInsufficientStorage)
			return
		}
		if err := writeFileAtomic(f, data, 0o600); err != nil {
			http.Error(w, "not saved", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	case http.MethodGet:
		data, err := os.ReadFile(f)
		if err != nil {
			http.Error(w, "none", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	case http.MethodDelete:
		// Whoever knows the id (derived from the name and password) may delete it
		s.bus.backupMu.Lock()
		_ = os.Remove(f)
		s.bus.backupMu.Unlock()
		_, _ = io.WriteString(w, "ok\n")
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// applyAdd: {"d": delta, "start": value when new, "min": value when it drops to 0 or below, "set": {fields},
// "once": a value of "d" field that makes it happen only once (a daily bonus)}. nil: nothing to do.
// Lei: the most one change may move, and the most a new purse starts with (the apps' START)
const (
	busLeiStep  = 1_000_000
	busLeiStart = 1000
)

func applyAdd(cur map[string]any, m map[string]any) map[string]any {
	set, _ := m["set"].(map[string]any)
	if once, ok := m["once"].(string); ok && once != "" {
		if cur["d"] == once {
			return nil
		}
		cur["d"] = once
	}
	v, has := cur["v"].(float64)
	if !has {
		v = num(m["start"])
	}
	v += num(m["d"])
	if v <= 0 {
		v = num(m["min"])
	}
	cur["v"] = v
	for k, x := range set {
		if k != "v" && k != "d" {
			cur[k] = x
		}
	}
	return cur
}

// applyIncr: counters "group.key" — {"inc": {...}, "max": {...}, "set": {...}, "week": "2026-W43", "weekly": [groups]}:
// a new week empties the weekly groups first.
func applyIncr(cur map[string]any, m map[string]any) map[string]any {
	if week, ok := m["week"].(string); ok && week != "" && cur["wk"] != week {
		cur["wk"] = week
		if groups, ok := m["weekly"].([]any); ok {
			for _, g := range groups {
				if name, ok := g.(string); ok {
					delete(cur, name)
				}
			}
		}
	}
	group := func(path string) (map[string]any, string) {
		i := strings.IndexByte(path, '.')
		if i <= 0 {
			return nil, ""
		}
		g, ok := cur[path[:i]].(map[string]any)
		if !ok {
			g = map[string]any{}
			cur[path[:i]] = g
		}
		return g, path[i+1:]
	}
	if inc, ok := m["inc"].(map[string]any); ok {
		for path, d := range inc {
			if g, k := group(path); g != nil {
				g[k] = num(g[k]) + num(d)
			}
		}
	}
	if mx, ok := m["max"].(map[string]any); ok {
		for path, d := range mx {
			if g, k := group(path); g != nil && num(d) > num(g[k]) {
				g[k] = num(d)
			}
		}
	}
	if set, ok := m["set"].(map[string]any); ok {
		for path, v := range set {
			if strings.Contains(path, ".") {
				if g, k := group(path); g != nil {
					g[k] = v
				}
			} else {
				cur[path] = v
			}
		}
	}
	return cur
}

// Relayed bytes per client: the rate and the burst (vars: a test floods on purpose).
var (
	busRelayRate  float64 = 128 << 10
	busRelayBurst float64 = 1 << 20
)

// busSpacesPerUID: connections one person may hold at once (a PC and a phone, each in a community or two).
const busSpacesPerUID = 6

// kick closes uid's connections to a space ("": every space), after a ban.
func (b *bus) kick(space, uid string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, sp := range b.spaces {
		if space != "" && name != space {
			continue
		}
		for c := range sp.clients {
			if c.uid == uid {
				c.close()
			}
		}
	}
}
