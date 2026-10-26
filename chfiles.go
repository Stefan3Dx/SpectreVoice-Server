package main

// Each channel's files: folders and files people keep there (not only what's linked in the chat). Who may read the
// channel may see and download them; anyone there may upload and make folders; one deletes one's own, and those
// with "files" anything.
//
//	<- {"t":"cf-list","ch":…,"folder":0}            -> {"t":"cf","ch","folder","path":[…],"items":[…],"used","quota"}
//	<- {"t":"cf-mkdir","ch":…,"folder":…,"name":…}
//	<- {"t":"cf-del","id":…}  <- {"t":"cf-rename","id":…,"name":…}
//	POST /cf/upload?ch=…&folder=…&name=…  (token)    GET /cf/<id>?token=…
//
// The files are on disk by id (<data>/channel-files/<id>), so no name can reach outside; the tree is in SQLite.

import (
	"database/sql"

	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type cfItem struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Dir    bool   `json:"dir,omitempty"`
	Size   int64  `json:"size,omitempty"`
	By     string `json:"by"`
	ByName string `json:"byName,omitempty"`
	At     int64  `json:"at"`
}

type channelFiles struct {
	s   *server
	dir string
	db  *sql.DB
}

func newChannelFiles(s *server, dir string) (*channelFiles, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	_, err := s.store.db.Exec(`CREATE TABLE IF NOT EXISTS chfiles(id INTEGER PRIMARY KEY, ch INTEGER NOT NULL, comm INTEGER NOT NULL, parent INTEGER NOT NULL DEFAULT 0,
name TEXT NOT NULL, dir INTEGER NOT NULL DEFAULT 0, size INTEGER NOT NULL DEFAULT 0, by TEXT NOT NULL, at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS chfiles_ch ON chfiles(ch, parent);
CREATE INDEX IF NOT EXISTS chfiles_comm ON chfiles(comm);`)
	if err != nil {
		return nil, err
	}
	return &channelFiles{s: s, dir: dir, db: s.store.db}, nil
}

// readable: uid may see channel ch's files now (a member of its community, in it or allowed past its lock).
func (cf *channelFiles) readable(uid string, ch int64) (int64, bool) {
	h := cf.s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.chans[ch]
	if c == nil {
		return 0, false
	}
	for _, o := range h.sessions {
		if o.uid == uid && o.comm == c.Comm && h.mayRead(o, ch) {
			return c.Comm, true
		}
	}
	return 0, false
}

func (cf *channelFiles) item(id int64) (cfItem, int64, int64, error) {
	var it cfItem
	var ch, comm int64
	var dir int
	err := cf.db.QueryRow(`SELECT id,name,dir,size,by,at,ch,comm FROM chfiles WHERE id=?`, id).Scan(&it.ID, &it.Name, &dir, &it.Size, &it.By, &it.At, &ch, &comm)
	it.Dir = dir == 1
	return it, ch, comm, err
}

func (cf *channelFiles) used(comm int64) int64 {
	var n sql.NullInt64
	_ = cf.db.QueryRow(`SELECT SUM(size) FROM chfiles WHERE comm=?`, comm).Scan(&n)
	return n.Int64
}

// folderOK: folder is 0 (the channel's top) or one of ch's folders.
func (cf *channelFiles) folderOK(ch, folder int64) bool {
	if folder == 0 {
		return true
	}
	var n int
	_ = cf.db.QueryRow(`SELECT COUNT(*) FROM chfiles WHERE id=? AND ch=? AND dir=1`, folder, ch).Scan(&n)
	return n == 1
}

func (cf *channelFiles) list(ss *session, ch, folder int64) {
	comm, ok := cf.readable(ss.uid, ch)
	if !ok || !cf.folderOK(ch, folder) {
		ss.refuse("cf-list", "gone", "no such channel or folder")
		return
	}
	rows, err := cf.db.Query(`SELECT f.id,f.name,f.dir,f.size,f.by,f.at,COALESCE(u.name,'') FROM chfiles f LEFT JOIN users u ON u.uid=f.by WHERE f.ch=? AND f.parent=? ORDER BY f.dir DESC, f.name COLLATE NOCASE`, ch, folder)
	if err != nil {
		ss.refuse("cf-list", "server", err.Error())
		return
	}
	items := []cfItem{}
	for rows.Next() {
		var it cfItem
		var dir int
		if rows.Scan(&it.ID, &it.Name, &dir, &it.Size, &it.By, &it.At, &it.ByName) == nil {
			it.Dir = dir == 1
			items = append(items, it)
		}
	}
	rows.Close()
	// The way up, for the breadcrumbs
	path := []cfItem{}
	for f, i := folder, 0; f != 0 && i < 32; i++ {
		var it cfItem
		var parent int64
		if cf.db.QueryRow(`SELECT id,name,parent FROM chfiles WHERE id=?`, f).Scan(&it.ID, &it.Name, &parent) != nil {
			break
		}
		path = append([]cfItem{it}, path...)
		f = parent
	}
	ss.send(map[string]any{"t": "cf", "ch": ch, "folder": folder, "path": path, "items": items,
		"used": cf.used(comm), "quota": cf.s.limits().ChannelFilesMB << 20, "maxFile": min(cf.s.cfg.fileMB, cf.s.limits().FileMB) << 20})
}

func (cf *channelFiles) op(ss *session, t string, m map[string]any) bool {
	if cf == nil {
		return false
	}
	switch t {
	case "cf-list":
		cf.list(ss, i64(m, "ch"), i64(m, "folder"))
	case "cf-mkdir":
		ch, folder := i64(m, "ch"), i64(m, "folder")
		name := cleanFileName(str(m, "name"))
		comm, ok := cf.readable(ss.uid, ch)
		if !ok || !cf.folderOK(ch, folder) || name == "" {
			ss.refuse(t, "bad", "can't make that folder")
			return true
		}
		var n int
		_ = cf.db.QueryRow(`SELECT COUNT(*) FROM chfiles WHERE ch=?`, ch).Scan(&n)
		if n >= 5000 {
			ss.refuse(t, "full", "too many files here")
			return true
		}
		if _, err := cf.db.Exec(`INSERT INTO chfiles(ch,comm,parent,name,dir,by,at) VALUES(?,?,?,?,1,?,?)`, ch, comm, folder, name, ss.uid, time.Now().UnixMilli()); err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		cf.list(ss, ch, folder)
	case "cf-del", "cf-rename":
		id := i64(m, "id")
		it, ch, _, err := cf.item(id)
		if err != nil {
			ss.refuse(t, "gone", "no such file")
			return true
		}
		if _, ok := cf.readable(ss.uid, ch); !ok {
			ss.refuse(t, "gone", "no such file")
			return true
		}
		h := cf.s.hub
		h.mu.Lock()
		boss := h.may(ss, pFiles)
		h.mu.Unlock()
		if it.By != ss.uid && !boss {
			ss.refuse(t, "role", "not yours")
			return true
		}
		var parent int64
		_ = cf.db.QueryRow(`SELECT parent FROM chfiles WHERE id=?`, id).Scan(&parent)
		if t == "cf-rename" {
			name := cleanFileName(str(m, "name"))
			if name == "" {
				ss.refuse(t, "bad", "a name, please")
				return true
			}
			_, _ = cf.db.Exec(`UPDATE chfiles SET name=? WHERE id=?`, name, id)
		} else {
			cf.removeTree(id)
		}
		cf.list(ss, ch, parent)
	default:
		return false
	}
	return true
}

// removeTree deletes a file, or a folder with everything in it.
func (cf *channelFiles) removeTree(id int64) {
	ids := []int64{id}
	for i := 0; i < len(ids) && i < 100000; i++ {
		rows, err := cf.db.Query(`SELECT id FROM chfiles WHERE parent=?`, ids[i])
		if err != nil {
			break
		}
		for rows.Next() {
			var c int64
			if rows.Scan(&c) == nil {
				ids = append(ids, c)
			}
		}
		rows.Close()
	}
	for _, x := range ids {
		_, _ = cf.db.Exec(`DELETE FROM chfiles WHERE id=?`, x)
		_ = os.Remove(filepath.Join(cf.dir, strconv.FormatInt(x, 10)))
	}
}

// dropChannels: deleted channels take their files with them.
func (cf *channelFiles) dropChannels(chs []int64) {
	if cf == nil {
		return
	}
	for _, ch := range chs {
		rows, err := cf.db.Query(`SELECT id FROM chfiles WHERE ch=? AND parent=0`, ch)
		if err != nil {
			continue
		}
		var top []int64
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				top = append(top, id)
			}
		}
		rows.Close()
		for _, id := range top {
			cf.removeTree(id)
		}
	}
}

func (cf *channelFiles) handleUpload(w http.ResponseWriter, r *http.Request) {
	if cf == nil {
		http.NotFound(w, r)
		return
	}
	uid, ok := cf.s.tokenUID(r)
	if !ok {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "POST the file", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	ch, _ := strconv.ParseInt(q.Get("ch"), 10, 64)
	folder, _ := strconv.ParseInt(q.Get("folder"), 10, 64)
	name := cleanFileName(q.Get("name"))
	comm, ok := cf.readable(uid, ch)
	if !ok || !cf.folderOK(ch, folder) || name == "" {
		http.Error(w, "no such channel or folder", http.StatusNotFound)
		return
	}
	lim := cf.s.limits()
	maxFile := min(cf.s.cfg.fileMB, lim.FileMB) << 20
	if r.ContentLength > maxFile {
		http.Error(w, "too big", http.StatusRequestEntityTooLarge)
		return
	}
	left := lim.ChannelFilesMB<<20 - cf.used(comm)
	if left <= 0 || r.ContentLength > left {
		http.Error(w, "the community's space for files is full", http.StatusInsufficientStorage)
		return
	}
	res, err := cf.db.Exec(`INSERT INTO chfiles(ch,comm,parent,name,size,by,at) VALUES(?,?,?,?,0,?,?)`, ch, comm, folder, name, uid, time.Now().UnixMilli())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	id, _ := res.LastInsertId()
	path := filepath.Join(cf.dir, strconv.FormatInt(id, 10))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o640)
	if err != nil {
		cf.removeTree(id)
		http.Error(w, err.Error(), 500)
		return
	}
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, min(maxFile, left)))
	f.Close()
	if err != nil {
		cf.removeTree(id)
		http.Error(w, "upload failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	_, _ = cf.db.Exec(`UPDATE chfiles SET size=? WHERE id=?`, n, id)
	log.Printf("channel file %d %q in %d (%d KB) by %s", id, name, ch, n>>10, uid)
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":%d,"size":%d}`, id, n)
}

// GET /cf/<id>?token=…
func (cf *channelFiles) handleDownload(w http.ResponseWriter, r *http.Request) {
	if cf == nil {
		http.NotFound(w, r)
		return
	}
	uid, ok := cf.s.tokenUID(r)
	if !ok {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/cf/"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	it, ch, _, err := cf.item(id)
	if err != nil || it.Dir {
		http.NotFound(w, r)
		return
	}
	if _, ok := cf.readable(uid, ch); !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(cf.dir, strconv.FormatInt(id, 10)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	ct := mime.TypeByExtension(filepath.Ext(it.Name))
	if ct == "" || strings.HasPrefix(ct, "text/html") {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": it.Name}))
	http.ServeContent(w, r, "", time.UnixMilli(it.At), f)
}
