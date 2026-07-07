// DropBridge — a tiny web drop-zone that streams dropped files to a peer node
// over the tailnet. Runs identically on both ZimaOS boxes, each pointing at the
// other. No Taildrop size/one-shot limits: files are streamed peer-to-peer.
// Folder drops preserve their directory structure on the receiving side.
package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

//go:embed static/*
var staticFS embed.FS

// inFlight counts active file transfers (ingest in, send out). handleStorageSet
// consults it so a received-dir switch doesn't hard-kill the process mid-stream.
var inFlight int64

// minFreeBytes is the free-space floor: refuse to store below it so a flood of
// uploads can't fill the disk to 0 (which would break the OS / other apps).
const minFreeBytes = 64 << 20 // 64 MiB

// maxBytes optionally caps a single stored file (DROPBRIDGE_MAX_BYTES, 0 = off).
var maxBytes = func() int64 {
	n, _ := strconv.ParseInt(env("DROPBRIDGE_MAX_BYTES", "0"), 10, 64)
	if n < 0 {
		n = 0
	}
	return n
}()

var (
	nodeName    = env("DROPBRIDGE_NODE", "this-node")
	peerName    = env("DROPBRIDGE_PEER_NAME", "peer")
	peerURL     = strings.TrimRight(env("DROPBRIDGE_PEER_URL", ""), "/")
	token       = fileEnv("DROPBRIDGE_TOKEN", "")
	incomingDir = env("DROPBRIDGE_INCOMING", "/data/incoming")
	listenAddr  = env("DROPBRIDGE_LISTEN", ":8787")
	ntfyURL     = strings.TrimRight(env("DROPBRIDGE_NTFY_URL", ""), "/") // optional
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// fileEnv reads a secret from the file named by <KEY>_FILE (the Docker/systemd
// secret convention) when that env var is set — keeping the value OUT of the
// process environment and `docker inspect`. Falls back to the plain <KEY> env
// var for back-compat. Trailing whitespace/newline is trimmed.
func fileEnv(k, def string) string {
	if p := os.Getenv(k + "_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				return s
			}
			log.Printf("warning: %s_FILE=%s is empty — falling back to %s", k, p, k)
		} else {
			log.Printf("warning: cannot read %s_FILE=%s: %v — falling back to %s", k, p, err, k)
		}
	}
	return env(k, def)
}

func main() {
	os.MkdirAll(stateDir, 0o755)
	incomingDir = resolveIncoming() // persisted choice → largest prepared disk → fallback
	if err := os.MkdirAll(incomingDir, 0o755); err != nil {
		log.Fatalf("cannot create incoming dir %s: %v", incomingDir, err)
	}
	loadShares()
	go sharesPersister() // single off-path writer for shares persistence
	loadPeers()
	if token == "" {
		log.Printf("WARNING: no DROPBRIDGE_TOKEN(_FILE) set — peer ingest is DISABLED " +
			"(fail-closed) and the bearer auth factor is off. Set a token for 2-node use.")
	}
	mux := http.NewServeMux()

	// Embedded assets: a read failure here is a build/packaging bug — fail loud.
	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		log.Fatalf("embed static/index.html: %v", err)
	}
	icon, err := staticFS.ReadFile("static/icon.svg")
	if err != nil {
		log.Fatalf("embed static/icon.svg: %v", err)
	}
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	})
	mux.HandleFunc("GET /icon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(icon)
	})

	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		// Needed by the loopback healthcheck AND tailnet peer-probe, so it can't be
		// fully guard()ed — but don't leak node/peer names to a LAN client on the
		// host-exposed port. Allow loopback OR a tailnet-trusted caller only.
		if ip := net.ParseIP(remoteHost(r)); !(ip != nil && ip.IsLoopback()) && !authTailnet(r) {
			httpErr(w, http.StatusForbidden, "tailnet identity required")
			return
		}
		writeJSON(w, map[string]any{"node": nodeName, "peer": peerName, "peerConfigured": peerURL != ""})
	})
	// The control plane is guard()ed: reachable over the tailnet/LAN or an
	// authenticated `tailscale serve` session, but NEVER from a public funnel
	// client (see authTailnet). /api/config stays open (healthcheck + peer probe),
	// /api/ingest keeps its own stricter bearer-token check, /s is public.
	mux.HandleFunc("GET /api/mesh", guard(handleMesh))  // live node telemetry for the console
	mux.HandleFunc("POST /api/send", guard(handleSend)) // browser → here → peer
	mux.HandleFunc("POST /api/ingest", handleIngest)    // peer → here (writes to disk; token-gated)
	mux.HandleFunc("GET /api/received", guard(handleReceived))
	mux.HandleFunc("GET /api/download", guard(handleDownload))
	mux.HandleFunc("POST /api/delete", guard(handleDelete))
	mux.HandleFunc("POST /api/share", guard(handleShareCreate)) // tailnet-only: mint a public link
	mux.HandleFunc("GET /api/shares", guard(handleSharesList))
	mux.HandleFunc("POST /api/unshare", guard(handleUnshare))
	mux.HandleFunc("GET /s/", handlePublicShare)             // PUBLIC (funnel): serves one shared file
	mux.HandleFunc("GET /api/peers", guard(handlePeersList)) // configured DropBridge peers
	mux.HandleFunc("POST /api/peers", guard(handlePeerAdd))  // add a tailnet node (probed)
	mux.HandleFunc("POST /api/peers/remove", guard(handlePeerRemove))
	mux.HandleFunc("GET /api/tailnet", guard(handleTailnet))     // tailnet devices as add-candidates
	mux.HandleFunc("GET /api/storage", guard(handleStorageList)) // candidate data disks + current
	mux.HandleFunc("POST /api/storage", guard(handleStorageSet)) // repoint the received-files dir

	log.Printf("DropBridge %q → peer %q (%s), state=%s, incoming=%s, listen=%s, ntfy=%s, funnel=%s",
		nodeName, peerName, orNone(peerURL), stateDir, incomingDir, listenAddr, orNone(ntfyURL), orNone(funnelBase))
	// No Read/WriteTimeout: ingest + peer streaming legitimately run for a long
	// time on big files. ReadHeaderTimeout + IdleTimeout bound slow/idle clients
	// (the /s/ route is public via funnel) without capping active transfers.
	srv := &http.Server{Addr: listenAddr, Handler: logReq(mux), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 120 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// handleSend delivers every uploaded part either to the peer (target=peer,
// default) or to THIS box's own inbox (target=self). The part's filename may
// carry a relative subpath (folder drops) — preserved in both cases.
func handleSend(w http.ResponseWriter, r *http.Request) {
	tgt := r.URL.Query().Get("target")
	toSelf := tgt == "self"
	var destURL string
	if !toSelf {
		destURL = resolveTarget(tgt)
		if destURL == "" {
			httpErr(w, http.StatusServiceUnavailable, "no such peer (add a node first)")
			return
		}
	}
	mr, err := r.MultipartReader()
	if err != nil {
		httpErr(w, http.StatusBadRequest, "expected multipart/form-data: "+err.Error())
		return
	}
	atomic.AddInt64(&inFlight, 1)
	defer atomic.AddInt64(&inFlight, -1)
	type result struct {
		Name  string `json:"name"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	var results []result
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			httpErr(w, http.StatusBadRequest, "read part: "+err.Error())
			return
		}
		raw := rawFilename(part)
		if part.FormName() != "files" || raw == "" {
			continue
		}
		rel := safeRel(raw)
		res := result{Name: rel, OK: true}
		if rel == "" {
			res.OK, res.Error = false, "invalid filename"
		} else if toSelf {
			if _, _, err := storeLocal(rel, part); err != nil {
				res.OK, res.Error = false, err.Error()
				log.Printf("store %q locally failed: %v", rel, err)
			}
		} else if err := streamToPeer(rel, part, destURL); err != nil {
			res.OK, res.Error = false, err.Error()
			log.Printf("send %q → %s failed: %v", rel, destURL, err)
		}
		part.Close()
		results = append(results, res)
	}
	// Do NOT report a failed delivery as success. Surface it in the status code so
	// the browser can't mistake a dropped/failed transfer for "Delivered ✓":
	//   all failed → 502, some failed → 207 (partial), all ok → 200.
	ok := 0
	for _, r := range results {
		if r.OK {
			ok++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case len(results) > 0 && ok == 0:
		w.WriteHeader(http.StatusBadGateway)
	case ok < len(results):
		w.WriteHeader(http.StatusMultiStatus)
	}
	json.NewEncoder(w).Encode(map[string]any{"results": results, "ok": ok, "total": len(results)})
}

// resolveTarget maps a send target to a peer URL: "" / "peer" → the default
// (first) peer; otherwise a peer ID (tailnet IP). "" if none matches.
func resolveTarget(tgt string) string {
	ps := snapshotPeers()
	if tgt == "" || tgt == "peer" {
		if len(ps) > 0 {
			return ps[0].URL
		}
		return ""
	}
	if p, ok := peerByID(tgt); ok {
		return p.URL
	}
	return ""
}

func streamToPeer(rel string, body io.Reader, dest string) error {
	req, err := http.NewRequest(http.MethodPost, dest+"/api/ingest", body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Filename", rel)
	req.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 6 * time.Hour}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("peer returned %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

func handleIngest(w http.ResponseWriter, r *http.Request) {
	// Fail closed: with no configured token there is no way to authenticate a
	// peer, so an empty token must DISABLE ingest rather than accept everyone
	// (the port is exposed on the host — an open ingest = anyone can write files).
	if token == "" {
		httpErr(w, http.StatusServiceUnavailable, "ingest disabled: no DROPBRIDGE_TOKEN configured")
		return
	}
	if !authOK(r) {
		httpErr(w, http.StatusUnauthorized, "bad or missing token")
		return
	}
	rel := safeRel(r.Header.Get("X-Filename"))
	if rel == "" {
		httpErr(w, http.StatusBadRequest, "missing/invalid X-Filename")
		return
	}
	atomic.AddInt64(&inFlight, 1)
	defer atomic.AddInt64(&inFlight, -1)
	stored, n, err := storeLocal(rel, r.Body)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("received %s (%d bytes)", stored, n)
	notify(stored, n)
	writeJSON(w, map[string]any{"ok": true, "stored": stored, "bytes": n})
}

// storeLocal writes a stream into the incoming dir under a (sanitized) relative
// path, creating subdirectories and never overwriting (conflict → -N suffix).
func storeLocal(rel string, body io.Reader) (stored string, n int64, err error) {
	// Disk-fill guard: refuse when the target disk is critically low, so an upload
	// flood can't drive free space to zero.
	if free, _ := diskFreeTotal(incomingDir); free > 0 && free < minFreeBytes {
		return "", 0, fmt.Errorf("insufficient disk space: only %s free", humanBytes(free))
	}
	full := filepath.Join(incomingDir, filepath.FromSlash(rel))
	if err = os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", 0, fmt.Errorf("mkdir: %w", err)
	}
	// uniquePath + O_EXCL is racy under concurrent same-name uploads: two callers
	// can pick the same free name and one loses with EEXIST. Retry on collision so
	// concurrent uploads get disambiguated (-N) instead of a spurious 500.
	var dst string
	var f *os.File
	for tries := 0; ; tries++ {
		dst = uniquePath(full)
		f, err = os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) || tries >= 50 {
			return "", 0, fmt.Errorf("create: %w", err)
		}
	}
	// Optional per-file cap: read one byte past the limit to detect an overflow.
	src := body
	if maxBytes > 0 {
		src = io.LimitReader(body, maxBytes+1)
	}
	n, err = io.Copy(f, src)
	cerr := f.Close()
	if err != nil {
		os.Remove(dst)
		return "", 0, fmt.Errorf("write: %w", err)
	}
	if maxBytes > 0 && n > maxBytes {
		os.Remove(dst)
		return "", 0, fmt.Errorf("file exceeds limit of %s", humanBytes(maxBytes))
	}
	if cerr != nil {
		return "", 0, fmt.Errorf("close: %w", cerr)
	}
	rp, _ := filepath.Rel(incomingDir, dst)
	return filepath.ToSlash(rp), n, nil
}

func handleReceived(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Name  string `json:"name"`
		Bytes int64  `json:"bytes"`
		MTime int64  `json:"mtime"`
	}
	items := []item{}
	walkErr := filepath.WalkDir(incomingDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err // surface a real walk failure instead of silently returning []
		}
		if d.IsDir() || isInternal(p) {
			return nil // internal metadata, not a received file
		}
		info, err := d.Info()
		if err != nil {
			log.Printf("received: stat %s failed: %v", p, err) // loud, not swallowed
			return nil
		}
		rel, _ := filepath.Rel(incomingDir, p)
		items = append(items, item{Name: filepath.ToSlash(rel), Bytes: info.Size(), MTime: info.ModTime().Unix()})
		return nil
	})
	if walkErr != nil {
		httpErr(w, http.StatusInternalServerError, "cannot read incoming dir: "+walkErr.Error())
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].MTime > items[j].MTime })
	writeJSON(w, map[string]any{"items": items})
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	rel := safeRel(r.URL.Query().Get("name"))
	if rel == "" {
		httpErr(w, http.StatusBadRequest, "missing/invalid name")
		return
	}
	full := filepath.Join(incomingDir, filepath.FromSlash(rel))
	info, err := os.Stat(full)
	if err != nil || info.IsDir() || isInternal(full) {
		httpErr(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(rel)))
	http.ServeFile(w, r, full)
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	rel := safeRel(r.URL.Query().Get("name"))
	if rel == "" {
		httpErr(w, http.StatusBadRequest, "missing/invalid name")
		return
	}
	full := filepath.Join(incomingDir, filepath.FromSlash(rel))
	info, err := os.Stat(full)
	if err != nil || info.IsDir() || isInternal(full) {
		httpErr(w, http.StatusNotFound, "not found")
		return
	}
	if err := os.Remove(full); err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("deleted %s", rel)
	writeJSON(w, map[string]any{"ok": true, "deleted": rel})
}

// notify fires a best-effort ntfy push when a file is received.
func notify(name string, size int64) {
	if ntfyURL == "" {
		return
	}
	go func() {
		req, err := http.NewRequest(http.MethodPost, ntfyURL, strings.NewReader(
			fmt.Sprintf("Received %s (%s) from %s", name, humanBytes(size), peerName)))
		if err != nil {
			return
		}
		req.Header.Set("Title", "DropBridge · "+nodeName)
		req.Header.Set("Tags", "inbox_tray")
		c := &http.Client{Timeout: 10 * time.Second}
		if resp, err := c.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
}

// ---- helpers ----

func authOK(r *http.Request) bool {
	if token == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// authTailnet gates the control plane. Trusted when the request (a) carries the
// bearer token, (b) arrives over loopback carrying a `tailscale serve` identity
// header (an authenticated tailnet user — serve proxies to 127.0.0.1 and injects
// the header host-side), or (c) originates directly from a tailnet CGNAT source
// IP. Everything else is rejected — notably a public `tailscale funnel` client
// (non-tailnet source, header stripped by Tailscale) AND a LAN client hitting the
// host-exposed :8787 directly.
//
// SECURITY: the `Tailscale-User-Login` header is only trustworthy when it was
// injected by the local serve proxy, i.e. the connection is from loopback. The
// app port is bound on the host (network_mode: host), so a LAN/other-container
// client could otherwise just SET that header themselves and pass the guard.
// Trusting it only on a loopback RemoteAddr closes that spoof.
// remoteHost is the bare host portion of RemoteAddr (no port). Never trust
// X-Forwarded-* here — RemoteAddr is the real transport peer.
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func authTailnet(r *http.Request) bool {
	if token != "" && authOK(r) {
		return true // valid bearer token (peers / automation)
	}
	host := remoteHost(r)
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() &&
		r.Header.Get("Tailscale-User-Login") != "" {
		return true // authenticated tailnet user via local `tailscale serve`
	}
	return isTailnetIP(host) // direct connection from a tailnet CGNAT peer
}

// guard wraps a handler so only authTailnet-trusted callers reach it.
func guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authTailnet(r) {
			httpErr(w, http.StatusForbidden, "tailnet identity required")
			return
		}
		h(w, r)
	}
}

// rawFilename returns the multipart filename WITHOUT Go's filepath.Base()
// stripping (which Part.FileName() applies) — so folder subpaths survive.
func rawFilename(p *multipart.Part) string {
	cd := p.Header.Get("Content-Disposition")
	if cd == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(cd)
	if err != nil {
		return ""
	}
	return params["filename"]
}

// safeRel cleans a client-supplied relative path: forward slashes, no leading
// slash, no ".." escape, no empty. Returns "" if unsafe. Keeps subdirectories.
func safeRel(n string) string {
	n = strings.TrimSpace(n)
	n = strings.ReplaceAll(n, "\\", "/")
	n = path.Clean("/" + n) // absolutize then clean → collapses .. that would escape
	n = strings.TrimPrefix(n, "/")
	if n == "" || n == "." || strings.HasPrefix(n, "../") || n == ".." {
		return ""
	}
	// reject any residual traversal or absolute
	for _, seg := range strings.Split(n, "/") {
		if seg == ".." {
			return ""
		}
	}
	return n
}

func uniquePath(p string) string {
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		cand := base + "-" + strconv.Itoa(i) + ext
		if _, err := os.Stat(cand); errors.Is(err, os.ErrNotExist) {
			return cand
		}
	}
}

func humanBytes(n int64) string {
	const u = "KMGT"
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	i := -1
	for f >= 1024 && i < len(u)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %cB", f, u[i])
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

func logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recordPresence(r)
		next.ServeHTTP(w, r)
		if r.URL.Path != "/api/received" {
			log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}
