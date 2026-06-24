package crypto

import (
	"encoding/base64"
	"strings"
	"testing"
)

// testKey is a valid 32-byte key encoded as base64.
func testKey() string {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestLoadKey_Valid(t *testing.T) {
	if _, err := LoadKey(testKey()); err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
}

func TestLoadKey_Rejects(t *testing.T) {
	cases := map[string]string{
		"empty":      "",
		"not base64": "!!!not base64!!!",
		"too short":  base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"too long":   base64.StdEncoding.EncodeToString(make([]byte, 33)),
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadKey(key); err == nil {
				t.Fatalf("expected error for %s key", name)
			}
		})
	}
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	c, err := LoadKey(testKey())
	if err != nil {
		t.Fatal(err)
	}
	plaintext := "https://hooks.slack.com/services/SECRET"
	enc, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	dec, err := c.Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if dec != plaintext {
		t.Errorf("round trip = %q, want %q", dec, plaintext)
	}
}

func TestEncrypt_FreshNonce(t *testing.T) {
	c, _ := LoadKey(testKey())
	a, _ := c.Encrypt("same")
	b, _ := c.Encrypt("same")
	if a == b {
		t.Error("two encrypts of the same plaintext should differ (fresh nonce)")
	}
}

func TestDecrypt_Tampered(t *testing.T) {
	c, _ := LoadKey(testKey())
	enc, _ := c.Encrypt("secret value")

	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the ciphertext body (past the 12-byte nonce).
	raw[len(raw)-1] ^= 0xFF
	tampered := base64.StdEncoding.EncodeToString(raw)

	if _, err := c.Decrypt(tampered); err == nil {
		t.Error("expected decrypt of tampered ciphertext to fail")
	}
}

func TestDecrypt_BadInputs(t *testing.T) {
	c, _ := LoadKey(testKey())
	if _, err := c.Decrypt("not base64 %%%"); err == nil {
		t.Error("expected error for non-base64 ciphertext")
	}
	if _, err := c.Decrypt(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Error("expected error for too-short ciphertext")
	}
}

func TestEncrypt_OutputIsBase64(t *testing.T) {
	c, _ := LoadKey(testKey())
	enc, _ := c.Encrypt("x")
	if strings.ContainsAny(enc, " \n\t") {
		t.Error("encrypted output should be clean base64")
	}
	if _, err := base64.StdEncoding.DecodeString(enc); err != nil {
		t.Errorf("output not valid base64: %v", err)
	}
}
