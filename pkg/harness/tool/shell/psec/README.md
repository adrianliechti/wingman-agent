This directory pins Microsoft's PSEC FlatBuffers schema and its upstream
provenance from `microsoft/mxc`. The LF-normalized schema SHA-256 is
`736cacef37a6c97a0cb97c7132da462a3b51811fca8f1e4076cac51085dfa516`.
The Microsoft MIT license applies to the schema.

Wingman's Windows encoder in `sandbox_psec_windows.go` writes only the PSEC
1.0 fields, using FlatBuffers Go runtime 25.12.19. It preserves schema slot
numbers and defaults and omits the 1.1 enumeration and ingress fields.
The encoder is handwritten; no generated Go bindings or build-time compiler
are required. The adjacent provenance file describes MXC's upstream Rust
generation toolchain, not Wingman's Go encoder.

Before changing this contract, verify the schema hash and test native Windows
creation, both PowerShell variants, filesystem carveouts, network policy,
process-tree teardown, and unchanged host security descriptors. Do not mix
the older `SBOX` schema with this `PSEC` contract. Unsupported hosts fail
without an ACL fallback.
