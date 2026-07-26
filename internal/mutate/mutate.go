// Package mutate implements TELL's deterministic mutation profile.
package mutate

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

const (
	// Profile is the name of the mutation profile implemented by this package.
	Profile = "tell-default-v1"
	// MaxPlannedCases is the largest number of candidates in the profile before
	// unchanged and duplicate candidates are removed.
	MaxPlannedCases = 29
)

const (
	kindTruncate = "truncate"
	kindDelete   = "delete"
	kindBitFlip  = "bit_flip"
	kindZero     = "zero"
	kindAppend   = "append"
)

// Descriptor is the stable, reportable description of one mutation. Fields
// that do not apply to a mutation are nil and therefore encode as JSON null.
type Descriptor struct {
	Kind       string  `json:"kind"`
	Offset     *int    `json:"offset"`
	NewLength  *int    `json:"new_length"`
	Mask       *uint8  `json:"mask"`
	DataBase64 *string `json:"data_base64"`
}

// Counts describes candidate planning and filtering for one profile run.
type Counts struct {
	Planned          int `json:"planned"`
	Unique           int `json:"unique"`
	SkippedUnchanged int `json:"skipped_unchanged"`
	SkippedDuplicate int `json:"skipped_duplicate"`
}

// Callback receives one retained candidate. candidate must be treated as
// read-only and is valid only until the callback returns. Callers that need to
// retain it must copy it. Callbacks are invoked sequentially.
type Callback func(id string, descriptor Descriptor, candidate []byte, byteLength int64, sha256Hex string) error

type candidateSpec struct {
	descriptor Descriptor
	offset     int
	newLength  int
	mask       byte
	data       []byte
}

// ForEach generates the profile in its fixed order. It executes callback once
// for every unique candidate that differs from seed, then discards that
// candidate before generating the next one. Full candidate inputs are never
// accumulated by this package.
func ForEach(seed []byte, callback Callback) (Counts, error) {
	return forEach(seed, nil, callback)
}

// indexDigestFunc is a test seam for exercising digest collisions in the
// deduplication index. A nil function uses each candidate's real SHA-256.
// Reported candidate hashes are always real SHA-256 values.
type indexDigestFunc func([]byte) [sha256.Size]byte

func forEach(seed []byte, indexDigest indexDigestFunc, callback Callback) (Counts, error) {
	if callback == nil {
		return Counts{}, fmt.Errorf("mutation callback is nil")
	}

	specs := plan(len(seed))
	counts := Counts{Planned: len(specs)}
	if counts.Planned > MaxPlannedCases {
		return Counts{}, fmt.Errorf("internal mutation plan exceeds maximum: %d", counts.Planned)
	}

	// Buckets retain descriptors only. If hashes collide, exact equality is
	// checked by comparing candidate bytes with the bytes described by every
	// previously retained descriptor in the bucket.
	seen := make(map[[sha256.Size]byte][]candidateSpec, len(specs))
	for _, spec := range specs {
		candidate := materialize(seed, spec)
		if bytes.Equal(candidate, seed) {
			counts.SkippedUnchanged++
			continue
		}

		realDigest := sha256.Sum256(candidate)
		indexKey := realDigest
		if indexDigest != nil {
			indexKey = indexDigest(candidate)
		}

		duplicate := false
		for _, previous := range seen[indexKey] {
			if describesBytes(seed, previous, candidate) {
				duplicate = true
				break
			}
		}
		if duplicate {
			counts.SkippedDuplicate++
			continue
		}

		seen[indexKey] = append(seen[indexKey], spec)
		counts.Unique++
		id := fmt.Sprintf("case-%04d", counts.Unique)
		if err := callback(id, spec.descriptor, candidate, int64(len(candidate)), hex.EncodeToString(realDigest[:])); err != nil {
			return counts, err
		}
	}

	return counts, nil
}

func plan(seedLength int) []candidateSpec {
	positions := selectedPositions(seedLength)
	specs := make([]candidateSpec, 0, MaxPlannedCases)

	for _, length := range positions {
		if length == seedLength {
			continue
		}
		newLength := length
		specs = append(specs, candidateSpec{
			descriptor: Descriptor{Kind: kindTruncate, NewLength: &newLength},
			newLength:  length,
		})
	}
	for _, offset := range positions {
		reportOffset := offset
		specs = append(specs, candidateSpec{
			descriptor: Descriptor{Kind: kindDelete, Offset: &reportOffset},
			offset:     offset,
		})
	}
	for _, mask := range []byte{0x01, 0x80} {
		for _, offset := range positions {
			reportOffset := offset
			reportMask := uint8(mask)
			specs = append(specs, candidateSpec{
				descriptor: Descriptor{Kind: kindBitFlip, Offset: &reportOffset, Mask: &reportMask},
				offset:     offset,
				mask:       mask,
			})
		}
	}
	for _, offset := range positions {
		reportOffset := offset
		specs = append(specs, candidateSpec{
			descriptor: Descriptor{Kind: kindZero, Offset: &reportOffset},
			offset:     offset,
		})
	}
	for _, data := range [][]byte{
		{0x00},
		{0xff},
		make([]byte, 8),
		bytes.Repeat([]byte{0xff}, 8),
	} {
		encoded := base64.StdEncoding.EncodeToString(data)
		specs = append(specs, candidateSpec{
			descriptor: Descriptor{Kind: kindAppend, DataBase64: &encoded},
			data:       data,
		})
	}

	return specs
}

func selectedPositions(seedLength int) []int {
	if seedLength == 0 {
		return nil
	}

	quarter, remainder := seedLength/4, seedLength%4
	threeQuarters := quarter*3 + (remainder*3)/4
	values := [...]int{0, quarter, seedLength / 2, threeQuarters, seedLength - 1}
	positions := make([]int, 0, len(values))
	for _, value := range values {
		if value < 0 || value >= seedLength {
			continue
		}
		if len(positions) == 0 || positions[len(positions)-1] != value {
			positions = append(positions, value)
		}
	}
	return positions
}

func materialize(seed []byte, spec candidateSpec) []byte {
	switch spec.descriptor.Kind {
	case kindTruncate:
		candidate := make([]byte, spec.newLength)
		copy(candidate, seed[:spec.newLength])
		return candidate
	case kindDelete:
		candidate := make([]byte, len(seed)-1)
		copy(candidate, seed[:spec.offset])
		copy(candidate[spec.offset:], seed[spec.offset+1:])
		return candidate
	case kindBitFlip:
		candidate := bytes.Clone(seed)
		candidate[spec.offset] ^= spec.mask
		return candidate
	case kindZero:
		candidate := bytes.Clone(seed)
		candidate[spec.offset] = 0
		return candidate
	case kindAppend:
		candidate := make([]byte, len(seed)+len(spec.data))
		copy(candidate, seed)
		copy(candidate[len(seed):], spec.data)
		return candidate
	default:
		panic("unknown mutation kind")
	}
}

// describesBytes checks exact equality without allocating another full
// candidate. This keeps live candidate storage bounded even if every index
// digest collides.
func describesBytes(seed []byte, spec candidateSpec, candidate []byte) bool {
	switch spec.descriptor.Kind {
	case kindTruncate:
		return len(candidate) == spec.newLength && bytes.Equal(candidate, seed[:spec.newLength])
	case kindDelete:
		return len(candidate) == len(seed)-1 &&
			bytes.Equal(candidate[:spec.offset], seed[:spec.offset]) &&
			bytes.Equal(candidate[spec.offset:], seed[spec.offset+1:])
	case kindBitFlip:
		return len(candidate) == len(seed) &&
			bytes.Equal(candidate[:spec.offset], seed[:spec.offset]) &&
			candidate[spec.offset] == seed[spec.offset]^spec.mask &&
			bytes.Equal(candidate[spec.offset+1:], seed[spec.offset+1:])
	case kindZero:
		return len(candidate) == len(seed) &&
			bytes.Equal(candidate[:spec.offset], seed[:spec.offset]) &&
			candidate[spec.offset] == 0 &&
			bytes.Equal(candidate[spec.offset+1:], seed[spec.offset+1:])
	case kindAppend:
		return len(candidate) == len(seed)+len(spec.data) &&
			bytes.Equal(candidate[:len(seed)], seed) &&
			bytes.Equal(candidate[len(seed):], spec.data)
	default:
		return false
	}
}
