## 1.0.0 — 2026-07-26

- Initial release of the Linux-first TELL failure-oracle auditor.
- Added the fixed `tell-default-v1` mutation profile and deterministic `tell-report-v1` JSON reports.
- Added direct process execution, bounded binary output capture, process-group cleanup, and sequential integration coverage.

TELL deterministically mutates a known-valid binary input and detects discrete differences in a target command’s rejection behavior.

TELL does not claim that generated candidates are malformed or provide general security, fuzz-coverage, side-channel, exploitability, memory-safety, or cryptographic guarantees.
