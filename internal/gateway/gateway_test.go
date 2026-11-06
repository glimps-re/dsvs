package gateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dsvs/internal/config"
	"dsvs/internal/core"
)

// The host picks the facade, the path picks the route: the same path does not mean the same thing
// depending on the host that received it.
func TestHostThenPathDecide(t *testing.T) {
	t.Parallel()
	api := upstream(t, "api")
	live := map[string]core.Backend{
		"alpha": app(t, "alpha", "alpha"),
		"beta":  app(t, "beta", "beta"),
		"gone":  nil,
	}
	fake := &testNet{
		live: live,
		facades: facades(
			config.Ingress{Host: "localhost", Routes: []config.Route{
				{Path: "/alpha", App: "alpha"},
				{Path: "/api/alpha", Proxy: config.Target{URL: api.URL, ChangeOrigin: true}},
				{Path: "/gone", App: "gone"},
				{Path: "/", Redirect: "/alpha/"},
			}},
			config.Ingress{Host: "beta.localhost", Routes: []config.Route{
				{Path: "/beta", App: "beta"},
			}},
		),
	}
	front := httptest.NewServer(NewProxy(0, fake, func(string, string) {}))
	t.Cleanup(front.Close)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(host, path string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, front.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if location := res.Header.Get("location"); location != "" {
			return res.StatusCode, location
		}
		return res.StatusCode, strings.TrimSpace(string(body))
	}

	cases := []struct {
		host, path string
		status     int
		body       string
	}{
		{"localhost", "/alpha/main.js", http.StatusOK, "alpha /alpha/main.js"},
		{"localhost:4200", "/alpha/main.js", http.StatusOK, "alpha /alpha/main.js"},
		{"beta.localhost", "/beta/main.js", http.StatusOK, "beta /beta/main.js"},
		{"localhost", "/api/alpha/analyses", http.StatusOK, "api /api/alpha/analyses " + addr(api.URL)},
		// localhost's catch-all redirects; the same address on beta.localhost has no route.
		{"localhost", "/beta/main.js", http.StatusFound, "/alpha/"},
		{"localhost", "/", http.StatusFound, "/alpha/"},
		{"beta.localhost", "/favicon.ico", http.StatusNotFound, "beta.localhost does not route /favicon.ico"},
		{"nowhere.test", "/alpha/", http.StatusNotFound, "no facade answers for nowhere.test"},
	}
	for _, c := range cases {
		if status, body := get(c.host, c.path); status != c.status || body != c.body {
			t.Errorf("%s%s = %d %q, want %d %q", c.host, c.path, status, body, c.status, c.body)
		}
	}

	// An app that was not started gives a 502 that names it, not an opaque connection error.
	if status, body := get("localhost", "/gone/x"); status != http.StatusBadGateway || !strings.Contains(body, "gone") {
		t.Errorf("GET /gone/x = %d %q, want a 502 naming the app", status, body)
	}
	// An app announced ready but unreachable goes through the ErrorHandler.
	live["gone"] = stub{name: "gone", addr: "127.0.0.1:1"}
	if status, body := get("localhost", "/gone/x"); status != http.StatusBadGateway || !strings.Contains(body, "gone") {
		t.Errorf("dead upstream = %d %q, want 502", status, body)
	}

	// The counters are those of the facade that received the request, not those of the port.
	if requests, failures := proxyOf(front).Stats("localhost"); requests != 7 || failures != 2 {
		t.Errorf("localhost = %d requests / %d errors, want 7 / 2", requests, failures)
	}
	if requests, failures := proxyOf(front).Stats("beta.localhost"); requests != 2 || failures != 1 {
		t.Errorf("beta.localhost = %d requests / %d errors, want 2 / 1", requests, failures)
	}
}

// A proxy target receives the whole path, prefixed with its own if it has one, and the host the
// route asks for: the browser's, or its own with changeOrigin.
func TestProxyTargetGetsHostAndPath(t *testing.T) {
	t.Parallel()
	seen := upstream(t, "seen")
	cases := []struct {
		name   string
		target config.Target
		want   string
	}{
		{"as is", config.Target{URL: seen.URL}, "seen /api/x localhost"},
		{"changeOrigin", config.Target{URL: seen.URL, ChangeOrigin: true}, "seen /api/x " + addr(seen.URL)},
		{"target with a path", config.Target{URL: seen.URL + "/gateway/"}, "seen /gateway/api/x localhost"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &testNet{facades: facades(config.Ingress{Name: "localhost", Host: "localhost",
				Routes: []config.Route{{Path: "/api", Proxy: c.target}}})}
			front := httptest.NewServer(NewProxy(0, fake, func(string, string) {}))
			t.Cleanup(front.Close)

			req, _ := http.NewRequest(http.MethodGet, front.URL+"/api/x", nil)
			req.Host = "localhost"
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			if got := strings.TrimSpace(string(body)); got != c.want {
				t.Errorf("the target saw %q, want %q", got, c.want)
			}
		})
	}
}

func TestUpgradeIsRelayed(t *testing.T) {
	t.Parallel()
	hijacked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\nproxied")
	}))
	t.Cleanup(hijacked.Close)

	back := stub{name: "app", addr: addr(hijacked.URL)}
	fake := &testNet{
		live: map[string]core.Backend{"app": back},
		facades: facades(config.Ingress{Host: "localhost",
			Routes: []config.Route{{Path: "/app", App: "app"}}}),
	}
	traced := make(chan string, 8)
	front := httptest.NewServer(NewProxy(0, fake, func(_, line string) { traced <- line }))
	t.Cleanup(front.Close)

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET /app/ HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var got string
	for buf := make([]byte, 512); !strings.Contains(got, "proxied"); {
		n, err := conn.Read(buf)
		got += string(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(got, "101 Switching Protocols") || !strings.Contains(got, "proxied") {
		t.Errorf("response = %q, want a proxied 101 with its body", got)
	}
	// An upgrade does not go through WriteHeader: the log must still say 101, not 200. The line is
	// only written at the end of the proxying, hence after the close.
	conn.Close()
	select {
	case line := <-traced:
		if !strings.Contains(line, "WS") || !strings.Contains(line, "101") {
			t.Errorf("log = %q, want a WS line with 101", line)
		}
	case <-time.After(3 * time.Second):
		t.Error("no log line for the upgrade")
	}
}

// An app that restarts listens elsewhere: the proxy follows it without keeping a warm connection to
// its previous address.
func TestUpstreamFollowsARestart(t *testing.T) {
	t.Parallel()
	before, after := app(t, "app", "before"), app(t, "app", "after")
	live := map[string]core.Backend{"app": before}
	fake := &testNet{
		live:    live,
		facades: facades(config.Ingress{Routes: []config.Route{{Path: "/app", App: "app"}}}),
	}
	front := httptest.NewServer(NewProxy(0, fake, func(string, string) {}))
	t.Cleanup(front.Close)

	get := func() string {
		t.Helper()
		res, err := http.Get(front.URL + "/app/x")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return strings.TrimSpace(string(body))
	}

	if got := get(); got != "before /app/x" {
		t.Fatalf("response = %q, want the first run", got)
	}
	live["app"] = stub{name: "app", addr: after.addr}
	if got := get(); got != "after /app/x" {
		t.Errorf("response = %q, want the second run: the proxy stayed on the dead address", got)
	}
}

// After a network change, the remote target no longer answers on the connections opened on the old
// one — they are dead without knowing it —, only on a new one. The first request afterwards must
// not reuse one.
func TestANetworkChangeLeavesItsConnectionsBehind(t *testing.T) {
	t.Parallel()
	var network atomic.Int32
	type openedOn struct{}
	staging := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(openedOn{}) != network.Load() {
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, "staging")
	}))
	staging.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, openedOn{}, network.Load())
	}
	staging.Start()
	t.Cleanup(staging.Close)
	fake := &testNet{facades: facades(config.Ingress{Routes: []config.Route{{Path: "/", Proxy: config.Target{URL: staging.URL}}}})}
	fake.seen.Store(&config.Observed{Exits: []config.Exit{{Via: "192.0.2.1", Dev: "eth0"}}})
	front := httptest.NewServer(NewProxy(0, fake, func(string, string) {}))
	t.Cleanup(front.Close)
	client := http.Client{Timeout: 2 * time.Second}
	get := func() error {
		res, err := client.Get(front.URL + "/")
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if body, _ := io.ReadAll(res.Body); string(body) != "staging" {
			return fmt.Errorf("%d %q", res.StatusCode, body)
		}
		return nil
	}

	if err := get(); err != nil {
		t.Fatalf("at the office: %v", err)
	}
	network.Add(1)
	fake.seen.Store(&config.Observed{Exits: []config.Exit{{Via: "198.51.100.1", Dev: "eth0"}}})
	if err := get(); err != nil {
		t.Errorf("the first request on the new network reused a connection from the old one: %v", err)
	}
}

// testNet is the session as the proxy sees it: the facades, the backends and the network come from
// the test. Host resolution is the real one.
type testNet struct {
	facades config.Config
	live    map[string]core.Backend
	seen    atomic.Pointer[config.Observed]
}

func facades(list ...config.Ingress) config.Config {
	cfg := config.Config{}
	for _, in := range list {
		cfg.Ingresses = append(cfg.Ingresses, config.NewIngress(in))
	}
	return cfg
}

func (t *testNet) Facade(host string) (config.Ingress, bool) { return t.facades.Ingress(host) }

func (t *testNet) Backend(app string) core.Backend { return t.live[app] }

func (t *testNet) Observed() *config.Observed { return t.seen.Load() }

func (t *testNet) Listen(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf(":%d", port))
}

// app sets up a test app and returns how to reach it, as the core would. The backend carries the
// app's name, as for real: it is through it that the proxy asks for a connection again.
func app(t *testing.T, name, says string) stub {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s", says, r.URL.RequestURI())
	}))
	t.Cleanup(srv.Close)
	return stub{name: name, addr: addr(srv.URL)}
}

// upstream sets up a remote target that echoes what it received: its path and its host.
func upstream(t *testing.T, name string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s %s", name, r.URL.RequestURI(), r.Host)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// stub is a test backend: the proxy never learns the address, it asks for a connection.
type stub struct {
	name string
	addr string
}

func (s stub) Name() string { return s.name }
func (s stub) Addr() string { return s.addr }

func (s stub) DialContext(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", s.addr)
}

func addr(url string) string { return strings.TrimPrefix(url, "http://") }

// proxyOf returns the proxy this test server serves, to ask it for its counters.
func proxyOf(front *httptest.Server) *Proxy { return front.Config.Handler.(*Proxy) }
