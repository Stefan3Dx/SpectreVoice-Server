package main

// Push notifications without credentials of one's own (every self-hosted server): the phones' FCM tokens belong
// to the app's Firebase project, so a server can't reach them by itself. The donation service relays them: a
// request signed with this server's key (the same as /v1/checkin), {"token","data","ttl"}; it keeps nothing but a
// count. What's in "data" is what a push carries anyway: nothing of a message ({"kind":"dm"}), and a call's caller
// sealed for the phone (calls.go). SV_PUSH_RELAY=false turns it off.

import "strings"

func (s *server) newRelayPusher() (*pusher, error) {
	if err := s.store.migratePush(); err != nil {
		return nil, err
	}
	p := &pusher{project: "relay", st: s.store, queue: make(chan pushJob, 1000)}
	p.relay = func(token string, data map[string]string, ttl string) error {
		var out struct {
			OK bool `json:"ok"`
		}
		_, err := s.lic.call("/v1/push", "push", map[string]any{"token": token, "data": data, "ttl": ttl}, &out)
		if err != nil && strings.Contains(err.Error(), "token no longer valid") {
			return errGoneToken
		}
		return err
	}
	go p.loop()
	return p, nil
}
