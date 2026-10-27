//go:build !plus

package main

// The public build: 30 places, whatever the tokens say. With an active donation it fetches the official Plus
// build (upgrade.go), which is what gives the donation's places.

func newEdition() edition { return freeEdition{} }

type freeEdition struct{}

func (freeEdition) name() string { return "free" }

func (freeEdition) start(l *licensing) {
	if l.hasActive() {
		go l.upgradeLoop()
	}
}

func (freeEdition) background(*licensing) {}

func (freeEdition) apply(*licensing, *limits, int64) {}

func (freeEdition) accepted(l *licensing) { go l.upgradeLoop() }

func (freeEdition) note(l *licensing) (string, string) {
	if !l.hasActive() {
		return "", ""
	}
	if err := l.upgradeErr(); err != nil {
		return "install-failed", "The donation is kept; its places come with the official build, which couldn't be installed yet: " + err.Error()
	}
	return "installing", "The donation is kept; its places come with the official build, being installed now (the server restarts by itself)."
}
