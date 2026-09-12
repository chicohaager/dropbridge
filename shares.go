// shares.go — public share links for received files via tailscale funnel.
//
// A share maps an unguessable token to one received file. Only GET /s/{token}
// is exposed to the public internet (tailscale funnel is mounted path-scoped to
// /s → http://127.0.0.1:8787/s); the rest of the app stays tailnet-only. So a
// share hands out exactly one file to someone who is NOT on the tailnet, and
// nothing else is reachable. Shares persist to a dotfile in stateDir so they
// survive container recreates; they are revocable and optionally expire.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// funnelBase is this box's public funnel origin, e.g.
// https://<node>.<your-tailnet>.ts.net:10000 — set per box in .env. Empty
// means funnel isn't wired up here; share creation then reports it clearly
// instead of handing out a link that only works inside the tailnet.
var funnelBase = strings.TrimRight(env("DROPBRIDGE_FUNNEL_BASE", ""), "/")

type share struct {
	Token     string `json:"token"`
	Name      string `json:"name"` // relative path inside incomingDir
	Created   int64  `json:"created"`
	Expires   int64  `json:"expires,omitempty"` // unix secs; 0 = never
	Downloads int64  `json:"downloads"`
}

var (
	sharesMu   sync.Mutex
	shares     = map[string]*share{} // token -> share
	sharesFile = filepath.Join(stateDir, ".dropbridge-shares.json")
	// sharesLoadOK is false when the file existed but could not be read/parsed —
	// then we must NOT persist, or we would clobber recoverable state with an
	// empty map. sharesSavePing decouples disk I/O from the request path.
	sharesLoadOK   = true
	sharesSavePing = make(chan struct{}, 1)
)

func loadShares() {
	var list []*share
	if _, err := readJSONFile(sharesFile, &list); err != nil {
		// A real read/parse error (EACCES/EIO/corrupt) is NOT "no shares yet" —
		// surface it and disable persistence so the next save can't overwrite the
		// file with [].
		log.Printf("loadShares: %v — shares NOT loaded, persistence DISABLED (won't clobber)", err)
		sharesLoadOK = false
		return
	}
	sharesMu.Lock()
	for _, s := range list {
		if s != nil && s.Token != "" {
			shares[s.Token] = s
		}
	}
	sharesMu.Unlock()
}

// saveShares requests a persist. It is a non-blocking, coalescing PING — callers
// may (and do) hold sharesMu, so it must never touch the disk itself. The actual
// atomic write happens on sharesPersister(), off the request/lock hot path (a
// public /s/ download flood then can't stall the tailnet control plane).
func saveShares() {
	select {
	case sharesSavePing <- struct{}{}:
	default: // a save is already pending; it will capture the latest state
	}
}

// sharesPersister is the single writer. Started once from main().
func sharesPersister() {
	for range sharesSavePing {
		if !sharesLoadOK {
			continue // never overwrite a file we failed to read
		}
		sharesMu.Lock()
		list := make([]*share, 0, len(shares))
		for _, s := range shares {
			cp := *s // deep-enough copy (scalar fields) so I/O runs lock-free
			list = append(list, &cp)
		}
		sharesMu.Unlock()
		if err := writeJSONAtomic(sharesFile, list); err != nil {
			log.Printf("saveShares: %v", err)
		}
	}
}

func newToken() (string, error) {
	var b [16]byte // 128-bit, unguessable
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *share) expired() bool { return s.Expires != 0 && time.Now().Unix() > s.Expires }

func (s *share) url() string {
	if funnelBase == "" {
		return "/s/" + s.Token // relative fallback; not publicly reachable
	}
	return funnelBase + "/s/" + s.Token
}

// publicView is the shape the UI consumes (adds the resolved URL + public flag).
type publicView struct {
	*share
	URL    string `json:"url"`
	Public bool   `json:"public"`
}

// isInternal reports whether a resolved path is one of our own metadata dotfiles,
// which must never be listed, downloaded, deleted, or publicly shared.
// Match ANY ".dropbridge-*" basename (shares/peers/settings + their ".tmp"
// siblings), not just an exact path: after the storage refactor the live state
// moved to stateDir, but a same-named leftover can sit in incomingDir — an
// exact-path check would miss it and let it be shared to the open internet.
func isInternal(full string) bool {
	return strings.HasPrefix(filepath.Base(full), ".dropbridge-")
}

func view(s *share) publicView {
	cp := *s // snapshot: never let a shared *share escape sharesMu (races Downloads++)
	return publicView{share: &cp, URL: s.url(), Public: funnelBase != ""}
}

// POST /api/share?name=<rel>&ttl=<secs>  (tailnet-only) → create/return a share.
// Reuses an existing live share for the same file so repeated clicks are stable.
func handleShareCreate(w http.ResponseWriter, r *http.Request) {
	// receivedFile also refuses our metadata dotfiles: minting a PUBLIC funnel
	// link for those would publish every share token to the open internet.
	rel, _, ok := receivedFile(r.URL.Query().Get("name"))
	if !ok {
		httpErr(w, http.StatusNotFound, "file not found")
		return
	}
	var ttl int64
	if q := r.URL.Query().Get("ttl"); q != "" {
		ttl, _ = strconv.ParseInt(q, 10, 64)
		if ttl < 0 {
			ttl = 0
		}
	}
	// Token generation happens before the lock: crypto/rand can block, and a
	// spare token on the reuse path costs nothing.
	tok, err := newToken()
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "token gen failed: "+err.Error())
		return
	}
	now := time.Now().Unix()

	// Build the response under the lock, but WRITE it after unlocking — a slow
	// client must not stall every other share op (incl. public /s/ downloads).
	sharesMu.Lock()
	var out publicView
	created := false
	for _, s := range shares { // reuse a still-valid share for this exact file
		if s.Name == rel && !s.expired() {
			if ttl > 0 { // refresh expiry to the newly requested window
				s.Expires = now + ttl
				saveShares()
			}
			out = view(s)
			break
		}
	}
	if out.share == nil {
		s := &share{Token: tok, Name: rel, Created: now}
		if ttl > 0 {
			s.Expires = now + ttl
		}
		shares[tok] = s
		saveShares()
		out, created = view(s), true
	}
	sharesMu.Unlock()
	if created {
		log.Printf("shared %q → %s", rel, out.URL)
	}
	writeJSON(w, out)
}

// GET /api/shares  (tailnet-only) → all live shares, keyed for UI lookup.
func handleSharesList(w http.ResponseWriter, r *http.Request) {
	sharesMu.Lock()
	changed := false
	out := []publicView{}
	for tok, s := range shares {
		if s.expired() {
			delete(shares, tok)
			changed = true
			continue
		}
		out = append(out, view(s))
	}
	if changed {
		saveShares()
	}
	sharesMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	writeJSON(w, map[string]any{"shares": out, "funnel": funnelBase != ""})
}

// POST /api/unshare?token=<tok>  (tailnet-only) → revoke.
func handleUnshare(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	sharesMu.Lock()
	_, ok := shares[tok]
	if ok {
		delete(shares, tok)
		saveShares()
	}
	sharesMu.Unlock()
	if !ok {
		httpErr(w, http.StatusNotFound, "no such share")
		return
	}
	log.Printf("unshared %s", tok)
	writeJSON(w, map[string]any{"ok": true, "revoked": tok})
}

// GET /s/{token} — the ONLY public (funnel-exposed) route. Streams the one
// shared file. Unknown/expired token → generic 404 (no enumeration leak).
func handlePublicShare(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.URL.Path, "/s/")
	if !validToken(tok) {
		http.NotFound(w, r)
		return
	}
	sharesMu.Lock()
	s := shares[tok]
	if s != nil && s.expired() {
		delete(shares, tok)
		saveShares()
		s = nil
	}
	name := ""
	if s != nil {
		name = s.Name
	}
	sharesMu.Unlock()
	if s == nil {
		http.NotFound(w, r)
		return
	}
	// defence in depth: re-resolve the stored path (re-sanitized, must still be a
	// regular non-internal file) before touching disk — it may have been deleted
	// after sharing.
	rel, full, ok := receivedFile(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Count only downloads we actually serve — not 404 probes — so a flood of bad
	// requests on the public route can't force a whole-file rewrite per hit.
	sharesMu.Lock()
	if cur := shares[tok]; cur != nil {
		cur.Downloads++
		saveShares()
	}
	sharesMu.Unlock()
	serveAttachment(w, r, rel, full)
}

// validToken accepts exactly the shape newToken mints: 32 lowercase hex chars.
// Anything else 404s before a map lookup — no path games, no enumeration hints.
func validToken(tok string) bool {
	if len(tok) != 32 {
		return false
	}
	for i := 0; i < len(tok); i++ {
		if c := tok[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
