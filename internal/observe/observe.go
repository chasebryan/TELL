// Package observe defines exact rejection observations and their deterministic
// equivalence-class identifiers.
package observe

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sort"
)

const classDomain = "TELL\x00OBSERVATION\x00V1\x00"

// ErrDigestCollision is returned rather than merging unequal observations
// that happen to have the same class digest.
var ErrDigestCollision = errors.New("observation class digest collision")

// Observation is the complete equality input for a normally rejected case.
type Observation struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// Equal compares the exit code and both streams byte for byte.
func (o Observation) Equal(other Observation) bool {
	return o.ExitCode == other.ExitCode &&
		bytes.Equal(o.Stdout, other.Stdout) &&
		bytes.Equal(o.Stderr, other.Stderr)
}

// Class is one exact rejection equivalence class.
type Class struct {
	ID      string
	Value   Observation
	CaseIDs []string
}

// Set accumulates rejection classes. It is not safe for concurrent use; TELL
// intentionally executes cases sequentially.
type Set struct {
	hasher  func([]byte) [sha256.Size]byte
	byID    map[string]int
	classes []Class
}

// NewSet creates an empty class set using SHA-256.
func NewSet() *Set {
	return newSet(sha256.Sum256)
}

func newSet(hasher func([]byte) [sha256.Size]byte) *Set {
	return &Set{
		hasher: hasher,
		byID:   make(map[string]int),
	}
}

// Add establishes membership through exact equality and returns the class ID.
func (s *Set) Add(caseID string, value Observation) (string, error) {
	preimage := encode(value)
	digest := s.hasher(preimage)
	id := "obs-" + hex.EncodeToString(digest[:])
	if index, ok := s.byID[id]; ok {
		if !s.classes[index].Value.Equal(value) {
			return "", ErrDigestCollision
		}
		s.classes[index].CaseIDs = append(s.classes[index].CaseIDs, caseID)
		return id, nil
	}

	// Copy output because callers may reuse their capture buffers.
	value.Stdout = append([]byte(nil), value.Stdout...)
	value.Stderr = append([]byte(nil), value.Stderr...)
	s.byID[id] = len(s.classes)
	s.classes = append(s.classes, Class{
		ID:      id,
		Value:   value,
		CaseIDs: []string{caseID},
	})
	return id, nil
}

// Classes returns a copy sorted lexicographically by class ID. Case IDs retain
// insertion (and therefore mutation) order.
func (s *Set) Classes() []Class {
	classes := make([]Class, len(s.classes))
	for i := range s.classes {
		classes[i] = Class{
			ID:      s.classes[i].ID,
			Value:   s.classes[i].Value,
			CaseIDs: append([]string(nil), s.classes[i].CaseIDs...),
		}
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i].ID < classes[j].ID })
	return classes
}

// ClassID returns the deterministic identifier for one observation.
func ClassID(value Observation) string {
	digest := sha256.Sum256(encode(value))
	return "obs-" + hex.EncodeToString(digest[:])
}

func encode(value Observation) []byte {
	// The maximum captured streams are bounded by the runner. The append-based
	// encoding keeps the exact bytes visible to the digest and is unambiguous.
	preimage := make([]byte, 0, len(classDomain)+4+8+len(value.Stdout)+8+len(value.Stderr))
	preimage = append(preimage, classDomain...)
	var numbers [8]byte
	binary.BigEndian.PutUint32(numbers[:4], uint32(value.ExitCode))
	preimage = append(preimage, numbers[:4]...)
	binary.BigEndian.PutUint64(numbers[:], uint64(len(value.Stdout)))
	preimage = append(preimage, numbers[:]...)
	preimage = append(preimage, value.Stdout...)
	binary.BigEndian.PutUint64(numbers[:], uint64(len(value.Stderr)))
	preimage = append(preimage, numbers[:]...)
	preimage = append(preimage, value.Stderr...)
	return preimage
}
