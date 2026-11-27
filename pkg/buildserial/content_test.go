package buildserial

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// shippedContent is the digest of what each serial ships: the CRDs, the
// component definitions and release.yaml. Add the new serial's digest when
// you bump Serial; never change an old one.
var shippedContent = map[int64]string{
	1: "5a46c40f7727f146",
	2: "60d50e558ac8f4ea", // sub-tenants, spec.naas and Capsule removed
}

// contentFiles are the files a build installs into the management cluster
// from: what the serial stamps.
var contentFiles = []string{
	"../../config/crd/bases/*.yaml",
	"../../pkg/catalog/definitions/*.yaml",
	"../../pkg/catalog/release.yaml",
}

func contentDigest(t *testing.T) string {
	t.Helper()
	h := sha256.New()
	for _, pattern := range contentFiles {
		names, err := filepath.Glob(pattern)
		if err != nil || len(names) == 0 {
			t.Fatalf("no files for %s (%v)", pattern, err)
		}
		slices.Sort(names)
		for _, name := range names {
			raw, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			h.Write([]byte(filepath.Base(name)))
			h.Write([]byte{0})
			h.Write(raw)
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// A change of the CRDs, the shipped definitions or release.yaml bumps
// Serial: else an older build with the same serial could write its
// content over the newer one, and a newer build could not tell.
func TestSerialCoversTheShippedContent(t *testing.T) {
	got := contentDigest(t)
	want, recorded := shippedContent[Serial]
	switch {
	case !recorded:
		t.Fatalf("serial %d has no recorded content: add %d: %q to shippedContent", Serial, Serial, got)
	case want != got:
		t.Fatalf("the CRDs, shipped definitions or release.yaml changed since serial %d was recorded: "+
			"bump Serial to %d in pkg/buildserial/buildserial.go and add %d: %q to shippedContent", Serial, Serial+1, Serial+1, got)
	}
}
