package atelet

import _ "embed"

// vendoredInternalFlags is a byte-identical copy of the vendored
// third_party/substrate/cmd/ateworker-gvisor/kodata/internal_flags.json,
// checked against it by TestVendoredInternalFlagsMatchesVendored
// (atelet_vendor_test.go). Go's embed directive only reads files under
// (or below) the package directory that holds the //go:embed line, and
// third_party/substrate is never written to, so the file this package
// embeds has to be its own copy here rather than the vendored one
// directly. stageKodata writes these bytes into ATE_DIR/kodata, exactly
// as it used to copy them from disk, so a node needs no checkout of this
// repo (and no third_party/substrate directory) to start.
//
//go:embed kodata/internal_flags.json
var vendoredInternalFlags []byte
