package model

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
)

// Provider is the cloud a row belongs to, the same names as the cloud.v1
// provider oneof arms. Identifier recognition is separate from installing
// and implementing the provider adapter; unsupported backends fail closed.
type Provider string

// Supported providers.
const (
	ProviderAWS   Provider = "aws"
	ProviderGCP   Provider = "gcp"
	ProviderAzure Provider = "azure"
)

// ErrInvalid marks a row that fails model validation.
var ErrInvalid = errors.New("invalid")

// Validate reports whether p is a supported provider.
func (p Provider) Validate() error {
	switch p {
	case ProviderAWS, ProviderGCP, ProviderAzure:
		return nil
	default:
		return fmt.Errorf("%w provider %q: must be aws, gcp or azure", ErrInvalid, p)
	}
}

var (
	awsAccountID = regexp.MustCompile(`^\d{12}$`)
	// GCP project IDs: 6-30 chars, lowercase letters, digits and hyphens,
	// starting with a letter and not ending with a hyphen.
	gcpProjectID = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	uuidPattern  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// newProviderConfigName returns "<provider>-<8 random hex>".
func newProviderConfigName(p Provider) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("provider config name: %w", err)
	}
	return string(p) + "-" + hex.EncodeToString(b), nil
}

// ValidateAccount checks a cloud account for its provider:
//
//	aws    account ID: 12 digits; no directory
//	gcp    project ID; no directory
//	azure  subscription ID: UUID; directory (Entra tenant) ID: UUID
func ValidateAccount(p Provider, accountID, directoryID string) error {
	if err := p.Validate(); err != nil {
		return err
	}
	var ok bool
	switch p {
	case ProviderAWS:
		ok = awsAccountID.MatchString(accountID)
	case ProviderGCP:
		ok = gcpProjectID.MatchString(accountID)
	case ProviderAzure:
		ok = uuidPattern.MatchString(accountID)
	}
	if !ok {
		return fmt.Errorf("%w %s account ID %q", ErrInvalid, p, accountID)
	}
	if p == ProviderAzure && !uuidPattern.MatchString(directoryID) {
		return fmt.Errorf("%w azure directory (Entra tenant) ID %q: must be a UUID", ErrInvalid, directoryID)
	}
	if p != ProviderAzure && directoryID != "" {
		return fmt.Errorf("%w %s account: directory ID is only for azure", ErrInvalid, p)
	}
	return nil
}
