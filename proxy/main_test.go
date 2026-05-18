package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testRemote = "127.0.0.1:54321"

// resetProjects replaces the global projectStore with a single entry covering
// 127.0.0.0/8 (so requests forged via httptest land in `name`'s project with
// the given allowlist). Returns a cleanup that restores an empty store.
func resetProjects(name string, patterns []string) func() {
	prev := projectStore
	projectStore = NewProjectStore()
	if err := projectStore.Upsert(name, "127.0.0.0/8", patterns, nil, nil); err != nil {
		panic(err)
	}
	return func() { projectStore = prev }
}

// reqAs builds a request with RemoteAddr set so projectFromRemote can match.
func reqAs(method, url, remote string) *http.Request {
	r := httptest.NewRequest(method, url, nil)
	r.RemoteAddr = remote
	return r
}

var wrapDefaultPatterns = []string{
	"api.anthropic.com",
	"platform.claude.com",
	"api.github.com",
	"github.com",
	"*.githubusercontent.com",
	"uploads.github.com",
}

func TestApproveEndpoint(t *testing.T) {
	host := "approve-test.example.com"
	project := "proj-a"
	req := httptest.NewRequest("POST", "/approve?project="+project+"&host="+host, nil)
	req.Header.Set("X-CSRF-Token", csrfToken)
	w := httptest.NewRecorder()
	handleApprove(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("got status %d, want %d", w.Code, http.StatusOK)
	}
	if !tempAllowStore.IsAllowed(tempAllowKey(project, host)) {
		t.Errorf("expected %s|%s temp-allowed", project, host)
	}
	if tempAllowStore.IsAllowed(tempAllowKey("other-proj", host)) {
		t.Errorf("approval bled into other project")
	}
}

func TestApproveEndpointCSRF(t *testing.T) {
	req := httptest.NewRequest("POST", "/approve?project=p&host=example.com", nil)
	w := httptest.NewRecorder()
	handleApprove(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("no token: got status %d, want %d", w.Code, http.StatusForbidden)
	}

	req = httptest.NewRequest("POST", "/approve?project=p&host=example.com", nil)
	req.Header.Set("X-CSRF-Token", "wrong-token")
	w = httptest.NewRecorder()
	handleApprove(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("wrong token: got status %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestApproveEndpointMissingProject(t *testing.T) {
	req := httptest.NewRequest("POST", "/approve?host=example.com", nil)
	req.Header.Set("X-CSRF-Token", csrfToken)
	w := httptest.NewRecorder()
	handleApprove(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestTemporaryAllowBypassesPending(t *testing.T) {
	defer resetProjects("p", nil)()

	origDuration := holdDuration
	holdDuration = 2 * time.Second
	defer func() { holdDuration = origDuration }()

	origResolve := resolveAndValidate
	resolveAndValidate = func(host string) (string, error) { return host, nil }
	defer func() { resolveAndValidate = origResolve }()

	tempAllowStore.Allow(tempAllowKey("p", "temphost.example.com"))

	start := time.Now()
	req := reqAs("GET", "http://temphost.example.com/", testRemote)
	w := httptest.NewRecorder()
	handleHTTP(w, req)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Errorf("request took %v, expected it to bypass pending", elapsed)
	}
	if w.Code == http.StatusForbidden {
		t.Error("got 403 Forbidden, expected request to be allowed (not blocked)")
	}
}

func TestApproveReleasesPending(t *testing.T) {
	defer resetProjects("p", nil)()

	origDuration := holdDuration
	holdDuration = 5 * time.Second
	defer func() { holdDuration = origDuration }()

	origResolve := resolveAndValidate
	resolveAndValidate = func(host string) (string, error) { return host, nil }
	defer func() { resolveAndValidate = origResolve }()

	ch, unsub, _ := eventStore.Subscribe()
	defer unsub()

	done := make(chan int, 1)
	go func() {
		req := reqAs("GET", "http://release-test.example.com/", testRemote)
		w := httptest.NewRecorder()
		handleHTTP(w, req)
		done <- w.Code
	}()

	select {
	case ev := <-ch:
		if ev.Status != "pending" {
			t.Fatalf("expected pending event, got %q", ev.Status)
		}
		if ev.Project != "p" {
			t.Fatalf("expected pending event for project p, got %q", ev.Project)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pending event")
	}

	pendingStore.ApproveHost("p", "release-test.example.com")

	select {
	case code := <-done:
		if code == http.StatusForbidden {
			t.Error("request was rejected, expected it to be approved")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for request to complete after approval")
	}
}

func TestApprovalIsProjectScoped(t *testing.T) {
	// Two projects on the same IP would never happen in production (each
	// project has a distinct CIDR), but the semantic test is: approving
	// host X for project A must not release a pending request for
	// host X from project B.
	defer resetProjects("a", nil)()

	origDuration := holdDuration
	holdDuration = 200 * time.Millisecond
	defer func() { holdDuration = origDuration }()

	origResolve := resolveAndValidate
	resolveAndValidate = func(host string) (string, error) { return host, nil }
	defer func() { resolveAndValidate = origResolve }()

	done := make(chan int, 1)
	go func() {
		req := reqAs("GET", "http://x.example.com/", testRemote)
		w := httptest.NewRecorder()
		handleHTTP(w, req)
		done <- w.Code
	}()

	time.Sleep(50 * time.Millisecond)
	// Approving the same host under a different project must NOT release.
	pendingStore.ApproveHost("b", "x.example.com")

	select {
	case code := <-done:
		if code != http.StatusForbidden {
			t.Errorf("expected forbidden (timed-out pending), got %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending didn't resolve")
	}
}

func TestTemporaryAllowExpiry(t *testing.T) {
	origDuration := tempAllowDuration
	tempAllowDuration = 50 * time.Millisecond
	defer func() { tempAllowDuration = origDuration }()

	tempAllowStore.Allow("expiry-test.example.com")
	if !tempAllowStore.IsAllowed("expiry-test.example.com") {
		t.Fatal("expected host to be allowed immediately after Allow()")
	}

	time.Sleep(100 * time.Millisecond)

	if tempAllowStore.IsAllowed("expiry-test.example.com") {
		t.Error("expected host to be expired after waiting past duration")
	}
}

func TestMatchesPatterns(t *testing.T) {
	tests := []struct {
		host    string
		allowed bool
	}{
		{"api.github.com", true},
		{"github.com", true},
		{"uploads.github.com", true},

		{"api.github.com:443", true},
		{"github.com:443", true},

		{"raw.githubusercontent.com", true},
		{"avatars.githubusercontent.com", true},

		{"api.anthropic.com", true},
		{"api.anthropic.com:443", true},

		{"API.GITHUB.COM", true},
		{"API.ANTHROPIC.COM", true},

		{"console.anthropic.com", false},
		{"anthropic.com", false},

		{"example.com", false},
		{"evil.com", false},
		{"notgithub.com", false},
		{"github.com.evil.com", false},
		{"fakegithub.com", false},

		{"sub.github.com", false},
		{"sub.api.github.com", false},

		{"notanthropicxcom", false},
		{"xanthropicxcom:443", false},

		{"api.github.com:22", false},
		{"github.com:8080", false},
		{"api.anthropic.com:9090", false},

		{"api.github.com:80", true},

		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			got := matchesPatterns(wrapDefaultPatterns, tt.host)
			if got != tt.allowed {
				t.Errorf("matchesPatterns(%q) = %v, want %v", tt.host, got, tt.allowed)
			}
		})
	}
}

func TestMatchesPatterns_EmptyAllowlist(t *testing.T) {
	if matchesPatterns(nil, "api.github.com") {
		t.Error("empty allowlist must reject everything")
	}
}

func TestProjectStore_Lookup(t *testing.T) {
	ps := NewProjectStore()
	if err := ps.Upsert("a", "10.1.0.0/16", []string{"api.a.example"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := ps.Upsert("b", "10.2.0.0/16", []string{"api.b.example"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := ps.LookupIP(net.ParseIP("10.1.4.5")); got == nil || got.Name != "a" {
		t.Errorf("10.1.4.5 → %v, want project a", got)
	}
	if got := ps.LookupIP(net.ParseIP("10.2.4.5")); got == nil || got.Name != "b" {
		t.Errorf("10.2.4.5 → %v, want project b", got)
	}
	if got := ps.LookupIP(net.ParseIP("10.9.9.9")); got != nil {
		t.Errorf("10.9.9.9 → %v, want nil", got)
	}
}

func TestProjectStore_UpsertReplaces(t *testing.T) {
	ps := NewProjectStore()
	_ = ps.Upsert("a", "10.0.0.0/8", []string{"old"}, nil, nil)
	_ = ps.Upsert("a-new", "10.0.0.0/8", []string{"new"}, nil, nil)
	if len(ps.entries) != 1 {
		t.Fatalf("expected 1 entry after upsert by same CIDR, got %d", len(ps.entries))
	}
	got := ps.LookupIP(net.ParseIP("10.1.1.1"))
	if got == nil || got.Name != "a-new" || len(got.Patterns) != 1 || got.Patterns[0] != "new" {
		t.Errorf("upsert did not replace: %+v", got)
	}
}

func TestProjectStore_UpsertCarriesPorts(t *testing.T) {
	ps := NewProjectStore()
	ports := []ProjectPort{{Name: "web", HostPort: 4100, ContainerPort: 4000, Scheme: "http"}}
	if err := ps.Upsert("a", "10.0.0.0/8", []string{"x"}, nil, ports); err != nil {
		t.Fatal(err)
	}
	got := ps.LookupIP(net.ParseIP("10.1.1.1"))
	if got == nil || len(got.Ports) != 1 || got.Ports[0].HostPort != 4100 || got.Ports[0].Name != "web" {
		t.Errorf("ports not carried through: %+v", got)
	}
}

func TestForwarderManager_LifecycleAndProxy(t *testing.T) {
	// Pick free host ports for the upstream and a forwarder-listen port.
	upstreamLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamLn.Close()
	go func() {
		conn, err := upstreamLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Echo a single line back.
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		conn.Write(buf[:n])
	}()
	upPort := upstreamLn.Addr().(*net.TCPAddr).Port

	// Manager listening within a tiny range. Use a port we know is free
	// by binding a zero-port socket and immediately freeing it.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostPort := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	prevAllow := forwarderAllowCIDRs
	defer func() { forwarderAllowCIDRs = prevAllow }()
	_, loopback, _ := net.ParseCIDR("127.0.0.0/8")
	forwarderAllowCIDRs = []*net.IPNet{loopback}

	mgr := NewForwarderManager(hostPort, hostPort)
	mgr.Apply("p", map[int]Upstream{
		hostPort: {ContainerIP: "127.0.0.1", ContainerPort: upPort},
	})
	defer mgr.Apply("p", nil)

	// Forwarder should be reachable; payload should round-trip.
	deadline := time.Now().Add(2 * time.Second)
	var conn net.Conn
	for time.Now().Before(deadline) {
		conn, err = net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)))
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer conn.Close()
	conn.Write([]byte("hello\n"))
	buf := make([]byte, 16)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "hello\n" {
		t.Fatalf("forwarder echo failed: n=%d err=%v got=%q", n, err, buf[:n])
	}

	// Removing the project should close the listener.
	mgr.Apply("p", nil)
	time.Sleep(50 * time.Millisecond)
	if _, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort))); err == nil {
		t.Errorf("expected listener closed after Apply with empty set")
	}
}

func TestForwarderManager_OutOfRangeRejected(t *testing.T) {
	mgr := NewForwarderManager(5000, 5099)
	mgr.Apply("p", map[int]Upstream{
		6000: {ContainerIP: "127.0.0.1", ContainerPort: 1234},
	})
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if _, ok := mgr.listeners[6000]; ok {
		t.Errorf("out-of-range port should not have a listener")
	}
}

func TestHandlePorts(t *testing.T) {
	defer resetProjects("solo", nil)()
	// Add ports onto the existing project.
	if err := projectStore.Upsert("solo", "127.0.0.0/8", nil, nil,
		[]ProjectPort{
			{Name: "web", HostPort: 4101, ContainerPort: 4000, Scheme: "http"},
			{Name: "ws", HostPort: 4102, ContainerPort: 4001, Scheme: "https"},
		}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handlePorts(w, httptest.NewRequest(http.MethodGet, "/ports", nil))
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`"name":"web"`, `"host_port":4101`, `"scheme":"https"`, `"project":"solo"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body %s", want, body)
		}
	}
}

func TestHandleHTTP_Blocked(t *testing.T) {
	defer resetProjects("p", nil)()

	origResolve := resolveAndValidate
	resolveAndValidate = func(host string) (string, error) { return host, nil }
	defer func() { resolveAndValidate = origResolve }()

	origDuration := holdDuration
	holdDuration = 50 * time.Millisecond
	defer func() { holdDuration = origDuration }()

	req := reqAs("GET", "http://example.com/", testRemote)
	w := httptest.NewRecorder()
	handleHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("got status %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestHandleHTTP_Allowed(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "ok")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "hello from backend")
	}))
	defer backend.Close()

	backendAddr := backend.Listener.Addr().String()
	backendHost, backendPort, _ := net.SplitHostPort(backendAddr)

	defer resetProjects("p", []string{backendHost})()

	allowedPorts[backendPort] = true
	defer delete(allowedPorts, backendPort)

	origResolve := resolveAndValidate
	resolveAndValidate = func(host string) (string, error) { return host, nil }
	defer func() { resolveAndValidate = origResolve }()

	req := reqAs("GET", fmt.Sprintf("http://%s:%s/test", backendHost, backendPort), testRemote)

	w := httptest.NewRecorder()
	handleHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("got status %d, want %d", w.Code, http.StatusOK)
	}
	if got := w.Body.String(); got != "hello from backend" {
		t.Errorf("got body %q, want %q", got, "hello from backend")
	}
	if got := w.Header().Get("X-Test"); got != "ok" {
		t.Errorf("got X-Test %q, want %q", got, "ok")
	}
}

func TestConnectBlocked(t *testing.T) {
	defer resetProjects("p", nil)()

	origResolve := resolveAndValidate
	resolveAndValidate = func(host string) (string, error) { return host, nil }
	defer func() { resolveAndValidate = origResolve }()

	origDuration := holdDuration
	holdDuration = 50 * time.Millisecond
	defer func() { holdDuration = origDuration }()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			handleConnect(w, r)
		} else {
			handleHTTP(w, r)
		}
	}))
	defer proxy.Close()

	conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("got status %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestConnectAllowed(t *testing.T) {
	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoListener.Close()

	go func() {
		for {
			c, err := echoListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()

	echoAddr := echoListener.Addr().String()
	echoHost, echoPort, _ := net.SplitHostPort(echoAddr)

	defer resetProjects("p", []string{echoHost})()

	allowedPorts[echoPort] = true
	defer delete(allowedPorts, echoPort)

	origResolve := resolveAndValidate
	resolveAndValidate = func(host string) (string, error) { return host, nil }
	defer func() { resolveAndValidate = origResolve }()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			handleConnect(w, r)
		} else {
			handleHTTP(w, r)
		}
	}))
	defer proxy.Close()

	conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want %d", resp.StatusCode, http.StatusOK)
	}

	msg := "hello through tunnel"
	fmt.Fprint(conn, msg)
	buf := make([]byte, len(msg))
	_, err = io.ReadFull(br, buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != msg {
		t.Errorf("got %q, want %q", string(buf), msg)
	}
}

func TestPendingToRejectedFlow(t *testing.T) {
	defer resetProjects("p", nil)()

	origResolve := resolveAndValidate
	resolveAndValidate = func(host string) (string, error) { return host, nil }
	defer func() { resolveAndValidate = origResolve }()

	origDuration := holdDuration
	holdDuration = 50 * time.Millisecond
	defer func() { holdDuration = origDuration }()

	ch, unsub, _ := eventStore.Subscribe()
	defer unsub()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := reqAs("GET", "http://blocked.example.com/", testRemote)
		w := httptest.NewRecorder()
		handleHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("got status %d, want %d", w.Code, http.StatusForbidden)
		}
	}()

	select {
	case ev := <-ch:
		if ev.Status != "pending" {
			t.Errorf("first event status = %q, want %q", ev.Status, "pending")
		}
		if ev.Project != "p" {
			t.Errorf("first event project = %q, want %q", ev.Project, "p")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pending event")
	}

	select {
	case ev := <-ch:
		if ev.Status != "rejected" {
			t.Errorf("second event status = %q, want %q", ev.Status, "rejected")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for rejected event")
	}

	<-done
}

func TestDNSBlocked_HTTP(t *testing.T) {
	defer resetProjects("p", nil)()

	origResolve := resolveAndValidate
	resolveAndValidate = func(host string) (string, error) {
		return "", fmt.Errorf("resolved to private IP 127.0.0.1")
	}
	defer func() { resolveAndValidate = origResolve }()

	ch, unsub, _ := eventStore.Subscribe()
	defer unsub()

	req := reqAs("GET", "http://private-host.example.com/", testRemote)
	w := httptest.NewRecorder()
	handleHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("got status %d, want %d", w.Code, http.StatusForbidden)
	}

	select {
	case ev := <-ch:
		if ev.Status != "non_public_ip" {
			t.Errorf("event status = %q, want %q", ev.Status, "non_public_ip")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for non_public_ip event")
	}
}

func TestDNSBlocked_CONNECT(t *testing.T) {
	defer resetProjects("p", nil)()

	origResolve := resolveAndValidate
	resolveAndValidate = func(host string) (string, error) {
		return "", fmt.Errorf("resolved to private IP 127.0.0.1")
	}
	defer func() { resolveAndValidate = origResolve }()

	ch, unsub, _ := eventStore.Subscribe()
	defer unsub()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			handleConnect(w, r)
		} else {
			handleHTTP(w, r)
		}
	}))
	defer proxy.Close()

	conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT private-host.example.com:443 HTTP/1.1\r\nHost: private-host.example.com:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("got status %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	select {
	case ev := <-ch:
		if ev.Status != "non_public_ip" {
			t.Errorf("event status = %q, want %q", ev.Status, "non_public_ip")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for non_public_ip event")
	}
}

func TestHandleProjectUpsert(t *testing.T) {
	prev := projectStore
	projectStore = NewProjectStore()
	defer func() { projectStore = prev }()

	body := strings.NewReader(`{"name":"myproj","cidr":"172.19.0.0/16","allowlist":["api.github.com","*.anthropic.com"],"denylist":["evil.example","*.tracker.example"]}`)
	req := httptest.NewRequest("POST", "/project", body)
	w := httptest.NewRecorder()
	handleProjectUpsert(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d (body: %s)", w.Code, http.StatusOK, w.Body.String())
	}
	p := projectStore.LookupIP(net.ParseIP("172.19.4.5"))
	if p == nil || p.Name != "myproj" {
		t.Fatalf("lookup after upsert: %+v", p)
	}
	if len(p.Patterns) != 2 || p.Patterns[0] != "api.github.com" {
		t.Errorf("patterns: %v", p.Patterns)
	}
	if len(p.Denylist) != 2 || p.Denylist[0] != "evil.example" {
		t.Errorf("denylist: %v", p.Denylist)
	}
}

func TestDenylistBypassesPending(t *testing.T) {
	prev := projectStore
	projectStore = NewProjectStore()
	if err := projectStore.Upsert("p", "127.0.0.0/8", nil, []string{"blocked.example.com", "*.ad.example"}, nil); err != nil {
		t.Fatal(err)
	}
	defer func() { projectStore = prev }()

	origDuration := holdDuration
	holdDuration = 5 * time.Second
	defer func() { holdDuration = origDuration }()

	origResolve := resolveAndValidate
	resolveAndValidate = func(host string) (string, error) { return host, nil }
	defer func() { resolveAndValidate = origResolve }()

	for _, host := range []string{"blocked.example.com", "tracker.ad.example"} {
		start := time.Now()
		req := reqAs("GET", "http://"+host+"/", testRemote)
		w := httptest.NewRecorder()
		handleHTTP(w, req)
		elapsed := time.Since(start)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: code=%d want 403", host, w.Code)
		}
		if elapsed > 500*time.Millisecond {
			t.Errorf("%s: took %v, expected immediate rejection (no pending hold)", host, elapsed)
		}
	}
}

func TestDenylistOverriddenByTempAllow(t *testing.T) {
	prev := projectStore
	projectStore = NewProjectStore()
	if err := projectStore.Upsert("p", "127.0.0.0/8", nil, []string{"override.example.com"}, nil); err != nil {
		t.Fatal(err)
	}
	defer func() { projectStore = prev }()

	origResolve := resolveAndValidate
	resolveAndValidate = func(host string) (string, error) { return host, nil }
	defer func() { resolveAndValidate = origResolve }()

	tempAllowStore.Allow(tempAllowKey("p", "override.example.com"))
	defer tempAllowStore.Revoke(tempAllowKey("p", "override.example.com"))

	req := reqAs("GET", "http://override.example.com/", testRemote)
	w := httptest.NewRecorder()
	handleHTTP(w, req)
	if w.Code == http.StatusForbidden {
		t.Error("temp-allow should override denylist, got 403")
	}
}

func TestHandleProjectUpsert_BadJSON(t *testing.T) {
	req := httptest.NewRequest("POST", "/project", strings.NewReader("nope"))
	w := httptest.NewRecorder()
	handleProjectUpsert(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleProjectUpsert_Required(t *testing.T) {
	req := httptest.NewRequest("POST", "/project", strings.NewReader(`{"name":"x"}`))
	w := httptest.NewRecorder()
	handleProjectUpsert(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// stubAddr lets us inject a LocalAddr into a request context.
type stubAddr struct{ s string }

func (a stubAddr) Network() string { return "tcp" }
func (a stubAddr) String() string  { return a.s }

func reqWithLocalAddr(method, path, localAddr string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	ctx := context.WithValue(req.Context(), http.LocalAddrContextKey, stubAddr{s: localAddr})
	return req.WithContext(ctx)
}

func TestDashboardGate_AllowedLocal(t *testing.T) {
	orig := dashboardAllowCIDRs
	_, n, _ := net.ParseCIDR("172.21.0.0/16")
	dashboardAllowCIDRs = []*net.IPNet{n}
	defer func() { dashboardAllowCIDRs = orig }()

	req := reqWithLocalAddr("GET", "/", "172.21.0.5:8081")
	if !dashboardAllowed(req) {
		t.Error("expected allowed for local addr in allowed CIDR")
	}
}

func TestDashboardGate_RejectedLocal(t *testing.T) {
	orig := dashboardAllowCIDRs
	_, n, _ := net.ParseCIDR("172.21.0.0/16")
	dashboardAllowCIDRs = []*net.IPNet{n}
	defer func() { dashboardAllowCIDRs = orig }()

	req := reqWithLocalAddr("GET", "/", "172.19.0.5:8081")
	if dashboardAllowed(req) {
		t.Error("expected reject for local addr outside allowed CIDR")
	}
}

func TestDashboardGate_NoLocalAddr(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	if dashboardAllowed(req) {
		t.Error("expected reject when no local addr in context")
	}
}
