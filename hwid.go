package main

// The machine a server runs on, as a few hashed parts (the machine id, the board, the network card, the root disk).
// A donation's places are tied to the server's key and to these: copied to another machine, they don't count.
// A part may change (a new network card, a moved disk): a token still counts while most of its parts match.
// Only the hashes leave the server, never the values.

import (
	"crypto/sha256"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// hwPart: one hashed part, "name:hash".
func hwPart(name, value string) string {
	h := sha256.Sum256([]byte("spectrevoice-hw:" + name + ":" + strings.TrimSpace(value)))
	return name + ":" + base64.RawURLEncoding.EncodeToString(h[:12])
}

// hwRoot is where the machine's files are read from ("/"; tests use a folder).
var hwRoot = "/"

func hwRead(p string) string {
	b, err := os.ReadFile(filepath.Join(hwRoot, p))
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(b))
	// Placeholders some boards and images carry are not identities
	switch strings.ToLower(v) {
	case "", "none", "default string", "to be filled by o.e.m.", "not specified", "0", "00000000-0000-0000-0000-000000000000",
		"03000200-0400-0500-0006-000700080009", "uninitialized":
		return ""
	}
	return v
}

// hardwareID: the machine's parts, sorted.
func hardwareID() []string {
	var out []string
	add := func(name, v string) {
		if v != "" {
			out = append(out, hwPart(name, v))
		}
	}
	m := hwRead("etc/machine-id")
	if m == "" {
		m = hwRead("var/lib/dbus/machine-id")
	}
	add("machine", m)
	add("product", hwRead("sys/class/dmi/id/product_uuid"))
	add("board", hwRead("sys/class/dmi/id/board_serial"))
	add("mac", firstMAC())
	add("disk", rootDisk())
	sort.Strings(out)
	return out
}

// firstMAC: the first real network card's address (not loopback, bridges, containers' or VPN links).
var firstMAC = func() string {
	ifs, err := net.Interfaces()
	if err != nil {
		return ""
	}
	sort.Slice(ifs, func(i, j int) bool { return ifs[i].Name < ifs[j].Name })
	for _, i := range ifs {
		n := i.Name
		if i.Flags&net.FlagLoopback != 0 || len(i.HardwareAddr) != 6 {
			continue
		}
		skip := false
		for _, p := range []string{"docker", "veth", "br-", "virbr", "tun", "tap", "wg", "zt", "tailscale", "cni", "flannel", "lxc"} {
			if strings.HasPrefix(n, p) {
				skip = true
				break
			}
		}
		// A locally administered address is made up (random MACs), not the card's
		if skip || i.HardwareAddr[0]&2 != 0 {
			continue
		}
		return i.HardwareAddr.String()
	}
	return ""
}

// rootDisk: the UUID of the filesystem / is on.
func rootDisk() string {
	b, err := os.ReadFile(filepath.Join(hwRoot, "proc/self/mountinfo"))
	if err != nil {
		return ""
	}
	dev := ""
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 4 && f[4] == "/" {
			// "… - fstype source options"
			for i, x := range f {
				if x == "-" && i+2 < len(f) {
					dev = f[i+2]
				}
			}
		}
	}
	if !strings.HasPrefix(dev, "/dev/") {
		return "" // a container's or a network root: no disk of its own
	}
	ents, _ := os.ReadDir(filepath.Join(hwRoot, "dev/disk/by-uuid"))
	for _, e := range ents {
		t, err := filepath.EvalSymlinks(filepath.Join(hwRoot, "dev/disk/by-uuid", e.Name()))
		if err == nil && filepath.Base(t) == filepath.Base(dev) {
			return e.Name()
		}
	}
	return ""
}

// hwMatch: do the parts a token names still describe this machine? More than half of them must.
func hwMatch(want, have []string) bool {
	if len(want) == 0 {
		return false
	}
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	n := 0
	for _, w := range want {
		if set[w] {
			n++
		}
	}
	return n >= len(want)/2+1
}
