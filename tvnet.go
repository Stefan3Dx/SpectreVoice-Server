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
//
// Certificates are checked (by Go here; ffmpeg 8 and later check their own by default). A provider with a
// self-signed or expired certificate is let through only when the owner allows it for the playlist ("allow
// untrusted certificate", tvSource.Insecure; tvManager.untrustedHost says which hosts: the playlist's own and its
// channels'). ffmpeg can't be told so for the segments an HLS playlist names (the hls demuxer doesn't pass
// -tls_verify/-ca_file on to them), so such a host's https addresses are given to ffmpeg as plain http to a marked
// name (tvMarkURL: "h.svtls.invalid:443", only ever between ffmpeg and the proxy on 127.0.0.1); the proxy sees the
// mark and fetches https://h itself, with the same SSRF rule, the certificate check skipped for that host only, and
// marks every https address in the HLS playlists (tvMarkPlaylist) and redirects it passes on from there (Go then
// checks those hosts' certificates, unless they are allowed too). With the setting off nothing is marked: ffmpeg
// does TLS itself through CONNECT, and a failure is told as errTVUntrusted (its message, or tvProbeTLS).

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var errTVBlocked = errors.New("that address is inside the server's own network (the operator can allow LAN sources with SV_TV_LAN=true)")

// errTVUntrusted: a provider's certificate didn't check out (self-signed, expired, made for another name).
var errTVUntrusted = errors.New("certificate not trusted: enable 'allow untrusted certificate' for this provider")

// tvIsCertErr: err is a certificate that didn't check out (not a network failure).
func tvIsCertErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errTVUntrusted) {
		return true
	}
	var cv *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var ci x509.CertificateInvalidError
	var he x509.HostnameError
	return errors.As(err, &cv) || errors.As(err, &ua) || errors.As(err, &ci) || errors.As(err, &he)
}

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

// tvDialTLS: TLS over d, the certificate checked unless [untrusted] (nil: none) allows the host; a check that
// fails is errTVUntrusted.
func tvDialTLS(d *net.Dialer, untrusted func(host string) bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		conn, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		cfg := &tls.Config{ServerName: host, InsecureSkipVerify: untrusted != nil && untrusted(strings.ToLower(host))}
		tc := tls.Client(conn, cfg)
		hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := tc.HandshakeContext(hctx); err != nil {
			_ = conn.Close()
			if tvIsCertErr(err) {
				return nil, errTVUntrusted
			}
			return nil, err
		}
		return tc, nil
	}
}

// tvProbeTLS: whether addr's (host:port) certificate checks out: nil, errTVUntrusted, or another error when it
// couldn't be told.
func tvProbeTLS(ctx context.Context, addr string, lan bool) error {
	c, err := tvDialTLS(tvDialer(lan), nil)(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

// tvClient: an HTTP client for the playlist and the logos (no proxy from the environment, at most 5 redirects,
// each to http(s) and checked like the first address; certificates checked, except for the hosts [untrusted]
// allows).
func tvClient(lan bool, timeout time.Duration, untrusted func(host string) bool) *http.Client {
	d := tvDialer(lan)
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           d.DialContext,
		DialTLSContext:        tvDialTLS(d, untrusted),
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
	if tvIsCertErr(err) {
		return errTVUntrusted
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
	// untrusted: the hosts whose certificate isn't checked, on the marked way (nil: none)
	untrusted func(host string) bool
	// blocked: the last time an address was refused (unix ns), so the TV can say why a channel didn't open
	blocked atomic.Int64
	// untrustedAt: the last time a certificate didn't check out on the marked way (unix ns)
	untrustedAt atomic.Int64
}

const tvProxyConns = 48

func startTVProxy(lan bool) (*tvProxy, error) { return startTVProxyTrusting(lan, nil) }

// startTVProxyTrusting: the proxy, the certificates of the hosts [untrusted] allows not checked on the marked way.
func startTVProxyTrusting(lan bool, untrusted func(host string) bool) (*tvProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	pass := hex.EncodeToString(b)
	p := &tvProxy{ln: ln, lan: lan, dial: tvDialer(lan), slots: make(chan struct{}, tvProxyConns), untrusted: untrusted}
	p.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte("sv:"+pass))
	p.env = "http://sv:" + pass + "@" + ln.Addr().String()
	p.tr = &http.Transport{Proxy: nil, DialContext: p.dial.DialContext, DialTLSContext: tvDialTLS(p.dial, untrusted), ResponseHeaderTimeout: 20 * time.Second, MaxIdleConns: 16, IdleConnTimeout: 30 * time.Second}
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
	// The marked way: https to the host before the mark, fetched here
	host, marked := strings.CutSuffix(strings.ToLower(r.URL.Hostname()), tvTLSMark)
	if marked {
		if host == "" || strings.Contains(host, ":") {
			http.Error(w, "no host", http.StatusBadRequest)
			return
		}
		port := r.URL.Port()
		if port == "" {
			port = "443"
		}
		out.URL.Scheme, out.URL.Host, out.Host = "https", net.JoinHostPort(host, port), ""
		out.Header.Del("Accept-Encoding") // Go asks for gzip and unpacks it itself: a playlist can be read to mark it
	}
	res, err := p.tr.RoundTrip(out)
	if err != nil {
		switch err = cleanErr(err); {
		case errors.Is(err, errTVBlocked):
			p.blocked.Store(time.Now().UnixNano())
			http.Error(w, "blocked", http.StatusForbidden)
		case errors.Is(err, errTVUntrusted):
			p.untrustedAt.Store(time.Now().UnixNano())
			http.Error(w, "certificate not trusted", http.StatusBadGateway)
		default:
			http.Error(w, "unreachable", http.StatusBadGateway)
		}
		return
	}
	defer res.Body.Close()
	for _, h := range hopHeaders {
		res.Header.Del(h)
	}
	var body io.Reader = res.Body
	if marked {
		// Redirects and HLS playlists name addresses: their https ones go the marked way too (Go checks the certificate,
		// unless the host is one the owner allowed), so ffmpeg never does TLS on its own for this provider
		if loc := res.Header.Get("Location"); loc != "" {
			res.Header.Set("Location", p.markURL(loc))
		}
		br := bufio.NewReaderSize(res.Body, 32<<10)
		body = br
		head, _ := br.Peek(10)
		if bytes.HasPrefix(bytes.TrimPrefix(head, []byte("ï»¿")), []byte("#EXTM3U")) || strings.Contains(strings.ToLower(res.Header.Get("Content-Type")), "mpegurl") {
			data, err := io.ReadAll(io.LimitReader(br, tvMarkMax+1))
			if err != nil || len(data) > tvMarkMax {
				http.Error(w, "unreachable", http.StatusBadGateway)
				return
			}
			data = tvMarkPlaylist(data, p.markURL)
			res.Header.Set("Content-Length", strconv.Itoa(len(data)))
			body = bytes.NewReader(data)
		}
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
		n, err := body.Read(buf)
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

// ====== The marked way (an untrusted certificate the owner allowed) ======

// tvTLSMark: the end of a host name that means "https to the host before it, through the proxy". .invalid is never
// a real name (RFC 2606), and ffmpeg never resolves it (it asks the proxy).
const tvTLSMark = ".svtls.invalid"

// tvMarkMax: an HLS playlist bigger than this isn't passed on the marked way.
const tvMarkMax = 8 << 20

// tvMarkURL: raw, if it's https to a host [want] says, as plain http to the marked name (the port said);
// anything else as it was.
func tvMarkURL(raw string, want func(host string) bool) string {
	if want == nil || len(raw) < 8 || !strings.EqualFold(raw[:8], "https://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.Contains(host, ":") || !want(host) { // an IPv6 address can't take the mark
		return raw
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	u.Scheme, u.Host = "http", host+tvTLSMark+":"+port
	return u.String()
}

// markURL: any https address the marked way, from an answer that came the marked way.
func (p *tvProxy) markURL(raw string) string {
	return tvMarkURL(raw, func(string) bool { return true })
}

var hlsURIAttr = regexp.MustCompile(`URI="([^"]*)"`)

// tvMarkPlaylist: an HLS playlist with its absolute addresses (segments, variants, the URI="…" of keys, maps and
// renditions) passed through mark. Relative ones need nothing: they resolve against the marked address.
func tvMarkPlaylist(data []byte, mark func(string) string) []byte {
	var b strings.Builder
	b.Grow(len(data) + 256)
	for line := range strings.Lines(string(data)) {
		text := strings.TrimRight(line, "\r\n")
		end := line[len(text):]
		switch t := strings.TrimSpace(text); {
		case strings.HasPrefix(t, "#"):
			text = hlsURIAttr.ReplaceAllStringFunc(text, func(m string) string {
				return `URI="` + mark(m[len(`URI="`):len(m)-1]) + `"`
			})
		case t != "":
			text = mark(t)
		}
		b.WriteString(text)
		b.WriteString(end)
	}
	return []byte(b.String())
}
