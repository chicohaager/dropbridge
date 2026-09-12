// mesh.go — real-data feed for the Living Mesh Console.
//
// GET /api/mesh returns live node telemetry so the console shows the actual
// tailnet instead of demo data. Data sources (all best-effort — any failure
// degrades gracefully to a partial answer rather than erroring):
//   - tailscaled LocalAPI over the unix socket: node identity, tailnet IPs,
//     tailscale version, peer online-state, latency (ping), and whois (who is
//     hitting this box → real presence).
//   - statfs on the incoming volume → /DATA free/total.
//   - /proc → kernel release + uptime.
//   - the configured peer's own /api/mesh?bare=1 → its disk/uptime/presence.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const tsSocket = "/var/run/tailscale/tailscaled.sock"

// peerHTTP is a SHARED client for peer probes (latency + bare telemetry). Reusing
// one client (and its Transport connection pool) avoids allocating a fresh
// Transport per peer per /api/mesh poll — which leaked idle connections and piled
// up under console polling / many peers.
var peerHTTP = &http.Client{Timeout: 3 * time.Second}

// tsClient dials the tailscaled unix socket; the Host is a fixed sentinel the
// LocalAPI expects. Short timeout so /api/mesh never hangs on a stuck daemon.
var tsClient = &http.Client{
	Timeout: 3 * time.Second,
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", tsSocket)
		},
	},
}

func tsGet(pathq string, v any) error {
	req, err := http.NewRequest(http.MethodGet, "http://local-tailscaled.sock/localapi/v0/"+pathq, nil)
	if err != nil {
		return err
	}
	req.Host = "local-tailscaled.sock"
	req.Header.Set("Sec-Tailscale", "localapi")
	resp, err := tsClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// ---- LocalAPI response shapes (only the fields we use) ----

type tsPeer struct {
	DNSName      string
	OS           string
	TailscaleIPs []string
	Online       bool
}
type tsStatus struct {
	Version      string
	BackendState string
	TailscaleIPs []string
	Self         *tsPeer
	Peer         map[string]*tsPeer
}

// peerByIP finds the status entry that owns a tailnet IP (nil if none).
func (st *tsStatus) peerByIP(ip string) *tsPeer {
	for _, p := range st.Peer {
		for _, pip := range p.TailscaleIPs {
			if pip == ip {
				return p
			}
		}
	}
	return nil
}

type tsWhois struct {
	UserProfile *struct {
		LoginName   string
		DisplayName string
	}
	Node *struct{ ComputedName string }
}
type tsPing struct {
	LatencySeconds float64
	Err            string
}

// ---- presence: who has touched this box recently ----

type whoisEntry struct {
	login string // "" = resolved-to-nothing (negative), cached with a short TTL
	at    time.Time
}

const (
	whoisPosTTL = 5 * time.Minute  // positive result cache lifetime
	whoisNegTTL = 30 * time.Second // negative result: short, so it re-resolves soon
)

var (
	presMu        sync.Mutex
	presence      = map[string]time.Time{}  // login -> last seen
	whoisMemo     = map[string]whoisEntry{} // ip -> resolved login (positive OR negative)
	whoisInflight = map[string]bool{}       // ip -> a whois goroutine is already running
)

// recordPresence resolves the requester's identity and stamps it. Called from
// the request logger. Serve injects Tailscale-User-Login on the :443/:8443 path;
// direct :8787 hits carry the real tailnet IP in RemoteAddr → whois it.
//
// For uncached IPs the whois runs OFF the request hot path (goroutine) so a slow
// or retrying LocalAPI call never delays the response. Two safeguards keep that
// from stampeding the LocalAPI socket: (1) SINGLEFLIGHT — only one whois goroutine
// per IP at a time; (2) a short NEGATIVE cache so an unresolvable IP doesn't
// re-spawn a 9s goroutine on every single request.
func recordPresence(r *http.Request) {
	if login := strings.TrimSpace(r.Header.Get("Tailscale-User-Login")); login != "" {
		stampPresence(login)
		return
	}
	ip := remoteHost(r)
	if isLoopback(ip) {
		return // local/serve-proxied without header — nothing to attribute
	}
	if !isTailnetIP(ip) {
		return // only tailnet peers have a whois identity; skip LAN/scan traffic
		// (recordPresence runs in logReq BEFORE guard, so unauth requests reach here).
	}
	login, fresh := lookupWhois(ip)
	if login != "" {
		stampPresence(login)
		return
	}
	if fresh {
		return // fresh negative result cached — don't re-spawn a whois
	}
	if !claimWhois(ip) {
		return // another whois goroutine for this IP is already running
	}
	go func() {
		defer releaseWhois(ip)
		if login := whoisLogin(ip); login != "" {
			stampPresence(login)
		}
	}()
}

func stampPresence(login string) {
	presMu.Lock()
	presence[login] = time.Now()
	presMu.Unlock()
}

// lookupWhois returns (login, fresh). fresh==true means a still-valid cache entry
// exists (positive or negative), so the caller must NOT start a new whois. A
// positive entry lives whoisPosTTL; a negative one only whoisNegTTL.
func lookupWhois(ip string) (login string, fresh bool) {
	presMu.Lock()
	defer presMu.Unlock()
	m, ok := whoisMemo[ip]
	if !ok {
		return "", false
	}
	ttl := whoisPosTTL
	if m.login == "" {
		ttl = whoisNegTTL
	}
	if time.Since(m.at) < ttl {
		return m.login, true
	}
	return "", false
}

// claimWhois marks ip as being resolved; returns false if a resolve is already
// in flight (singleflight). releaseWhois clears it.
func claimWhois(ip string) bool {
	presMu.Lock()
	defer presMu.Unlock()
	if whoisInflight[ip] {
		return false
	}
	whoisInflight[ip] = true
	return true
}

func releaseWhois(ip string) {
	presMu.Lock()
	delete(whoisInflight, ip)
	presMu.Unlock()
}

// whoisLogin resolves a tailnet IP to a login via the tailscaled LocalAPI. whois
// can transiently fail or return empty (cold tailscaled, socket contention), so
// we retry a few times. Both positive AND negative results are cached (negative
// with a short TTL via lookupWhois) — caching the negative bounds re-resolution
// churn, while the short TTL means a transient cold-daemon miss self-heals fast
// (the old "cache empty forever" presence-blanking bug stays fixed).
func whoisLogin(ip string) string {
	login := ""
	for attempt := 0; attempt < 3 && login == ""; attempt++ {
		var w tsWhois
		if err := tsGet("whois?addr="+ip+":1", &w); err == nil {
			if w.UserProfile != nil && w.UserProfile.LoginName != "" {
				login = w.UserProfile.LoginName
			} else if w.Node != nil && w.Node.ComputedName != "" {
				login = w.Node.ComputedName
			}
		}
	}
	presMu.Lock()
	whoisMemo[ip] = whoisEntry{login: login, at: time.Now()}
	presMu.Unlock()
	return login
}

func activeUsers(window time.Duration) []string {
	presMu.Lock()
	defer presMu.Unlock()
	out := []string{}
	for login, at := range presence {
		if time.Since(at) <= window {
			out = append(out, login)
		} else {
			delete(presence, login)
		}
	}
	return out
}

// ---- host telemetry ----

func firstLine(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
}

func uptimeStr() string {
	f := strings.Fields(firstLine("/proc/uptime"))
	if len(f) == 0 {
		return ""
	}
	secs, _ := strconv.ParseFloat(f[0], 64)
	d := time.Duration(secs) * time.Second
	days := int(d.Hours()) / 24
	hrs := int(d.Hours()) % 24
	if days > 0 {
		return strconv.Itoa(days) + "d " + strconv.Itoa(hrs) + "h"
	}
	return strconv.Itoa(hrs) + "h " + strconv.Itoa(int(d.Minutes())%60) + "m"
}

func diskFreeTotal(path string) (free, total int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		log.Printf("statfs %s failed: %v", path, err) // loud: a real disk shouldn't read as 0/0
		return 0, 0
	}
	// statfs counts blocks in f_frsize units (what df uses); f_bsize is only the
	// preferred I/O size and can differ on some filesystems.
	return int64(st.Bavail) * int64(st.Frsize), int64(st.Blocks) * int64(st.Frsize)
}

// firstV4 returns a node's Tailscale CGNAT IPv4 (100.64.0.0/10). Peers carry both
// a v4 and a v6; some tailnet-internal entries are v6-only (route artifacts) — we
// key on the v4 and treat "no v4" as "not a real addressable device".
func firstV4(ips []string) string {
	for _, ip := range ips {
		if strings.HasPrefix(ip, "100.") {
			return ip
		}
	}
	return ""
}

func shortName(dns string) string {
	dns = strings.TrimSuffix(dns, ".")
	if i := strings.IndexByte(dns, '.'); i > 0 {
		return dns[:i]
	}
	return dns
}

// ---- the node payload ----

type meshNode struct {
	ID        string   `json:"id"`
	Role      string   `json:"role,omitempty"`
	TailnetIP string   `json:"tailnetIP,omitempty"`
	Online    bool     `json:"online"`
	OS        string   `json:"os,omitempty"`
	Kernel    string   `json:"kernel,omitempty"`
	Tailscale string   `json:"tailscale,omitempty"`
	Uptime    string   `json:"uptime,omitempty"`
	FreeBytes int64    `json:"freeBytes"`
	TotBytes  int64    `json:"totalBytes"`
	LatencyMs float64  `json:"latencyMs,omitempty"`
	Users     []string `json:"users"`
	Self      bool     `json:"self,omitempty"`
}

// selfNode assembles this box's real telemetry. st is the caller's LocalAPI
// status snapshot (nil when tailscaled was unreachable → env-name fallback).
func selfNode(st *tsStatus) meshNode {
	free, total := diskFreeTotal(incomingDir)
	n := meshNode{
		ID: nodeName, Role: env("DROPBRIDGE_ROLE", ""), Online: true, Self: true,
		OS: env("DROPBRIDGE_OS", "ZimaOS"), Kernel: firstLine("/proc/sys/kernel/osrelease"),
		Uptime: uptimeStr(), FreeBytes: free, TotBytes: total, Users: activeUsers(5 * time.Minute),
	}
	if st != nil && st.Self != nil {
		n.ID = shortName(st.Self.DNSName)
		n.TailnetIP = firstV4(st.Self.TailscaleIPs)
		n.Tailscale = strings.SplitN(st.Version, "-", 2)[0]
	}
	return n
}

func handleMesh(w http.ResponseWriter, r *http.Request) {
	// ONE LocalAPI status snapshot per poll, shared by self + every peer.
	var st tsStatus
	stp := &st
	out := map[string]any{"generated": time.Now().Unix(), "tailscaled": true}
	if err := tsGet("status", &st); err != nil {
		// Surface a dead local tailnet daemon instead of silently showing a
		// healthy-looking mesh (peers then fall back to HTTP-only probing).
		out["tailscaled"] = false
		out["tailscaleError"] = err.Error()
		stp = nil
	}
	out["self"] = selfNode(stp)

	cfgs := snapshotPeers()
	if r.URL.Query().Get("bare") == "1" || len(cfgs) == 0 {
		out["peers"] = []meshNode{}
		writeJSON(w, out)
		return
	}

	// enrich every configured peer concurrently so /api/mesh stays snappy even
	// with several (some offline) nodes.
	peers := make([]meshNode, len(cfgs))
	var wg sync.WaitGroup
	for i, c := range cfgs {
		wg.Add(1)
		go func(i int, c peerCfg) {
			defer wg.Done()
			peers[i] = enrichPeer(c, &st)
		}(i, c)
	}
	wg.Wait()
	out["peers"] = peers
	writeJSON(w, out)
}

// enrichPeer builds one peer's live telemetry from the shared status snapshot +
// its own /api/mesh?bare=1 + a latency probe.
func enrichPeer(c peerCfg, st *tsStatus) meshNode {
	peer := meshNode{ID: c.Name, TailnetIP: c.ID, Users: []string{}}
	if peer.ID == "" {
		peer.ID = c.ID
	}
	// online + real DNS name from LocalAPI status
	if p := st.peerByIP(c.ID); p != nil {
		peer.Online = p.Online
		if n := shortName(p.DNSName); n != "" {
			peer.ID = n
		}
	}
	// latency: prefer the LocalAPI disco ping (pure RTT); DERP-only links return
	// nothing → HTTP round-trip fallback below.
	if c.ID != "" {
		var pr tsPing
		if err := tsGet("ping?ip="+c.ID+"&type=disco", &pr); err == nil && pr.LatencySeconds > 0 {
			peer.LatencyMs = pr.LatencySeconds * 1000
		}
	}
	// enrich disk/uptime/version/presence from the peer's own /api/mesh?bare=1
	if enr := fetchPeerBare(c.URL); enr != nil {
		peer.Online = true
		if enr.ID != "" {
			peer.ID = enr.ID
		}
		peer.OS, peer.Kernel, peer.Tailscale, peer.Uptime = enr.OS, enr.Kernel, enr.Tailscale, enr.Uptime
		peer.FreeBytes, peer.TotBytes = enr.FreeBytes, enr.TotBytes
		if enr.Users != nil {
			peer.Users = enr.Users
		}
	}
	if peer.LatencyMs == 0 {
		peer.LatencyMs = pingPeerHTTP(c.URL)
	}
	return peer
}

// pingPeerHTTP times a minimal request to a peer's /api/config as an end-to-end
// latency proxy (ms) when the LocalAPI disco ping is unavailable.
func pingPeerHTTP(url string) float64 {
	req, err := http.NewRequest(http.MethodGet, url+"/api/config", nil)
	if err != nil {
		return 0
	}
	start := time.Now()
	resp, err := peerHTTP.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	return float64(time.Since(start).Microseconds()) / 1000.0
}

func fetchPeerBare(url string) *meshNode {
	// SECURITY: never attach the bearer token here. /api/mesh?bare=1 is
	// unauthenticated telemetry, and `url` derives from a user-supplied peer
	// entry — sending the token would leak it to whatever host was added as a
	// "peer" (see the peer-add tailnet-IP restriction in peers.go).
	req, err := http.NewRequest(http.MethodGet, url+"/api/mesh?bare=1", nil)
	if err != nil {
		return nil
	}
	resp, err := peerHTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Self meshNode `json:"self"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return nil
	}
	return &body.Self
}
