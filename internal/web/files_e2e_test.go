package web_test

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/artifacts"
	"github.com/ggwpgoend/devin-key-manager/internal/crypto"
	"github.com/ggwpgoend/devin-key-manager/internal/devin"
	"github.com/ggwpgoend/devin-key-manager/internal/handoffs"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/manager"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
	"github.com/ggwpgoend/devin-key-manager/internal/web"
)

// filesFixture spins up a manager backed by the real artifacts repo with two
// pre-populated files on disk: a JSON text-like artifact and a PNG binary.
// This lets us exercise the zip / open-folder / preview endpoints without
// network calls.
type filesFixture struct {
	t       *testing.T
	dir     string
	handler http.Handler
	mgr     *manager.Manager
	repo    *artifacts.Repo
	sess    sessions.Session
	jsonArt artifacts.Artifact
	binArt  artifacts.Artifact
}

func newFilesFixture(t *testing.T) *filesFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	cipher, err := crypto.LoadOrCreateCipher(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	db, err := store.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	keysRepo := keys.NewRepo(db, cipher)
	tasksRepo := tasks.NewRepo(db)
	sessionsRepo := sessions.NewRepo(db)
	handoffsRepo := handoffs.NewRepo(db)
	artRepo := artifacts.NewRepo(db)

	// Devin client is never called in these tests, but we still need a factory.
	factory := func(string) *devin.Client {
		return devin.NewClient("unused", devin.WithBaseURL("http://invalid.local"))
	}
	mgr := manager.New(keysRepo, tasksRepo, sessionsRepo, handoffsRepo, manager.Options{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ClientFactory: factory,
		Artifacts:     artRepo,
	})
	artRoot := filepath.Join(dir, "artifacts")
	dl := artifacts.NewDownloader(artRepo, artRoot, mgr.BearerForSession,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	mgr.SetDownloader(dl)

	// Seed a key, task, session — enough to satisfy foreign keys.
	k, err := keysRepo.Create(ctx, keys.CreateInput{
		Label: "fix-1", Plan: keys.PlanTrial, APIKey: "sk-fix",
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	tsk, err := tasksRepo.Create(ctx, tasks.CreateInput{
		Title: "demo", Prompt: "demo",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	sess, err := sessionsRepo.Create(ctx, sessions.CreateInput{
		TaskID: tsk.ID, KeyID: k.ID,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := sessionsRepo.AttachDevinSessionID(ctx, sess.ID, "devin-fix-1"); err != nil {
		t.Fatalf("attach devin session: %v", err)
	}

	sessDir := filepath.Join(artRoot, sess.ID)
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// JSON file (text-like).
	jsonPath := filepath.Join(sessDir, "config.json")
	if err := os.WriteFile(jsonPath, []byte(`{"hello":"world","items":[1,2,3]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	jsonArt, err := artRepo.Create(ctx, artifacts.CreateInput{
		TaskID: tsk.ID, SessionID: sess.ID, Filename: "config.json",
		RemoteURL: "https://example.invalid/config.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := artRepo.MarkReady(ctx, jsonArt.ID, artifacts.ReadyInput{
		LocalPath: jsonPath, ContentType: "application/json", SizeBytes: 32, SHA256: "deadbeef",
		Filename: "config.json",
	}); err != nil {
		t.Fatal(err)
	}
	jsonArt.LocalPath = jsonPath
	jsonArt.ContentType = "application/json"
	jsonArt.SizeBytes = 32
	jsonArt.SHA256 = "deadbeef"
	jsonArt.Status = artifacts.StatusReady

	// PNG file (binary).
	pngPath := filepath.Join(sessDir, "snap.png")
	pngBody := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if err := os.WriteFile(pngPath, pngBody, 0o644); err != nil {
		t.Fatal(err)
	}
	binArt, err := artRepo.Create(ctx, artifacts.CreateInput{
		TaskID: tsk.ID, SessionID: sess.ID, Filename: "snap.png",
		RemoteURL: "https://example.invalid/snap.png",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := artRepo.MarkReady(ctx, binArt.ID, artifacts.ReadyInput{
		LocalPath: pngPath, ContentType: "image/png", SizeBytes: int64(len(pngBody)),
		SHA256: "cafe", Filename: "snap.png",
	}); err != nil {
		t.Fatal(err)
	}
	binArt.LocalPath = pngPath
	binArt.ContentType = "image/png"
	binArt.SizeBytes = int64(len(pngBody))
	binArt.SHA256 = "cafe"
	binArt.Status = artifacts.StatusReady

	srv, err := web.NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)),
		web.Deps{
			Keys: keysRepo, Tasks: tasksRepo, Sessions: sessionsRepo,
			Handoffs: handoffsRepo, Artifacts: artRepo, Manager: mgr,
		},
		filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatalf("web server: %v", err)
	}
	return &filesFixture{
		t: t, dir: dir, handler: srv.Handler(), mgr: mgr, repo: artRepo,
		sess: sess, jsonArt: jsonArt, binArt: binArt,
	}
}

func TestFiles_DownloadAllZip(t *testing.T) {
	f := newFilesFixture(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sessions/"+f.sess.ID+"/files.zip", nil)
	f.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("zip status=%d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type=%q want application/zip", ct)
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "session-"+f.sess.ID+".zip") {
		t.Errorf("Content-Disposition=%q missing session id", cd)
	}
	zr, err := zip.NewReader(bytes.NewReader(rr.Body.Bytes()), int64(rr.Body.Len()))
	if err != nil {
		t.Fatalf("zip reader: %v", err)
	}
	// 2 files + 1 manifest = 3 entries (#19 audit trail).
	if len(zr.File) != 3 {
		t.Fatalf("zip entries=%d want 3 (files+manifest) (%v)", len(zr.File), zr.File)
	}
	names := map[string]bool{}
	for _, fh := range zr.File {
		names[fh.Name] = true
	}
	if !names["config.json"] || !names["snap.png"] || !names["_manifest.txt"] {
		t.Errorf("unexpected zip names: %v", names)
	}
}

func TestFiles_DownloadAllZip_HandlesDuplicateNames(t *testing.T) {
	f := newFilesFixture(t)
	ctx := context.Background()

	// Insert a second artifact with the same filename to exercise the
	// uniqueZipName collision path.
	dupPath := filepath.Join(f.dir, "artifacts", f.sess.ID, "config-extra.json")
	if err := os.WriteFile(dupPath, []byte(`{"second":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	dup, err := f.repo.Create(ctx, artifacts.CreateInput{
		TaskID: f.jsonArt.TaskID, SessionID: f.sess.ID, Filename: "config.json",
		RemoteURL: "https://example.invalid/dup-config.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.MarkReady(ctx, dup.ID, artifacts.ReadyInput{
		LocalPath: dupPath, ContentType: "application/json", SizeBytes: 15,
		SHA256: "feed", Filename: "config.json",
	}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions/"+f.sess.ID+"/files.zip", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("zip: %d", rr.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(rr.Body.Bytes()), int64(rr.Body.Len()))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	names := map[string]bool{}
	for _, fh := range zr.File {
		names[fh.Name] = true
	}
	// Both copies present, second one renamed.
	if !names["config.json"] {
		t.Errorf("expected config.json in zip; got %v", names)
	}
	if !names["config-2.json"] {
		t.Errorf("expected duplicated config-2.json in zip; got %v", names)
	}
}

func TestFiles_OpenFolder(t *testing.T) {
	f := newFilesFixture(t)
	hadCall := ""
	orig := web.SetOpenInFileManagerForTest(func(dir string) error {
		hadCall = dir
		return nil
	})
	defer web.SetOpenInFileManagerForTest(orig)

	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/sessions/"+f.sess.ID+"/files/open", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("open folder: %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Opened ") {
		t.Errorf("body=%q want to contain Opened", rr.Body.String())
	}
	wantDir := filepath.Join(f.mgr.ArtifactsRoot(), f.sess.ID)
	if hadCall != wantDir {
		t.Errorf("openInFileManager dir=%q want %q", hadCall, wantDir)
	}
}

func TestFiles_OpenFolder_PropagatesError(t *testing.T) {
	f := newFilesFixture(t)
	orig := web.SetOpenInFileManagerForTest(func(string) error { return errSentinel{} })
	defer web.SetOpenInFileManagerForTest(orig)

	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/sessions/"+f.sess.ID+"/files/open", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "could not open folder") {
		t.Errorf("body=%q want a friendly error", rr.Body.String())
	}
}

type errSentinel struct{}

func (errSentinel) Error() string { return "stub" }

func TestPreview_Text(t *testing.T) {
	f := newFilesFixture(t)
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/artifacts/"+f.jsonArt.ID+"/preview", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		"config.json",
		"language-json",
		`{&#34;hello&#34;:&#34;world&#34;`, // html-escaped contents
		`href="/artifacts/` + f.jsonArt.ID + `/download"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview missing %q", want)
		}
	}
}

func TestPreview_BinaryRedirectsImage(t *testing.T) {
	f := newFilesFixture(t)
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/artifacts/"+f.binArt.ID+"/preview", nil))
	if rr.Code != http.StatusSeeOther {
		t.Errorf("image preview status=%d want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); !strings.HasSuffix(loc, "/raw") {
		t.Errorf("image preview redirect to %q, want .../raw", loc)
	}
}

func TestPreview_BinaryNonImage(t *testing.T) {
	f := newFilesFixture(t)
	ctx := context.Background()
	// Add a binary, non-image artifact (e.g. a .exe).
	exePath := filepath.Join(f.dir, "artifacts", f.sess.ID, "out.exe")
	if err := os.WriteFile(exePath, []byte{0x4D, 0x5A, 0x90, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	exe, err := f.repo.Create(ctx, artifacts.CreateInput{
		TaskID: f.jsonArt.TaskID, SessionID: f.sess.ID, Filename: "out.exe",
		RemoteURL: "https://example.invalid/out.exe",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.MarkReady(ctx, exe.ID, artifacts.ReadyInput{
		LocalPath: exePath, ContentType: "application/octet-stream", SizeBytes: 4,
		SHA256: "beef", Filename: "out.exe",
	}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/artifacts/"+exe.ID+"/preview", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "binary") {
		t.Errorf("expected 'binary' explanation in preview body, got %q", rr.Body.String())
	}
}

func TestPreview_LargeFileTruncated(t *testing.T) {
	f := newFilesFixture(t)
	ctx := context.Background()
	bigPath := filepath.Join(f.dir, "artifacts", f.sess.ID, "big.txt")
	// 257 KiB so we exceed the 256 KiB cap.
	body := bytes.Repeat([]byte("AB"), 132000)
	if err := os.WriteFile(bigPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	big, err := f.repo.Create(ctx, artifacts.CreateInput{
		TaskID: f.jsonArt.TaskID, SessionID: f.sess.ID, Filename: "big.txt",
		RemoteURL: "https://example.invalid/big.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.MarkReady(ctx, big.ID, artifacts.ReadyInput{
		LocalPath: bigPath, ContentType: "text/plain", SizeBytes: int64(len(body)),
		SHA256: "abcd", Filename: "big.txt",
	}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/artifacts/"+big.ID+"/preview", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("preview: %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "256 KiB") {
		t.Errorf("expected truncation notice in body")
	}
}

func TestSessionFilesPage_HasDownloadAllAndOpenFolder(t *testing.T) {
	f := newFilesFixture(t)
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions/"+f.sess.ID+"/files", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("files: %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"Download all (zip)",
		`href="/sessions/` + f.sess.ID + `/files.zip"`,
		"Open folder",
		`hx-post="/sessions/` + f.sess.ID + `/files/open"`,
		"Stored at",
		`href="/artifacts/` + f.jsonArt.ID + `/preview"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("files page missing %q", want)
		}
	}
	// Sanity: page time/idempotency check — don't get a server error.
	if strings.Contains(body, "internal server error") {
		t.Errorf("server error rendered: %s", body)
	}
	_ = time.Now()
}
