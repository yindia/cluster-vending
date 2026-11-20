//go:build e2e

package e2e

import "github.com/google/uuid"

// newRequestID is a fresh idempotency key: every call the harness makes is
// a new request, so a retry of the harness never answers with an old one.
func newRequestID() string { return uuid.NewString() }
