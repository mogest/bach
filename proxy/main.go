package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var holdDuration = 5 * time.Second
var tempAllowDuration = 10 * time.Minute
var maxSubscribers = 32

var proxyTransport = &http.Transport{
	DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	MaxIdleConns:        100,
	IdleConnTimeout:     90 * time.Second,
	DisableKeepAlives:   true,
	TLSHandshakeTimeout: 10 * time.Second,
}

// privateNetworks contains RFC 1918, loopback, and link-local ranges.
var privateNetworks = func() []*net.IPNet {
	cidrs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	}
	nets := make([]*net.IPNet, len(cidrs))
	for i, c := range cidrs {
		_, n, _ := net.ParseCIDR(c)
		nets[i] = n
	}
	return nets
}()

func isPrivateIP(ip net.IP) bool {
	for _, n := range privateNetworks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

var resolveAndValidate = defaultResolveAndValidate

func defaultResolveAndValidate(host string) (string, error) {
	hostname, port, err := net.SplitHostPort(host)
	if err != nil {
		return "", fmt.Errorf("invalid host: %w", err)
	}
	ips, err := net.LookupIP(hostname)
	if err != nil {
		return "", fmt.Errorf("DNS lookup failed: %w", err)
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return "", fmt.Errorf("resolved to private IP %s", ip)
		}
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("no addresses for %s", hostname)
	}
	return net.JoinHostPort(ips[0].String(), port), nil
}

type TemporaryAllowStore struct {
	mu    sync.Mutex
	hosts map[string]time.Time
}

func NewTemporaryAllowStore() *TemporaryAllowStore {
	return &TemporaryAllowStore{hosts: make(map[string]time.Time)}
}

func (s *TemporaryAllowStore) Allow(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts[host] = time.Now().Add(tempAllowDuration)
}

func (s *TemporaryAllowStore) Revoke(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.hosts, host)
}

func (s *TemporaryAllowStore) IsAllowed(host string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.hosts[host]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(s.hosts, host)
		return false
	}
	return true
}

func (s *TemporaryAllowStore) Snapshot() map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := make(map[string]time.Time, len(s.hosts))
	for k, exp := range s.hosts {
		if now.Before(exp) {
			out[k] = exp
		}
	}
	return out
}

var tempAllowStore = NewTemporaryAllowStore()

var allowedPorts = map[string]bool{
	"443": true,
	"80":  true,
}

// Project is the per-project context the proxy enforces against.
type Project struct {
	Name     string
	Patterns []string
	Denylist []string
	Ports    []ProjectPort
}

// ProjectPort is a host port allocation reported by bach for dashboard display.
type ProjectPort struct {
	Name          string `json:"name"`
	Session       string `json:"session"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Scheme        string `json:"scheme"`
}

// ProjectStore maps source CIDRs to Project metadata. bach registers each
// project's network subnet + allowlist via POST /project on startup.
type ProjectStore struct {
	mu      sync.RWMutex
	entries []projectEntry
}

type projectEntry struct {
	cidr *net.IPNet
	proj *Project
}

func NewProjectStore() *ProjectStore { return &ProjectStore{} }

func (s *ProjectStore) Upsert(name, cidrStr string, patterns, denylist []string, ports []ProjectPort) error {
	_, n, err := net.ParseCIDR(cidrStr)
	if err != nil {
		return err
	}
	p := &Project{Name: name, Patterns: patterns, Denylist: denylist, Ports: ports}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.entries {
		if s.entries[i].cidr.String() == n.String() {
			s.entries[i] = projectEntry{cidr: n, proj: p}
			return nil
		}
	}
	s.entries = append(s.entries, projectEntry{cidr: n, proj: p})
	return nil
}

func (s *ProjectStore) LookupIP(ip net.IP) *Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.entries {
		if e.cidr.Contains(ip) {
			return e.proj
		}
	}
	return nil
}

func (s *ProjectStore) List() []*Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Project, len(s.entries))
	for i, e := range s.entries {
		out[i] = e.proj
	}
	return out
}

var projectStore = NewProjectStore()

// ---------- TCP forwarder ----------
//
// Session containers live on --internal docker networks, where port
// publishing is silently no-op'd. Host->session traffic therefore enters
// via this proxy (which is dual-homed: bach-internet bridge + each
// project's --internal network) and is TCP-forwarded to the session's
// container IP. The proxy container pre-publishes a port range
// (BACH_FORWARD_RANGE, e.g. "4100-4199") that bach allocates within.

// Upstream is a forwarder target — the IP+port of a session container on
// one of the project --internal networks, plus the project name for
// logging.
type Upstream struct {
	Project       string
	ContainerIP   string
	ContainerPort int
}

func (u Upstream) Addr() string {
	return net.JoinHostPort(u.ContainerIP, strconv.Itoa(u.ContainerPort))
}

// ForwarderManager keeps a listener per host port. Each listener accepts
// connections that arrived on the bach-internet (or dashboard-allow) CIDR
// — connections originating from inside a project --internal network are
// rejected on accept. Listeners are created lazily and closed when the
// last project that referenced their host port goes away.
type ForwarderManager struct {
	mu        sync.Mutex
	listeners map[int]*forwarder
	rangeLo   int
	rangeHi   int
}

type forwarder struct {
	hostPort int
	listener net.Listener
	upstream Upstream
	stopOnce sync.Once
}

func NewForwarderManager(lo, hi int) *ForwarderManager {
	return &ForwarderManager{
		listeners: make(map[int]*forwarder),
		rangeLo:   lo,
		rangeHi:   hi,
	}
}

func (m *ForwarderManager) InRange(port int) bool {
	return m.rangeLo > 0 && port >= m.rangeLo && port <= m.rangeHi
}

// Apply replaces this project's contribution to the forwarder map with
// `want` (host port -> upstream). Listeners no longer referenced by this
// project are closed; new ones are started lazily.
func (m *ForwarderManager) Apply(project string, want map[int]Upstream) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Drop any existing entries for this project so we can re-add the
	// new set; ports that disappear get closed at the end.
	for port, fwd := range m.listeners {
		if fwd.upstream.Project == project {
			if _, keep := want[port]; !keep {
				fwd.stop()
				delete(m.listeners, port)
			}
		}
	}

	// Add or update entries.
	for port, up := range want {
		up.Project = project
		if fwd, ok := m.listeners[port]; ok {
			fwd.upstream = up
			continue
		}
		if !m.InRange(port) {
			log.Printf("forwarder: refused port %d for project=%s (out of range %d-%d)",
				port, project, m.rangeLo, m.rangeHi)
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
		if err != nil {
			log.Printf("forwarder: listen :%d failed: %v", port, err)
			continue
		}
		fwd := &forwarder{hostPort: port, listener: ln, upstream: up}
		m.listeners[port] = fwd
		go fwd.serve()
		log.Printf("forwarder: :%d -> %s (project=%s)", port, up.Addr(), project)
	}
}

func (f *forwarder) stop() {
	f.stopOnce.Do(func() {
		_ = f.listener.Close()
		log.Printf("forwarder: :%d stopped", f.hostPort)
	})
}

func (f *forwarder) serve() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *forwarder) handle(client net.Conn) {
	defer client.Close()
	// Reject connections that didn't arrive on a host-facing interface
	// (i.e. from a session container looping back via bach-proxy:<port>).
	if la, ok := client.LocalAddr().(*net.TCPAddr); ok {
		if !forwarderAllowed(la.IP) {
			log.Printf("forwarder: :%d rejected (local=%s)", f.hostPort, la.IP)
			return
		}
	}
	upstream, err := net.DialTimeout("tcp", f.upstream.Addr(), 5*time.Second)
	if err != nil {
		log.Printf("forwarder: :%d dial %s: %v", f.hostPort, f.upstream.Addr(), err)
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, client); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

// forwarderAllowCIDRs is the set of local-arrival interface CIDRs from
// which the TCP forwarder accepts connections. Defaults to the dashboard
// allow CIDRs (host loopback + bach-internet), which is what we want:
// host-published ports arrive on the loopback-side interface, and
// session-container connections arrive on a project --internal interface
// and are rejected here.
var forwarderAllowCIDRs []*net.IPNet

func forwarderAllowed(ip net.IP) bool {
	for _, n := range forwarderAllowCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

var forwarderManager *ForwarderManager

type PendingRequest struct {
	ID        string
	Project   string
	Host      string
	Method    string
	CreatedAt time.Time
	done      chan bool
}

type PendingStore struct {
	mu       sync.Mutex
	requests map[string]*PendingRequest
	counter  int
}

func NewPendingStore() *PendingStore {
	return &PendingStore{requests: make(map[string]*PendingRequest)}
}

func (ps *PendingStore) Add(project, host, method string) *PendingRequest {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.counter++
	req := &PendingRequest{
		ID:        fmt.Sprintf("pending-%d", ps.counter),
		Project:   project,
		Host:      host,
		Method:    method,
		CreatedAt: time.Now(),
		done:      make(chan bool, 1),
	}
	ps.requests[req.ID] = req
	return req
}

func (ps *PendingStore) Remove(id string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.requests, id)
}

func (ps *PendingStore) ApproveHost(project, host string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, req := range ps.requests {
		if req.Host == host && req.Project == project {
			select {
			case req.done <- true:
			default:
			}
		}
	}
}

func (ps *PendingStore) List() []*PendingRequest {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := make([]*PendingRequest, 0, len(ps.requests))
	for _, r := range ps.requests {
		out = append(out, r)
	}
	return out
}

var pendingStore = NewPendingStore()

type ProxyEvent struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Project   string    `json:"project"`
	Method    string    `json:"method"`
	Host      string    `json:"host"`
	Status    string    `json:"status"`
}

var eventIDCounter atomic.Int64

func nextEventID() string {
	return fmt.Sprintf("evt-%d", eventIDCounter.Add(1))
}

type EventStore struct {
	mu          sync.Mutex
	events      []ProxyEvent
	capacity    int
	subscribers map[chan ProxyEvent]struct{}
}

func NewEventStore(capacity int) *EventStore {
	return &EventStore{
		events:      make([]ProxyEvent, 0, capacity),
		capacity:    capacity,
		subscribers: make(map[chan ProxyEvent]struct{}),
	}
}

func (s *EventStore) Add(e ProxyEvent) {
	s.mu.Lock()
	if len(s.events) >= s.capacity {
		s.events = s.events[1:]
	}
	s.events = append(s.events, e)
	for ch := range s.subscribers {
		select {
		case ch <- e:
		default:
		}
	}
	s.mu.Unlock()
}

func (s *EventStore) Recent() []ProxyEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ProxyEvent, len(s.events))
	copy(out, s.events)
	return out
}

func (s *EventStore) Subscribe() (chan ProxyEvent, func(), error) {
	ch := make(chan ProxyEvent, 64)
	s.mu.Lock()
	if len(s.subscribers) >= maxSubscribers {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("too many subscribers")
	}
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subscribers, ch)
		s.mu.Unlock()
	}, nil
}

func (s *EventStore) Update(id string, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.events {
		if s.events[i].ID == id {
			s.events[i].Status = status
			for ch := range s.subscribers {
				select {
				case ch <- s.events[i]:
				default:
				}
			}
			break
		}
	}
}

var eventStore = NewEventStore(1000)

// matchesPatterns returns true if host matches any pattern in the list.
// Patterns are either exact (`api.github.com`) or wildcard suffix
// (`*.githubusercontent.com` matches both `githubusercontent.com` and
// `raw.githubusercontent.com`). Match is case-insensitive on the hostname;
// if `host` includes `:port`, the port must be in `allowedPorts`.
func matchesPatterns(patterns []string, host string) bool {
	hostname, port, err := net.SplitHostPort(host)
	if err != nil {
		hostname = host
		port = ""
	}
	hostname = strings.ToLower(hostname)

	if port != "" && !allowedPorts[port] {
		return false
	}

	for _, pattern := range patterns {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		if strings.HasPrefix(pattern, "*.") {
			suffix := pattern[1:]
			if hostname == pattern[2:] || strings.HasSuffix(hostname, suffix) {
				return true
			}
		} else {
			if hostname == pattern {
				return true
			}
		}
	}
	return false
}

func projectFromRemote(r *http.Request) *Project {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	return projectStore.LookupIP(ip)
}

func projectLabel(p *Project) string {
	if p == nil {
		return "?"
	}
	return p.Name
}

func normalizeHost(host string) string {
	hostname, _, err := net.SplitHostPort(host)
	if err != nil {
		hostname = host
	}
	return strings.ToLower(hostname)
}

func isValidDomain(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, c := range label {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || (c == '-' && i > 0 && i < len(label)-1)) {
				return false
			}
		}
	}
	return true
}

func validateAndAuthorize(w http.ResponseWriter, r *http.Request, method, host, port string) string {
	proj := projectFromRemote(r)
	projName := projectLabel(proj)

	mkEvent := func(id, status string) ProxyEvent {
		return ProxyEvent{
			ID: id, Timestamp: time.Now(), Project: projName,
			Method: method, Host: normalizeHost(host), Status: status,
		}
	}

	if !isValidDomain(host) {
		log.Printf("%s invalid_domain project=%s host=%s", method, projName, host)
		eventStore.Add(mkEvent(nextEventID(), "invalid_domain"))
		http.Error(w, "Forbidden", http.StatusForbidden)
		return ""
	}

	hostPort := net.JoinHostPort(host, port)

	dialAddr, err := resolveAndValidate(hostPort)
	if err != nil {
		log.Printf("%s non_public_ip project=%s host=%s err=%v", method, projName, host, err)
		eventStore.Add(mkEvent(nextEventID(), "non_public_ip"))
		http.Error(w, "Forbidden", http.StatusForbidden)
		return ""
	}

	if !allowedPorts[port] {
		log.Printf("%s blocked_port project=%s host=%s port=%s", method, projName, host, port)
		ev := mkEvent(nextEventID(), "invalid_port")
		ev.Host = strings.ToLower(hostPort)
		eventStore.Add(ev)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return ""
	}

	var patterns, denylist []string
	if proj != nil {
		patterns = proj.Patterns
		denylist = proj.Denylist
	}
	allowed := matchesPatterns(patterns, hostPort)
	tempAllowed := !allowed && tempAllowStore.IsAllowed(tempAllowKey(projName, host))
	// Denylist is checked AFTER temp-allow so a user-issued approval via the
	// dashboard still overrides the project's denylist.
	denied := !allowed && !tempAllowed && matchesPatterns(denylist, hostPort)
	log.Printf("%s project=%s host=%s allowed=%t temp_allowed=%t denied=%t", method, projName, host, allowed, tempAllowed, denied)

	if allowed || tempAllowed {
		status := "allowed"
		if tempAllowed {
			status = "temp_allowed"
		}
		eventStore.Add(mkEvent(nextEventID(), status))
	} else if denied {
		// Skip the pending hold — user can still issue an approval from the
		// dashboard at any time, which lands as a temp-allow on the next request.
		eventStore.Add(mkEvent(nextEventID(), "rejected"))
		http.Error(w, "Forbidden", http.StatusForbidden)
		return ""
	} else {
		evtID := nextEventID()
		eventStore.Add(mkEvent(evtID, "pending"))

		pending := pendingStore.Add(projName, host, method)
		defer pendingStore.Remove(pending.ID)

		select {
		case approved := <-pending.done:
			if !approved {
				eventStore.Update(evtID, "rejected")
				http.Error(w, "Forbidden", http.StatusForbidden)
				return ""
			}
		case <-time.After(holdDuration):
			eventStore.Update(evtID, "rejected")
			http.Error(w, "Forbidden", http.StatusForbidden)
			return ""
		}
		eventStore.Update(evtID, "temp_allowed")
	}

	return dialAddr
}

// tempAllowKey scopes temp-allow entries per project so an approval in one
// project doesn't bleed into another.
func tempAllowKey(project, host string) string {
	return project + "|" + normalizeHost(host)
}

func handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	dialAddr := validateAndAuthorize(w, r, r.Method, host, port)
	if dialAddr == "" {
		return
	}

	dest, err := net.DialTimeout("tcp", dialAddr, 10*time.Second)
	if err != nil {
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer dest.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	client, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer client.Close()

	client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	done := make(chan struct{}, 2)
	go func() { io.Copy(dest, client); done <- struct{}{} }()
	go func() { io.Copy(client, dest); done <- struct{}{} }()
	<-done
}

func handleHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Hostname()
	if host == "" {
		host = r.Host
	}
	port := r.URL.Port()
	if port == "" {
		port = "80"
	}

	dialAddr := validateAndAuthorize(w, r, r.Method, host, port)
	if dialAddr == "" {
		return
	}

	outReq := r.Clone(r.Context())
	outReq.URL.Host = dialAddr
	resp, err := proxyTransport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

var csrfToken = func() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}()

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, ""
	}
	fmt.Fprintf(w, dashboardHTML, csrfToken, html.EscapeString(host), html.EscapeString(port))
}

func handleApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-CSRF-Token") != csrfToken {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	host := r.URL.Query().Get("host")
	project := r.URL.Query().Get("project")
	if host == "" || project == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	tempAllowStore.Allow(tempAllowKey(project, host))
	pendingStore.ApproveHost(project, host)
	w.WriteHeader(http.StatusOK)
}

func handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-CSRF-Token") != csrfToken {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	host := r.URL.Query().Get("host")
	project := r.URL.Query().Get("project")
	if host == "" || project == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	tempAllowStore.Revoke(tempAllowKey(project, host))
	w.WriteHeader(http.StatusOK)
}

// handleProjectUpsert registers (or replaces) a project mapping. Source-IP
// based: bach POSTs the project's --internal network CIDR plus its allowlist.
// Reached only from BACH_DASH_ALLOW_CIDR (host loopback / bach-internet).
func handleProjectUpsert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Name      string        `json:"name"`
		CIDR      string        `json:"cidr"`
		Allowlist []string      `json:"allowlist"`
		Denylist  []string      `json:"denylist"`
		Ports     []ProjectPort `json:"ports"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Name == "" || body.CIDR == "" {
		http.Error(w, "name and cidr required", http.StatusBadRequest)
		return
	}
	if err := projectStore.Upsert(body.Name, body.CIDR, body.Allowlist, body.Denylist, body.Ports); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("project registered name=%s cidr=%s allow=%v deny=%v ports=%d", body.Name, body.CIDR, body.Allowlist, body.Denylist, len(body.Ports))
	w.WriteHeader(http.StatusOK)
}

// handleForwards is the control-plane endpoint bach uses to register the
// TCP forwards for a project. Body: {project, forwards:[{host_port,
// container_ip, container_port}]}. Replaces this project's existing
// forwards entirely.
func handleForwards(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Project  string `json:"project"`
		Forwards []struct {
			HostPort      int    `json:"host_port"`
			ContainerIP   string `json:"container_ip"`
			ContainerPort int    `json:"container_port"`
		} `json:"forwards"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Project == "" {
		http.Error(w, "project required", http.StatusBadRequest)
		return
	}
	want := map[int]Upstream{}
	for _, f := range body.Forwards {
		if f.HostPort == 0 || f.ContainerIP == "" || f.ContainerPort == 0 {
			continue
		}
		want[f.HostPort] = Upstream{
			Project:       body.Project,
			ContainerIP:   f.ContainerIP,
			ContainerPort: f.ContainerPort,
		}
	}
	if forwarderManager != nil {
		forwarderManager.Apply(body.Project, want)
	}
	w.WriteHeader(http.StatusOK)
}

// handlePorts returns the current per-project port allocations as JSON for
// the dashboard. Gated by the same dashboard-CIDR check as the rest of the
// dash endpoints.
func handlePorts(w http.ResponseWriter, r *http.Request) {
	type row struct {
		Project       string `json:"project"`
		Session       string `json:"session"`
		Name          string `json:"name"`
		HostPort      int    `json:"host_port"`
		ContainerPort int    `json:"container_port"`
		Scheme        string `json:"scheme"`
	}
	out := []row{}
	for _, p := range projectStore.List() {
		for _, port := range p.Ports {
			scheme := port.Scheme
			if scheme == "" {
				scheme = "http"
			}
			out = append(out, row{p.Name, port.Session, port.Name, port.HostPort, port.ContainerPort, scheme})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// handleTempAllows returns active temp-allow entries with expiry timestamps
// (unix ms) so the dashboard can render minutes-remaining countdowns.
func handleTempAllows(w http.ResponseWriter, r *http.Request) {
	type row struct {
		Project   string `json:"project"`
		Host      string `json:"host"`
		ExpiresAt int64  `json:"expires_at"`
	}
	out := []row{}
	for key, exp := range tempAllowStore.Snapshot() {
		i := strings.Index(key, "|")
		if i < 0 {
			continue
		}
		out = append(out, row{Project: key[:i], Host: key[i+1:], ExpiresAt: exp.UnixMilli()})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	for _, e := range eventStore.Recent() {
		data, _ := json.Marshal(e)
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	flusher.Flush()

	ch, unsub, err := eventStore.Subscribe()
	if err != nil {
		http.Error(w, "Too Many Connections", http.StatusServiceUnavailable)
		return
	}
	defer unsub()

	for {
		select {
		case e := <-ch:
			data, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// dashboardAllowCIDRs is the set of local-interface CIDRs from which the
// dashboard accepts connections. The check is against the LOCAL address the
// TCP connection arrived on (i.e. which of the proxy's NICs received it), not
// the remote address. This is what isolates session containers from the
// dashboard: a session connecting to bach-proxy:8081 arrives at the proxy's
// project-network interface, which is not in this allowlist.
var dashboardAllowCIDRs []*net.IPNet

func dashboardAllowed(r *http.Request) bool {
	la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	host, _, err := net.SplitHostPort(la.String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range dashboardAllowCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func parseCIDRs(raw string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, c := range strings.Split(raw, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("bad CIDR %q: %w", c, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func loadConfig() {
	if raw := os.Getenv("BACH_ALLOWED_PORTS"); raw != "" {
		ports := map[string]bool{}
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				ports[p] = true
			}
		}
		if len(ports) > 0 {
			allowedPorts = ports
		}
	}
	if v := os.Getenv("BACH_HOLD_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			holdDuration = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("BACH_TEMP_ALLOW_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			tempAllowDuration = time.Duration(n) * time.Second
		}
	}
	raw := os.Getenv("BACH_DASH_ALLOW_CIDR")
	if raw == "" {
		raw = "127.0.0.0/8"
	}
	cidrs, err := parseCIDRs(raw)
	if err != nil {
		log.Fatalf("BACH_DASH_ALLOW_CIDR: %v", err)
	}
	dashboardAllowCIDRs = cidrs
	// Reuse the dashboard's allow CIDRs for the TCP forwarder: connections
	// must arrive on the same host-facing/internal-bridge interfaces.
	forwarderAllowCIDRs = cidrs

	lo, hi := 0, 0
	if r := os.Getenv("BACH_FORWARD_RANGE"); r != "" {
		parts := strings.SplitN(r, "-", 2)
		if len(parts) == 2 {
			a, errA := strconv.Atoi(strings.TrimSpace(parts[0]))
			b, errB := strconv.Atoi(strings.TrimSpace(parts[1]))
			if errA == nil && errB == nil && a > 0 && b >= a {
				lo, hi = a, b
			}
		}
		if lo == 0 {
			log.Printf("BACH_FORWARD_RANGE %q invalid, forwarder disabled", r)
		}
	}
	forwarderManager = NewForwarderManager(lo, hi)
}

func main() {
	loadConfig()

	proxyAddr := os.Getenv("BACH_PROXY_LISTEN")
	if proxyAddr == "" {
		proxyAddr = ":8080"
	}
	dashAddr := os.Getenv("BACH_DASH_LISTEN")
	if dashAddr == "" {
		dashAddr = ":8081"
	}

	server := &http.Server{
		Addr: proxyAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				handleConnect(w, r)
			} else {
				handleHTTP(w, r)
			}
		}),
	}

	dashMux := http.NewServeMux()
	dashMux.HandleFunc("/", handleDashboard)
	dashMux.HandleFunc("/events", handleSSE)
	dashMux.HandleFunc("/approve", handleApprove)
	dashMux.HandleFunc("/revoke", handleRevoke)
	dashMux.HandleFunc("/project", handleProjectUpsert)
	dashMux.HandleFunc("/ports", handlePorts)
	dashMux.HandleFunc("/temp-allows", handleTempAllows)
	dashMux.HandleFunc("/forwards", handleForwards)

	dashServer := &http.Server{
		Addr: dashAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !dashboardAllowed(r) {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			dashMux.ServeHTTP(w, r)
		}),
	}

	go func() {
		log.Printf("dashboard listening on %s (allow_cidr=%v)", dashAddr, cidrStrings(dashboardAllowCIDRs))
		log.Fatal(dashServer.ListenAndServe())
	}()

	log.Printf("proxy listening on %s (per-project allowlist via POST /project)", proxyAddr)
	log.Fatal(server.ListenAndServe())
}

func cidrStrings(nets []*net.IPNet) []string {
	out := make([]string, len(nets))
	for i, n := range nets {
		out[i] = n.String()
	}
	return out
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="csrf-token" content="%s">
<title>bach.proxy</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@300;400;500;600;700&display=swap" rel="stylesheet">
<style>
  :root {
    --bg: #0a0b0d;
    --panel: #0f1116;
    --line: #1b1e25;
    --line-soft: #14171c;
    --line-strong: #2a2f38;
    --text: #e6e7ea;
    --text-mute: #9ca0aa;
    --text-dim: #5b6068;
    --text-dimmer: #3a3e46;
    --row-hover: #14171c;
    --green: #65d49a;
    --green-soft: #65d49a14;
    --blue: #7aaaff;
    --blue-soft: #7aaaff14;
    --amber: #f0b95a;
    --amber-soft: #f0b95a14;
    --red: #ef6b6b;
    --red-soft: #ef6b6b14;
    --violet: #b89cf2;
    --violet-soft: #b89cf214;
    --accent: #b8ed5a;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  html, body { background: var(--bg); }
  body {
    font-family: "JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, monospace;
    font-feature-settings: "ss01", "ss02", "cv02", "cv11";
    color: var(--text);
    font-size: 15px;
    line-height: 1.5;
    padding: 32px 36px 72px;
    max-width: 1240px;
    margin: 0 auto;
    -webkit-font-smoothing: antialiased;
    text-rendering: geometricPrecision;
  }
  /* header */
  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding-bottom: 18px;
    margin-bottom: 28px;
    border-bottom: 1px solid var(--line);
  }
  .brand {
    display: flex;
    align-items: baseline;
    gap: 14px;
    font-weight: 700;
    letter-spacing: -0.02em;
  }
  .brand .mark {
    font-size: 22px;
    color: var(--text);
  }
  .brand .mark .accent { color: var(--accent); }
  .live {
    display: inline-flex;
    align-items: center;
    gap: 10px;
    font-size: 12px;
    text-transform: uppercase;
    letter-spacing: 0.16em;
    color: var(--text-mute);
    font-weight: 500;
  }
  .live .dot {
    width: 8px; height: 8px;
    border-radius: 50%%;
    background: var(--accent);
    box-shadow: 0 0 0 0 var(--accent);
    animation: pulse 2.4s ease-out infinite;
  }
  .live.off .dot { background: var(--text-dimmer); animation: none; box-shadow: none; }
  @keyframes pulse {
    0%%   { box-shadow: 0 0 0 0 rgba(184, 237, 90, 0.45); }
    70%%  { box-shadow: 0 0 0 7px rgba(184, 237, 90, 0); }
    100%% { box-shadow: 0 0 0 0 rgba(184, 237, 90, 0); }
  }
  /* section */
  section { margin-bottom: 40px; }
  .section-head {
    display: flex;
    align-items: center;
    gap: 14px;
    margin-bottom: 14px;
  }
  .section-title {
    display: flex;
    align-items: baseline;
    gap: 12px;
    font-size: 13px;
    text-transform: uppercase;
    letter-spacing: 0.18em;
    font-weight: 600;
    color: var(--text);
    white-space: nowrap;
  }
  .section-title .idx { color: var(--text-dim); font-weight: 400; }
  .section-title .name { color: var(--text); }
  .section-rule {
    flex: 1;
    height: 1px;
    background: var(--line);
  }
  .chips {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
    margin-bottom: 14px;
  }
  .chip {
    display: inline-flex;
    align-items: center;
    gap: 9px;
    padding: 6px 13px 6px 11px;
    font-size: 13px;
    letter-spacing: 0.02em;
    border: 1px solid var(--line);
    border-radius: 999px;
    background: transparent;
    color: var(--text-mute);
    font-weight: 500;
    font-variant-numeric: tabular-nums;
    transition: border-color 120ms ease, color 120ms ease;
  }
  .chip .swatch {
    width: 7px; height: 7px;
    border-radius: 50%%;
    background: var(--text-dim);
  }
  .chip .n { color: var(--text); font-weight: 600; }
  .chip[data-k="allowed"]      { color: var(--green); border-color: rgba(101, 212, 154, 0.25); }
  .chip[data-k="allowed"] .swatch { background: var(--green); }
  .chip[data-k="temp_allowed"] { color: var(--blue); border-color: rgba(122, 170, 255, 0.25); }
  .chip[data-k="temp_allowed"] .swatch { background: var(--blue); }
  .chip[data-k="pending"]      { color: var(--amber); border-color: rgba(240, 185, 90, 0.30); }
  .chip[data-k="pending"] .swatch { background: var(--amber); }
  .chip[data-k="rejected"]     { color: var(--red); border-color: rgba(239, 107, 107, 0.25); }
  .chip[data-k="rejected"] .swatch { background: var(--red); }
  .chip[data-k="blocked"]      { color: var(--violet); border-color: rgba(184, 156, 242, 0.25); }
  .chip[data-k="blocked"] .swatch { background: var(--violet); }
  .chip.zero { opacity: 0.5; }
  .chip.zero .n { color: var(--text-mute); }
  /* tables */
  .table-wrap {
    border: 1px solid var(--line);
    border-radius: 6px;
    overflow: hidden;
    background: var(--panel);
  }
  table {
    width: 100%%;
    border-collapse: collapse;
    font-size: 14px;
  }
  thead tr {
    background: var(--line-soft);
  }
  th {
    text-align: left;
    padding: 11px 16px;
    border-bottom: 1px solid var(--line);
    color: var(--text-dim);
    font-weight: 500;
    font-size: 12px;
    text-transform: uppercase;
    letter-spacing: 0.14em;
    white-space: nowrap;
  }
  th.num, td.num {
    text-align: right;
    font-variant-numeric: tabular-nums;
  }
  th.act, td.act {
    text-align: right;
    width: 1%%;
    white-space: nowrap;
  }
  tbody tr {
    border-bottom: 1px solid var(--line-soft);
    transition: background 120ms ease;
  }
  tbody tr:last-child { border-bottom: none; }
  tbody tr:hover { background: var(--row-hover); }
  td {
    padding: 11px 16px;
    color: var(--text);
    vertical-align: middle;
  }
  td.project { color: var(--text-mute); }
  td.host {
    font-weight: 500;
    color: var(--text);
    word-break: break-all;
  }
  td.status {
    color: var(--text-mute);
    font-size: 13px;
  }
  td.status .ttl {
    margin-left: 8px;
    padding: 1px 7px;
    border-radius: 3px;
    background: var(--blue-soft);
    color: var(--blue);
    font-size: 11.5px;
    font-variant-numeric: tabular-nums;
    letter-spacing: 0.02em;
  }
  td.ago, td.session { color: var(--text-mute); font-size: 13px; }
  td.num { color: var(--text); font-weight: 500; }
  td.num.zero { color: var(--text-dimmer); font-weight: 400; }
  td a {
    color: var(--blue);
    text-decoration: none;
    border-bottom: 1px dotted rgba(122, 170, 255, 0.35);
    padding-bottom: 1px;
  }
  td a:hover { border-bottom-style: solid; }
  td .arrow { color: var(--text-dimmer); padding: 0 6px; }
  /* status dot prefix */
  .dot {
    display: inline-block;
    width: 8px; height: 8px;
    border-radius: 50%%;
    margin-right: 11px;
    vertical-align: 1px;
    background: var(--text-dim);
    flex-shrink: 0;
  }
  .dot.allowed       { background: var(--green); }
  .dot.temp_allowed  { background: var(--blue); }
  .dot.pending       { background: var(--amber); box-shadow: 0 0 0 0 var(--amber); animation: pulse-amber 2s ease-out infinite; }
  .dot.rejected      { background: var(--red); }
  .dot.non_public_ip,
  .dot.invalid_port,
  .dot.invalid_domain { background: var(--violet); }
  @keyframes pulse-amber {
    0%%   { box-shadow: 0 0 0 0 rgba(240, 185, 90, 0.55); }
    70%%  { box-shadow: 0 0 0 6px rgba(240, 185, 90, 0); }
    100%% { box-shadow: 0 0 0 0 rgba(240, 185, 90, 0); }
  }
  /* pending row highlight */
  tr.pending td.host { color: var(--amber); }
  tr.rejected td.host { color: var(--red); }
  tr.temp_allowed td.host { color: var(--blue); }
  tr.non_public_ip td.host,
  tr.invalid_port td.host,
  tr.invalid_domain td.host { color: var(--violet); }
  /* buttons */
  .btn {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 6px 13px;
    border-radius: 4px;
    border: 1px solid var(--line-strong);
    background: transparent;
    color: var(--text-mute);
    font-family: inherit;
    font-size: 12px;
    font-weight: 500;
    letter-spacing: 0.08em;
    text-transform: uppercase;
    cursor: pointer;
    transition: all 120ms ease;
  }
  .btn:hover {
    color: var(--text);
    border-color: var(--text-dim);
  }
  .btn.allow:hover {
    color: #0a0b0d;
    background: var(--green);
    border-color: var(--green);
  }
  .btn.revoke:hover {
    color: #0a0b0d;
    background: var(--red);
    border-color: var(--red);
  }
  /* empty */
  .empty {
    padding: 44px 16px;
    text-align: center;
    color: var(--text-dim);
    font-size: 14px;
  }
  .empty .cursor {
    display: inline-block;
    width: 8px; height: 15px;
    margin-left: 5px;
    vertical-align: -2px;
    background: var(--text-dim);
    animation: blink 1.1s steps(2, end) infinite;
  }
  @keyframes blink { 50%% { opacity: 0; } }
  /* footer */
  footer {
    margin-top: 40px;
    padding-top: 18px;
    border-top: 1px solid var(--line);
    display: flex;
    justify-content: space-between;
    color: var(--text-dim);
    font-size: 12px;
    text-transform: uppercase;
    letter-spacing: 0.14em;
  }
  footer a { color: var(--text-dim); text-decoration: none; }
  footer a:hover { color: var(--text-mute); }
</style>
</head>
<body>
<header>
  <div class="brand">
    <span class="mark">bach<span class="accent">.</span></span>
  </div>
  <span class="live" id="live"><span class="dot"></span>live</span>
</header>

<section>
  <div class="section-head">
    <div class="section-title"><span class="idx">01</span><span class="name">published ports</span></div>
    <div class="section-rule"></div>
  </div>
  <div class="table-wrap">
    <table>
      <thead><tr>
        <th>project</th>
        <th>session</th>
        <th>name</th>
        <th class="num">host</th>
        <th class="num">container</th>
        <th>link</th>
      </tr></thead>
      <tbody id="ports"></tbody>
    </table>
    <div class="empty" id="ports-empty">no ports published</div>
  </div>
</section>

<section>
  <div class="section-head">
    <div class="section-title"><span class="idx">02</span><span class="name">outbound requests</span></div>
    <div class="section-rule"></div>
  </div>
  <div class="chips">
    <span class="chip" data-k="allowed" id="allowed-count"><span class="swatch"></span>allowed <span class="n">0</span></span>
    <span class="chip" data-k="temp_allowed" id="temp-allowed-count"><span class="swatch"></span>temp <span class="n">0</span></span>
    <span class="chip" data-k="pending" id="pending-count"><span class="swatch"></span>pending <span class="n">0</span></span>
    <span class="chip" data-k="rejected" id="rejected-count"><span class="swatch"></span>rejected <span class="n">0</span></span>
    <span class="chip" data-k="blocked" id="blocked-count"><span class="swatch"></span>blocked <span class="n">0</span></span>
  </div>
  <div class="table-wrap">
    <table>
      <thead><tr>
        <th>project</th>
        <th>host</th>
        <th>status</th>
        <th class="num">1m</th>
        <th class="num">10m</th>
        <th>last seen</th>
        <th class="act"></th>
      </tr></thead>
      <tbody id="events"></tbody>
    </table>
    <div class="empty" id="empty-msg">waiting for requests<span class="cursor"></span></div>
  </div>
</section>

<footer>
  <span>auto · refresh 1s</span>
  <span>%s<span style="color:var(--text-dimmer);">:</span>%s</span>
</footer>
<script>
(function() {
  var tbody = document.getElementById("events");
  var emptyMsg = document.getElementById("empty-msg");
  var liveEl = document.getElementById("live");

  var chipEls = {
    allowed:      document.getElementById("allowed-count"),
    temp_allowed: document.getElementById("temp-allowed-count"),
    pending:      document.getElementById("pending-count"),
    rejected:     document.getElementById("rejected-count"),
    blocked:      document.getElementById("blocked-count"),
  };

  var csrfToken = document.querySelector('meta[name="csrf-token"]').content;

  var hosts = {};
  var tempExpiry = {};

  var statusLabels = { allowed: "allowed", temp_allowed: "temp allowed", pending: "pending", rejected: "rejected", non_public_ip: "non-public ip", invalid_port: "invalid port", invalid_domain: "invalid domain" };

  function esc(s) {
    var d = document.createElement("div");
    d.textContent = s;
    return d.innerHTML;
  }

  function ago(ts) {
    var s = Math.floor((Date.now() - ts) / 1000);
    if (s < 5) return "just now";
    if (s < 60) return s + "s ago";
    var m = Math.floor(s / 60);
    return m + "m ago";
  }

  function effectiveStatus(h) {
    if (h.pendingCount > 0) return "pending";
    return h.latestStatus === "pending" ? "rejected" : h.latestStatus;
  }

  function renderRow(h) {
    var now = Date.now();
    var oneMin = now - 60000;
    var tenMin = now - 600000;
    var count1 = 0, count10 = 0;
    for (var i = h.timestamps.length - 1; i >= 0; i--) {
      var t = h.timestamps[i];
      if (t >= oneMin) count1++;
      if (t >= tenMin) count10++;
      else break;
    }
    var st = effectiveStatus(h);
    h.tr.className = st;
    var hostArg = esc(h.host).replace(/"/g, "&quot;");
    var projArg = esc(h.project).replace(/"/g, "&quot;");
    var actionCell = "<td class='act'></td>";
    if (st === "pending" || st === "rejected") {
      actionCell = "<td class='act'><button class='btn allow' onclick='approveHost(\"" + projArg + "\",\"" + hostArg + "\")'>allow</button></td>";
    } else if (st === "temp_allowed") {
      actionCell = "<td class='act'><button class='btn revoke' onclick='revokeHost(\"" + projArg + "\",\"" + hostArg + "\")'>revoke</button></td>";
    }
    var c1cls = "num" + (count1 === 0 ? " zero" : "");
    var c10cls = "num" + (count10 === 0 ? " zero" : "");
    var statusLabel = statusLabels[st];
    if (st === "temp_allowed") {
      var exp = tempExpiry[h.key];
      if (exp) {
        var minsLeft = Math.max(1, Math.ceil((exp - Date.now()) / 60000));
        statusLabel += " <span class='ttl'>" + minsLeft + "m</span>";
      }
    }
    h.tr.innerHTML = "<td class='project'>" + esc(h.project) + "</td>" +
      "<td class='host'><span class='dot " + st + "'></span>" + esc(h.host) + "</td>" +
      "<td class='status'>" + statusLabel + "</td>" +
      "<td class='" + c1cls + "'>" + count1 + "</td>" +
      "<td class='" + c10cls + "'>" + count10 + "</td>" +
      "<td class='ago'>" + ago(h.lastSeen) + "</td>" +
      actionCell;
  }

  function rowKey(project, host) { return project + "|" + host; }

  function getOrCreate(project, host) {
    var key = rowKey(project, host);
    if (hosts[key]) return hosts[key];
    var tr = document.createElement("tr");
    var h = { project: project, host: host, key: key, latestStatus: "allowed", timestamps: [], tr: tr, lastSeen: 0, pendingCount: 0 };
    hosts[key] = h;
    tbody.appendChild(tr);
    emptyMsg.style.display = "none";
    return h;
  }

  function bumpToBottom(h) {
    if (tbody.lastChild !== h.tr) {
      tbody.appendChild(h.tr);
    }
  }

  function handleEvent(ev) {
    var h = getOrCreate(ev.project || "?", ev.host);
    var ts = new Date(ev.timestamp).getTime();

    if (ev.status === "pending") {
      h.timestamps.push(ts);
      h.lastSeen = ts;
      h.pendingCount++;
      h.latestStatus = "pending";
      if (!h.pendingIDs) h.pendingIDs = {};
      h.pendingIDs[ev.id] = true;
    } else if (ev.id && h.pendingIDs && h.pendingIDs[ev.id]) {
      delete h.pendingIDs[ev.id];
      h.pendingCount--;
      h.latestStatus = ev.status;
    } else {
      h.timestamps.push(ts);
      h.lastSeen = ts;
      h.latestStatus = ev.status;
    }

    bumpToBottom(h);
    renderRow(h);
    updateBadges();
  }

  function setChip(key, n) {
    var el = chipEls[key];
    if (!el) return;
    el.querySelector(".n").textContent = n;
    if (n === 0) el.classList.add("zero");
    else el.classList.remove("zero");
  }

  function updateBadges() {
    var counts = { allowed: 0, temp_allowed: 0, pending: 0, rejected: 0, blocked: 0 };
    for (var k in hosts) {
      var st = effectiveStatus(hosts[k]);
      if (counts[st] !== undefined) counts[st]++;
      else counts.blocked++;
    }
    setChip("allowed", counts.allowed);
    setChip("temp_allowed", counts.temp_allowed);
    setChip("pending", counts.pending);
    setChip("rejected", counts.rejected);
    setChip("blocked", counts.blocked);
  }
  updateBadges();

  function cleanup() {
    var cutoff = Date.now() - 600000;
    for (var k in hosts) {
      var h = hosts[k];
      while (h.timestamps.length > 0 && h.timestamps[0] < cutoff) {
        h.timestamps.shift();
      }
      if (h.timestamps.length === 0 && h.pendingCount === 0) {
        tbody.removeChild(h.tr);
        delete hosts[k];
      } else {
        renderRow(h);
      }
    }
    if (Object.keys(hosts).length === 0) {
      emptyMsg.style.display = "";
    }
    updateBadges();
  }

  function refreshTempAllows() {
    fetch("/temp-allows").then(function(r) { return r.json(); }).then(function(rows) {
      var next = {};
      (rows || []).forEach(function(row) {
        next[row.project + "|" + row.host] = row.expires_at;
      });
      tempExpiry = next;
    }).catch(function() {});
  }
  refreshTempAllows();

  setInterval(function() { cleanup(); refreshTempAllows(); }, 1000);

  window.approveHost = function(project, host) {
    var key = rowKey(project, host);
    fetch("/approve?project=" + encodeURIComponent(project) + "&host=" + encodeURIComponent(host), {
      method: "POST",
      headers: { "X-CSRF-Token": csrfToken }
    }).then(function(resp) {
      if (resp.ok && hosts[key]) {
        hosts[key].latestStatus = "temp_allowed";
        renderRow(hosts[key]);
        updateBadges();
      }
    });
  };

  window.revokeHost = function(project, host) {
    var key = rowKey(project, host);
    fetch("/revoke?project=" + encodeURIComponent(project) + "&host=" + encodeURIComponent(host), {
      method: "POST",
      headers: { "X-CSRF-Token": csrfToken }
    }).then(function(resp) {
      if (resp.ok && hosts[key]) {
        hosts[key].latestStatus = "rejected";
        renderRow(hosts[key]);
        updateBadges();
      }
    });
  };

  var es = new EventSource("/events");
  es.onopen = function() { liveEl.classList.remove("off"); };
  es.onerror = function() { liveEl.classList.add("off"); };
  es.onmessage = function(e) {
    liveEl.classList.remove("off");
    handleEvent(JSON.parse(e.data));
  };

  var portsBody = document.getElementById("ports");
  var portsEmpty = document.getElementById("ports-empty");
  function refreshPorts() {
    fetch("/ports").then(function(r) { return r.json(); }).then(function(rows) {
      portsBody.innerHTML = "";
      if (!rows || rows.length === 0) {
        portsEmpty.style.display = "";
        return;
      }
      portsEmpty.style.display = "none";
      rows.sort(function(a, b) {
        if (a.project !== b.project) return a.project < b.project ? -1 : 1;
        if (a.session !== b.session) return a.session < b.session ? -1 : 1;
        return a.host_port - b.host_port;
      });
      rows.forEach(function(row) {
        var url = (row.scheme || "http") + "://localhost:" + row.host_port + "/";
        var tr = document.createElement("tr");
        tr.innerHTML = "<td class='project'>" + esc(row.project) + "</td>" +
          "<td class='session'>" + esc(row.session || "") + "</td>" +
          "<td>" + esc(row.name) + "</td>" +
          "<td class='num'>" + row.host_port + "</td>" +
          "<td class='num'>" + row.container_port + "</td>" +
          "<td><a href='" + esc(url) + "' target='_blank' rel='noopener'>" + esc(url) + "</a></td>";
        portsBody.appendChild(tr);
      });
    }).catch(function() {});
  }
  refreshPorts();
  setInterval(refreshPorts, 3000);
})();
</script>
</body>
</html>`
