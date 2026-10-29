package main

// Device linking (docs/LINKING.md in the apps' repository): a short-lived meeting place where two apps run a PAKE
// (CPace) with a code only they know, then pass sealed messages. The server only relays opaque bytes: it never sees
// the code's secret part, and what it relays can't be opened or tested against guesses offline.
//
// /v1/link is a WebSocket that needs no sign-in (a phone just installed has no identity yet, and the server may have
// a password): no member is created, nothing is kept on disk, and everything is small and short-lived.
//
//	opener → {"t":"link-open"}                     ← {"t":"link-opened","id":"K7Q","ttl":300}
//	joiner → {"t":"link-join","id":"K7Q","m":…}    ← {"t":"link-joined"};  opener ← {"t":"link-peer","m":…}
//	either → {"t":"link-msg","m":…}                the other ← {"t":"link-msg","m":…}
//	either → {"t":"link-close","why":…,"peer":…}   the other ← {"t":"link-closed","why":…}
//	refusals: {"t":"link-error","why":"gone|busy|tries|rate|size|bad|full|state"}
//
// One rendezvous per opener socket, at most one joiner at a time, at most linkMaxJoins joins in all (each join is
// one guess at the code), linkTTL from its opening, linkMaxMsgs relayed messages, linkMaxMsg bytes each. Per
// address: a few sockets at once, and opens and joins on a budget.

import (
	"crypto/rand"
	"encoding/json"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	linkAlphabet   = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base32, as the apps
	linkIDLen      = 3
	linkMaxJoins   = 3
	linkMaxMsgs    = 24
	linkMaxMsg     = 32 << 10
	linkMaxOpen    = 2000       // rendezvous open at once on the whole server
	linkPerIP      = 6          // sockets at once from one address
	linkOpenRate   = 10.0 / 600 // per address: 10 opens in 10 minutes (a burst of 5)
	linkOpenBurst  = 5
	linkJoinRate   = 20.0 / 600 // per address: 20 joins in 10 minutes (a burst of 8)
	linkJoinBurst  = 8
	linkSessionMax = 6 // joins on one socket
)

var linkTTL = 5 * time.Minute

type linkConn struct {
	ws  *websocket.Conn
	wmu sync.Mutex
	ip  string
}

func (c *linkConn) send(v any) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = c.ws.WriteJSON(v)
}

type rendezvous struct {
	id     string
	opener *linkConn
	joiner *linkConn
	joins  int
	msgs   int
	until  time.Time
	timer  *time.Timer
}

type linkHub struct {
	mu    sync.Mutex
	open  map[string]*rendezvous
	byIP  map[string]int
	opens map[string]*bucket
	joins map[string]*bucket
}

func newLinkHub() *linkHub {
	return &linkHub{open: map[string]*rendezvous{}, byIP: map[string]int{}, opens: map[string]*bucket{}, joins: map[string]*bucket{}}
}

func (l *linkHub) budget(m map[string]*bucket, ip string, rate, burst float64) bool {
	b := m[ip]
	if b == nil {
		b = &bucket{}
		m[ip] = b
	}
	ok := b.take(rate, burst)
	if len(m) > 10000 { // forget the full ones now and then (a new address starts with a full burst)
		for k, v := range m {
			if v.tokens >= burst-1 && time.Since(v.at) > 10*time.Minute {
				delete(m, k)
			}
		}
	}
	return ok
}

func linkID() string {
	b := make([]byte, linkIDLen)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(linkAlphabet))))
		b[i] = linkAlphabet[n.Int64()]
	}
	return string(b)
}

// closeLocked ends a rendezvous: the other side told, both sockets closed (the apps start again with a new code).
func (l *linkHub) closeLocked(r *rendezvous, why string) {
	if l.open[r.id] == r {
		delete(l.open, r.id)
	}
	if r.timer != nil {
		r.timer.Stop()
	}
	for _, c := range []*linkConn{r.opener, r.joiner} {
		if c != nil {
			go func(c *linkConn) {
				c.send(map[string]any{"t": "link-closed", "why": why})
				_ = c.ws.Close()
			}(c)
		}
	}
	r.opener, r.joiner = nil, nil
}

// joinerLeftLocked: the joiner went (or gave up); the opener is told, and once no try is left the rendezvous ends.
func (l *linkHub) joinerLeftLocked(r *rendezvous, why string) {
	r.joiner = nil
	if r.opener == nil {
		return
	}
	if r.joins >= linkMaxJoins {
		l.closeLocked(r, "tries")
		return
	}
	op := r.opener
	go op.send(map[string]any{"t": "link-closed", "why": why, "peer": true})
}

func (s *server) handleLink(w http.ResponseWriter, r *http.Request) {
	l := s.link
	ip := requestIP(r)
	l.mu.Lock()
	if l.byIP[ip] >= linkPerIP {
		l.mu.Unlock()
		http.Error(w, "too many connections from your address", http.StatusTooManyRequests)
		return
	}
	l.byIP[ip]++
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.byIP[ip]--; l.byIP[ip] <= 0 {
			delete(l.byIP, ip)
		}
		l.mu.Unlock()
	}()
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(linkMaxMsg + 1024)
	c := &linkConn{ws: ws, ip: ip}
	var mine *rendezvous   // opened by this socket
	var joined *rendezvous // joined by this socket
	joinsHere := 0
	refuse := func(why string) { c.send(map[string]any{"t": "link-error", "why": why}) }
	defer func() {
		l.mu.Lock()
		if mine != nil && mine.opener == c {
			l.closeLocked(mine, "gone")
		}
		if joined != nil && joined.joiner == c {
			l.joinerLeftLocked(joined, "gone")
		}
		l.mu.Unlock()
	}()
	deadline := time.Now().Add(linkTTL + 30*time.Second) // a socket lives no longer than a rendezvous
	for {
		_ = ws.SetReadDeadline(deadline)
		var m struct {
			T    string          `json:"t"`
			ID   string          `json:"id"`
			M    json.RawMessage `json:"m"`
			Why  string          `json:"why"`
			Peer bool            `json:"peer"`
		}
		if ws.ReadJSON(&m) != nil {
			return
		}
		// What is relayed is one string (the apps' base64), passed on as it came
		var payload string
		if len(m.M) > 0 && json.Unmarshal(m.M, &payload) != nil {
			refuse("bad")
			continue
		}
		if len(payload) > linkMaxMsg {
			refuse("size")
			continue
		}
		why := m.Why
		if len(why) > 32 {
			why = why[:32]
		}
		l.mu.Lock()
		switch m.T {
		case "link-open":
			switch {
			case mine != nil || joined != nil:
				l.mu.Unlock()
				refuse("state")
				continue
			case len(l.open) >= linkMaxOpen:
				l.mu.Unlock()
				refuse("full")
				continue
			case !l.budget(l.opens, ip, linkOpenRate, linkOpenBurst):
				l.mu.Unlock()
				refuse("rate")
				continue
			}
			id := linkID()
			for l.open[id] != nil {
				id = linkID()
			}
			rv := &rendezvous{id: id, opener: c, until: time.Now().Add(linkTTL)}
			rv.timer = time.AfterFunc(linkTTL, func() {
				l.mu.Lock()
				defer l.mu.Unlock()
				if l.open[id] == rv {
					l.closeLocked(rv, "expired")
				}
			})
			l.open[id] = rv
			mine = rv
			l.mu.Unlock()
			c.send(map[string]any{"t": "link-opened", "id": id, "ttl": int(linkTTL / time.Second)})
		case "link-join":
			rv := l.open[m.ID]
			var why string
			switch {
			case mine != nil || joined != nil:
				why = "state"
			case joinsHere >= linkSessionMax || !l.budget(l.joins, ip, linkJoinRate, linkJoinBurst):
				why = "rate"
			case rv == nil || rv.opener == nil:
				why = "gone"
			case rv.joiner != nil:
				why = "busy"
			case rv.joins >= linkMaxJoins:
				why = "tries"
			case payload == "":
				why = "bad"
			}
			joinsHere++
			if why != "" {
				l.mu.Unlock()
				refuse(why)
				continue
			}
			rv.joins++
			rv.joiner = c
			rv.msgs++
			joined = rv
			op := rv.opener
			l.mu.Unlock()
			c.send(map[string]any{"t": "link-joined"})
			op.send(map[string]any{"t": "link-peer", "m": payload})
		case "link-msg":
			var to *linkConn
			rv := mine
			if rv == nil {
				rv = joined
			}
			if rv != nil && rv.opener != nil && rv.joiner != nil && (rv.opener == c || rv.joiner == c) {
				if rv.msgs >= linkMaxMsgs {
					l.mu.Unlock()
					refuse("rate")
					continue
				}
				rv.msgs++
				to = rv.opener
				if to == c {
					to = rv.joiner
				}
			}
			l.mu.Unlock()
			if to == nil || payload == "" {
				refuse("state")
				continue
			}
			to.send(map[string]any{"t": "link-msg", "m": payload})
		case "link-close":
			switch {
			case mine != nil && mine.opener == c && m.Peer && mine.joiner != nil:
				// The opener sends this joiner away (its code was wrong); the rendezvous waits for another try
				j := mine.joiner
				mine.joiner = nil
				go func() {
					j.send(map[string]any{"t": "link-closed", "why": why})
					_ = j.ws.Close()
				}()
				if mine.joins >= linkMaxJoins {
					l.closeLocked(mine, "tries")
					l.mu.Unlock()
					return
				}
				l.mu.Unlock()
				continue
			case mine != nil && mine.opener == c:
				l.closeLocked(mine, why)
			case joined != nil && joined.joiner == c:
				l.joinerLeftLocked(joined, why)
			}
			l.mu.Unlock()
			return
		default:
			l.mu.Unlock()
			refuse("bad")
		}
	}
}
