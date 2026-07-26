package mutate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"testing"
)

type collectedCase struct {
	id         string
	descriptor Descriptor
	input      []byte
	length     int64
	hash       string
}

func collect(t *testing.T, seed []byte) (Counts, []collectedCase) {
	t.Helper()
	var cases []collectedCase
	counts, err := ForEach(seed, func(id string, descriptor Descriptor, candidate []byte, length int64, hash string) error {
		cases = append(cases, collectedCase{
			id:         id,
			descriptor: descriptor,
			input:      bytes.Clone(candidate),
			length:     length,
			hash:       hash,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("ForEach() error = %v", err)
	}
	return counts, cases
}

func TestEmptySeedExactProfile(t *testing.T) {
	counts, cases := collect(t, nil)
	if want := (Counts{Planned: 4, Unique: 4}); counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}

	wantInputs := [][]byte{
		{0x00},
		{0xff},
		make([]byte, 8),
		bytes.Repeat([]byte{0xff}, 8),
	}
	wantData := []string{"AA==", "/w==", "AAAAAAAAAAA=", "//////////8="}
	for i := range cases {
		assertCaseMetadata(t, cases[i], i, wantInputs[i])
		assertDescriptor(t, cases[i].descriptor, kindAppend, nil, nil, nil, &wantData[i])
	}

	wantHashes := []string{
		"6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d",
		"a8100ae6aa1940d0b663bb31cd466142ebbdbd5187131b92d93818987832eb89",
		"af5570f5a1810b7af78caf4bc70a660f0df51e42baf91d4de5b2328de0e83dfc",
		"12a3ae445661ce5dee78d0650d33362dec29c4f82af05e7e57fb595bbbacf0ca",
	}
	for i := range cases {
		if cases[i].hash != wantHashes[i] {
			t.Errorf("case %d hash = %q, want %q", i, cases[i].hash, wantHashes[i])
		}
	}
}

func TestOneByteSeedNoOpAndDuplicateElimination(t *testing.T) {
	counts, cases := collect(t, []byte{0x00})
	wantCounts := Counts{Planned: 9, Unique: 7, SkippedUnchanged: 1, SkippedDuplicate: 1}
	if counts != wantCounts {
		t.Fatalf("counts = %+v, want %+v", counts, wantCounts)
	}

	want := []struct {
		kind       string
		offset     *int
		newLength  *int
		mask       *uint8
		dataBase64 *string
		input      []byte
	}{
		{kind: kindTruncate, newLength: integer(0), input: []byte{}},
		{kind: kindBitFlip, offset: integer(0), mask: u8(0x01), input: []byte{0x01}},
		{kind: kindBitFlip, offset: integer(0), mask: u8(0x80), input: []byte{0x80}},
		{kind: kindAppend, dataBase64: str("AA=="), input: []byte{0x00, 0x00}},
		{kind: kindAppend, dataBase64: str("/w=="), input: []byte{0x00, 0xff}},
		{kind: kindAppend, dataBase64: str("AAAAAAAAAAA="), input: make([]byte, 9)},
		{kind: kindAppend, dataBase64: str("//////////8="), input: append([]byte{0x00}, bytes.Repeat([]byte{0xff}, 8)...)},
	}
	for i := range want {
		assertCaseMetadata(t, cases[i], i, want[i].input)
		assertDescriptor(t, cases[i].descriptor, want[i].kind, want[i].offset, want[i].newLength, want[i].mask, want[i].dataBase64)
	}
	if cases[0].hash != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("empty candidate hash = %q", cases[0].hash)
	}
}

func TestAllZeroSeedExactOrderAndFiltering(t *testing.T) {
	seed := make([]byte, 4)
	counts, cases := collect(t, seed)
	wantCounts := Counts{Planned: 24, Unique: 16, SkippedUnchanged: 4, SkippedDuplicate: 4}
	if counts != wantCounts {
		t.Fatalf("counts = %+v, want %+v", counts, wantCounts)
	}

	var wantInputs [][]byte
	var wantDescriptors []Descriptor
	for length := 0; length < 4; length++ {
		wantInputs = append(wantInputs, make([]byte, length))
		wantDescriptors = append(wantDescriptors, Descriptor{Kind: kindTruncate, NewLength: integer(length)})
	}
	for _, mask := range []byte{0x01, 0x80} {
		for offset := 0; offset < 4; offset++ {
			input := make([]byte, 4)
			input[offset] = mask
			wantInputs = append(wantInputs, input)
			wantDescriptors = append(wantDescriptors, Descriptor{Kind: kindBitFlip, Offset: integer(offset), Mask: u8(mask)})
		}
	}
	appendData := [][]byte{{0}, {0xff}, make([]byte, 8), bytes.Repeat([]byte{0xff}, 8)}
	for _, data := range appendData {
		wantInputs = append(wantInputs, append(bytes.Clone(seed), data...))
		encoded := map[int]string{1: "AA==", 8: "AAAAAAAAAAA="}[len(data)]
		if data[0] == 0xff {
			encoded = map[int]string{1: "/w==", 8: "//////////8="}[len(data)]
		}
		wantDescriptors = append(wantDescriptors, Descriptor{Kind: kindAppend, DataBase64: str(encoded)})
	}

	if len(cases) != len(wantInputs) {
		t.Fatalf("case count = %d, want %d", len(cases), len(wantInputs))
	}
	for i := range cases {
		assertCaseMetadata(t, cases[i], i, wantInputs[i])
		if !reflect.DeepEqual(cases[i].descriptor, wantDescriptors[i]) {
			t.Errorf("case %d descriptor = %#v, want %#v", i, cases[i].descriptor, wantDescriptors[i])
		}
	}
}

func TestOrdinarySeedExactCandidateOrder(t *testing.T) {
	seed := []byte("abcdefgh")
	counts, cases := collect(t, seed)
	wantCounts := Counts{Planned: MaxPlannedCases, Unique: 28, SkippedDuplicate: 1}
	if counts != wantCounts {
		t.Fatalf("counts = %+v, want %+v", counts, wantCounts)
	}

	var wantInputs [][]byte
	var wantDescriptors []Descriptor
	positions := []int{0, 2, 4, 6, 7}
	for _, length := range positions {
		wantInputs = append(wantInputs, bytes.Clone(seed[:length]))
		wantDescriptors = append(wantDescriptors, Descriptor{Kind: kindTruncate, NewLength: integer(length)})
	}
	// Deleting the final byte duplicates truncation to length seven and is not retained.
	for _, offset := range positions[:4] {
		input := append(bytes.Clone(seed[:offset]), seed[offset+1:]...)
		wantInputs = append(wantInputs, input)
		wantDescriptors = append(wantDescriptors, Descriptor{Kind: kindDelete, Offset: integer(offset)})
	}
	for _, mask := range []byte{0x01, 0x80} {
		for _, offset := range positions {
			input := bytes.Clone(seed)
			input[offset] ^= mask
			wantInputs = append(wantInputs, input)
			wantDescriptors = append(wantDescriptors, Descriptor{Kind: kindBitFlip, Offset: integer(offset), Mask: u8(mask)})
		}
	}
	for _, offset := range positions {
		input := bytes.Clone(seed)
		input[offset] = 0
		wantInputs = append(wantInputs, input)
		wantDescriptors = append(wantDescriptors, Descriptor{Kind: kindZero, Offset: integer(offset)})
	}
	for _, item := range []struct {
		data    []byte
		encoded string
	}{
		{[]byte{0x00}, "AA=="},
		{[]byte{0xff}, "/w=="},
		{make([]byte, 8), "AAAAAAAAAAA="},
		{bytes.Repeat([]byte{0xff}, 8), "//////////8="},
	} {
		wantInputs = append(wantInputs, append(bytes.Clone(seed), item.data...))
		wantDescriptors = append(wantDescriptors, Descriptor{Kind: kindAppend, DataBase64: str(item.encoded)})
	}

	if len(cases) != len(wantInputs) {
		t.Fatalf("case count = %d, want %d", len(cases), len(wantInputs))
	}
	for i := range cases {
		assertCaseMetadata(t, cases[i], i, wantInputs[i])
		if !reflect.DeepEqual(cases[i].descriptor, wantDescriptors[i]) {
			t.Errorf("case %d descriptor = %#v, want %#v", i, cases[i].descriptor, wantDescriptors[i])
		}
	}
}

func TestEveryMutationStartsFromOriginalSeed(t *testing.T) {
	seed := []byte("abcdefgh")
	_, cases := collect(t, seed)
	for _, c := range cases {
		if c.descriptor.Kind != kindBitFlip {
			continue
		}
		offset := int(*c.descriptor.Offset)
		for i := range seed {
			want := seed[i]
			if i == offset {
				want ^= *c.descriptor.Mask
			}
			if c.input[i] != want {
				t.Fatalf("%s is cumulative: byte %d = %#x, want %#x", c.id, i, c.input[i], want)
			}
		}
	}
}

func TestDigestCollisionIndexStillUsesExactEquality(t *testing.T) {
	seed := []byte("abcde")
	var cases []collectedCase
	counts, err := forEach(seed, func([]byte) [sha256.Size]byte { return [sha256.Size]byte{} }, func(id string, descriptor Descriptor, candidate []byte, length int64, hash string) error {
		cases = append(cases, collectedCase{id: id, descriptor: descriptor, input: bytes.Clone(candidate), length: length, hash: hash})
		return nil
	})
	if err != nil {
		t.Fatalf("forEach() error = %v", err)
	}
	wantCounts := Counts{Planned: MaxPlannedCases, Unique: 28, SkippedDuplicate: 1}
	if counts != wantCounts {
		t.Fatalf("counts = %+v, want %+v", counts, wantCounts)
	}
	seen := make(map[string]struct{}, len(cases))
	for _, c := range cases {
		key := string(c.input)
		if _, exists := seen[key]; exists {
			t.Fatalf("duplicate input retained for %s", c.id)
		}
		seen[key] = struct{}{}
		if c.hash != hashHex(c.input) {
			t.Errorf("%s reported forced index digest instead of SHA-256", c.id)
		}
	}
}

func TestMaximumPlannedCases(t *testing.T) {
	for length := 0; length <= 64; length++ {
		counts, _ := collect(t, make([]byte, length))
		if counts.Planned > MaxPlannedCases {
			t.Fatalf("length %d planned %d cases, maximum is %d", length, counts.Planned, MaxPlannedCases)
		}
	}
	counts, _ := collect(t, []byte{1, 2, 3, 4, 5})
	if counts.Planned != MaxPlannedCases {
		t.Fatalf("five-byte seed planned %d cases, want %d", counts.Planned, MaxPlannedCases)
	}
}

func TestStreamingCallbackIsSequentialAndCandidatesAreIndependent(t *testing.T) {
	seed := bytes.Repeat([]byte{0x5a}, 1<<20)
	callbackActive := false
	callbacks := 0
	_, err := ForEach(seed, func(_ string, _ Descriptor, candidate []byte, _ int64, _ string) error {
		if callbackActive {
			t.Fatal("callbacks overlapped")
		}
		callbackActive = true
		callbacks++
		if len(candidate) > 0 {
			candidate[0] ^= 0xff
		}
		callbackActive = false
		return nil
	})
	if err != nil {
		t.Fatalf("ForEach() error = %v", err)
	}
	if callbacks == 0 {
		t.Fatal("no callbacks")
	}
	if !bytes.Equal(seed, bytes.Repeat([]byte{0x5a}, len(seed))) {
		t.Fatal("callback mutation changed the original seed")
	}
	// The package retains only descriptors in its deduplication index; the
	// large candidate is materialized only for the duration of each callback.
}

func TestLiveCandidateAllocationIsBounded(t *testing.T) {
	const seedBytes = 4 << 20
	seed := bytes.Repeat([]byte{0x5a}, seedBytes)
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	peak := baseline.HeapAlloc

	counts, err := ForEach(seed, func(_ string, _ Descriptor, candidate []byte, _ int64, _ string) error {
		// Force unreachable candidates to be collected while keeping the current
		// callback-scoped candidate live through the measurement.
		runtime.GC()
		var current runtime.MemStats
		runtime.ReadMemStats(&current)
		if current.HeapAlloc > peak {
			peak = current.HeapAlloc
		}
		runtime.KeepAlive(candidate)
		return nil
	})
	runtime.KeepAlive(seed)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Unique == 0 {
		t.Fatal("no mutation candidates were measured")
	}

	// A seed plus one current candidate requires roughly 8 MiB. Allow a wide
	// margin for runtime/package bookkeeping, while remaining far below the
	// memory needed to retain the profile's many full-size candidates.
	const maximumAdditionalLiveHeap = 5 * seedBytes
	if peak > baseline.HeapAlloc+maximumAdditionalLiveHeap {
		t.Fatalf("additional live heap = %d bytes, want <= %d (baseline=%d peak=%d)", peak-baseline.HeapAlloc, maximumAdditionalLiveHeap, baseline.HeapAlloc, peak)
	}
}

func TestCallbackErrorStopsIteration(t *testing.T) {
	wantErr := errors.New("stop")
	calls := 0
	counts, err := ForEach([]byte("abcde"), func(string, Descriptor, []byte, int64, string) error {
		calls++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if calls != 1 || counts.Unique != 1 || counts.Planned != MaxPlannedCases {
		t.Fatalf("calls = %d, counts = %+v", calls, counts)
	}
}

func TestNilCallbackRejected(t *testing.T) {
	counts, err := ForEach([]byte("seed"), nil)
	if err == nil {
		t.Fatal("ForEach() error = nil")
	}
	if counts != (Counts{}) {
		t.Fatalf("counts = %+v, want zero", counts)
	}
}

func TestDescriptorJSONFieldOrderAndNulls(t *testing.T) {
	encoded, err := json.Marshal(Descriptor{Kind: kindDelete, Offset: integer(2)})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	want := `{"kind":"delete","offset":2,"new_length":null,"mask":null,"data_base64":null}`
	if string(encoded) != want {
		t.Fatalf("descriptor JSON = %s, want %s", encoded, want)
	}
}

func assertCaseMetadata(t *testing.T, got collectedCase, index int, wantInput []byte) {
	t.Helper()
	wantID := fmt.Sprintf("case-%04d", index+1)
	if got.id != wantID {
		t.Errorf("id = %q, want %q", got.id, wantID)
	}
	if !bytes.Equal(got.input, wantInput) {
		t.Errorf("%s input = %x, want %x", got.id, got.input, wantInput)
	}
	if got.length != int64(len(wantInput)) {
		t.Errorf("%s length = %d, want %d", got.id, got.length, len(wantInput))
	}
	if got.hash != hashHex(wantInput) {
		t.Errorf("%s hash = %q, want %q", got.id, got.hash, hashHex(wantInput))
	}
}

func assertDescriptor(t *testing.T, got Descriptor, kind string, offset, newLength *int, mask *uint8, dataBase64 *string) {
	t.Helper()
	want := Descriptor{Kind: kind, Offset: offset, NewLength: newLength, Mask: mask, DataBase64: dataBase64}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("descriptor = %#v, want %#v", got, want)
	}
}

func hashHex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func integer(value int) *int   { return &value }
func u8(value uint8) *uint8    { return &value }
func str(value string) *string { return &value }
