package checksum_test

import (
	"strings"
	"testing"

	"github.com/anywaye/darkroom/internal/checksum"
)

// The published FIPS 180-4 vectors. Hard-coded rather than computed, because a
// test that hashes with the same code it is testing proves only that the code is
// self-consistent. `AGENTS.md` is blunt about the class of test that does not
// count: "a test that silently passes without proving anything is not [honest]."
func TestSHA256HexMatchesThePublishedVectors(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"abc", "abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{
			"the 56-byte two-block vector",
			"abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq",
			"248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := checksum.SHA256Hex([]byte(tc.in)); got != tc.want {
				t.Fatalf("SHA256Hex(%q) = %s, want %s", tc.name, got, tc.want)
			}
		})
	}
}

// The two hash paths are used by different object-store backends — the
// in-memory store hands back one []byte, a real S3 read is chunked. If they
// disagreed, an upload could verify against one and fail against the other,
// which is exactly the kind of bug that only shows up in production.
//
// The sizes are chosen to land ON and either side of a chunk boundary, because
// the divergence this guards against is an off-by-one at exactly that boundary
// and a test that only used round sizes would pass with one.
func TestStreamingAndOneShotAgree(t *testing.T) {
	for _, size := range []int{0, 1, 1023, 1024, checksum.HashChunkBytes, checksum.HashChunkBytes + 7} {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i % 251)
		}
		oneShot := checksum.SHA256Hex(data)
		streamed := checksum.SHA256HexStreaming(checksum.Chunks(data))
		if oneShot != streamed {
			t.Fatalf("hash paths disagree at size %d: one-shot %s, streaming %s", size, oneShot, streamed)
		}
	}
}

// Chunks is what makes the equality above mean anything, so it gets its own
// assertion: the chunks must reconstruct the input exactly, and a single-chunk
// input must not be handed to the hasher twice.
func TestChunksReconstructTheInput(t *testing.T) {
	for _, size := range []int{0, 1, checksum.HashChunkBytes - 1, checksum.HashChunkBytes, 3*checksum.HashChunkBytes + 11} {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i)
		}
		var rebuilt []byte
		var seen int
		for chunk := range checksum.Chunks(data) {
			seen++
			rebuilt = append(rebuilt, chunk...)
		}
		if string(rebuilt) != string(data) {
			t.Fatalf("Chunks(%d) did not reconstruct the input", size)
		}
		want := 0
		if size > 0 {
			want = (size + checksum.HashChunkBytes - 1) / checksum.HashChunkBytes
		}
		if seen != want {
			t.Fatalf("Chunks(%d) yielded %d chunks, want %d", size, seen, want)
		}
	}
}

// A caller that abandons the sequence mid-way must not be handed the rest of a
// gigabyte. This is the reason Chunks is a range-over-func rather than a
// slice: a slice has already allocated.
func TestChunksStopsWhenTheCallerStops(t *testing.T) {
	data := make([]byte, 4*checksum.HashChunkBytes)
	seen := 0
	for range checksum.Chunks(data) {
		seen++
		break
	}
	if seen != 1 {
		t.Fatalf("the caller stopped after one chunk and Chunks yielded %d", seen)
	}
}

func TestFormatIs64HexAndNothingElse(t *testing.T) {
	if !checksum.IsValidFormat(checksum.SHA256Hex([]byte("x"))) {
		t.Fatal("a real sha256 is not a well-formed checksum")
	}
	if !checksum.IsValidFormat(strings.Repeat("A", 64)) {
		t.Error("uppercase hex is well-formed")
	}
	for _, bad := range []string{"", "abc", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("g", 64), "  "} {
		if checksum.IsValidFormat(bad) {
			t.Errorf("accepted a malformed checksum: %q", bad)
		}
	}
}

func TestComparisonIsCaseInsensitiveButStorageIsLowercase(t *testing.T) {
	lower := checksum.SHA256Hex([]byte("payload"))
	upper := strings.ToUpper(lower)
	if !checksum.Match(lower, upper) {
		t.Error("a client that hex-encoded with an uppercase alphabet was rejected for its casing")
	}
	if !checksum.Match(upper, lower) {
		t.Error("Match is not symmetric")
	}
	if checksum.Match(lower, checksum.SHA256Hex([]byte("different"))) {
		t.Error("two different payloads matched")
	}
	// The stored value is always lowercase, and that is a property of the
	// encoder rather than of Match — asserted so a future "normalise with
	// strings.ToUpper" would fail here.
	if got := checksum.SHA256Hex([]byte("payload")); got != strings.ToLower(got) {
		t.Fatalf("SHA256Hex returned a non-lowercase digest: %s", got)
	}
}
