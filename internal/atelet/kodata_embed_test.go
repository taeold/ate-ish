package atelet

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestEmbeddedInternalFlagsMatchesVendored checks kodata/internal_flags.json
// (embedded into the binary, kodata_embed.go) stays byte-identical to the
// vendored third_party/substrate/cmd/ateworker-gvisor/kodata/internal_flags.json
// it was copied from. The embed exists only because go:embed cannot reach
// outside this package's own directory; this test is what keeps that copy
// honest without ever writing to third_party/substrate itself.
func TestEmbeddedInternalFlagsMatchesVendored(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/atelet/kodata_embed_test.go -> repo root is three dirs up.
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	vendored := filepath.Join(repoRoot,
		"third_party/substrate/cmd/ateworker-gvisor/kodata/internal_flags.json")

	want, err := os.ReadFile(vendored)
	if err != nil {
		t.Fatalf("reading vendored internal_flags.json: %v", err)
	}
	if string(want) != string(vendoredInternalFlags) {
		t.Fatalf("internal/atelet/kodata/internal_flags.json has drifted from %s; "+
			"copy the vendored file over it", vendored)
	}
}
