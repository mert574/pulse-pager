// Package crypto does AES-256-GCM encryption for secret fields, plus the
// startup key check. It imports nothing from the rest of the app.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// HashToken returns the SHA-256 of an opaque secret token, hex-encoded. This is
// what we store for refresh tokens, invitation tokens, and API keys (RFC-003 5.2):
// the token is high-entropy random, so a fast one-way hash is enough and there is
// nothing to brute-force, and a DB read cannot replay the secret. It is one-way,
// never decrypted.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Cipher wraps an AES-256-GCM AEAD.
type Cipher struct {
	aead cipher.AEAD
}

// LoadKey decodes PULSE_SECRET_KEY (base64), checks it is exactly 32 bytes, and
// returns a Cipher. On any problem it returns an error so main can exit non-zero
// (fail closed, never plaintext fallback). Called once at startup.
func LoadKey(b64 string) (*Cipher, error) {
	if b64 == "" {
		return nil, errors.New("secret key is empty")
	}
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("secret key is not valid base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secret key must be exactly 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("could not create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("could not create GCM: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt returns base64(nonce || ciphertext || gcmTag). A fresh random 96-bit
// nonce is generated for each call.
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("could not read random nonce: %w", err)
	}
	// Seal appends ciphertext+tag onto nonce, so the output is nonce||ciphertext||tag.
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. Used by store when reading secret columns.
func (c *Cipher) Decrypt(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("ciphertext is not valid base64: %w", err)
	}
	nonceSize := c.aead.NonceSize()
	if len(raw) < nonceSize {
		return "", errors.New("ciphertext is too short")
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("could not decrypt: %w", err)
	}
	return string(plaintext), nil
}
