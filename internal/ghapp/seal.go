package ghapp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
)

// Sealed is a secret encrypted with AES-256-GCM. The associated data binds
// it to its owner, so a sealed value can't be moved to another record.
type Sealed string

func seal(key []byte, plaintext, owner string) (Sealed, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plaintext), []byte("xu-ghapp:"+owner))
	return Sealed("v1:" + base64.RawStdEncoding.EncodeToString(ct)), nil
}

func (s Sealed) open(key []byte, owner string) (string, error) {
	rest, ok := strings.CutPrefix(string(s), "v1:")
	if !ok {
		return "", errors.New("unknown sealed format")
	}
	raw, err := base64.RawStdEncoding.DecodeString(rest)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("sealed value too short")
	}
	pt, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte("xu-ghapp:"+owner))
	if err != nil {
		return "", errors.New("can't decrypt (wrong XU_SECRET_KEY, or the value was tampered with)")
	}
	return string(pt), nil
}
