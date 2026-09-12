package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useTempIncoming points incomingDir at a fresh temp dir for one test.
func useTempIncoming(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := incomingDir
	incomingDir = dir
	t.Cleanup(func() { incomingDir = old })
	return dir
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSafeRel(t *testing.T) {
	cases := map[string]string{
		"a.txt":                  "a.txt",
		"dir/sub/a.txt":          "dir/sub/a.txt",
		"/abs/a.txt":             "abs/a.txt",
		"../../etc/passwd":       "etc/passwd", // ".." resolved against the root, never above incomingDir
		"dir/../../x":            "x",
		"..":                     "",
		".":                      "",
		"":                       "",
		"   ":                    "",
		`win\style\a.txt`:        "win/style/a.txt",
		"nul\x00byte.txt":        "",
		"./a/./b//c.txt":         "a/b/c.txt",
		"Übersicht 2026.pdf":     "Übersicht 2026.pdf",
		".dropbridge-peers.json": ".dropbridge-peers.json", // safeRel is path-only; reserved names are refused where they matter
	}
	for in, want := range cases {
		if got := safeRel(in); got != want {
			t.Errorf("safeRel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReceivedFile(t *testing.T) {
	dir := useTempIncoming(t)
	mustWrite(t, filepath.Join(dir, "ok.txt"), "x")
	mustWrite(t, filepath.Join(dir, "sub", "deep.txt"), "y")
	mustWrite(t, filepath.Join(dir, ".dropbridge-shares.json"), "[]")
	outside := filepath.Join(t.TempDir(), "secret.txt")
	mustWrite(t, outside, "z")
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}

	good := []string{"ok.txt", "sub/deep.txt", "/ok.txt", "../ok.txt", "sub/../ok.txt"}
	for _, n := range good {
		if _, full, ok := receivedFile(n); !ok || !strings.HasPrefix(full, dir) {
			t.Errorf("receivedFile(%q) = %q,%v — want a file inside %s", n, full, ok, dir)
		}
	}
	bad := map[string]string{
		"":                        "empty",
		"missing.txt":             "missing",
		"sub":                     "directory",
		".dropbridge-shares.json": "internal metadata",
		"../../etc/passwd":        "traversal (resolves to <incoming>/etc/passwd, which doesn't exist)",
	}
	for n, why := range bad {
		if _, _, ok := receivedFile(n); ok {
			t.Errorf("receivedFile(%q) accepted — must refuse: %s", n, why)
		}
	}
	// A symlink to a regular file outside incomingDir IS a regular file after Stat.
	// That is deliberate (host admins may link a big file in); this test pins the
	// behaviour so a change is a conscious decision, not an accident.
	if _, _, ok := receivedFile("link.txt"); !ok {
		t.Errorf("receivedFile(symlink→regular) refused; behaviour changed")
	}
}

func TestStoreLocalConflictAndReserved(t *testing.T) {
	dir := useTempIncoming(t)
	for i, want := range []string{"a.txt", "a-1.txt", "a-2.txt"} {
		got, n, err := storeLocal("a.txt", strings.NewReader("hello"))
		if err != nil || got != want || n != 5 {
			t.Fatalf("store #%d: got %q,%d,%v want %q", i, got, n, err, want)
		}
	}
	if got, _, err := storeLocal("folder/nested/b.bin", strings.NewReader("1")); err != nil || got != "folder/nested/b.bin" {
		t.Fatalf("nested store: %q %v", got, err)
	}
	if _, _, err := storeLocal(".dropbridge-peers.json", strings.NewReader("[]")); err == nil {
		t.Fatal("reserved metadata name was stored — it would be invisible and undeletable")
	}
	if _, err := os.Stat(filepath.Join(dir, ".dropbridge-peers.json")); !os.IsNotExist(err) {
		t.Fatal("reserved name left a file on disk")
	}
}

func TestDeletePrunesEmptyFolders(t *testing.T) {
	dir := useTempIncoming(t)
	mustWrite(t, filepath.Join(dir, "drop", "sub", "one.txt"), "1")
	mustWrite(t, filepath.Join(dir, "drop", "keep.txt"), "2")

	rec := httptest.NewRecorder()
	handleDelete(rec, httptest.NewRequest(http.MethodPost, "/api/delete?name=drop/sub/one.txt", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, "drop", "sub")); !os.IsNotExist(err) {
		t.Error("empty folder drop/sub was not pruned")
	}
	if _, err := os.Stat(filepath.Join(dir, "drop")); err != nil {
		t.Error("non-empty folder drop/ must survive")
	}
	rec = httptest.NewRecorder()
	handleDelete(rec, httptest.NewRequest(http.MethodPost, "/api/delete?name=drop/keep.txt", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete #2: %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "drop")); !os.IsNotExist(err) {
		t.Error("now-empty folder drop/ was not pruned")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("incomingDir itself must never be removed")
	}
	rec = httptest.NewRecorder()
	handleDelete(rec, httptest.NewRequest(http.MethodPost, "/api/delete?name=nope.txt", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing file: %d, want 404", rec.Code)
	}
}

func TestDownloadHeaders(t *testing.T) {
	dir := useTempIncoming(t)
	mustWrite(t, filepath.Join(dir, "Übersicht 2026.pdf"), "%PDF")
	rec := httptest.NewRecorder()
	handleDownload(rec, httptest.NewRequest(http.MethodGet, "/api/download?name="+url.QueryEscape("Übersicht 2026.pdf"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("download: %d", rec.Code)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, "filename*=utf-8''%C3%9Cbersicht%202026.pdf") {
		t.Errorf("Content-Disposition = %q — want RFC 2231 encoded non-ASCII name", cd)
	}
	for _, c := range cd {
		if c > 127 {
			t.Fatalf("raw non-ASCII byte in header: %q", cd)
		}
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff missing")
	}
}

func TestAuthTailnetSpoofedServeHeader(t *testing.T) {
	old := token
	token = ""
	t.Cleanup(func() { token = old })
	mk := func(remote, hdr string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/mesh", nil)
		r.RemoteAddr = remote
		if hdr != "" {
			r.Header.Set("Tailscale-User-Login", hdr)
		}
		return r
	}
	if authTailnet(mk("172.16.5.50:4444", "someone@example.com")) {
		t.Error("LAN client with a self-set serve header must NOT pass")
	}
	if !authTailnet(mk("127.0.0.1:4444", "someone@example.com")) {
		t.Error("loopback + serve header must pass")
	}
	if authTailnet(mk("127.0.0.1:4444", "")) {
		t.Error("bare loopback without identity must NOT pass")
	}
	if !authTailnet(mk("100.100.1.2:4444", "")) {
		t.Error("tailnet CGNAT source must pass")
	}
	if authTailnet(mk("100.200.1.2:4444", "")) {
		t.Error("100.200/16 is outside 100.64.0.0/10 — must NOT pass")
	}
}

func TestSmallHelpers(t *testing.T) {
	for ip, want := range map[string]bool{"100.64.0.1": true, "100.127.255.254": true, "100.63.255.255": false, "100.128.0.0": false, "10.0.0.1": false, "nope": false} {
		if got := isTailnetIP(ip); got != want {
			t.Errorf("isTailnetIP(%s)=%v", ip, got)
		}
	}
	for h, want := range map[string]bool{"127.0.0.1": true, "::1": true, "127.5.5.5": true, "100.64.0.1": false, "": false} {
		if got := isLoopback(h); got != want {
			t.Errorf("isLoopback(%q)=%v", h, got)
		}
	}
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 1536: "1.5 KB", 5 << 30: "5.0 GB", 3 << 40: "3.0 TB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d)=%q want %q", n, got, want)
		}
	}
	tok, err := newToken()
	if err != nil || !validToken(tok) {
		t.Fatalf("fresh token %q rejected (%v)", tok, err)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 32), strings.ToUpper(tok), tok + "/", "../" + tok[3:]} {
		if validToken(bad) {
			t.Errorf("validToken(%q) accepted", bad)
		}
	}
}

func TestJSONFileRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	var got []peerCfg
	if found, err := readJSONFile(p, &got); found || err != nil {
		t.Fatalf("missing file: found=%v err=%v — want found=false, nil", found, err)
	}
	want := []peerCfg{{ID: "100.64.0.9", Name: "box", URL: "http://100.64.0.9:8787", Added: 42}}
	if err := writeJSONAtomic(p, want); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file left behind")
	}
	if found, err := readJSONFile(p, &got); !found || err != nil || len(got) != 1 || got[0] != want[0] {
		t.Fatalf("roundtrip: found=%v err=%v got=%+v", found, err, got)
	}
	mustWrite(t, p, "{not json")
	if found, err := readJSONFile(p, &got); !found || err == nil {
		t.Errorf("corrupt file must report found=true + error, got %v %v", found, err)
	}
}
