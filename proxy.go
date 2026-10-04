package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// BERMUDA Stealth Gateway NG — L7 Streaming Edge & Camouflage Shield
// Linux Kernel splice(2) Zero-Copy TCP Relay & Byte-Exact OpenResty Emulation
// Single-User Dedicated Profile — Zero-Leak Active Probe Defense
// ---------------------------------------------------------------------------

const (
	defaultPathXH = "/api/v1/sync"
	defaultPathWS = "/api/v1/live"
	defaultPathTR = "/api/v1/gateway"

	// High-entropy path for Railway healthcheck; keeps origin topology secret
	defaultHealthPath = "/.well-known/hc-5b1e7c"

	openrestyServerToken = "openresty"

	// Authentic OpenResty 1.19.9.1 RPM index.html metadata:
	// size = 1097 bytes = 0x449, mtime = 1628285554 = 0x610daa72
	openrestyIndexLastModified = "Fri, 06 Aug 2021 21:32:34 GMT"
	openrestyIndexETag         = "\"610daa72-449\""

	// Authentic OpenResty 1.19.9.1 RPM 50x.html metadata:
	// size = 982 bytes = 0x3d6
	openresty50xLastModified = openrestyIndexLastModified
	openresty50xETag         = "\"610daa72-3d6\""

	defaultProxyBufferSize  = 64 * 1024
	defaultTransportBufSize = 64 * 1024
	bufferPoolCapacity      = 256
	halfCloseGrace          = 30 * time.Second
	backendDialTimeout      = 5 * time.Second
	backendHandshakeTimeout = 5 * time.Second
	tunnelDrainPollInterval = 100 * time.Millisecond
)

// openrestyWelcomeHTML: official openresty-1.19.9.1 RPM html/index.html (exactly 1097 bytes, LF)
const openrestyWelcomeHTML = "<!DOCTYPE html>\n" +
	"<html>\n" +
	"<head>\n" +
	"<meta content=\"text/html;charset=utf-8\" http-equiv=\"Content-Type\">\n" +
	"<meta content=\"utf-8\" http-equiv=\"encoding\">\n" +
	"<title>Welcome to OpenResty!</title>\n" +
	"<style>\n" +
	"    body {\n" +
	"        width: 35em;\n" +
	"        margin: 0 auto;\n" +
	"        font-family: Tahoma, Verdana, Arial, sans-serif;\n" +
	"    }\n" +
	"</style>\n" +
	"</head>\n" +
	"<body>\n" +
	"<h1>Welcome to OpenResty!</h1>\n" +
	"<p>If you see this page, the OpenResty web platform is successfully installed and\n" +
	"working. Further configuration is required.</p>\n" +
	"\n" +
	"<p>For online documentation and support please refer to our\n" +
	"<a href=\"https://openresty.org/\">openresty.org</a> site<br/>\n" +
	"Commercial support is available at\n" +
	"<a href=\"https://openresty.com/\">openresty.com</a>.</p>\n" +
	"<p>We have articles on troubleshooting issues like <a href=\"https://blog.openresty.com/en/lua-cpu-flame-graph/?src=wb\">high CPU usage</a> and\n" +
	"<a href=\"https://blog.openresty.com/en/how-or-alloc-mem/\">large memory usage</a> on <a href=\"https://blog.openresty.com/\">our official blog site</a>.\n" +
	"<p><em>Thank you for flying <a href=\"https://openresty.org/\">OpenResty</a>.</em></p>\n" +
	"</body>\n" +
	"</html>\n"

// openresty50xHTML: official openresty-1.19.9.1 RPM html/50x.html (exactly 982 bytes, LF)
const openresty50xHTML = "<!DOCTYPE html>\n" +
	"<html>\n" +
	"<head>\n" +
	"<meta content=\"text/html;charset=utf-8\" http-equiv=\"Content-Type\">\n" +
	"<meta content=\"utf-8\" http-equiv=\"encoding\">\n" +
	"<title>Error</title>\n" +
	"<style>\n" +
	"    body {\n" +
	"        width: 35em;\n" +
	"        margin: 0 auto;\n" +
	"        font-family: Tahoma, Verdana, Arial, sans-serif;\n" +
	"    }\n" +
	"</style>\n" +
	"</head>\n" +
	"<body>\n" +
	"<h1>An error occurred.</h1>\n" +
	"<p>Sorry, the page you are looking for is currently unavailable.<br/>\n" +
	"Please try again later.</p>\n" +
	"<p>If you are the system administrator of this resource then you should check\n" +
	"the <a href=\"http://nginx.org/r/error_log\">error log</a> for details.</p>\n" +
	"<p>We have articles on troubleshooting issues like <a href=\"https://blog.openresty.com/en/lua-cpu-flame-graph/?src=wb\">high CPU usage</a> and\n" +
	"<a href=\"https://blog.openresty.com/en/how-or-alloc-mem/\">large memory usage</a> on <a href=\"https://blog.openresty.com/\">our official blog site</a>.\n" +
	"<p><em>Faithfully yours, <a href=\"https://openresty.org/\">OpenResty</a>.</em></p>\n" +
	"</body>\n" +
	"</html>\n"

// Stock nginx templates (CRLF)
const openresty404HTML = "<html>\r\n<head><title>404 Not Found</title></head>\r\n<body>\r\n<center><h1>404 Not Found</h1></center>\r\n<hr><center>openresty</center>\r\n</body>\r\n</html>\r\n"
const openresty405HTML = "<html>\r\n<head><title>405 Not Allowed</title></head>\r\n<body>\r\n<center><h1>405 Not Allowed</h1></center>\r\n<hr><center>openresty</center>\r\n</body>\r\n</html>\r\n"

// recycledBufferPool implements httputil.BufferPool with a bounded free list.
type recycledBufferPool struct {
	size int
	ch   chan []byte
}

func newRecycledBufferPool(size int) *recycledBufferPool {
	return &recycledBufferPool{size: size, ch: make(chan []byte, bufferPoolCapacity)}
}

func (p *recycledBufferPool) Get() []byte {
	select {
	case b := <-p.ch:
		return b
	default:
		return make([]byte, p.size)
	}
}

func (p *recycledBufferPool) Put(b []byte) {
	if cap(b) != p.size {
		return
	}
	b = b[:cap(b)]
	select {
	case p.ch <- b:
	default:
	}
}

type HealthResponse struct {
	Status     string                   `json:"status"`
	Draining   bool                     `json:"draining"`
	Healthy    bool                     `json:"healthy"`
	UptimeSec  int64                    `json:"uptime_sec"`
	Supervisor SupervisorHealthSnapshot `json:"supervisor"`
	Telemetry  TelemetrySnapshot        `json:"telemetry"`
}

type TelemetrySnapshot struct {
	OpenConnections int64 `json:"open_connections"`
	ActiveTunnels   int64 `json:"active_tunnels"`
	TunnelsOpened   int64 `json:"tunnels_opened_total"`
	TotalRequests   int64 `json:"total_requests"`
}

type Gateway struct {
	sup *Supervisor

	pathXH, pathWS, pathTR          string
	backendXH, backendWS, backendTR string
	healthPath                      string
	adminToken                      string

	xhProxy *httputil.ReverseProxy
	tr      *http.Transport
	dialer  net.Dialer
	bufPool *recycledBufferPool

	draining    atomic.Bool
	openConns   atomic.Int64
	totalReq    atomic.Int64
	tunnelsLive atomic.Int64
	tunnelsHit  atomic.Int64

	tunnelsMu sync.Mutex
	tunnels   map[*net.TCPConn]struct{}

	startedAt time.Time
}

func NewGateway(sup *Supervisor) *Gateway {
	return newGateway(sup, defaultProxyBufferSize, defaultTransportBufSize)
}

func newGateway(sup *Supervisor, proxyBuf, transportBuf int) *Gateway {
	g := &Gateway{
		sup:        sup,
		pathXH:     getEnv("BERMUDA_PATH_XH", defaultPathXH),
		pathWS:     getEnv("BERMUDA_PATH_WS", defaultPathWS),
		pathTR:     getEnv("BERMUDA_PATH_TR", defaultPathTR),
		backendXH:  getEnv("BERMUDA_BACKEND_XH", defaultLoopbackXH),
		backendWS:  getEnv("BERMUDA_BACKEND_WS", defaultLoopbackWS),
		backendTR:  getEnv("BERMUDA_BACKEND_TR", defaultLoopbackTR),
		healthPath: getEnv("BERMUDA_HEALTH_PATH", defaultHealthPath),
		adminToken: strings.TrimSpace(getEnv("BERMUDA_ADMIN_TOKEN", "")),
		bufPool:    newRecycledBufferPool(proxyBuf),
		tunnels:    make(map[*net.TCPConn]struct{}),
		startedAt:  time.Now(),
		dialer:     net.Dialer{Timeout: backendDialTimeout, KeepAlive: -1},
	}
	g.tr = newLoopbackTransport(transportBuf)
	proxy, err := newBackendProxy(g.backendXH, g.tr, g.bufPool)
	if err != nil {
		log.Fatalf("[Gateway] Fatal: invalid XHTTP backend address %q: %v", g.backendXH, err)
	}
	g.xhProxy = proxy
	return g
}

func newLoopbackTransport(bufSize int) *http.Transport {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: -1}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   512,
		MaxConnsPerHost:       512,
		IdleConnTimeout:       150 * time.Second,
		DisableCompression:    true,
		ReadBufferSize:        bufSize,
		WriteBufferSize:       bufSize,
		ResponseHeaderTimeout: 120 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

func newBackendProxy(targetAddr string, tr http.RoundTripper, bp httputil.BufferPool) (*httputil.ReverseProxy, error) {
	targetURL, err := url.Parse("http://" + targetAddr)
	if err != nil {
		return nil, err
	}
	if targetURL.Host == "" {
		return nil, errors.New("missing target host")
	}
	return &httputil.ReverseProxy{
		Transport:     tr,
		BufferPool:    bp,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = targetURL.Scheme
			pr.Out.URL.Host = targetURL.Host
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Accel-Buffering", "no")
			if cfIP := pr.In.Header.Get("CF-Connecting-IP"); cfIP != "" {
				pr.Out.Header.Set("CF-Connecting-IP", cfIP)
			}
		},
		ModifyResponse: func(res *http.Response) error {
			switch res.StatusCode {
			case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed:
				rewriteTo404(res)
			default:
				res.Header.Set("X-Accel-Buffering", "no")
				res.Header.Set("Server", openrestyServerToken)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if !errors.Is(err, context.Canceled) {
				log.Printf("[Proxy] Error: backend %s unreachable: %v", targetAddr, err)
			}
			camoOpenResty(w, r, http.StatusBadGateway, openresty50xHTML, map[string]string{
				"Last-Modified": openresty50xLastModified,
				"ETag":          openresty50xETag,
				"Accept-Ranges": "bytes",
			})
		},
	}, nil
}

// rewriteTo404 hides Go/Xray internal error signatures behind the authentic OpenResty 404 HTML.
func rewriteTo404(res *http.Response) {
	if res.Body != nil {
		_ = res.Body.Close()
	}
	res.StatusCode = http.StatusNotFound
	res.Status = "404 Not Found"
	res.Header = http.Header{}
	res.Header.Set("Server", openrestyServerToken)
	res.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	res.Header.Set("Content-Type", "text/html")
	res.Header.Set("Content-Length", strconv.Itoa(len(openresty404HTML)))
	res.Body = io.NopCloser(strings.NewReader(openresty404HTML))
	res.ContentLength = int64(len(openresty404HTML))
	res.TransferEncoding = nil
}

func (g *Gateway) SetDraining()     { g.draining.Store(true) }
func (g *Gateway) IsDraining() bool { return g.draining.Load() }

func (g *Gateway) CloseIdleBackendConns() {
	if g.tr != nil {
		g.tr.CloseIdleConnections()
	}
}

func (g *Gateway) TrackConnState(_ net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		g.openConns.Add(1)
	case http.StateClosed, http.StateHijacked:
		g.openConns.Add(-1)
	}
}

// track registers raw *net.TCPConn pairs directly into the registry.
// Without struct wrappers, Go's runtime preserves concrete type assertions for splice(2).
func (g *Gateway) track(a, b *net.TCPConn) {
	g.tunnelsMu.Lock()
	g.tunnels[a] = struct{}{}
	g.tunnels[b] = struct{}{}
	g.tunnelsMu.Unlock()
	g.tunnelsLive.Add(1)
	g.tunnelsHit.Add(1)
}

func (g *Gateway) untrack(a, b *net.TCPConn) {
	g.tunnelsMu.Lock()
	delete(g.tunnels, a)
	delete(g.tunnels, b)
	g.tunnelsMu.Unlock()
	g.tunnelsLive.Add(-1)
}

// WaitTunnels blocks until all active hijacked WebSocket tunnels terminate or ctx expires.
func (g *Gateway) WaitTunnels(ctx context.Context) {
	t := time.NewTicker(tunnelDrainPollInterval)
	defer t.Stop()
	for g.tunnelsLive.Load() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// CloseTunnels forcefully terminates all active spliced TCP sockets during graceful shutdown.
func (g *Gateway) CloseTunnels() {
	g.tunnelsMu.Lock()
	conns := make([]*net.TCPConn, 0, len(g.tunnels))
	for c := range g.tunnels {
		conns = append(conns, c)
	}
	g.tunnelsMu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func matchBase(p, base string) bool {
	return p == base || strings.HasPrefix(p, base+"/")
}

func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.totalReq.Add(1)
		p := r.URL.Path
		switch {
		case p == g.healthPath:
			g.handleHealth(w, r)
		case matchBase(p, g.pathXH):
			if !g.validXHTTP(r) {
				g.camo404(w, r)
				return
			}
			_ = http.NewResponseController(w).EnableFullDuplex()
			g.xhProxy.ServeHTTP(w, r)
		case matchBase(p, g.pathWS):
			g.serveWS(w, r, g.backendWS)
		case matchBase(p, g.pathTR):
			g.serveWS(w, r, g.backendTR)
		default:
			g.serveCamouflage(w, r)
		}
	})
}

// validXHTTP permits POST requests (chunks/stream-one) or GET with session segments.
func (g *Gateway) validXHTTP(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost:
		return true
	case http.MethodGet:
		return strings.Trim(strings.TrimPrefix(r.URL.Path, g.pathXH), "/") != ""
	}
	return false
}

func headerHasToken(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// validWSUpgrade validates RFC 6455 requirements for incoming WebSocket handshakes.
func validWSUpgrade(r *http.Request) bool {
	if r.Method != http.MethodGet || r.ProtoMajor != 1 || r.ProtoMinor < 1 {
		return false
	}
	if !headerHasToken(r.Header, "Connection", "upgrade") || !headerHasToken(r.Header, "Upgrade", "websocket") {
		return false
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(r.Header.Get("Sec-WebSocket-Key"))
	return err == nil && len(key) == 16
}

// serveWS validates the upgrade, completes it against Xray, then splices raw TCP.
// Zero wrappers are placed around TCPConn, allowing Linux kernel splice(2) to engage.
func (g *Gateway) serveWS(w http.ResponseWriter, r *http.Request, backend string) {
	if !validWSUpgrade(r) {
		g.camo404(w, r)
		return
	}
	if g.draining.Load() {
		camoOpenResty(w, r, http.StatusBadGateway, openresty50xHTML, nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), backendDialTimeout)
	raw, err := g.dialer.DialContext(ctx, "tcp", backend)
	cancel()
	if err != nil {
		camoOpenResty(w, r, http.StatusBadGateway, openresty50xHTML, nil)
		return
	}
	bc, ok := raw.(*net.TCPConn)
	if !ok {
		_ = raw.Close()
		camoOpenResty(w, r, http.StatusBadGateway, openresty50xHTML, nil)
		return
	}

	_ = bc.SetDeadline(time.Now().Add(backendHandshakeTimeout))
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Body = http.NoBody
	out.ContentLength = 0
	if _, has := out.Header["User-Agent"]; !has {
		out.Header["User-Agent"] = []string{""}
	}
	if cfIP := r.Header.Get("CF-Connecting-IP"); cfIP != "" {
		out.Header.Set("CF-Connecting-IP", cfIP)
	}

	if err := out.Write(bc); err != nil {
		_ = bc.Close()
		camoOpenResty(w, r, http.StatusBadGateway, openresty50xHTML, nil)
		return
	}
	br := bufio.NewReaderSize(bc, 4096)
	resp, err := http.ReadResponse(br, out)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		_ = bc.Close()
		g.camo404(w, r)
		return
	}
	_ = bc.SetDeadline(time.Time{})

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = bc.Close()
		camoOpenResty(w, r, http.StatusBadGateway, openresty50xHTML, nil)
		return
	}
	cc, brw, err := hj.Hijack()
	if err != nil {
		_ = bc.Close()
		return
	}
	ct, ok := cc.(*net.TCPConn)
	if !ok {
		_ = cc.Close()
		_ = bc.Close()
		return
	}
	_ = ct.SetDeadline(time.Time{})

	var head bytes.Buffer
	head.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	_ = resp.Header.Write(&head)
	head.WriteString("\r\n")
	if n := br.Buffered(); n > 0 {
		pending, _ := br.Peek(n)
		head.Write(pending)
	}
	if _, err := ct.Write(head.Bytes()); err != nil {
		_ = ct.Close()
		_ = bc.Close()
		return
	}
	if n := brw.Reader.Buffered(); n > 0 {
		pending, _ := brw.Reader.Peek(n)
		if _, err := bc.Write(pending); err != nil {
			_ = ct.Close()
			_ = bc.Close()
			return
		}
	}
	g.relay(ct, bc)
}

// relay moves bytes bi-directionally with io.Copy on raw *net.TCPConn pairs (splice on Linux).
func (g *Gateway) relay(client, backend *net.TCPConn) {
	g.track(client, backend)
	defer g.untrack(client, backend)
	done := make(chan struct{}, 2)
	go pipe(backend, client, done)
	go pipe(client, backend, done)
	<-done
	<-done
	_ = client.Close()
	_ = backend.Close()
}

func pipe(dst, src *net.TCPConn, done chan<- struct{}) {
	_, err := io.Copy(dst, src)
	if err != nil {
		_ = dst.Close()
		_ = src.Close()
	} else {
		_ = dst.CloseWrite()
		_ = dst.SetReadDeadline(time.Now().Add(halfCloseGrace))
	}
	done <- struct{}{}
}

func (g *Gateway) camo404(w http.ResponseWriter, r *http.Request) {
	camoOpenResty(w, r, http.StatusNotFound, openresty404HTML, nil)
}

func (g *Gateway) serveCamouflage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		camoOpenResty(w, r, http.StatusMethodNotAllowed, openresty405HTML, nil)
		return
	}
	clean := path.Clean("/" + r.URL.Path)
	switch {
	case clean == "/" || clean == "/index.html":
		if requestNotModified(r, openrestyIndexETag, openrestyIndexLastModified) {
			camoNotModified(w, r, openrestyIndexETag, openrestyIndexLastModified)
			return
		}
		camoOpenResty(w, r, http.StatusOK, openrestyWelcomeHTML, map[string]string{
			"Last-Modified": openrestyIndexLastModified,
			"ETag":          openrestyIndexETag,
			"Accept-Ranges": "bytes",
		})
	case clean == "/50x.html":
		if requestNotModified(r, openresty50xETag, openresty50xLastModified) {
			camoNotModified(w, r, openresty50xETag, openresty50xLastModified)
			return
		}
		camoOpenResty(w, r, http.StatusOK, openresty50xHTML, map[string]string{
			"Last-Modified": openresty50xLastModified,
			"ETag":          openresty50xETag,
			"Accept-Ranges": "bytes",
		})
	default:
		g.camo404(w, r)
	}
}

func requestNotModified(r *http.Request, etag, lastModified string) bool {
	if inm := strings.TrimSpace(r.Header.Get("If-None-Match")); inm != "" {
		if inm == "*" {
			return true
		}
		for _, c := range strings.Split(inm, ",") {
			if strings.EqualFold(strings.TrimSpace(c), etag) {
				return true
			}
		}
		return false
	}
	ims := strings.TrimSpace(r.Header.Get("If-Modified-Since"))
	if ims == "" {
		return false
	}
	t, err := http.ParseTime(ims)
	if err != nil {
		return false
	}
	lm, err := http.ParseTime(lastModified)
	return err == nil && t.Equal(lm)
}

// handleHealth outputs a clean plain-text status for Railway edge probes,
// while exposing full JSON telemetry only when presented with the secret admin token.
func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		g.camo404(w, r)
		return
	}
	snap := g.sup.Snapshot()
	snap.ChildPID = 0
	draining := g.draining.Load()
	healthy := !draining && snap.Running && snap.Ready
	status, code := "ok", http.StatusOK
	switch {
	case draining:
		status, code = "draining", http.StatusServiceUnavailable
	case !snap.Running:
		status, code = "down", http.StatusServiceUnavailable
	case !snap.Ready:
		status, code = "starting", http.StatusServiceUnavailable
	}
	h := w.Header()
	h.Set("Server", openrestyServerToken)
	h.Set("Cache-Control", "no-store, no-cache, must-revalidate")

	tok := r.Header.Get("X-Bermuda-Token")
	if g.adminToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(g.adminToken)) == 1 {
		h.Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		if r.Method == http.MethodHead {
			return
		}
		_ = json.NewEncoder(w).Encode(HealthResponse{
			Status:     status,
			Draining:   draining,
			Healthy:    healthy,
			UptimeSec:  int64(time.Since(g.startedAt).Seconds()),
			Supervisor: snap,
			Telemetry: TelemetrySnapshot{
				OpenConnections: g.openConns.Load(),
				ActiveTunnels:   g.tunnelsLive.Load(),
				TunnelsOpened:   g.tunnelsHit.Load(),
				TotalRequests:   g.totalReq.Load(),
			},
		})
		return
	}

	h.Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	if r.Method == http.MethodHead {
		return
	}
	if healthy {
		_, _ = io.WriteString(w, "ok\n")
	} else {
		_, _ = io.WriteString(w, status+"\n")
	}
}

func camoNotModified(w http.ResponseWriter, r *http.Request, etag, lastModified string) {
	h := w.Header()
	h.Set("Server", openrestyServerToken)
	h.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	h.Set("Last-Modified", lastModified)
	h.Set("ETag", etag)
	if shouldCloseConnection(r) {
		h.Set("Connection", "close")
	} else {
		h.Set("Connection", "keep-alive")
	}
	w.WriteHeader(http.StatusNotModified)
}

func camoOpenResty(w http.ResponseWriter, r *http.Request, code int, body string, extra map[string]string) {
	h := w.Header()
	h.Set("Server", openrestyServerToken)
	h.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	h.Set("Content-Type", "text/html")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if shouldCloseConnection(r) {
		h.Set("Connection", "close")
	} else {
		h.Set("Connection", "keep-alive")
	}
	for k, v := range extra {
		h.Set(k, v)
	}
	w.WriteHeader(code)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, body)
}

func shouldCloseConnection(r *http.Request) bool {
	return r.Close || headerHasToken(r.Header, "Connection", "close")
}
