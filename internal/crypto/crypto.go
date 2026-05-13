// Package crypto provides AES-256-GCM encryption for API keys at rest using a
// 32-byte master key persisted next to the database. The master key is
// auto-generated on first start so end users never have to manage one.
//
// Threat model: the manager runs locally on a single-user Windows host. The
// master key file is created with mode 0600 to keep other local users out, but
// it intentionally does not defend against an attacker who already has full
// access to the user's filesystem. For that level of protection, switch to
// Windows DPAPI in a future release.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	// MasterKeyBytes is the length of the AES-256 master key.
	MasterKeyBytes = 32
	// nonceBytes is the length of the GCM nonce.
	nonceBytes = 12
)

// ErrInvalidMasterKey is returned when the on-disk master key file is missing,
// truncated, or otherwise unusable.
var ErrInvalidMasterKey = errors.New("crypto: invalid master key file")

// Cipher wraps an AES-GCM AEAD primed with a master key. All methods are safe
// for concurrent use because AEAD instances from crypto/cipher are.
type Cipher struct {
	aead cipher.AEAD
}

// LoadOrCreateCipher reads the master key from path or, if it does not exist,
// generates a new random 32-byte key and writes it with mode 0600. Returns a
// Cipher ready for Seal/Open operations.
func LoadOrCreateCipher(path string) (*Cipher, error) {
	key, err := loadOrCreateMasterKey(path)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: aes init: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: gcm init: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// EncryptString seals plaintext into a base64-encoded ciphertext suitable for
// SQLite storage. The output includes a random nonce prepended to the GCM
// ciphertext, so it can be decrypted later with only the master key.
func (c *Cipher) EncryptString(plaintext string) (string, error) {
	nonce := make([]byte, nonceBytes)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// DecryptString reverses EncryptString. Any tampering with the ciphertext
// causes an error because GCM authenticates the payload.
func (c *Cipher) DecryptString(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("crypto: base64 decode: %w", err)
	}
	if len(raw) < nonceBytes {
		return "", ErrInvalidMasterKey
	}
	nonce, ct := raw[:nonceBytes], raw[nonceBytes:]
	pt, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: open: %w", err)
	}
	return string(pt), nil
}

func loadOrCreateMasterKey(path string) ([]byte, error) {
	if data, err := os.ReadFile(path); err == nil {
		if len(data) != MasterKeyBytes {
			return nil, fmt.Errorf("%w: expected %d bytes at %s, got %d", ErrInvalidMasterKey, MasterKeyBytes, path, len(data))
		}
		return data, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("crypto: read master key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("crypto: mkdir master key dir: %w", err)
	}
	key := make([]byte, MasterKeyBytes)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("crypto: generate master key: %w", err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("crypto: write master key: %w", err)
	}
	return key, nil
}
