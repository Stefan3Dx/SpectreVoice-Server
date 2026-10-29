package main

// Push notifications (Firebase Cloud Messaging, HTTP v1) for people who aren't connected: private messages,
// mentions (@Name, and @everyone for those allowed), community invites to games are left to the apps.
//
//	<- {"t":"push-token","token":…,"platform":"android"}   this device's FCM token (kept for the uid)
//	<- {"t":"push-off","token":…}                          forget it (sign out, notifications off)
//
// The server needs SV_PUSH_CREDENTIALS: a Firebase service account's key (JSON). The message is a data message,
// so the app shows it its own way; nothing is kept at Google beyond delivery.

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

type pusher struct {
	project  string
	email    string
	tokenURI string
	key      *rsa.PrivateKey
	api      string // https://fcm.googleapis.com (tests point it elsewhere)
	http     *http.Client
	st       *store

	mu      sync.Mutex
	access  string
	expires time.Time
	queue   chan pushJob
}

type pushJob struct {
	uid  string
	data map[string]string
}

func newPusher(credFile string, st *store) (*pusher, error) {
	raw, err := os.ReadFile(credFile)
	if err != nil {
		return nil, err
	}
	var a struct {
		ProjectID   string `json:"project_id"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("service account file: %w", err)
	}
	block, _ := pem.Decode([]byte(a.PrivateKey))
	if block == nil || a.ClientEmail == "" || a.ProjectID == "" {
		return nil, errors.New("service account file has no private_key, client_email or project_id")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("service account key isn't RSA")
	}
	if a.TokenURI == "" {
		a.TokenURI = "https://oauth2.googleapis.com/token"
	}
	if err := st.migratePush(); err != nil {
		return nil, err
	}
	p := &pusher{project: a.ProjectID, email: a.ClientEmail, tokenURI: a.TokenURI, key: rk, api: "https://fcm.googleapis.com",
		http: &http.Client{Timeout: 15 * time.Second}, st: st, queue: make(chan pushJob, 1000)}
	go p.loop()
	return p, nil
}

func (st *store) migratePush() error {
	_, err := st.db.Exec(`CREATE TABLE IF NOT EXISTS push_tokens(token TEXT PRIMARY KEY, uid TEXT NOT NULL, platform TEXT NOT NULL DEFAULT '', updated INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS push_tokens_uid ON push_tokens(uid);`)
	return err
}

func (st *store) pushTokens(uid string) []string {
	rows, err := st.db.Query(`SELECT token FROM push_tokens WHERE uid=?`, uid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			out = append(out, t)
		}
	}
	return out
}

func (p *pusher) accessToken() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.access != "" && time.Now().Before(p.expires) {
		return p.access, nil
	}
	enc := base64.RawURLEncoding
	now := time.Now().Unix()
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iss": p.email, "scope": "https://www.googleapis.com/auth/firebase.messaging",
		"aud": p.tokenURI, "iat": now, "exp": now + 3600})
	unsigned := enc.EncodeToString(head) + "." + enc.EncodeToString(claims)
	h := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	resp, err := p.http.PostForm(p.tokenURI, url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {unsigned + "." + enc.EncodeToString(sig)}})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &t) != nil || t.AccessToken == "" {
		return "", fmt.Errorf("google token: %s", resp.Status)
	}
	p.access = t.AccessToken
	p.expires = time.Now().Add(time.Duration(max(t.ExpiresIn-120, 60)) * time.Second)
	return p.access, nil
}

// notify queues a notification for uid's devices (dropped when the queue is full: it's only a nudge).
func (p *pusher) notify(uid string, data map[string]string) {
	if p == nil {
		return
	}
	select {
	case p.queue <- pushJob{uid, data}:
	default:
	}
}

func (p *pusher) loop() {
	for j := range p.queue {
		for _, tok := range p.st.pushTokens(j.uid) {
			if err := p.send(tok, j.data); err != nil {
				log.Printf("push %s: %v", j.uid, err)
			}
		}
	}
}

var errGoneToken = errors.New("token no longer valid")

func (p *pusher) send(token string, data map[string]string) error {
	at, err := p.accessToken()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"message": map[string]any{
		"token": token, "data": data,
		"android": map[string]any{"priority": "HIGH", "ttl": "86400s"},
	}})
	req, _ := http.NewRequest("POST", p.api+"/v1/projects/"+p.project+"/messages:send", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+at)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == 404 || (resp.StatusCode == 400 && bytes.Contains(b, []byte("UNREGISTERED"))) {
		_, _ = p.st.db.Exec(`DELETE FROM push_tokens WHERE token=?`, token)
		return errGoneToken
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("fcm %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// maxPushTokens: devices kept per person for notifications.
const maxPushTokens = 10

var pushTokenRe = regexp.MustCompile(`^[A-Za-z0-9_:\-.]{20,1000}$`)

func (s *server) pushOp(ss *session, t string, m map[string]any) bool {
	switch t {
	case "push-token":
		tok := str(m, "token")
		if !pushTokenRe.MatchString(tok) {
			ss.refuse(t, "bad", "bad token")
			return true
		}
		if s.push == nil {
			return true // the server doesn't send push notifications: nothing to keep
		}
		_, err := s.store.db.Exec(`INSERT INTO push_tokens(token,uid,platform,updated) VALUES(?,?,?,?) ON CONFLICT(token) DO UPDATE SET uid=excluded.uid, platform=excluded.platform, updated=excluded.updated`,
			tok, ss.uid, clip(str(m, "platform"), 20), time.Now().UnixMilli())
		if err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		// A person's devices, the newest few: tokens made up by the thousand can't fill the database (or make each
		// notification thousands of requests to Google)
		_, _ = s.store.db.Exec(`DELETE FROM push_tokens WHERE uid=? AND token NOT IN (SELECT token FROM push_tokens WHERE uid=? ORDER BY updated DESC, rowid DESC LIMIT ?)`,
			ss.uid, ss.uid, maxPushTokens)
	case "push-off":
		if s.push != nil {
			_, _ = s.store.db.Exec(`DELETE FROM push_tokens WHERE token=? AND uid=?`, str(m, "token"), ss.uid)
		}
	default:
		return false
	}
	return true
}

// online: uid has a connection now.
func (h *hub) online(uid string) bool {
	for _, o := range h.sessions {
		if o.uid == uid {
			return true
		}
	}
	return false
}

var mentionRe = regexp.MustCompile(`@([\p{L}\p{N}_.\-]{1,32})`)

// pushFor tells the offline people a message concerns: the other one of a private chat; those @named in a
// channel (everyone, for @everyone when allowed).
func (h *hub) pushFor(ss *session, msg *chatMsg, ch int64, other string) {
	p := h.s.push
	if p == nil {
		return
	}
	text := msg.Text
	if msg.Enc != "" {
		// End to end encrypted: the server has no words to send, and says only who wrote (the app: "New message from X")
		text = ""
	}
	if len([]rune(text)) > 180 {
		text = string([]rune(text)[:180]) + "…"
	}
	if other != "" {
		h.mu.Lock()
		on := h.online(other)
		h.mu.Unlock()
		if !on {
			p.notify(other, h.s.pushData(map[string]string{"kind": "dm", "title": msg.Name, "body": text, "from": ss.uid, "conv": "u:" + ss.uid}))
		}
		return
	}
	found := mentionRe.FindAllStringSubmatch(msg.Text, 20)
	if len(found) == 0 {
		return
	}
	h.mu.Lock()
	var comm int64
	var cname string
	if ch < 0 {
		comm, cname = -ch, "general"
	} else if c := h.chans[ch]; c != nil {
		comm, cname = c.Comm, c.Name
		if c.pass != "" {
			h.mu.Unlock()
			return // a locked channel: its words don't leave it, not even as a notification
		}
	}
	everyone := h.may(ss, pMentionAll)
	h.mu.Unlock()
	if comm == 0 {
		return
	}
	names, _ := h.s.store.namesIn(comm)
	want := map[string]bool{}
	for _, f := range found {
		n := strings.ToLower(f[1])
		if (n == "everyone" || n == "toti" || n == "toți") && everyone {
			for _, uids := range names {
				for _, u := range uids {
					want[u] = true
				}
			}
			break
		}
		for _, u := range names[n] {
			want[u] = true
		}
	}
	delete(want, ss.uid)
	if len(want) > 500 {
		return
	}
	h.mu.Lock()
	for u := range want {
		if h.online(u) {
			delete(want, u)
		}
	}
	h.mu.Unlock()
	for u := range want {
		p.notify(u, h.s.pushData(map[string]string{"kind": "mention", "title": msg.Name + " · #" + cname, "body": text, "from": ss.uid, "conv": map[bool]string{true: fmt.Sprintf("g:%d", comm), false: fmt.Sprintf("c:%d", ch)}[ch < 0], "comm": fmt.Sprint(comm)}))
	}
}

// pushData: what goes through Google to the phone. By default nothing of the message (who, what): only that there
// is one, and where, so the app opens it from this server. SV_PUSH_CONTENT=true sends the name and text too.
func (s *server) pushData(d map[string]string) map[string]string {
	out := d
	if !s.cfg.pushContent {
		if d["kind"] == "dm" {
			out = map[string]string{"kind": "dm"} // not even who wrote (conv names them): the app asks its server
		} else if d["kind"] == "event" {
			out = map[string]string{"kind": "event", "ev": d["ev"], "when": d["when"]} // not the title
		} else {
			out = map[string]string{"kind": d["kind"], "conv": d["conv"], "comm": d["comm"]}
		}
	}
	// Which server it's from (the app may know several): the host the invites name
	if u, err := url.Parse(s.cfg.inviteBase); err == nil && u.Host != "" {
		out["srv"] = u.Host
	}
	return out
}
