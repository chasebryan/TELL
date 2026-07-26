// Package report defines and writes the deterministic tell-report-v1 format.
package report

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	Schema      = "tell-report-v1"
	ToolName    = "tell"
	ToolVersion = "1.0.0"
)

type Document struct {
	Schema           string           `json:"schema"`
	Tool             Tool             `json:"tool"`
	Verdict          string           `json:"verdict"`
	Reasons          []string         `json:"reasons"`
	Configuration    Configuration    `json:"configuration"`
	Command          []string         `json:"command"`
	Seed             Input            `json:"seed"`
	Baseline         Execution        `json:"baseline"`
	MutationCounts   MutationCounts   `json:"mutation_counts"`
	Cases            []Case           `json:"cases"`
	RejectionClasses []RejectionClass `json:"rejection_classes"`
}

type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Configuration struct {
	Transport               string `json:"transport"`
	MutationProfile         string `json:"mutation_profile"`
	TimeoutMS               int64  `json:"timeout_ms"`
	MaxOutputBytesPerStream int    `json:"max_output_bytes_per_stream"`
}

type Input struct {
	ByteLength int    `json:"byte_length"`
	SHA256Hex  string `json:"sha256_hex"`
}

type Execution struct {
	Outcome     string      `json:"outcome"`
	Termination Termination `json:"termination"`
	Stdout      Stream      `json:"stdout"`
	Stderr      Stream      `json:"stderr"`
}

type Termination struct {
	Kind         string `json:"kind"`
	ExitCode     *int   `json:"exit_code"`
	SignalNumber *int   `json:"signal_number"`
}

type Stream struct {
	CapturedByteLength int    `json:"captured_byte_length"`
	SHA256Hex          string `json:"sha256_hex"`
	DataBase64         string `json:"data_base64"`
	Truncated          bool   `json:"truncated"`
}

type MutationCounts struct {
	Planned          int `json:"planned"`
	Unique           int `json:"unique"`
	SkippedUnchanged int `json:"skipped_unchanged"`
	SkippedDuplicate int `json:"skipped_duplicate"`
}

type Case struct {
	ID                 string    `json:"id"`
	Mutation           Mutation  `json:"mutation"`
	Input              Input     `json:"input"`
	Execution          Execution `json:"execution"`
	ObservationClassID *string   `json:"observation_class_id"`
}

type Mutation struct {
	Kind       string  `json:"kind"`
	Offset     *int    `json:"offset"`
	NewLength  *int    `json:"new_length"`
	Mask       *uint8  `json:"mask"`
	DataBase64 *string `json:"data_base64"`
}

type RejectionClass struct {
	ID               string   `json:"id"`
	ExitCode         int      `json:"exit_code"`
	StdoutByteLength int      `json:"stdout_byte_length"`
	StdoutSHA256Hex  string   `json:"stdout_sha256_hex"`
	StderrByteLength int      `json:"stderr_byte_length"`
	StderrSHA256Hex  string   `json:"stderr_sha256_hex"`
	CaseIDs          []string `json:"case_ids"`
}

// NewDocument initializes fixed schema and tool fields and all required arrays.
func NewDocument() Document {
	return Document{
		Schema: Schema,
		Tool: Tool{
			Name:    ToolName,
			Version: ToolVersion,
		},
		Reasons:          []string{},
		Command:          []string{},
		Cases:            []Case{},
		RejectionClasses: []RejectionClass{},
	}
}

// NewInput returns length and lowercase SHA-256 metadata without retaining the
// input bytes.
func NewInput(data []byte) Input {
	digest := sha256.Sum256(data)
	return Input{ByteLength: len(data), SHA256Hex: hex.EncodeToString(digest[:])}
}

// NewStream encodes captured bytes losslessly using padded standard Base64.
func NewStream(data []byte, truncated bool) Stream {
	digest := sha256.Sum256(data)
	return Stream{
		CapturedByteLength: len(data),
		SHA256Hex:          hex.EncodeToString(digest[:]),
		DataBase64:         base64.StdEncoding.EncodeToString(data),
		Truncated:          truncated,
	}
}

// Marshal produces deterministic two-space-indented JSON with HTML escaping
// disabled and exactly one trailing newline.
func Marshal(document Document) ([]byte, error) {
	ensureArrays(&document)
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return nil, fmt.Errorf("encode report: %w", err)
	}
	return output.Bytes(), nil
}

func ensureArrays(document *Document) {
	if document.Reasons == nil {
		document.Reasons = []string{}
	}
	if document.Command == nil {
		document.Command = []string{}
	}
	if document.Cases == nil {
		document.Cases = []Case{}
	}
	if document.RejectionClasses == nil {
		document.RejectionClasses = []RejectionClass{}
	}
	for i := range document.RejectionClasses {
		if document.RejectionClasses[i].CaseIDs == nil {
			document.RejectionClasses[i].CaseIDs = []string{}
		}
	}
}

// WriteAtomic writes a private same-directory temporary file and renames it
// only after the complete report has been written, synced, and closed.
func WriteAtomic(path string, document Document) (err error) {
	return WriteAtomicContext(context.Background(), path, document)
}

// WriteAtomicContext is WriteAtomic with interruption checks throughout
// serialization and temporary-file publication. Cancellation before the final
// rename removes the temporary file and leaves the destination untouched.
func WriteAtomicContext(ctx context.Context, path string, document Document) (err error) {
	if err := contextError(ctx); err != nil {
		return err
	}
	data, err := Marshal(document)
	if err != nil {
		return err
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	if path == "" {
		return errors.New("report path is empty")
	}
	directory := filepath.Dir(path)
	base := filepath.Base(path)
	temporary, err := os.CreateTemp(directory, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary report: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()

	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("set temporary report permissions: %w", err)
	}
	if err := writeContext(ctx, temporary, data); err != nil {
		return fmt.Errorf("write temporary report: %w", err)
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary report: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary report: %w", err)
	}
	// Rename is the publication linearization point. A cancellation observed
	// before it leaves the previous destination in place.
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish report: %w", err)
	}
	return nil
}

func writeContext(ctx context.Context, writer *os.File, data []byte) error {
	const chunkSize = 64 * 1024
	for len(data) > 0 {
		if err := contextError(ctx); err != nil {
			return err
		}
		chunk := data
		if len(chunk) > chunkSize {
			chunk = chunk[:chunkSize]
		}
		written, err := writer.Write(chunk)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func contextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("report publication canceled: %w", err)
	}
	return nil
}
