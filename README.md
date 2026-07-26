# TELL
Malformed inputs should not tell.

TELL is a Linux-first black-box failure-oracle auditor that checks whether a target command rejects a deterministic set of presumed-invalid binary mutants uniformly.

TELL deterministically mutates a known-valid binary input and detects discrete differences in a target command’s rejection behavior.

## Build and install

TELL requires Linux and Go 1.26.x. Production builds use only the Go standard library and disable cgo.

```sh
git clone https://github.com/chasebryan/TELL.git
cd TELL
CGO_ENABLED=0 go build -trimpath -buildvcs=false -o tell ./cmd/tell
```

To install from a checkout into the active Go binary directory:

```sh
CGO_ENABLED=0 go install -trimpath ./cmd/tell
```

## Run an audit

```sh
tell run \
  --seed valid-message.bin \
  --stdin \
  --timeout 2s \
  --max-output-bytes 65536 \
  --report tell-report-v1.json \
  -- ./decoder --strict "literal argument"
```

`--seed`, `--stdin`, the `--` separator, and a command are required. Everything after `--` is passed as the command's literal argument vector. TELL executes the target directly; it does not invoke a shell or perform expansion, interpolation, globbing, redirection, or transcoding. The caller's working directory and environment are inherited.

## PASS rule

An audit passes only when all of the following are true:

1. The baseline seed exits normally with code `0`.
2. At least one unique mutation different from the seed is executed.
3. Every mutation exits normally with a nonzero code.
4. Every mutation has exactly the same observation tuple: `(exit code, exact stdout bytes, exact stderr bytes)`.

An accepted mutation, a differing rejection, a signal, a timeout, or output overflow makes a completed audit fail. A launch, capture, cleanup, report-writing, or internal error makes the audit incomplete. Ordinary audit failures do not stop the remaining mutation cases; infrastructure failures do.

## Exit codes

| Code | Meaning |
| ---: | --- |
| `0` | Completed audit: PASS |
| `1` | Completed audit: FAIL |
| `2` | Usage, setup, invalid baseline, interruption, launch, runner, cleanup, report-write, unsupported-platform, or internal error |

## Safety

> **TELL is not a sandbox.** The target runs with the caller's privileges and may perform filesystem, network, or other side effects. TELL uses Linux process groups for cleanup, but process groups do not prevent a deliberately hostile descendant from escaping with `setsid` or `setpgid`. Audit only commands and inputs you are prepared to execute in the current environment.

## Reports and sensitive data

> Reports contain the complete command argument vector and the target's raw stdout and stderr, which may be sensitive. Review a report before publishing or sharing it.

The deterministic JSON schema is named `tell-report-v1`. Reports contain seed and mutant length/hash metadata, never the input bytes themselves. File reports are atomically published with mode `0600`; incomplete audits do not replace an existing report. Report determinism excludes target nondeterminism: v1 executes each input once, so nondeterministic or environment-dependent target output can create apparent differences.

## Fixed v1 limits

| Limit | Value |
| --- | ---: |
| Seed size | 16 MiB |
| Default timeout | 2 seconds |
| Allowed timeout | 10 milliseconds through 60 seconds, in whole milliseconds |
| Default output cap | 65,536 bytes per stream |
| Maximum output cap | 1,048,576 bytes per stream |
| Pipe cleanup grace | 250 milliseconds |
| Maximum planned cases | 29 |

v1 supports stdin input only, runs the baseline and cases sequentially, and uses the fixed `tell-default-v1` mutation profile. The empty seed is valid. Non-Linux builds return an unsupported-platform error.

## Limitations and nonclaims

TELL does not prove:

- That generated candidates are actually malformed.
- That all malformed inputs are rejected.
- Constant-time behavior or absence of side channels.
- Exploitability or vulnerability severity.
- Memory safety.
- Cryptographic correctness.
- Complete input-space or fuzz coverage.
- General security.

The fixed mutations are deterministic, presumed-invalid probes—not random, exhaustive, grammar-aware, coverage-guided, or cumulative fuzzing. TELL does not isolate the target, and its observation equivalence excludes time, resource use, process identity, scheduling, and side channels.

## Development

```sh
make format
make test
make race
make check
make reproducible
```

The normative v1 behavior, schema, limits, and compatibility policy are specified in [SPEC.md](SPEC.md).
