package observe

import (
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
)

func TestObservationEqualityIsExact(t *testing.T) {
	base := Observation{ExitCode: 7, Stdout: []byte{'o', 0, 0xff, '\n'}, Stderr: []byte("err\n")}
	tests := []struct {
		name  string
		value Observation
	}{
		{"exit code", Observation{ExitCode: 8, Stdout: base.Stdout, Stderr: base.Stderr}},
		{"stdout", Observation{ExitCode: 7, Stdout: []byte{'o', 0, 0xfe, '\n'}, Stderr: base.Stderr}},
		{"stderr", Observation{ExitCode: 7, Stdout: base.Stdout, Stderr: []byte("err")}},
		{"swapped streams", Observation{ExitCode: 7, Stdout: base.Stderr, Stderr: base.Stdout}},
	}
	if !base.Equal(base) {
		t.Fatal("observation does not equal itself")
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if base.Equal(test.value) {
				t.Fatal("unequal observation compared equal")
			}
			if ClassID(base) == ClassID(test.value) {
				t.Fatal("distinct test observations unexpectedly share a class ID")
			}
		})
	}
}

func TestKnownClassDigest(t *testing.T) {
	value := Observation{ExitCode: 7, Stdout: []byte{0, 0xff}, Stderr: []byte("err\n")}
	const want = "obs-9b329a3ac1ec5905731539740bedbe62f4c963b1d94621d1eddb1a3abba8f2b6"
	if got := ClassID(value); got != want {
		t.Fatalf("ClassID() = %q, want %q", got, want)
	}
}

func TestSetMembershipOrderingAndCopies(t *testing.T) {
	set := NewSet()
	one := Observation{ExitCode: 2, Stdout: []byte("one"), Stderr: []byte{0xff}}
	two := Observation{ExitCode: 3, Stdout: []byte("two"), Stderr: []byte{0}}
	twoID, err := set.Add("case-0001", two)
	if err != nil {
		t.Fatal(err)
	}
	oneID, err := set.Add("case-0002", one)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Add("case-0003", two); err != nil {
		t.Fatal(err)
	}
	// Verify Add retained exact bytes rather than aliases.
	two.Stdout[0] = 'X'

	classes := set.Classes()
	if len(classes) != 2 {
		t.Fatalf("len(Classes()) = %d, want 2", len(classes))
	}
	if classes[0].ID > classes[1].ID {
		t.Fatalf("classes are not lexicographically sorted: %q, %q", classes[0].ID, classes[1].ID)
	}
	byID := map[string]Class{classes[0].ID: classes[0], classes[1].ID: classes[1]}
	if got := byID[twoID].CaseIDs; !reflect.DeepEqual(got, []string{"case-0001", "case-0003"}) {
		t.Fatalf("two case IDs = %#v", got)
	}
	if got := byID[oneID].CaseIDs; !reflect.DeepEqual(got, []string{"case-0002"}) {
		t.Fatalf("one case IDs = %#v", got)
	}
	if got := string(byID[twoID].Value.Stdout); got != "two" {
		t.Fatalf("stored stdout = %q, want two", got)
	}

	classes[0].CaseIDs[0] = "changed"
	if set.Classes()[0].CaseIDs[0] == "changed" {
		t.Fatal("Classes returned aliased case ID storage")
	}
}

func TestSetRejectsDigestCollision(t *testing.T) {
	zeroDigest := func([]byte) [sha256.Size]byte { return [sha256.Size]byte{} }
	set := newSet(zeroDigest)
	first := Observation{ExitCode: 1, Stdout: []byte("a")}
	if _, err := set.Add("case-0001", first); err != nil {
		t.Fatal(err)
	}
	if _, err := set.Add("case-0002", first); err != nil {
		t.Fatalf("equal observation rejected: %v", err)
	}
	_, err := set.Add("case-0003", Observation{ExitCode: 1, Stdout: []byte("b")})
	if !errors.Is(err, ErrDigestCollision) {
		t.Fatalf("collision error = %v, want %v", err, ErrDigestCollision)
	}
}
