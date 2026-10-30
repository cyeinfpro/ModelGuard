# Third-party source and data

- `internal/pluginv1/{plugin.proto,plugin.pb.go,plugin_grpc.pb.go,runtime.go}` copied from Wei-Shaw/sub2api checkout `e390f824ee8e71298316377c88fe0c696aa4b92c`, `backend/pkg/pluginapi/v1`. LGPL-3.0; see root LICENSE. Files retained unchanged except gofmt; Go package imports use the local module.
- `internal/fingerprint/` copied from the user's Magpie Guard checkout `64569ba` in `magpie-codex-guard-terra-20080627`, MIT; `licenses/Magpie-Guard-MIT.txt` preserves attribution. `internal/guard/challenge.go` adapts its random numerical challenge generator.
- Embedded fingerprint artifacts originate from Ikaleio/lm-detector `a4ef212df06817e8071acd05f00219f4c9eb2f9a`, MIT, copyright xqy2006. See `licenses/lm-detector-MIT.txt` and `internal/fingerprint/data/ORIGIN.md` for exact hashes and provenance.
- `internal/fingerprint/testdata` is used only in offline parity tests; it is not an enrollment store and is not included in the plugin ZIP.
- HashiCorp go-plugin / go-hclog (MPL-2.0), gRPC-Go (Apache-2.0), and protobuf-go (BSD-3-Clause) are used as Go module dependencies. Their exact versions and checksums are recorded in go.mod / go.sum.

This distribution is supplied with complete buildable source under LGPL-3.0; MIT-covered components retain their original terms and notices. No private signing key, server credential, raw production answer or production account export is distributed.
