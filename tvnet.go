package main

// The TV's way out (tv.go): every address it opens, the playlist, the channels' logos and everything ffmpeg reads
// for a channel, may only be a public one, never the server's own network (SSRF), unless the operator allows LAN
// sources (SV_TV_LAN=true). The check is made on the address actually connected to (after DNS), so a name that
// points inside, or a redirect there, is refused too.
//
// ffmpeg can't be given a dialer, so it goes through a small proxy of ours on 127.0.0.1 (http_proxy in its
// environment, with a password of its own): plain HTTP as a forwarding proxy, HTTPS through CONNECT. The proxy
// connects only where the rule allows, which covers an HLS playlist's segments on other hosts and redirects too.
// Nothing about the requests is logged (the addresses hold the subscription's credentials).

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var errTVBlocked = errors.New("that address is inside the server's own network (the operator can allow LAN sources with SV_TV_LAN=true)")

// tvLoopbackOK: tests' sources are on 127.0.0.1; never on in production (the server's own ports are there).
var tvLoopbackOK = false

func mustCIDRs(list ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(list))
	for _, c := range list {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}

var (
	// Never: "this network", link-local (cloud metadata), documentation and benchmark ranges, multicast, reserved,
	// NAT64 (it can reach IPv4 inside), discard
	tvNever = mustCIDRs("0.0.0.0/8", "169.254.0.0/16", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "::/128", "fe80::/10", "ff00::/8", "64:ff9b::/96", "64:ff9b:1::/48",
		"100::/64", "2001:db8::/32", "2002::/16")
	// The LAN: only with SV_TV_LAN
	tvLAN = mustCIDRs("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7")
)

// tvIPAllowed: may the TV connect to ip ([lan]: the operator allows LAN sources).
func tvIPAllowed(ip net.IP, lan bool) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() {
		return tvLoopbackOK
	}
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, n := range tvNever {
		if n.Contains(ip) {
			return false
		}
	}
	for _, n := range tvLAN {
		if n.Contains(ip) {
			return lan
		}
	}
	return true
}

// tvDialer connects only where tvIPAllowed says (checked on the address resolved, just before connecting).
func tvDialer(lan bool) *net.Dialer {
	return &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if !tvIPAllowed(net.ParseIP(host), lan) {
				return errTVBlocked
			}
			return nil
		},
	}
}

// tvClient: an HTTP client for the playlist and the logos (no proxy from the environment, at most 5 redirects,
// each to http(s) and checked like the first address).
func tvClient(lan bool, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           tvDialer(lan).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return errors.New("redirected to something that isn't http(s)")
		}
		return nil
	}}
}

// tvCheckURL: an address the TV may be given (http or https, a host, no spaces); an address that is a private
// IP itself is refused at once (a name is checked when connecting).
func tvCheckURL(raw string, lan bool) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 4096 || strings.ContainsAny(raw, " \t\r\n") {
		return nil, errors.New("not an address")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, errors.New("the address must start with http:// or https://")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !tvIPAllowed(ip, lan) {
		return nil, errTVBlocked
	}
	return u, nil
}

// cleanErr: an HTTP client's error without the address in it (url.Error repeats the whole address).
func cleanErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var oe *net.OpError
	if errors.As(err, &oe) && errors.Is(oe.Err, errTVBlocked) {
		return errTVBlocked
	}
	if errors.Is(err, errTVBlocked) {
		return errTVBlocked
	}
	return err
}

// ====== The proxy ffmpeg goes through ======

type tvProxy struct {
	ln    net.Listener
	srv   *http.Server
	auth  string // the Proxy-Authorization header ffmpeg sends
	env   string // http_proxy for ffmpeg
	lan   bool
	dial  *net.Dialer
	tr    *http.Transport
	slots chan struct{}
	// blocked: the last time an address was refused (unix ns), so the TV can say why a channel didn't open
	blocked atomic.Int64
}

const tvProxyConns = 48

func startTVProxy(lan bool) (*tvProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	pass := hex.EncodeToString(b)
	p := &tvProxy{ln: ln, lan: lan, dial: tvDialer(lan), slots: make(chan struct{}, tvProxyConns)}
	p.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte("sv:"+pass))
	p.env = "http://sv:" + pass + "@" + ln.Addr().String()
	p.tr = &http.Transport{Proxy: nil, DialContext: p.dial.DialContext, ResponseHeaderTimeout: 20 * time.Second, MaxIdleConns: 16, IdleConnTimeout: 30 * time.Second}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

func (p *tvProxy) close() {
	_ = p.srv.Close()
	p.tr.CloseIdleConnections()
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *tvProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Proxy-Authorization")), []byte(p.auth)) != 1 {
		w.Header().Set("Proxy-Authenticate", `Basic realm="tv"`)
		http.Error(w, "no", http.StatusProxyAuthRequired)
		return
	}
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "only http here", http.StatusBadRequest)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	res, err := p.tr.RoundTrip(out)
	if err != nil {
		if errors.Is(cleanErr(err), errTVBlocked) {
			p.blocked.Store(time.Now().UnixNano())
			http.Error(w, "blocked", http.StatusForbidden)
		} else {
			http.Error(w, "unreachable", http.StatusBadGateway)
		}
		return
	}
	defer res.Body.Close()
	for _, h := range hopHeaders {
		res.Header.Del(h)
	}
	for k, vs := range res.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(res.StatusCode)
	buf := make([]byte, 32<<10)
	fl, _ := w.(http.Flusher)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *tvProxy) connect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	up, err := p.dial.DialContext(ctx, "tcp", r.Host)
	cancel()
	if err != nil {
		if errors.Is(cleanErr(err), errTVBlocked) {
			p.blocked.Store(time.Now().UnixNano())
			http.Error(w, "blocked", http.StatusForbidden)
		} else {
			http.Error(w, "unreachable", http.StatusBadGateway)
		}
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = up.Close()
		http.Error(w, "no", http.StatusInternalServerError)
		return
	}
	down, rw, err := hj.Hijack()
	if err != nil {
		_ = up.Close()
		return
	}
	_, _ = down.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	var once sync.Once
	done := func() { once.Do(func() { _ = up.Close(); _ = down.Close() }) }
	go func() {
		// What the client already sent after the CONNECT, then the rest
		if n := rw.Reader.Buffered(); n > 0 {
			b, _ := rw.Reader.Peek(n)
			_, _ = up.Write(b)
		}
		_, _ = io.Copy(up, down)
		done()
	}()
	_, _ = io.Copy(down, up)
	done()
}
