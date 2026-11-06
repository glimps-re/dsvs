package gateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dsvs/internal/build"
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
				{Path: "/old", Redirect: "/alpha"},
				{Path: "/", Redirect: "/alpha/"},
			}},
			config.Ingress{Host: "beta.localhost", Routes: []config.Route{
				{Path: "/beta", App: "beta"},
			}},
		),
	}
	front := httptest.NewServer(NewProxy(fake, func(string, string) {}))
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
		// localhost's catch-all redirects keeping the rest of the path and the query; the same
		// address on beta.localhost has no route.
		{"localhost", "/beta/main.js", http.StatusFound, "/alpha/beta/main.js"},
		{"localhost", "/products/12?color=red", http.StatusFound, "/alpha/products/12?color=red"},
		{"localhost", "/", http.StatusFound, "/alpha/"},
		// The rest is what follows the route's prefix: without a rest, the target as written.
		{"localhost", "/old/x?y=1", http.StatusFound, "/alpha/x?y=1"},
		{"localhost", "/old", http.StatusFound, "/alpha"},
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
	if requests, failures := proxyOf(front).Stats("localhost"); requests != 10 || failures != 2 {
		t.Errorf("localhost = %d requests / %d errors, want 10 / 2", requests, failures)
	}
	if requests, failures := proxyOf(front).Stats("beta.localhost"); requests != 2 || failures != 1 {
		t.Errorf("beta.localhost = %d requests / %d errors, want 2 / 1", requests, failures)
	}
}

// An address that cannot answer says why and what to do, always as a 502: the app is stopped, not
// ready yet, fallen with its code, or the remote target is unreachable — named by the route's host,
// never by the address DNS or the apps' private network put behind it.
func TestA502SaysWhyAndWhatToDo(t *testing.T) {
	t.Parallel()
	fell := core.Outcome{Code: 3}
	closing, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closing.Close() })
	go func() {
		for {
			conn, err := closing.Accept()
			if err != nil {
				return
			}
			// Read the request before closing: a close on unread bytes goes out as a reset.
			_, _ = http.ReadRequest(bufio.NewReader(conn))
			conn.Close()
		}
	}()
	fake := &testNet{
		live: map[string]core.Backend{"silent": stub{name: "silent", addr: "127.0.0.1:1"}},
		snaps: map[string]core.Snapshot{
			"slow":    {Status: core.Starting},
			"fallen":  {Status: core.Idle, Last: &fell},
			"leaving": {Status: core.Stopping},
			"silent":  {Status: core.Ready},
			"astray": {Status: core.Starting, Astray: &core.Astray{On: []string{"127.0.0.1:5173", "[::1]:5173"},
				Want: core.Endpoint{IP: "10.77.0.3", Port: 4123}}},
		},
		facades: facades(config.Ingress{Routes: []config.Route{
			{Path: "/stopped", App: "stopped"},
			{Path: "/slow", App: "slow"},
			{Path: "/fallen", App: "fallen"},
			{Path: "/leaving", App: "leaving"},
			{Path: "/silent", App: "silent"},
			{Path: "/astray", App: "astray"},
			{Path: "/staging", Proxy: config.Target{URL: "http://localhost:1"}},
			{Path: "/demo", Proxy: config.Target{URL: "http://" + closing.Addr().String()}},
			// A documentation address: no network reaches it, whether it says so or stays silent.
			{Path: "/elsewhere", Proxy: config.Target{URL: "http://192.0.2.1:9"}},
		}}),
	}
	fake.seen.Store(&config.Observed{Exits: []config.Exit{{Via: "198.51.100.1", Dev: "eth0"}}})
	front := httptest.NewServer(NewProxy(fake, func(string, string) {}))
	t.Cleanup(front.Close)
	get := func(path string) (int, string) {
		t.Helper()
		res, err := http.Get(front.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, strings.TrimSpace(string(body))
	}

	// Out of reach, and only then, the page points to the network the session sees.
	status, said := get("/elsewhere/x")
	if status != http.StatusBadGateway || !strings.HasPrefix(said, "192.0.2.1:9 is unreachable from this network (eth0 via 198.51.100.1): ") ||
		!strings.HasSuffix(said, build.Name()+" state says the network the session sees") {
		t.Errorf("/elsewhere/x = %d %q, want 502, out of reach from this network", status, said)
	}
	for path, want := range map[string]string{
		"/stopped/":  "stopped is stopped: " + build.Name() + " start stopped",
		"/slow/":     "slow is starting and does not answer yet: " + build.Name() + " logs slow",
		"/fallen/":   "fallen fell, exited (3): " + build.Name() + " logs fallen, then " + build.Name() + " start fallen",
		"/leaving/":  "leaving is stopping: " + build.Name() + " start leaving brings it back",
		"/silent/":   "silent did not answer: connection refused; " + build.Name() + " logs silent",
		"/astray/":   "astray is starting, but listens on 127.0.0.1:5173, [::1]:5173, not on 10.77.0.3:4123: its command must make it listen on {ip} and {port}",
		"/staging/x": "localhost:1 refuses connections: is the server down?",
		"/demo/x":    closing.Addr().String() + " cannot be reached: the connection closed before any answer",
	} {
		if status, got := get(path); status != http.StatusBadGateway || got != want {
			t.Errorf("%s = %d %q, want 502 %q", path, status, got, want)
		}
	}
}

// A browser that opens the address of a starting app gets a page that says so and reloads when the
// address serves something else; any other client gets today's 502, right away.
func TestABrowserWaitsForAStartingApp(t *testing.T) {
	t.Parallel()
	fake := &testNet{
		snaps:   map[string]core.Snapshot{"front": {Status: core.Starting}},
		facades: facades(config.Ingress{Host: "shop.localhost", Routes: []config.Route{{Path: "/", App: "front"}}}),
	}
	front := httptest.NewServer(NewProxy(fake, func(string, string) {}))
	t.Cleanup(front.Close)
	get := func(path string, headers ...string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, front.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "shop.localhost"
		for i := 0; i < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res, string(body)
	}
	starting := "front is starting and does not answer yet: " + build.Name() + " logs front"

	// A browser says so through Sec-Fetch-Mode, and failing that — an origin it does not deem
	// secure — through a navigation's Accept. A fetch that asks for HTML stays a fetch.
	for _, headers := range [][]string{
		{"Sec-Fetch-Mode", "navigate", "Accept", "text/html,application/xhtml+xml,*/*;q=0.8"},
		{"Accept", "text/html,application/xhtml+xml,*/*;q=0.8"},
	} {
		res, page := get("/products/12", headers...)
		if res.StatusCode != http.StatusServiceUnavailable || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") ||
			!strings.Contains(page, "<title>front is starting…</title>") || !strings.Contains(page, starting) ||
			!strings.Contains(page, `<script src="/__dsvs/reload.js" data-apps="front starting">`) {
			t.Errorf("navigation %v = %d %q, want the waiting page as a 503", headers, res.StatusCode, page)
		}
	}
	for _, headers := range [][]string{
		nil,
		{"Accept", "*/*"},
		{"Sec-Fetch-Mode", "cors", "Accept", "text/html"},
		{"Sec-Fetch-Mode", "no-cors", "Accept", "image/avif,image/*"},
	} {
		if res, body := get("/products/12", headers...); res.StatusCode != http.StatusBadGateway || strings.TrimSpace(body) != starting {
			t.Errorf("request %v = %d %q, want the 502 %q", headers, res.StatusCode, body, starting)
		}
	}
	if res, script := get("/__dsvs/reload.js"); res.StatusCode != http.StatusOK || !strings.Contains(script, `new EventSource("/__dsvs/events")`) {
		t.Errorf("/__dsvs/reload.js = %d %q", res.StatusCode, script)
	}

	// The stream tells what the address serves on opening, then when it changes: the app fallen,
	// the reloaded page gets the 502 that says why.
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/__dsvs/events", nil)
	req.Host = "shop.localhost"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	next := events(t, res.Body, "apps")
	if apps := next(); apps != "front starting" {
		t.Errorf("the stream opens on %q, want \"front starting\"", apps)
	}
	fake.become("front", core.Snapshot{Status: core.Idle, Last: &core.Outcome{Code: 1}})
	if apps := next(); apps != "front idle" {
		t.Errorf("the stream says %q, want \"front idle\"", apps)
	}
	fell := "front fell, exited (1): " + build.Name() + " logs front, then " + build.Name() + " start front"
	if res, body := get("/", "Sec-Fetch-Mode", "navigate"); res.StatusCode != http.StatusBadGateway || strings.TrimSpace(body) != fell {
		t.Errorf("navigation after the fall = %d %q, want the 502 %q", res.StatusCode, body, fell)
	}
}

// An app whose declaration reloads its pages serves its documents with the script, which reloads
// them on the next applied configuration; its fetches, its scripts, and the other apps' pages pass
// through untouched.
func TestThePagesOfAnAppThatAsksForItReloadWithTheConfiguration(t *testing.T) {
	t.Parallel()
	served := func(name string) stub {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// What the browser accepts does not reach it: neither its copy, nor br.
			if r.Header.Get("If-None-Match") != "" || strings.Contains(r.Header.Get("Accept-Encoding"), "br") {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"v1"`)
			if strings.HasSuffix(r.URL.Path, ".js") {
				w.Header().Set("Content-Type", "text/javascript")
				fmt.Fprint(w, "console.log(1)")
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, "<!doctype html><title>%s</title>", name)
		}))
		t.Cleanup(srv.Close)
		return stub{name: name, addr: addr(srv.URL)}
	}
	fake := &testNet{
		live: map[string]core.Backend{"docs": served("docs"), "api": served("api")},
		facades: facades(config.Ingress{Host: "shop.localhost", Routes: []config.Route{
			{Path: "/", App: "docs"}, {Path: "/api", App: "api"},
		}}),
	}
	fake.facades.Apps = []config.App{{Name: "docs", Reload: config.Reload{Page: true}}, {Name: "api"}}
	fake.apply()
	front := httptest.NewServer(NewProxy(fake, func(string, string) {}))
	t.Cleanup(front.Close)
	get := func(path string, headers ...string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, front.URL+path, nil)
		req.Host = "shop.localhost"
		for i := 0; i < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		// The transport does not ask for gzip on its own: the request is a browser's.
		res, err := (&http.Transport{DisableCompression: true}).RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res, string(body)
	}
	navigate := []string{"Sec-Fetch-Mode", "navigate", "Accept", "text/html", "Accept-Encoding", "gzip, br", "If-None-Match", `"v1"`}

	res, page := get("/guide", navigate...)
	script := `<script src="/__dsvs/reload.js" data-config="1"></script>`
	if res.StatusCode != http.StatusOK || page != "<!doctype html><title>docs</title>"+script+"\n" {
		t.Errorf("document = %d %q, want the whole page and the script", res.StatusCode, page)
	}
	if res.ContentLength != -1 || res.Header.Get("ETag") != "" || res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v, want neither length nor ETag, and no-store", res.Header)
	}
	for _, c := range []struct {
		path    string
		headers []string
	}{
		{"/guide", []string{"Sec-Fetch-Mode", "cors", "Accept", "text/html"}},
		{"/main.js", navigate[:2]},
		{"/api/x", navigate[:4]},
	} {
		if _, body := get(c.path, c.headers...); strings.Contains(body, "reload.js") {
			t.Errorf("%s %v got the script: %q", c.path, c.headers, body)
		}
	}

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/__dsvs/events", nil)
	req.Host = "shop.localhost"
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	next := events(t, stream.Body, "config")
	if seen := next(); seen != "1" {
		t.Errorf("the stream opens on configuration %q, want 1", seen)
	}
	fake.apply()
	if seen := next(); seen != "2" {
		t.Errorf("the stream says configuration %q, want 2", seen)
	}
}

// events reads an SSE stream and returns how to wait for a fact's next value.
func events(t *testing.T, body io.Reader, fact string) func() string {
	lines := bufio.NewReader(body)
	return func() string {
		t.Helper()
		event := ""
		for {
			line, err := lines.ReadString('\n')
			if err != nil {
				t.Fatalf("the stream stopped: %v", err)
			}
			if name, found := strings.CutPrefix(line, "event: "); found {
				event = strings.TrimSpace(name)
			}
			if data, found := strings.CutPrefix(line, "data: "); found && event == fact {
				return strings.TrimSpace(data)
			}
		}
	}
}

// An open stream does not hold back the proxy's stop: it never ends on its own.
func TestAnOpenStreamDoesNotHoldTheStop(t *testing.T) {
	t.Parallel()
	fake := &testNet{facades: facades(config.Ingress{Routes: []config.Route{{Path: "/", App: "front"}}})}
	proxy := NewProxy(fake, func(string, string) {})
	if err := proxy.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := http.Get("http://" + fake.listener.Addr().String() + "/__dsvs/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if _, err := bufio.NewReader(res.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	stopping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	began := time.Now()
	if err := proxy.Stop(stopping); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > time.Second {
		t.Errorf("the stop waited %v for the open stream", took)
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
			front := httptest.NewServer(NewProxy(fake, func(string, string) {}))
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
	front := httptest.NewServer(NewProxy(fake, func(_, line string) { traced <- line }))
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
	front := httptest.NewServer(NewProxy(fake, func(string, string) {}))
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
	front := httptest.NewServer(NewProxy(fake, func(string, string) {}))
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
	snaps   map[string]core.Snapshot
	seen    atomic.Pointer[config.Observed]

	generation atomic.Uint64

	// mu guards what a test changes while the proxy serves: the apps' state, and who listens to it.
	mu       sync.Mutex
	watchers []chan core.Event
	listener net.Listener
}

// become changes an app's state and wakes up whoever listens to the session, as the supervisor
// would.
func (t *testNet) become(app string, snap core.Snapshot) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.snaps[app] = snap
	t.wake(core.Event{Source: app, Kind: core.Changed})
}

// apply counts an applied configuration and wakes up whoever listens, as a reload would.
func (t *testNet) apply() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.generation.Add(1)
	t.wake(core.Event{Kind: core.Reloaded})
}

func (t *testNet) wake(event core.Event) {
	for _, ears := range t.watchers {
		ears <- event
	}
}

func (t *testNet) Config() config.Config { return t.facades }

func (t *testNet) Generation() uint64 { return t.generation.Load() }

func facades(list ...config.Ingress) config.Config {
	cfg := config.Config{}
	for _, in := range list {
		cfg.Ingresses = append(cfg.Ingresses, config.NewIngress(in))
	}
	return cfg
}

func (t *testNet) Facade(host string) (config.Ingress, bool) { return t.facades.Ingress(host) }

func (t *testNet) Backend(app string) core.Backend { return t.live[app] }

func (t *testNet) Snapshot(app string) core.Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snaps[app]
}

func (t *testNet) Watch() (<-chan core.Event, func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ears := make(chan core.Event, 8)
	t.watchers = append(t.watchers, ears)
	return ears, func() {}
}

func (t *testNet) Observed() *config.Observed { return t.seen.Load() }

func (t *testNet) Listen() (net.Listener, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	t.mu.Lock()
	t.listener = listener
	t.mu.Unlock()
	return listener, err
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
