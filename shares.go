// shares.go — public share links for received files via tailscale funnel.
//
// A share maps an unguessable token to one received file. Only GET /s/{token}
// is exposed to the public internet (tailscale funnel is mounted path-scoped to
// /s → http://127.0.0.1:8787/s); the rest of the app stays tailnet-only. So a
// share hands out exactly one file to someone who is NOT on the tailnet, and
// nothing else is reachable. Shares persist to a dotfile in the incoming dir so
// they survive container recreputs; they are revocable and optionally expire.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
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
	b, err := os.ReadFile(sharesFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// A real read error (EACCES/EIO) is NOT "no shares yet" — surface it and
			// disable persistence so the next save can't overwrite the file with [].
			log.Printf("loadShares: cannot read %s: %v — persistence DISABLED (won't clobber)", sharesFile, err)
			sharesLoadOK = false
		}
		return // ErrNotExist → genuinely no shares yet, fine
	}
	var list []*share
	if err := json.Unmarshal(b, &list); err != nil {
		log.Printf("loadShares: %s is corrupt (%v) — shares NOT loaded, persistence DISABLED", sharesFile, err)
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

		b, err := json.MarshalIndent(list, "", "  ")
		if err != nil {
			log.Printf("saveShares: marshal failed: %v", err)
			continue
		}
		tmp := sharesFile + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			log.Printf("saveShares: write %s failed: %v", tmp, err)
			continue
		}
		if err := os.Rename(tmp, sharesFile); err != nil {
			log.Printf("saveShares: rename failed: %v", err)
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
	rel := safeRel(r.URL.Query().Get("name"))
	if rel == "" {
		httpErr(w, http.StatusBadRequest, "missing/invalid name")
		return
	}
	full := filepath.Join(incomingDir, filepath.FromSlash(rel))
	if info, err := os.Stat(full); err != nil || info.IsDir() || isInternal(full) {
		// isInternal: refuse to mint a PUBLIC funnel link for our own metadata
		// dotfiles — that would publish every share token to the open internet.
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

	sharesMu.Lock()
	defer sharesMu.Unlock()
	// reuse a still-valid share for this exact file
	for _, s := range shares {
		if s.Name == rel && !s.expired() {
			if ttl > 0 { // refresh expiry to the newly requested window
				s.Expires = time.Now().Unix() + ttl
				saveShares()
			}
			writeJSON(w, view(s))
			return
		}
	}
	tok, err := newToken()
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "token gen failed: "+err.Error())
		return
	}
	s := &share{Token: tok, Name: rel, Created: time.Now().Unix()}
	if ttl > 0 {
		s.Expires = time.Now().Unix() + ttl
	}
	shares[tok] = s
	saveShares()
	log.Printf("shared %q → %s", rel, s.url())
	writeJSON(w, view(s))
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
	if tok == "" || strings.ContainsAny(tok, "/.") {
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
	// defence in depth: re-sanitize the stored path before touching disk.
	rel := safeRel(name)
	if rel == "" {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(incomingDir, filepath.FromSlash(rel))
	info, err := os.Stat(full)
	if err != nil || info.IsDir() || isInternal(full) {
		http.NotFound(w, r) // deleted after sharing, or an internal metadata file
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
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(rel)))
	http.ServeFile(w, r, full)
}
