package results

import (
	"context"
	"errors"
	"fmt"
	"net"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
)

// Errors of the store. Test them with errors.Is; the error says which
// bucket and key besides.
var (
	// ErrNotFound: the bucket or the object does not exist (a scan has not
	// written yet, or the bucket is not made).
	ErrNotFound = errors.New("results: not found")
	// ErrAccessDenied: the account's role cannot be assumed or may not do
	// the call.
	ErrAccessDenied = errors.New("results: access denied")
	// ErrTimeout: the call did not finish within its bound.
	ErrTimeout = errors.New("results: timed out")
	// ErrTooLarge: an object is above the size bound, written or read.
	ErrTooLarge = errors.New("results: object above the size bound")
	// ErrCorrupt: the object is not what a writer of this package writes.
	ErrCorrupt = errors.New("results: unreadable object")
)

// Notice is what a reader tells its caller when results are not available:
// a coverage notice for a response, never details of the failure. Empty
// for a nil error.
func Notice(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNotFound):
		return "results unavailable: none have been stored for this cluster yet"
	case errors.Is(err, ErrAccessDenied):
		return "results unavailable: access to the results bucket was denied"
	case errors.Is(err, ErrTimeout):
		return "results unavailable: reading them timed out"
	case errors.Is(err, ErrTooLarge):
		return "results unavailable: the stored results are larger than the platform reads"
	case errors.Is(err, ErrCorrupt):
		return "results unavailable: the stored results cannot be read"
	}
	return "results unavailable: they could not be read"
}

// classify wraps an S3 or transport error in the package's, keeping the
// cause (both are matched by errors.Is). A cancelled caller is not a
// timeout; unknown errors pass through with what was being done.
func classify(err error, what string) error {
	if err == nil {
		return nil
	}
	var netErr net.Error
	var api smithy.APIError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout():
		return wrap(ErrTimeout, what, err)
	case errors.As(err, &api):
		switch api.ErrorCode() {
		case "NoSuchKey", "NoSuchBucket", "NotFound":
			return wrap(ErrNotFound, what, err)
		case "AccessDenied", "AccessDeniedException", "Forbidden", "ExpiredToken", "InvalidAccessKeyId",
			"InvalidToken", "SignatureDoesNotMatch", "AllAccessDisabled", "AccountProblem":
			return wrap(ErrAccessDenied, what, err)
		}
	}
	var resp *awshttp.ResponseError
	if errors.As(err, &resp) {
		switch resp.HTTPStatusCode() {
		case 404:
			return wrap(ErrNotFound, what, err)
		case 403:
			return wrap(ErrAccessDenied, what, err)
		}
	}
	return errorf(what, err)
}

// notModified reports whether err is S3's answer to a conditional GET whose
// ETag is still the object's.
func notModified(err error) bool {
	var resp *awshttp.ResponseError
	return errors.As(err, &resp) && resp.HTTPStatusCode() == 304
}

func wrap(kind error, what string, cause error) error {
	return fmt.Errorf("%w: %s: %w", kind, what, cause)
}

func errorf(what string, cause error) error { return fmt.Errorf("results: %s: %w", what, cause) }
