package model

import (
	"crypto/rand"
	"encoding/hex"
)

// NewMessageID mints a random assistant message id ("msg_" + 16 hex bytes).
func NewMessageID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "msg_" + hex.EncodeToString(b[:])
}

// NewID mints a random id with a prefix.
func NewID(prefix string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}
