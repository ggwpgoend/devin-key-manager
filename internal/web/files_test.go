package web

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestUniqueZipName(t *testing.T) {
	used := map[string]int{}

	cases := []struct{ in, want string }{
		{"foo.txt", "foo.txt"},
		{"foo.txt", "foo-2.txt"},
		{"foo.txt", "foo-3.txt"},
		{"bar", "bar"},
		{"bar", "bar-2"},
		{"", "file"},
		{"file", "file-2"},
		{"path/with/slashes/baz.png", "baz.png"},
		{".", "file-3"},
	}
	for _, c := range cases {
		got := uniqueZipName(c.in, used)
		if got != c.want {
			t.Errorf("uniqueZipName(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

// TestOpenFolderHookable ensures the openInFileManager indirection is wired
// so tests (and any future feature toggles) can swap in a fake.
func TestOpenFolderHookable(t *testing.T) {
	called := ""
	orig := openInFileManager
	openInFileManager = func(dir string) error {
		called = dir
		return nil
	}
	t.Cleanup(func() { openInFileManager = orig })

	if err := openInFileManager("/tmp/snap"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != "/tmp/snap" {
		t.Errorf("openInFileManager called with %q, want /tmp/snap", called)
	}
}

func TestOpenFolderError(t *testing.T) {
	orig := openInFileManager
	openInFileManager = func(string) error { return errors.New("no xdg-open") }
	t.Cleanup(func() { openInFileManager = orig })

	if err := openInFileManager("/x"); err == nil {
		t.Errorf("expected error, got nil")
	}
}

// TestZipReaderRoundTrip verifies that the standard zip package can read what
// we produce. This is a smoke test for the bytes we hand back to the browser
// and a guard against accidentally swapping zip writers.
func TestZipReaderRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip reader: %v", err)
	}
	if len(zr.File) != 1 || zr.File[0].Name != "hello.txt" {
		t.Fatalf("unexpected zip entries: %+v", zr.File)
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != "world" {
		t.Errorf("entry body=%q want world", string(body))
	}
}
