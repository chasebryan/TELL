package report

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func intPointer(value int) *int          { return &value }
func stringPointer(value string) *string { return &value }

func TestGoldenReportBytes(t *testing.T) {
	document := NewDocument()
	document.Verdict = "fail"
	document.Reasons = []string{"accepted_mutation"}
	document.Configuration = Configuration{
		Transport:               "stdin",
		MutationProfile:         "tell-default-v1",
		TimeoutMS:               2000,
		MaxOutputBytesPerStream: 65536,
	}
	document.Command = []string{"fixture", "<&>"}
	document.Seed = NewInput(nil)
	document.Baseline = Execution{
		Outcome: "accepted",
		Termination: Termination{
			Kind:     "exit",
			ExitCode: intPointer(0),
		},
		Stdout: NewStream([]byte{0, 0xff}, false),
		Stderr: NewStream([]byte("err\n"), false),
	}
	document.MutationCounts = MutationCounts{Planned: 1, Unique: 1}
	document.Cases = []Case{{
		ID: "case-0001",
		Mutation: Mutation{
			Kind:       "append",
			DataBase64: stringPointer("AA=="),
		},
		Input: NewInput(nil),
		Execution: Execution{
			Outcome: "accepted",
			Termination: Termination{
				Kind:     "exit",
				ExitCode: intPointer(0),
			},
			Stdout: NewStream(nil, false),
			Stderr: NewStream(nil, false),
		},
	}}

	got, err := Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{
  "schema": "tell-report-v1",
  "tool": {
    "name": "tell",
    "version": "1.0.0"
  },
  "verdict": "fail",
  "reasons": [
    "accepted_mutation"
  ],
  "configuration": {
    "transport": "stdin",
    "mutation_profile": "tell-default-v1",
    "timeout_ms": 2000,
    "max_output_bytes_per_stream": 65536
  },
  "command": [
    "fixture",
    "<&>"
  ],
  "seed": {
    "byte_length": 0,
    "sha256_hex": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
  },
  "baseline": {
    "outcome": "accepted",
    "termination": {
      "kind": "exit",
      "exit_code": 0,
      "signal_number": null
    },
    "stdout": {
      "captured_byte_length": 2,
      "sha256_hex": "06eb7d6a69ee19e5fbdf749018d3d2abfa04bcbd1365db312eb86dc7169389b8",
      "data_base64": "AP8=",
      "truncated": false
    },
    "stderr": {
      "captured_byte_length": 4,
      "sha256_hex": "2ccde4875ec595757efdf23d7b1336fcd69cf0fb869310b12a0d219c52817b20",
      "data_base64": "ZXJyCg==",
      "truncated": false
    }
  },
  "mutation_counts": {
    "planned": 1,
    "unique": 1,
    "skipped_unchanged": 0,
    "skipped_duplicate": 0
  },
  "cases": [
    {
      "id": "case-0001",
      "mutation": {
        "kind": "append",
        "offset": null,
        "new_length": null,
        "mask": null,
        "data_base64": "AA=="
      },
      "input": {
        "byte_length": 0,
        "sha256_hex": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
      },
      "execution": {
        "outcome": "accepted",
        "termination": {
          "kind": "exit",
          "exit_code": 0,
          "signal_number": null
        },
        "stdout": {
          "captured_byte_length": 0,
          "sha256_hex": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
          "data_base64": "",
          "truncated": false
        },
        "stderr": {
          "captured_byte_length": 0,
          "sha256_hex": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
          "data_base64": "",
          "truncated": false
        }
      },
      "observation_class_id": null
    }
  ],
  "rejection_classes": []
}
`
	if string(got) != want {
		t.Fatalf("golden report mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if bytes.Contains(got, []byte(`\u003c`)) || bytes.Contains(got, []byte(`\u0026`)) {
		t.Fatal("HTML escaping was enabled")
	}
	if !bytes.HasSuffix(got, []byte("\n")) || bytes.HasSuffix(got, []byte("\n\n")) {
		t.Fatal("report must end with exactly one newline")
	}

	again, err := Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, again) {
		t.Fatal("two marshals produced different bytes")
	}
}

func TestMarshalRequiredArraysAreNeverNull(t *testing.T) {
	data, err := Marshal(Document{})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"reasons": []`, `"command": []`, `"cases": []`, `"rejection_classes": []`} {
		if !bytes.Contains(data, []byte(field)) {
			t.Fatalf("report does not contain %s:\n%s", field, data)
		}
	}
}

func TestWriteAtomicIsPrivateAndCleansTemporaryFiles(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "report.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	document := NewDocument()
	if err := WriteAtomic(path, document); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("report mode = %#o, want 0600", got)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "report.json" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("unexpected files after publication: %s", strings.Join(names, ", "))
	}
}

func TestWriteFailureLeavesDestinationUntouchedAndCleansTemporaryFile(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "report.json")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(destination, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(destination, NewDocument()); err == nil {
		t.Fatal("WriteAtomic succeeded when destination was a non-empty directory")
	}
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep" {
		t.Fatalf("sentinel = %q, want keep", data)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "report.json" {
		t.Fatalf("temporary artifact remained: %#v", entries)
	}
}

func TestCancellationDuringWriteLeavesDestinationUntouched(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "report.json")
	if err := os.WriteFile(destination, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	document := NewDocument()
	document.Command = []string{strings.Repeat("x", 200_000)}
	ctx := &cancelAfterChecks{cancelAt: 4}
	err := WriteAtomicContext(ctx, destination, document)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteAtomicContext error = %v, want context cancellation", err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing" {
		t.Fatalf("destination = %q, want existing", data)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "report.json" {
		t.Fatalf("temporary artifact remained: %#v", entries)
	}
}

type cancelAfterChecks struct {
	calls    int
	cancelAt int
	canceled bool
}

func (c *cancelAfterChecks) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterChecks) Done() <-chan struct{}       { return nil }
func (c *cancelAfterChecks) Value(any) any               { return nil }

func (c *cancelAfterChecks) Err() error {
	c.calls++
	if c.canceled || c.calls >= c.cancelAt {
		c.canceled = true
		return context.Canceled
	}
	return nil
}
