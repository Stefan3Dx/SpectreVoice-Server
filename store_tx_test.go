package main

import (
	"testing"
	"time"
)

// Deleting several channels is all or nothing: one that can't go keeps the others (and their messages).
func TestDeleteChannelsAllOrNothing(t *testing.T) {
	s, _ := newTestServer(t, nil)
	st := s.store
	var ids []int64
	for _, n := range []string{"One", "Two", "Three"} {
		c := &channel{Name: n, Comm: spaceID}
		if err := st.saveChannel(c); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
		if _, err := st.db.Exec(`INSERT INTO messages(conv,from_uid,from_name,text,at) VALUES(?,?,?,?,?)`, chanConv(c.ID), "U", "U", "hi", time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	count := func(q string, args ...any) int {
		var n int
		_ = st.db.QueryRow(q, args...).Scan(&n)
		return n
	}
	left := func() (chans, msgs int) {
		for _, id := range ids {
			chans += count(`SELECT COUNT(*) FROM channels WHERE id=?`, id)
			msgs += count(`SELECT COUNT(*) FROM messages WHERE conv=?`, chanConv(id))
		}
		return
	}
	if _, err := st.db.Exec(`CREATE TRIGGER boom BEFORE DELETE ON channels WHEN old.id=` + itoa(int(ids[1])) + ` BEGIN SELECT RAISE(ABORT,'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if err := st.deleteChannels(ids); err == nil {
		t.Fatal("no error")
	}
	if c, m := left(); c != 3 || m != 3 {
		t.Fatalf("after a failed delete: %d channels, %d messages", c, m)
	}
	_, _ = st.db.Exec(`DROP TRIGGER boom`)
	if err := st.deleteChannels(ids); err != nil {
		t.Fatal(err)
	}
	if c, m := left(); c != 0 || m != 0 {
		t.Fatalf("after the delete: %d channels, %d messages", c, m)
	}
}
