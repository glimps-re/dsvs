package gateway

import (
	"bufio"
	"context"
	"crypto/tls"
	_ "embed"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"dsvs/internal/build"
	"dsvs/internal/config"
	"dsvs/internal/core"
)

// Net is what the proxy expects of the session: the facade that answers for a host, how to reach an
// app and, when it does not answer, where it stands, how to open the public port — which, depending
// on the network, is not opened in the same place —, what the machine shows, which says on which
// network a remote target is reached, and how to learn that something changed: a wake-up, and the
// generation of the applied configuration. The proxy targets live in the facade: the configuration
// decides them; what an app asks of its pages, in its declaration.
type Net interface {
	Config() config.Config
	Generation() uint64
	Facade(host string) (config.Ingress, bool)
	Backend(app string) core.Backend
	Snapshot(app string) core.Snapshot
	Listen() (net.Listener, error)
	Observed() *config.Observed
	Watch() (<-chan core.Event, func())
}

// Proxy implements core.Gateway by proxying in Go, without an external process.
type Proxy struct {
	net Net
	say func(ingress, line string)

	// The server is recreated on every start: an http.Server does not serve again after it stops.
	// quit closes when it stops: an event stream never ends on its own, and the stop would wait for
	// it.
	mu     sync.Mutex
	server *http.Server
	quit   chan struct{}
	relays map[string]*relay
	counts map[string]*counters
}

// idleTimeout bounds the life of the connections kept warm: a target that leaves the configuration
// holds nothing forever.
const idleTimeout = 90 * time.Second

// reaching bounds the opening of a connection to a remote target. A network that does not reach it
// often drops the packets without answering, and the kernel only gives up after two minutes: the
// request would stay silent all that time, whereas a 502 that names the target reads at once. A
// reachable target, even through a tunnel, answers in far less.
const reaching = 5 * time.Second

// relay is the path to a target: the proxy and its transport. via recognizes the next target.
type relay struct {
	proxy *httputil.ReverseProxy
	tr    *http.Transport
	via   any
}

// reached tells a remote target apart from one time to the next: its address, and the observation
// under which it is reached. A connection opened on another network died with it, and reusing it
// would leave the request waiting for a response that will not come.
type reached struct {
	url string
	on  *config.Observed
}

type counters struct {
	requests atomic.Int64
	failures atomic.Int64
}

// NewProxy proxies for the session; say writes to the named facade's log.
func NewProxy(network Net, say func(ingress, line string)) *Proxy {
	return &Proxy{net: network, say: say, relays: map[string]*relay{}, counts: map[string]*counters{}}
}

func (p *Proxy) Start(ctx context.Context) error {
	listener, err := p.net.Listen()
	if err != nil {
		return err
	}
	// ErrorLog: the standard server writes its incidents to stderr by default — the TUI's screen.
	server := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second, ErrorLog: p.logger("")}
	quit := make(chan struct{})
	server.RegisterOnShutdown(func() { close(quit) })
	p.mu.Lock()
	p.server, p.quit = server, quit
	p.mu.Unlock()

	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.say("", fmt.Sprintf("!! %v", err))
		}
	}()
	return nil
}

// Stop returns as soon as the in-flight requests are done; websocket connections only close when
// the listener is cut.
func (p *Proxy) Stop(ctx context.Context) error {
	p.mu.Lock()
	server := p.server
	p.server = nil
	p.mu.Unlock()
	if server == nil {
		return nil
	}
	err := server.Shutdown(ctx)
	server.Close()
	if errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

func (p *Proxy) Stats(ingress string) (requests, failures int) {
	c := p.counters(ingress)
	return int(c.requests.Load()), int(c.failures.Load())
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	in, known := p.net.Facade(r.Host)
	if !known {
		// No facade claims this host: nobody to tell the request to.
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "no facade answers for %s\n", r.Host)
		return
	}
	// What dsvs serves for itself is not a request of the facade: neither counted nor told.
	if name, own := strings.CutPrefix(r.URL.Path, config.Reserved+"/"); own {
		p.own(w, r, name)
		return
	}
	p.counters(in.Name).requests.Add(1)

	route, matched := in.Match(r.URL.Path)
	switch {
	case !matched:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "%s does not route %s\n", in.Name, r.URL.Path)
		p.trace(in, r, http.StatusNotFound, "no route", start)

	case route.Kind() == config.ToRedirect:
		to := route.Redirection(r.URL)
		http.Redirect(w, r, to, http.StatusFound)
		p.trace(in, r, http.StatusFound, "↪ "+to, start)

	case route.Kind() == config.ToProxy:
		target := route.Proxy
		recorder := &recorder{ResponseWriter: w, status: http.StatusOK}
		p.upstream(in, target).ServeHTTP(recorder, r)
		p.trace(in, r, recorder.status, target.Host(), start)

	default:
		back := p.net.Backend(route.App)
		switch {
		case back != nil:
			p.trace(in, r, p.serve(w, r, in, back), route.App, start)
		case navigates(r) && p.coming(route.App):
			p.await(w, r.Host, route.App)
			p.trace(in, r, http.StatusServiceUnavailable, route.App+" starting", start)
		default:
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintln(w, p.unanswered(route.App, nil))
			p.trace(in, r, http.StatusBadGateway, route.App+" not ready", start)
		}
	}
}

// navigates says whether the request is that of a browser opening a page: only it reads a waiting
// page and runs its script; a fetch, an image, curl or an agent want today's response.
// Sec-Fetch-Mode says so, and no script can write it. A browser only sends it to a secure origin —
// *.localhost is one, an /etc/hosts host served over http is not — and without it, it is the Accept
// of a navigation that says so, a fetch's being */*.
func navigates(r *http.Request) bool {
	mode := r.Header.Get("Sec-Fetch-Mode")
	switch {
	case r.Method != http.MethodGet:
		return false
	case mode != "":
		return mode == "navigate"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// coming says whether the app is on its way to "ready": started or restarted, and not already seen
// listening elsewhere — that one will not answer on its own, and the 502 says why.
func (p *Proxy) coming(app string) bool {
	snap := p.net.Snapshot(app)
	return (snap.Status == core.Starting || snap.Status == core.Restarting) && snap.Astray == nil
}

// await serves the waiting page: what the 502 would say, and the script that reloads it as soon as
// the address serves something else — the app ready, or down, whose reason the 502 will then tell.
// A 503, because that is what it is: the service is not there yet, and will be. Without
// Retry-After: a browser does not read it on a page, and the delay is not known.
func (p *Proxy) await(w http.ResponseWriter, host, app string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, waiting, html.EscapeString(app), html.EscapeString(p.unanswered(app, nil)),
		config.Reserved, html.EscapeString(p.apps(host)))
}

const waiting = `<!doctype html>
<meta charset="utf-8">
<title>%[1]s is starting…</title>
<pre>%[2]s
this page reloads when %[1]s answers</pre>
<script src="%[3]s/reload.js" data-apps="%[4]s"></script>
`

// reload is the script that reloads a page when its address serves something other than what it
// saw.
//
//go:embed reload.js
var reload string

// own serves what dsvs keeps for itself under config.Reserved, on every address: the script that
// reloads a page, and the stream that tells it when.
func (p *Proxy) own(w http.ResponseWriter, r *http.Request, name string) {
	switch name {
	case "reload.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		io.WriteString(w, reload)
	case "events":
		p.stream(w, r)
	default:
		http.NotFound(w, r)
	}
}

// stream tells, as server-sent events, what a page served by the request's address may have seen:
// one event per fact, on opening, then each time one changes. It wakes up on every change of the
// session and re-reads the facts, so a lost wake-up does not make one missed: the next one re-reads
// them. It stops with the client or the server.
func (p *Proxy) stream(w http.ResponseWriter, r *http.Request) {
	changes, stop := p.net.Watch()
	defer stop()
	p.mu.Lock()
	quit := p.quit
	p.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	said := map[string]string{}
	for {
		for name, now := range p.facts(r.Host) {
			if before, sent := said[name]; !sent || before != now {
				said[name] = now
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, now)
			}
		}
		http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
			return
		case <-quit:
			return
		case <-changes:
		}
	}
}

// facts are what a page served by this address may have seen, and carries in the script's data-*:
// where the apps of its facade stand, for the waiting page, and the generation of the applied
// configuration, for the pages of an app that reloads with it.
func (p *Proxy) facts(host string) map[string]string {
	return map[string]string{"apps": p.apps(host), "config": strconv.FormatUint(p.net.Generation(), 10)}
}

// apps tells in one line where each app of this host's facade stands.
func (p *Proxy) apps(host string) string {
	in, _ := p.net.Facade(host)
	var said []string
	for _, app := range in.Apps() {
		snap := p.net.Snapshot(app)
		state := app + " " + snap.Status.String()
		if snap.Astray != nil {
			state += " astray"
		}
		said = append(said, state)
	}
	return strings.Join(said, ", ")
}

// serve proxies the request to the app and returns its response's status. When its declaration
// reloads its pages and a browser opens one, the document goes out with the reload script — and the
// app serves it whole and readable: without compression, which would have to be undone, nor 304,
// which would resurrect a copy carrying another generation.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, in config.Ingress, back core.Backend) int {
	doc := &page{ResponseWriter: w}
	if declared, _ := p.net.Config().App(back.Name()); declared.Reload.Page && navigates(r) {
		for _, header := range []string{"Accept-Encoding", "If-None-Match", "If-Modified-Since"} {
			r.Header.Del(header)
		}
		doc.script = fmt.Sprintf("<script src=\"%s/reload.js\" data-config=\"%d\"></script>\n", config.Reserved, p.net.Generation())
	}
	recorder := &recorder{ResponseWriter: doc, status: http.StatusOK}
	p.app(in, back).ServeHTTP(recorder, r)
	doc.end()
	return recorder.status
}

// page appends a script at the end of an HTML document, when it has one to append: after the app's
// last byte, so a streamed document waits for nothing. The browser puts it in the body. The
// announced length is no longer right, it goes away, and the response goes out chunked; a document
// compressed anyway passes through untouched. A CSP that refuses the script leaves the page intact.
type page struct {
	http.ResponseWriter
	script string
	html   bool
}

func (p *page) WriteHeader(status int) {
	header := p.Header()
	p.html = p.script != "" && strings.HasPrefix(header.Get("Content-Type"), "text/html") && header.Get("Content-Encoding") == ""
	if p.html {
		header.Del("Content-Length")
		header.Del("ETag")
		header.Del("Last-Modified")
		header.Set("Cache-Control", "no-store")
	}
	p.ResponseWriter.WriteHeader(status)
}

// end writes the script, once the document is whole.
func (p *page) end() {
	if p.html {
		io.WriteString(p.ResponseWriter, p.script)
	}
}

func (p *page) Unwrap() http.ResponseWriter { return p.ResponseWriter }

// app returns the proxy to an app's current run. The app's name stands in for the host: the
// connection is opened by the backend, so the real address appears nowhere in the request.
func (p *Proxy) app(in config.Ingress, back core.Backend) *httputil.ReverseProxy {
	name := back.Name()
	unanswered := func(err error) string { return p.unanswered(name, err) }
	return p.reuse(in, "app "+name, back.Addr(), unanswered, func() (*http.Transport, func(*httputil.ProxyRequest)) {
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				live := p.net.Backend(name)
				if live == nil {
					return nil, fmt.Errorf("%s is not listening", name)
				}
				return live.DialContext(ctx)
			},
			IdleConnTimeout: idleTimeout,
		}
		return tr, func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = name
			// The dev-server sees the browser's host: it is the one it allows, and the one it
			// rewrites in its redirects.
			pr.Out.Host = pr.In.Host
		}
	})
}

// upstream returns the proxy to a proxy target: the real host, its TLS, and the Host header the
// target expects.
func (p *Proxy) upstream(in config.Ingress, target config.Target) *httputil.ReverseProxy {
	base, err := url.Parse(target.URL)
	if err != nil {
		base = &url.URL{Scheme: "http", Host: target.URL}
	}
	prefix := strings.TrimSuffix(base.Path, "/")
	unreached := func(err error) string { return p.unreached(target.Host(), err) }
	return p.reuse(in, "proxy "+target.URL, reached{target.URL, p.net.Observed()}, unreached, func() (*http.Transport, func(*httputil.ProxyRequest)) {
		tr := &http.Transport{
			DialContext:     (&net.Dialer{Timeout: reaching}).DialContext,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: !target.Verify()}, //nolint:gosec // secure: false, like the proxy.conf it replaces
			IdleConnTimeout: idleTimeout,
		}
		return tr, func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = base.Scheme
			pr.Out.URL.Host = base.Host
			pr.Out.URL.Path = prefix + pr.In.URL.Path
			// changeOrigin: the target is a remote vhost, it needs its own host. Without it, the
			// browser's is kept, as http-proxy does.
			pr.Out.Host = pr.In.Host
			if target.ChangeOrigin {
				pr.Out.Host = base.Host
			}
		}
	})
}

// reuse keeps one proxy per target, named by key, and rebuilds it when via — what tells it apart
// from one run or one network to the next — changes. explain tells whoever made the request why the
// target did not answer; the facade's log keeps the failure as is.
func (p *Proxy) reuse(in config.Ingress, key string, via any, explain func(error) string, construct func() (*http.Transport, func(*httputil.ProxyRequest))) *httputil.ReverseProxy {
	p.mu.Lock()
	defer p.mu.Unlock()
	if kept, known := p.relays[key]; known {
		if kept.via == via {
			return kept.proxy
		}
		kept.tr.CloseIdleConnections()
	}
	tr, rewrite := construct()
	fresh := &relay{tr: tr, via: via}
	fresh.proxy = &httputil.ReverseProxy{
		Transport:     tr,
		Rewrite:       rewrite,
		FlushInterval: -1,
		// ErrorLog receives what the standard proxy tells outside ErrorHandler — an interrupted
		// response copy, for instance — which would otherwise go to stderr, the TUI's screen.
		ErrorLog: p.logger(in.Name),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintln(w, explain(err))
			p.say(in.Name, fmt.Sprintf("!! %s %s %s: %v", r.Method, r.URL.Path, key, err))
		},
	}
	p.relays[key] = fresh
	return fresh.proxy
}

// unanswered tells whoever opens an app's address why it does not answer, and what to do: a 502's
// page is all they see of it. err is the connection failure, when the app said it was ready.
func (p *Proxy) unanswered(app string, err error) string {
	snap, cli := p.net.Snapshot(app), build.Name()
	switch {
	case snap.Status == core.Ready && err != nil:
		return fmt.Sprintf("%s did not answer: %s; %s logs %s", app, cause(err), cli, app)
	case snap.Status == core.Stopping:
		return fmt.Sprintf("%s is stopping: %s start %s brings it back", app, cli, app)
	case snap.Astray != nil:
		return fmt.Sprintf("%s is starting, but %s: its command must make it listen on {ip} and {port}", app, snap.Astray)
	case snap.Status != core.Idle:
		return fmt.Sprintf("%s is %s and does not answer yet: %s logs %s", app, snap.Status, cli, app)
	case snap.Last != nil && snap.Last.Fell():
		return fmt.Sprintf("%s fell, %s: %s logs %s, then %s start %s", app, snap.Last, cli, app, cli, app)
	}
	return fmt.Sprintf("%s is stopped: %s start %s", app, cli, app)
}

// unreached tells whoever made the request why a remote target did not answer, and where to look: a
// server that refuses is probably down, a server out of reach is so from this network — and only
// then does the status tell the network the session sees.
func (p *Proxy) unreached(host string, err error) string {
	var unnamed *net.DNSError
	var late interface{ Timeout() bool }
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return host + " refuses connections: is the server down?"
	case errors.As(err, &unnamed), errors.As(err, &late) && late.Timeout(),
		errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return fmt.Sprintf("%s is unreachable from this network (%s): %s; %s state says the network the session sees",
			host, p.net.Observed(), cause(err), build.Name())
	}
	return host + " cannot be reached: " + cause(err)
}

// cause says why a connection failed, without the addresses it tried: the one DNS returned, the one
// it left from — the apps' private network —, which nobody typed.
func cause(err error) string {
	var unnamed *net.DNSError
	var failed *net.OpError
	var call *os.SyscallError
	switch {
	case errors.Is(err, io.EOF):
		// What a network says that accepts the connection but leads nowhere: the apps' private
		// network, whose gatekeeper replays the connection from the machine.
		return "the connection closed before any answer"
	case errors.As(err, &unnamed):
		return "its name does not resolve (" + unnamed.Err + ")"
	case errors.As(err, &call):
		return call.Err.Error()
	case errors.As(err, &failed):
		return failed.Err.Error()
	}
	return err.Error()
}

func (p *Proxy) counters(ingress string) *counters {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, known := p.counts[ingress]; known {
		return c
	}
	c := &counters{}
	p.counts[ingress] = c
	return c
}

// trace logs the request in the facade that received it and, alone in this file, counts the
// failures: the status is enough to recognize them, wherever they were decided.
func (p *Proxy) trace(in config.Ingress, r *http.Request, status int, note string, start time.Time) {
	if status >= 400 {
		p.counters(in.Name).failures.Add(1)
	}
	method := r.Method
	if r.Header.Get("Upgrade") != "" {
		method = "WS"
	}
	p.say(in.Name, fmt.Sprintf("%-4s %-40s %3d  %-22s %s",
		method, truncate(r.URL.RequestURI(), 40), status, note, took(time.Since(start))))
}

// recorder records the response's status. Unwrap lets http.NewResponseController reach the real
// ResponseWriter, which the proxy needs to hijack the connection of an upgrade.
type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Hijack: an upgrade leaves the HTTP world without going through WriteHeader. It is the only place
// where one can note that the response was a 101, and it is what the log shows for a websocket.
func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffered, err := http.NewResponseController(r.ResponseWriter).Hijack()
	if err == nil {
		r.status = http.StatusSwitchingProtocols
	}
	return conn, buffered, err
}

// logger turns a log into a log.Logger: the internal messages of the server and of the standard
// proxy go where the other traces go, never to stderr. Without a facade name, they are about the
// port and go to the session's log.
func (p *Proxy) logger(ingress string) *log.Logger {
	return log.New(writerFunc(func(b []byte) (int, error) {
		p.say(ingress, "!! "+strings.TrimRight(string(b), "\n"))
		return len(b), nil
	}), "", 0)
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(b []byte) (int, error) { return w(b) }

func took(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	return d.Round(time.Millisecond).String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
