// storage.go — user-selectable received-files folder.
//
// The received-files dir is DYNAMIC: resolved at startup from a persisted choice,
// else the largest usable data disk under /media, else a legacy fallback. State
// (peers/shares/settings) lives in a separate FIXED stateDir so the received dir
// can be repointed without losing mesh config.
//
// The container runs non-root (uid 1000) and cannot create folders on the
// root-owned /media disk roots. A root setup step (rollout / install.sh) pre-makes
// and chowns "<disk>/DropBridge" on each usable disk — "prepared" disks. The app
// only ever writes inside those, which also bounds where received files can land.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// stateDir holds peers.json / shares.json / settings.json — never moves.
var stateDir = env("DROPBRIDGE_STATE", "/state")

const (
	mediaRoot = "/media"     // where ZimaOS mounts data disks
	dbSub     = "DropBridge" // per-disk dir the root setup prepares (chown uid)
)

type settings struct {
	Incoming string `json:"incoming"`
}

var settingsFile = filepath.Join(stateDir, ".dropbridge-settings.json")

func loadSettings() (s settings) {
	if _, err := readJSONFile(settingsFile, &s); err != nil {
		log.Printf("loadSettings: %v — using disk scan / env fallback", err)
	}
	return
}

func saveSettings(s settings) error { return writeJSONAtomic(settingsFile, s) }

// readJSONFile loads a JSON state file. found=false (nil error) when the file
// simply doesn't exist yet; any other read or parse failure is returned so the
// caller can decide NOT to persist over a recoverable file.
func readJSONFile(p string, v any) (found bool, err error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot read %s: %w", p, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return true, fmt.Errorf("%s is corrupt: %w", p, err)
	}
	return true, nil
}

// writeJSONAtomic persists v as indented JSON via tmp-file + rename, so a crash
// mid-write can never leave a truncated state file behind.
func writeJSONAtomic(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", p, err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("rename %s: %w", p, err)
	}
	return nil
}

// storageDisk is one candidate data disk under /media.
type storageDisk struct {
	Path       string `json:"path"`  // received dir: /media/<x>/DropBridge/incoming
	Disk       string `json:"disk"`  // mount point: /media/<x>
	Label      string `json:"label"` // <x>
	TotalBytes int64  `json:"totalBytes"`
	FreeBytes  int64  `json:"freeBytes"`
	Prepared   bool   `json:"prepared"` // <disk>/DropBridge writable by us (setup ran)
	Current    bool   `json:"current"`
}

var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "tmpfs": true, "devtmpfs": true, "overlay": true,
	"squashfs": true, "cgroup": true, "cgroup2": true, "mqueue": true, "devpts": true,
	"ramfs": true, "autofs": true, "debugfs": true, "tracefs": true, "bpf": true,
}

// scanDisks enumerates top-level data disks under /media, skipping eMMC, pseudo
// filesystems and fuse/cloud mounts. "Prepared" marks disks the non-root app can
// actually receive into (root setup has run).
func scanDisks() []storageDisk {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()

	cur := incomingDir
	seen := map[string]bool{}
	var out []storageDisk
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		pre := strings.Fields(line[:sep])
		post := strings.Fields(line[sep+3:])
		if len(pre) < 5 || len(post) < 2 {
			continue
		}
		mp := unescapeOctal(pre[4])
		fstype, source := post[0], post[1]
		if !strings.HasPrefix(mp, mediaRoot+"/") || seen[mp] {
			continue
		}
		if strings.Count(mp, "/") != 2 { // only /media/<disk>, not nested mounts
			continue
		}
		if pseudoFS[fstype] || strings.HasPrefix(fstype, "fuse") || strings.Contains(source, "mmcblk") {
			continue // pseudo / cloud-fuse / eMMC
		}
		free, total := diskFreeTotal(mp)
		if total == 0 {
			continue
		}
		seen[mp] = true
		recv := filepath.Join(mp, dbSub, "incoming")
		out = append(out, storageDisk{
			Path: recv, Disk: mp, Label: filepath.Base(mp),
			TotalBytes: total, FreeBytes: free,
			Prepared: dirWritable(filepath.Join(mp, dbSub)),
			Current:  cur == recv,
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].TotalBytes > out[b].TotalBytes })
	return out
}

// dirWritable reports whether dir exists and the running uid can create inside it.
func dirWritable(dir string) bool {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return false
	}
	probe := filepath.Join(dir, ".dropbridge-wtest")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return false
	}
	os.Remove(probe)
	return true
}

// unescapeOctal decodes mountinfo's \0xx escaping of space/tab/newline/backslash.
func unescapeOctal(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// resolveIncoming picks the received-files dir at startup: persisted choice (if
// still writable) → largest prepared disk → legacy env fallback.
func resolveIncoming() string {
	if s := loadSettings(); s.Incoming != "" && ensureIncoming(s.Incoming) {
		return s.Incoming
	}
	for _, d := range scanDisks() {
		if d.Prepared && ensureIncoming(d.Path) {
			if err := saveSettings(settings{Incoming: d.Path}); err != nil {
				log.Printf("resolveIncoming: %v — choice will be re-derived on next start", err)
			}
			return d.Path
		}
	}
	return env("DROPBRIDGE_INCOMING", "/data/incoming")
}

// ensureIncoming creates the incoming dir (parent must be prepared/writable) and
// confirms we can write to it.
func ensureIncoming(p string) bool {
	if err := os.MkdirAll(p, 0o755); err != nil {
		return false
	}
	return dirWritable(p)
}

// GET /api/storage — list candidate disks + the current received dir. guard()ed.
func handleStorageList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"current": incomingDir, "disks": scanDisks()})
}

// POST /api/storage {path} — set the received dir. Validates it's under /media,
// no traversal, and writable; persists, then restarts to apply (the container's
// restart policy brings it back with the new dir).
func handleStorageSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		httpErr(w, http.StatusBadRequest, "path required")
		return
	}
	clean := filepath.Clean(body.Path)
	// Must be a real, PREPARED candidate disk from scanDisks() — which already
	// excludes eMMC, pseudo and fuse/cloud filesystems. An "under /media + writable"
	// check alone would accept e.g. a DropBridge dir on the eMMC, defeating the
	// native-data-disk guarantee the listing enforces.
	prepared := false
	for _, d := range scanDisks() {
		if d.Path == clean && d.Prepared {
			prepared = true
			break
		}
	}
	if !prepared {
		httpErr(w, http.StatusBadRequest, "not a prepared data disk — pick one from the storage list")
		return
	}
	// No-op if it's already the current folder: don't restart. This removes the
	// "POST the same path to force a restart" primitive and avoids pointless churn.
	if clean == incomingDir {
		writeJSON(w, map[string]any{"ok": true, "incoming": clean, "restarting": false, "note": "already the current folder"})
		return
	}
	if !ensureIncoming(clean) {
		httpErr(w, http.StatusBadRequest, "folder not writable — run the disk setup for this disk first")
		return
	}
	// Don't sever an active transfer: refuse while one is streaming (the client can
	// retry in a moment). Combined with the drain loop below, this avoids the hard
	// os.Exit corrupting an in-flight ingest/send and leaving a partial file.
	if atomic.LoadInt64(&inFlight) > 0 {
		httpErr(w, http.StatusConflict, "a transfer is in progress — try again in a moment")
		return
	}
	if err := saveSettings(settings{Incoming: clean}); err != nil {
		httpErr(w, http.StatusInternalServerError, "could not persist choice")
		return
	}
	writeJSON(w, map[string]any{"ok": true, "incoming": clean, "restarting": true})
	go func() {
		time.Sleep(600 * time.Millisecond) // let the response flush
		// Wait (bounded) for any transfer that raced in to finish before exiting,
		// so we re-read the new incoming dir without cutting a stream mid-write.
		deadline := time.Now().Add(30 * time.Second)
		for atomic.LoadInt64(&inFlight) > 0 && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
		log.Printf("storage repointed to %s — restarting to apply", clean)
		os.Exit(0)
	}()
}
