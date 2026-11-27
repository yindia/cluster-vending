// Package buildserial is the platform build's serial: a number stamped on
// what a build installs into the management cluster (the
// CustomResourceDefinitions, the shipped component definitions, the
// shipped release), so an older build (a rollback, an old pod still
// starting) never writes over what a newer build installed: a CRD it
// applied with an older schema would prune the newer fields, and a
// catalog it synced would revert the newer one.
//
// Bump Serial in the change that edits config/crd/bases, a file of
// pkg/catalog/definitions or pkg/catalog/release.yaml (CONTRIBUTING.md);
// TestSerialCoversTheShippedContent fails until it is.
package buildserial

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Serial is this build's serial. It only ever goes up.
const Serial int64 = 2

// Annotation holds the serial of the build that last wrote an object.
const Annotation = "cloud.cluster-vending.io/build-serial"

// Of is the serial stamped in annotations; 0 when none is (written by a
// build before serials, or not by a build at all).
func Of(annotations map[string]string) int64 {
	n, err := strconv.ParseInt(annotations[Annotation], 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Newer reports whether annotations carry a serial above this build's, and
// which.
func Newer(annotations map[string]string) (bool, int64) {
	n := Of(annotations)
	return n > Serial, n
}

// Stamp is annotations with this build's serial; annotations is not
// changed.
func Stamp(annotations map[string]string) map[string]string {
	out := make(map[string]string, len(annotations)+1)
	maps.Copy(out, annotations)
	out[Annotation] = strconv.FormatInt(Serial, 10)
	return out
}

var (
	mu      sync.Mutex
	refused = map[string]int64{}
)

// Refuse records that this build did not write what (an object a newer
// build, of serial stored, installed). Check reports it.
func Refuse(what string, stored int64) {
	mu.Lock()
	defer mu.Unlock()
	refused[what] = stored
}

// Refused describes what this build refused to write; "" when nothing.
func Refused() string {
	mu.Lock()
	defer mu.Unlock()
	if len(refused) == 0 {
		return ""
	}
	parts := make([]string, 0, len(refused))
	for _, what := range slices.Sorted(maps.Keys(refused)) {
		parts = append(parts, fmt.Sprintf("%s (serial %d)", what, refused[what]))
	}
	return fmt.Sprintf("build serial %d is older than what a newer build installed, which it did not write over: %s; "+
		"run the newer build (docs/architecture.md, Upgrade and rollback the platform)", Serial, strings.Join(parts, ", "))
}

// Check is a ready check (controller-runtime healthz.Checker): it fails
// while this build refused to write over a newer build's objects, so a
// rollout to an older build stops at its first pod.
func Check(*http.Request) error {
	if why := Refused(); why != "" {
		return fmt.Errorf("%s", why)
	}
	return nil
}

// ResetForTest forgets the refusals recorded so far. For tests.
func ResetForTest() {
	mu.Lock()
	defer mu.Unlock()
	refused = map[string]int64{}
}
