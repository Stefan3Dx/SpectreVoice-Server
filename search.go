package main

import (
	"strings"
	"time"
	"unicode/utf8"
)

// Chat search: {"t":"search","q":"…","to":"c:<ch>" | "g:<comm>" | "*","before":<id>} → {"t":"search",
// "q","to","items":[msg…],"more":bool}. The same access as history (a channel one may read, the community's chat);
// "*" is every channel and the server chat this session may read now. Newest first, 50 a page ("before": the oldest
// id of the last page), case-insensitive, two a second per session. Private chats are never searched here: they are
// end-to-end encrypted (e2e.go), and the apps search them themselves, over what they decrypted (the older plaintext
// ones too, so search behaves the same in every private chat).

const (
	searchPage   = 50
	searchMinLen = 2
	searchMaxLen = 64
)

// searchOp: may this session search now? Two a second, bursts of five.
func (ss *session) searchOp() bool {
	now := time.Now()
	if ss.sRefill.IsZero() {
		ss.sTokens = 5
	} else {
		ss.sTokens = min(5, ss.sTokens+now.Sub(ss.sRefill).Seconds()*2)
	}
	ss.sRefill = now
	if ss.sTokens < 1 {
		return false
	}
	ss.sTokens--
	return true
}

func (h *hub) search(ss *session, m map[string]any) {
	q := strings.TrimSpace(str(m, "q"))
	if n := utf8.RuneCountInString(q); n < searchMinLen || n > searchMaxLen {
		ss.refuse("search", "bad", "2 to 64 characters")
		return
	}
	if !h.staff(ss.uid) && !ss.searchOp() {
		ss.refuse("search", "rate", "slow down")
		return
	}
	to := str(m, "to")
	var convs []string
	if strings.HasPrefix(to, "u:") || strings.HasPrefix(to, "d:") {
		ss.refuse("search", "e2e", "private chats are end-to-end encrypted: the app searches them itself")
		return
	}
	if to == "*" {
		convs = h.readableConvs(ss)
	} else {
		conv, _, _, err := h.target(ss, to)
		if err != nil {
			ss.refuse("search", "bad", err.Error())
			return
		}
		convs = []string{conv}
	}
	items, more, err := h.s.store.searchFor(convs, q, i64(m, "before"), searchPage)
	if err != nil {
		ss.refuse("search", "server", err.Error())
		return
	}
	if items == nil {
		items = []chatMsg{}
	}
	ss.send(map[string]any{"t": "search", "q": q, "to": to, "items": items, "more": more})
}

// readableConvs: every conversation the session may read now and the server searches: the channels of its community
// it may read and the community's chat (never a private chat).
func (h *hub) readableConvs(ss *session) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for id, c := range h.chans {
		if c.Comm == ss.comm && c.Kind != "spacer" && h.mayRead(ss, id) {
			out = append(out, chanConv(id))
		}
	}
	if ss.comm != 0 {
		out = append(out, "g:"+itoa(int(ss.comm)))
	}
	return out
}

// searchFor finds up to limit messages with q in them (case-insensitive) in convs (never a private chat's, nor an
// encrypted line); newest first, older than before (0: the newest); more says whether older matches exist.
func (st *store) searchFor(convs []string, q string, before int64, limit int) ([]chatMsg, bool, error) {
	if before <= 0 {
		before = 1 << 62
	}
	// LIKE is case-insensitive for ASCII in SQLite; % and _ in the words are escaped so they match themselves
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	args := []any{"%" + esc + "%", before}
	where := "deleted=0 AND enc='' AND conv NOT LIKE 'd:%' AND text LIKE ? ESCAPE '\\' AND id<?"
	var scope []string
	for _, c := range convs {
		if !strings.HasPrefix(c, "d:") {
			scope = append(scope, c)
			args = append(args, c)
		}
	}
	if len(scope) == 0 {
		return nil, false, nil
	}
	where += " AND conv IN (" + strings.TrimSuffix(strings.Repeat("?,", len(scope)), ",") + ")"
	args = append(args, limit+1)
	rows, err := st.db.Query(`SELECT `+msgCols+` FROM messages WHERE `+where+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	items, err := st.scanMessages(rows)
	if err != nil {
		return nil, false, err
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, more, nil
}
