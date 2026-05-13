package crypto

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	c, err := LoadOrCreateCipher(path)
	if err != nil {
		t.Fatalf("create cipher: %v", err)
	}
	plaintext := "sk-live-this-is-not-a-real-key-12345"
	ct, err := c.EncryptString(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if ct == plaintext {
		t.Fatal("ciphertext equals plaintext")
	}
	pt, err := c.DecryptString(ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if pt != plaintext {
		t.Fatalf("round trip mismatch: %q vs %q", pt, plaintext)
	}
}

func TestPersistentKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	c1, err := LoadOrCreateCipher(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	ct, err := c1.EncryptString("secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	c2, err := LoadOrCreateCipher(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	pt, err := c2.DecryptString(ct)
	if err != nil {
		t.Fatalf("decrypt across instances: %v", err)
	}
	if pt != "secret" {
		t.Fatalf("got %q", pt)
	}
}

func TestPermissionsOnFresh(t *testing.T) {
	if os.Getenv("CI_SKIP_PERM") != "" {
		t.Skip("permission check disabled")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if _, err := LoadOrCreateCipher(path); err != nil {
		t.Fatalf("create cipher: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("expected mode 0600, got %o", info.Mode().Perm())
	}
}

func TestTampering(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadOrCreateCipher(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	ct, err := c.EncryptString("hello")
	if err != nil {
		t.Fatal(err)
	}
	bad := ct[:len(ct)-1] + "A"
	if _, err := c.DecryptString(bad); err == nil {
		t.Fatal("expected error on tampered ciphertext")
	}
}
