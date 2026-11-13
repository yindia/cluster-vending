package reconcile

import "github.com/yindia/cluster-vending/pkg/fault"

// ImportCannotSucceed reports whether what the lookup of an import said is
// final: the resource does not exist where the import says it is (a typo,
// another region or account), or AWS refuses what was asked for. Asking
// again gives the same answer, so the import stops (BLOCKED, with the
// reason) until it is deleted and made again.
//
// Everything else is tried again: a lookup that did not answer or was
// throttled, and credentials that cannot read the resource (the account's
// role can be fixed without touching the import).
func ImportCannotSucceed(err error) bool {
	return fault.Is(err, fault.NotFound) || fault.Is(err, fault.Invalid)
}

// ImportStoppedDetail is the plan line of an import that stopped: what the
// lookup said, and what to do about it.
func ImportStoppedDetail(err error) string {
	message := "the lookup failed"
	if f, ok := fault.As(err); ok {
		message = f.Message
	}
	return message + "; the import is not tried again: delete it and import again"
}
