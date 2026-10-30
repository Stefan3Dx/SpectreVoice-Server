package main

// Sealed push notifications and the push relay.
//
// Sealed: an app that sends a push key with its token ("key": 32 random bytes, base64) gets its notifications
// encrypted for it (AES-256-GCM, AAD "sv-push-v1"): Google (and the relay) see only {"sv":"1","x":<base64 iv‖ct>}.
// Inside: what the app shows and opens (kind, title, from, conv, srv, id) and, for a private message, its
// end-to-end envelope when it fits (the app decrypts it with its device key to show a preview; the server still
// never has the words). Without a key (older apps) the open, contentless data goes as before (pushData).
//
// A server without SV_PUSH_CREDENTIALS sends through the donation service's relay (pushrelay.go): it sees the
// phones' tokens and these sealed blobs (or the open contentless data for older apps), nothing else.

import (
	"encoding/base64"
	"encoding/json"
)

const (
	pushSealAAD = "sv-push-v1"
	// What may go inside a sealed notification: FCM's data payload is 4096 bytes in all; base64 and the GCM tag
	// grow it by a third
	pushSealMax = 2800
)

type pushTarget struct{ token, key string }

// pushTargets: uid's phones and their push keys ("" for an app that sent none).
func (st *store) pushTargets(uid string) []pushTarget {
	rows, err := st.db.Query(`SELECT token, pkey FROM push_tokens WHERE uid=?`, uid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []pushTarget
	for rows.Next() {
		var t pushTarget
		if rows.Scan(&t.token, &t.key) == nil {
			out = append(out, t)
		}
	}
	return out
}

// pushKeyOK: a push key as the apps make it (32 bytes, base64).
func pushKeyOK(k string) bool {
	b, err := base64.StdEncoding.DecodeString(k)
	return err == nil && len(b) == 32
}

// sealPushData: data sealed for the phone whose push key is key (calls.go's sealPush), as FCM data.
func sealPushData(key string, data map[string]string) (map[string]string, bool) {
	plain, _ := json.Marshal(data)
	if len(plain) > pushSealMax {
		return nil, false
	}
	x, err := sealPush(key, data)
	if err != nil {
		return nil, false
	}
	return map[string]string{"sv": "1", "x": x}, true
}

// notifySealed queues a notification whose sealed form (for apps with a push key) says more than open (for
// the others, through Google as is).
func (p *pusher) notifySealed(uid string, open, sealed map[string]string) {
	if p == nil {
		return
	}
	select {
	case p.queue <- pushJob{uid: uid, data: open, sealed: sealed}:
	default:
	}
}

// dataFor: what goes to one phone for job j.
func (j pushJob) dataFor(t pushTarget) map[string]string {
	if t.key == "" {
		return j.data
	}
	in := j.sealed
	if in == nil {
		in = j.data
	}
	if out, ok := sealPushData(t.key, in); ok {
		return out
	}
	// Too big (an envelope for many devices): without it
	if _, has := in["e"]; has {
		less := map[string]string{}
		for k, v := range in {
			if k != "e" {
				less[k] = v
			}
		}
		if out, ok := sealPushData(t.key, less); ok {
			return out
		}
	}
	return j.data
}
