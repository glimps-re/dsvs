package gateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dsvs/internal/config"
	"dsvs/internal/core"
)

// Net is what the proxy expects of the session: the facade that answers for a host, how to reach an
// app, how to open the public port — which, depending on the network, is not opened in the same place
// —, and what the machine shows, which says on which network a remote target is reached. The proxy
// targets, for their part, are in the facade: the configuration decides them.
type Net interface {
	Facade(host string) (config.Ingress, bool)
	Backend(app string) core.Backend
	Listen(port int) (net.Listener, error)
	Observed() *config.Observed
}

// Proxy implements core.Gateway by proxying in Go, without an external process.
type Proxy struct {
	port int
	net  Net
	say  func(ingress, line string)

	// The server is recreated on every start: an http.Server does not serve again after it stops.
	mu     sync.Mutex
	server *http.Server
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
func NewProxy(port int, network Net, say func(ingress, line string)) *Proxy {
	return &Proxy{port: port, net: network, say: say, relays: map[string]*relay{}, counts: map[string]*counters{}}
}

func (p *Proxy) Start(ctx context.Context) error {
	listener, err := p.net.Listen(p.port)
	if err != nil {
		return err
	}
	// ErrorLog: the standard server writes its incidents to stderr by default — the TUI's screen.
	server := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second, ErrorLog: p.logger("")}
	p.mu.Lock()
	p.server = server
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
	p.counters(in.Name).requests.Add(1)

	route, matched := in.Match(r.URL.Path)
	switch {
	case !matched:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "%s does not route %s\n", in.Name, r.URL.Path)
		p.trace(in, r, http.StatusNotFound, "no route", start)

	case route.Kind() == config.ToRedirect:
		http.Redirect(w, r, route.Redirect, http.StatusFound)
		p.trace(in, r, http.StatusFound, "↪ "+route.Redirect, start)

	case route.Kind() == config.ToProxy:
		target := route.Proxy
		recorder := &recorder{ResponseWriter: w, status: http.StatusOK}
		p.upstream(in, target).ServeHTTP(recorder, r)
		p.trace(in, r, recorder.status, target.Host(), start)

	default:
		back := p.net.Backend(route.App)
		if back == nil {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "%s is not listening yet\n", route.App)
			p.trace(in, r, http.StatusBadGateway, route.App+" not ready", start)
			return
		}
		recorder := &recorder{ResponseWriter: w, status: http.StatusOK}
		p.app(in, back).ServeHTTP(recorder, r)
		p.trace(in, r, recorder.status, route.App, start)
	}
}

// app returns the proxy to an app's current run. The app's name stands in for the host: the
// connection is opened by the backend, so the real address appears nowhere in the request.
func (p *Proxy) app(in config.Ingress, back core.Backend) *httputil.ReverseProxy {
	name := back.Name()
	return p.reuse(in, "app "+name, back.Addr(), func() (*http.Transport, func(*httputil.ProxyRequest)) {
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
	return p.reuse(in, "proxy "+target.URL, reached{target.URL, p.net.Observed()}, func() (*http.Transport, func(*httputil.ProxyRequest)) {
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
// from one run or one network to the next — changes.
func (p *Proxy) reuse(in config.Ingress, key string, via any, build func() (*http.Transport, func(*httputil.ProxyRequest))) *httputil.ReverseProxy {
	p.mu.Lock()
	defer p.mu.Unlock()
	if kept, known := p.relays[key]; known {
		if kept.via == via {
			return kept.proxy
		}
		kept.tr.CloseIdleConnections()
	}
	tr, rewrite := build()
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
			fmt.Fprintf(w, "%s: %v\n", key, err)
			p.say(in.Name, fmt.Sprintf("!! %s %s %s: %v", r.Method, r.URL.Path, key, err))
		},
	}
	p.relays[key] = fresh
	return fresh.proxy
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
