// peers.go — the multi-node model. DropBridge started life 2-node (one
// DROPBRIDGE_PEER_URL); this lets the user add more DropBridge boxes from their
// tailnet at runtime. A peer is another box running this same container with the
// SAME bearer token — nothing else can receive a stream, so "add node" probes
// the candidate's /api/config and refuses anything that isn't DropBridge.
//
// The ordered peer list persists to a dotfile in stateDir (survives recreate
// when /state is bind-mounted). peersList[0] is the default send target, so the
// old single-peer behaviour is just "one entry in the list", seeded from
// DROPBRIDGE_PEER_URL.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var errNotDropBridge = errors.New("no DropBridge /api/config response")

type peerCfg struct {
	ID    string `json:"id"`   // tailnet IP — stable key
	Name  string `json:"name"` // display name (remote node name or MagicDNS short)
	URL   string `json:"url"`  // http://<ip>:8787
	Added int64  `json:"added"`
}

var (
	peersMu   sync.Mutex
	peersList []peerCfg
	peersFile = filepath.Join(stateDir, ".dropbridge-peers.json")
	// peersLoadOK is false when the file existed but couldn't be read/parsed — then
	// we must not persist, or a save would clobber recoverable state.
	peersLoadOK = true
)

func loadPeers() {
	var list []peerCfg
	if _, err := readJSONFile(peersFile, &list); err != nil {
		// A real read/parse error is NOT "no peers yet": disable persistence and
		// don't seed/save, so we can't overwrite a recoverable peers file.
		log.Printf("loadPeers: %v — peers NOT loaded, persistence DISABLED (won't clobber)", err)
		peersLoadOK = false
		return
	}
	peersMu.Lock()
	defer peersMu.Unlock()
	peersList = list
	// back-compat: seed from the single-peer env if the list is still empty. Parse
	// with net/url — the same parser net/http dials with (see handlePeerAdd).
	if len(peersList) == 0 && peerURL != "" {
		u, err := url.Parse(peerURL)
		if err != nil || u.Hostname() == "" {
			log.Printf("loadPeers: DROPBRIDGE_PEER_URL %q is not a valid URL — ignored", peerURL)
			return
		}
		peersList = []peerCfg{{ID: u.Hostname(), Name: peerName, URL: peerURL, Added: time.Now().Unix()}}
		savePeersLocked()
	}
}

// savePeersLocked persists atomically. Caller holds peersMu.
func savePeersLocked() {
	if !peersLoadOK {
		return // never overwrite a peers file we failed to read
	}
	if err := writeJSONAtomic(peersFile, peersList); err != nil {
		log.Printf("savePeersLocked: %v", err)
	}
}

// isTailnetIP reports whether host is a Tailscale CGNAT IPv4 (100.64.0.0/10).
func isTailnetIP(host string) bool {
	ip4 := net.ParseIP(host).To4()
	return ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127
}

func peerByID(id string) (peerCfg, bool) {
	peersMu.Lock()
	defer peersMu.Unlock()
	for _, p := range peersList {
		if p.ID == id {
			return p, true
		}
	}
	return peerCfg{}, false
}

func snapshotPeers() []peerCfg {
	peersMu.Lock()
	defer peersMu.Unlock()
	out := make([]peerCfg, len(peersList))
	copy(out, peersList)
	return out
}

// selfTailnetIP returns this box's own tailnet IP (to exclude it from add
// candidates and reject "add self").
func selfTailnetIP() string {
	var st tsStatus
	if err := tsGet("status", &st); err == nil && st.Self != nil {
		return firstV4(st.Self.TailscaleIPs)
	}
	return ""
}

// GET /api/peers — the configured DropBridge peers, in order (first = default).
func handlePeersList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"peers": snapshotPeers()})
}

// POST /api/peers?ip=<tailnet-ip>  (or ?url=http://host:port) — probe the
// candidate's /api/config; add only if it's really DropBridge.
func handlePeerAdd(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("url"))
	if raw == "" {
		if ip := strings.TrimSpace(r.URL.Query().Get("ip")); ip != "" {
			raw = "http://" + ip + ":8787"
		}
	}
	raw = strings.TrimRight(raw, "/")
	// SECURITY: parse with net/url and derive the host with u.Hostname() — the SAME
	// value net/http will dial. The old hand-rolled hostFromURL parsed differently
	// from net/http, so a crafted authority (e.g. userinfo with a bare tailnet IP)
	// could pass the tailnet check here yet make the client CONNECT elsewhere,
	// leaking the bearer token. Rejecting userinfo + reusing u.Hostname() closes it.
	u, perr := url.Parse(raw)
	if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		httpErr(w, http.StatusBadRequest, "missing/invalid ip or url")
		return
	}
	if u.User != nil {
		httpErr(w, http.StatusBadRequest, "url must not contain credentials")
		return
	}
	id := u.Hostname()
	if id == "" {
		httpErr(w, http.StatusBadRequest, "cannot parse host from url")
		return
	}
	// Normalized dial/storage URL: scheme + host[:port] only — no path/query/userinfo.
	dialURL := u.Scheme + "://" + u.Host
	// SECURITY: only tailnet CGNAT IPs may become peers. This stops SSRF (probing
	// internal/metadata hosts) and, crucially, ensures the bearer token + streamed
	// files can only ever leave to an actual tailnet member on /api/ingest —
	// never to an arbitrary attacker-supplied host.
	if !isTailnetIP(id) {
		httpErr(w, http.StatusBadRequest, "peer must be a tailnet IP (100.64.0.0/10)")
		return
	}
	if id == selfTailnetIP() {
		httpErr(w, http.StatusBadRequest, "that's this box")
		return
	}
	if _, exists := peerByID(id); exists {
		httpErr(w, http.StatusConflict, "already added")
		return
	}
	// probe: must answer /api/config like a DropBridge node.
	name, err := probeDropBridge(dialURL)
	if err != nil {
		httpErr(w, http.StatusBadGateway, "not a DropBridge node at "+dialURL+" ("+err.Error()+") — is the container running there with the same token?")
		return
	}
	p := peerCfg{ID: id, Name: name, URL: dialURL, Added: time.Now().Unix()}
	peersMu.Lock()
	// Re-check under the lock: the existence check above released peersMu before a
	// multi-second probe, so two concurrent adds of the same IP could both reach
	// here and append a duplicate. Re-scan before committing.
	for _, ex := range peersList {
		if ex.ID == id {
			peersMu.Unlock()
			httpErr(w, http.StatusConflict, "already added")
			return
		}
	}
	peersList = append(peersList, p)
	savePeersLocked()
	peersMu.Unlock()
	log.Printf("peer added: %s (%s)", name, dialURL)
	writeJSON(w, p)
}

// POST /api/peers/remove?id=<tailnet-ip>
func handlePeerRemove(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	peersMu.Lock()
	kept := peersList[:0:0]
	found := false
	for _, p := range peersList {
		if p.ID == id {
			found = true
			continue
		}
		kept = append(kept, p)
	}
	if found {
		peersList = kept
		savePeersLocked()
	}
	peersMu.Unlock()
	if !found {
		httpErr(w, http.StatusNotFound, "no such peer")
		return
	}
	log.Printf("peer removed: %s", id)
	writeJSON(w, map[string]any{"ok": true, "removed": id})
}

// probeDropBridge hits <url>/api/config and returns the remote node name if it
// looks like DropBridge; error otherwise.
func probeDropBridge(url string) (string, error) {
	resp, err := peerHTTP.Get(url + "/api/config")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var cfg struct {
		Node           string `json:"node"`
		PeerConfigured *bool  `json:"peerConfigured"`
	}
	if json.NewDecoder(resp.Body).Decode(&cfg) != nil || cfg.PeerConfigured == nil {
		return "", errNotDropBridge
	}
	if cfg.Node == "" {
		return "unknown", nil
	}
	return cfg.Node, nil
}

// GET /api/tailnet — every tailnet device (from LocalAPI status) as an add
// candidate, flagged if already a peer / is this box.
func handleTailnet(w http.ResponseWriter, r *http.Request) {
	type dev struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		IP     string `json:"ip"`
		OS     string `json:"os,omitempty"`
		Online bool   `json:"online"`
		Added  bool   `json:"added"`
		Self   bool   `json:"self,omitempty"`
	}
	var st tsStatus
	if err := tsGet("status", &st); err != nil {
		httpErr(w, http.StatusServiceUnavailable, "tailscaled unreachable: "+err.Error())
		return
	}
	added := map[string]bool{}
	for _, p := range snapshotPeers() {
		added[p.ID] = true
	}
	selfIP := ""
	if st.Self != nil {
		selfIP = firstV4(st.Self.TailscaleIPs)
	}
	out := []dev{}
	for _, p := range st.Peer {
		ip := firstV4(p.TailscaleIPs)
		if ip == "" {
			continue // v6-only tailnet artifact, not an addressable device
		}
		name := shortName(p.DNSName)
		if name == "" {
			name = ip
		}
		out = append(out, dev{
			ID: ip, Name: name, IP: ip, OS: p.OS,
			Online: p.Online, Added: added[ip], Self: ip == selfIP,
		})
	}
	// st.Peer is a map → random order per call; sort so the picker doesn't
	// reshuffle every time it opens (online first, then by name).
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, map[string]any{"devices": out})
}
