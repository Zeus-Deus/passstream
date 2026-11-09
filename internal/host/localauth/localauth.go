// Package localauth is the mutual HMAC challenge-response between the
// sender page and the daemon (CONTRACT §11.2).
package localauth

import (
	"crypto/hmac"
	"crypto/sha256"
)

// Roles bind each proof to its side, so a proof can't be reflected.
const (
	RoleDaemon byte = 0x01
	RolePage   byte = 0x02
)

const label = "PassStream v1 local auth"

// Proof returns HMAC-SHA-256(K, role || T), T = label || key_id || nonce_p || nonce_d.
func Proof(key []byte, role byte, keyID, nonceP, nonceD []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte{role})
	m.Write([]byte(label))
	m.Write(keyID)
	m.Write(nonceP)
	m.Write(nonceD)
	return m.Sum(nil)
}

// Verify compares a proof in constant time.
func Verify(key []byte, role byte, keyID, nonceP, nonceD, proof []byte) bool {
	return hmac.Equal(Proof(key, role, keyID, nonceP, nonceD), proof)
}
