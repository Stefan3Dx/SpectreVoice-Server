// A minimal Spectre Voice bot (docs/BOTS.md): it answers "!ping" with "pong" in the chat it was asked in, and
// greets whoever comes into its voice channel. Its key lives in bot.key (made on the first run); print the uid
// with -uid and have a community manager register it (bot-add) before it connects.
//
//	go run . -uid
//	go run . -server wss://voice.example.com/v1/ws
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	server := flag.String("server", "ws://localhost:7880/v1/ws", "the server's WebSocket address")
	keyFile := flag.String("key", "bot.key", "where the bot's key is kept")
	showUID := flag.Bool("uid", false, "print the bot's uid (for bot-add) and exit")
	flag.Parse()

	key, spki := loadKey(*keyFile)
	if *showUID {
		fmt.Println(uidOf(spki))
		return
	}
	for {
		if err := run(*server, key, spki); err != nil {
			log.Printf("%v; again in 10 s", err)
		}
		time.Sleep(10 * time.Second)
	}
}

// loadKey reads the P-256 key, or makes one.
func loadKey(path string) (*ecdsa.PrivateKey, []byte) {
	if raw, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(raw)
		if block != nil {
			if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
				spki, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
				return k, spki
			}
		}
		log.Fatalf("%s isn't a key", path)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	der, _ := x509.MarshalECPrivateKey(k)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		log.Fatal(err)
	}
	spki, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	log.Printf("new key in %s; its uid is %s", path, uidOf(spki))
	return k, spki
}

// uidOf: the identity's uid, as the server works it out (a short hash of the public key).
func uidOf(spki []byte) string {
	h := sha256.Sum256(spki)
	return base64.RawURLEncoding.EncodeToString(h[:15])
}

// run connects once and serves until the connection ends.
func run(server string, key *ecdsa.PrivateKey, spki []byte) error {
	u, err := url.Parse(server)
	if err != nil {
		return err
	}
	ws, _, err := websocket.DefaultDialer.Dial(server, nil)
	if err != nil {
		return err
	}
	defer ws.Close()
	var ch struct {
		T string `json:"t"`
		N string `json:"n"`
	}
	if err := ws.ReadJSON(&ch); err != nil || ch.T != "challenge" {
		return fmt.Errorf("no challenge: %v", err)
	}
	nonce, _ := base64.StdEncoding.DecodeString(ch.N)
	// v2: the signature names the server we dialed (its host name, lower case, no port)
	signed := append([]byte("spectrevoice-auth-v2:"+strings.ToLower(u.Hostname())+"\n"), nonce...)
	digest := sha256.Sum256(signed)
	sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err := ws.WriteJSON(map[string]any{"t": "hello", "v": 2, "pub": base64.StdEncoding.EncodeToString(spki),
		"sig": base64.StdEncoding.EncodeToString(sig), "name": "Echo", "client": "bot/echo-1.0", "dev": "echo-bot"}); err != nil {
		return err
	}
	var welcome struct {
		T       string `json:"t"`
		Message string `json:"message"`
		SID     int    `json:"sid"`
		UID     string `json:"uid"`
		Bot     bool   `json:"bot"`
		Members []struct {
			SID int   `json:"sid"`
			Ch  int64 `json:"ch"`
		} `json:"members"`
	}
	if err := ws.ReadJSON(&welcome); err != nil {
		return err
	}
	if welcome.T != "welcome" {
		return fmt.Errorf("refused: %s", welcome.Message)
	}
	if !welcome.Bot {
		return fmt.Errorf("this key isn't registered as a bot yet; ask a manager to bot-add %s", welcome.UID)
	}
	log.Printf("in as %s (sid %d)", welcome.UID, welcome.SID)
	myCh := int64(0)
	where := map[int]int64{} // every session's channel, to notice who comes into ours
	for _, m := range welcome.Members {
		where[m.SID] = m.Ch
		if m.SID == welcome.SID {
			myCh = m.Ch
		}
	}
	say := func(to, text string) error { return ws.WriteJSON(map[string]any{"t": "say", "to": to, "text": text}) }
	for {
		var m map[string]any
		if err := ws.ReadJSON(&m); err != nil {
			return err
		}
		switch m["t"] {
		case "msg":
			msg, _ := m["m"].(map[string]any)
			text, _ := msg["text"].(string)
			to, _ := msg["to"].(string)
			from, _ := msg["from"].(string)
			if from == welcome.UID || !strings.HasPrefix(to, "c:") && !strings.HasPrefix(to, "g:") {
				continue
			}
			if strings.TrimSpace(text) == "!ping" {
				if err := say(to, "pong"); err != nil {
					return err
				}
			}
		case "member":
			mem, _ := m["m"].(map[string]any)
			sid := int(num(mem["sid"]))
			ch := int64(num(mem["ch"]))
			name, _ := mem["name"].(string)
			old, known := where[sid]
			where[sid] = ch
			if sid == welcome.SID {
				myCh = ch
			} else if known && old != ch && ch == myCh && myCh != 0 && mem["bot"] != true {
				if err := say(fmt.Sprintf("c:%d", myCh), "Welcome, "+name+"!"); err != nil {
					return err
				}
			}
		case "member-left":
			delete(where, int(num(m["sid"])))
		case "error":
			b, _ := json.Marshal(m)
			log.Printf("server: %s", b)
		case "kicked":
			return fmt.Errorf("kicked: %v", m["why"])
		}
	}
}

func num(v any) float64 { f, _ := v.(float64); return f }
