package dockerhost

import (
	"crypto/sha256"
	"encoding/hex"
)

// OwnerTag is a short, name-safe tag of an owner (the address of the mocker
// that creates Docker objects). Backends put it into every object name, so
// that concurrent mockers on one Docker host, whose resource IDs are
// sequential and therefore equal, never collide: 10 lowercase hex characters
// of sha256(owner).
func OwnerTag(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:5])
}
