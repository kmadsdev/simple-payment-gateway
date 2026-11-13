// Package id creates random, prefixed identifiers such as "pay_3f9a...".
package id

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns prefix + "_" + 20 hex characters (80 random bits).
func New(prefix string) string {
	b := make([]byte, 10)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return prefix + "_" + hex.EncodeToString(b)
}

// Secret returns n random bytes as hex. Used for API keys and webhook secrets.
func Secret(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
