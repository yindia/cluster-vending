package tenant

import (
	"crypto/rand"
	"encoding/hex"
)

// externalIDBytes is the random part of an account's external ID: 32
// bytes, 64 hex characters (sts:ExternalId takes 2 to 1224).
const externalIDBytes = 32

// newExternalID is a fresh external ID for an account: random, so no
// other tenant can guess it and have the platform assume this tenant's
// role for them (the confused deputy the condition guards against).
func newExternalID() (string, error) {
	b := make([]byte, externalIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
