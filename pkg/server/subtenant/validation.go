package subtenant

import (
	"errors"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

func validateKeys(keys []string) error {
	if len(keys) < 1 || len(keys) > 64 {
		return invalid("namespace_keys must contain 1–64 unique DNS keys")
	}
	seen := map[string]bool{}
	for _, key := range keys {
		if len(key) > 27 || len(validation.IsDNS1123Label(key)) != 0 {
			return invalid("namespace key %q must be a DNS label of at most 27 characters", key)
		}
		if seen[key] {
			return invalid("namespace key %q is duplicated", key)
		}
		seen[key] = true
	}
	return nil
}

func appendOnlyKeys(before, after []string) error {
	next := make(map[string]bool, len(after))
	for _, key := range after {
		next[key] = true
	}
	for _, key := range before {
		if !next[key] {
			return invalid("namespace_keys are append-only; existing key %q cannot be removed or renamed", key)
		}
	}
	return nil
}

// Quota values are parsed by Kubernetes, never by floating-point arithmetic.
// Count quotas must additionally be whole numbers; zero is a valid way to
// disallow a resource within a namespace budget.
func validateQuota(namespaceCount int, hard map[string]string, keys []string) error {
	if namespaceCount < 1 || namespaceCount > 64 || namespaceCount < len(keys) {
		return invalid("quota.namespace_count must be 1–64 and cover all namespace keys")
	}
	if len(hard) == 0 || len(hard) > 12 {
		return invalid("quota.hard must contain 1–12 resource limits")
	}
	for key, value := range hard {
		if len(key) > 253 || len(value) > 64 {
			return invalid("quota resource or quantity is too long")
		}
		if !quotaResource(key) {
			return invalid("unsupported quota resource %q", key)
		}
		quantity, err := resource.ParseQuantity(value)
		if err != nil || quantity.Sign() < 0 {
			return invalid("quota.hard[%q] must be a non-negative Kubernetes resource quantity", key)
		}
		if quotaCount(key) && quantity.Cmp(*resource.NewQuantity(quantity.Value(), resource.DecimalSI)) != 0 {
			return invalid("quota.hard[%q] must be a whole resource count", key)
		}
	}
	return nil
}

func quotaResource(key string) bool {
	switch key {
	case "requests.cpu", "requests.memory", "limits.cpu", "limits.memory", "requests.ephemeral-storage", "limits.ephemeral-storage", "requests.storage", "persistentvolumeclaims", "pods", "services", "secrets", "configmaps":
		return true
	default:
		return false
	}
}

func quotaCount(key string) bool {
	switch key {
	case "pods", "services", "secrets", "configmaps", "persistentvolumeclaims":
		return true
	default:
		return false
	}
}

func unresolvedIdentities() error {
	return errors.New("access identity references require a platform-configured identity resolver; none is configured")
}
