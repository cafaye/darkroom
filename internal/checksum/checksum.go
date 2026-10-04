// Package checksum is sha256, hex, and the rules about who gets to compute a
// checksum.
//
// The checksum is the load-bearing part of the upload flow, so the rules are
// stated here rather than in a handler:
//
//  1. The client sends the sha256 of the bytes it is about to upload.
//  2. That claim is written to the `assets` row as `pending` — it is a *claim*,
//     not a fact, and it is never what complete compares against.
//  3. Complete computes sha256 over what storage actually holds and writes
//     *that* to the row. If the two differ, the upload is rejected with 422 and
//     the asset is marked `failed`.
//
// Step 3 is the whole security property of the signed-upload pattern. A service
// that trusted the client's claim would let a client register an asset whose
// `checksum` describes bytes it never uploaded — and that checksum is what every
// downstream dedupe and every future integrity check is keyed on.
//
// `AGENTS.md`, from the other side: "The checksum is computed, never trusted.
// The client's claim is written at create and compared against at complete; the
// row ends up carrying the computed value. Do not add a path that stores the
// claimed one."
//
// crypto/sha256 is the implementation and there is no hand-rolled digest
// anywhere in this repository — the Go standard library's SHA-256 is FIPS
// 180-4 and is the thing the published test vectors below are for.
package checksum

import (
	"crypto/sha256"
	"encoding/hex"
	"iter"
	"strings"
)

// SHA256Hex returns the lowercase hex sha256 of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HashChunkBytes is the chunk size for streaming reads: 1 MiB.
//
// Large enough that per-chunk overhead disappears, small enough that a 1 GiB
// verify holds 1 MiB. It is a CONSTANT rather than a literal at each call site
// because it is the number that makes the streaming and one-shot paths provably
// equivalent — see Chunks.
const HashChunkBytes = 1024 * 1024

// SHA256HexStreaming hashes chunks in order and returns the lowercase hex
// sha256.
//
// The streaming path exists because the naive SHA256Hex over the []byte
// returned by `objectstore.Store.Get` is fine for the in-memory store and wrong
// for a real 1 GiB object: it is one contiguous allocation that is already
// resident because Get returned it. Streaming is what an S3 read path uses, and
// both call the same comparison logic, so the two backends cannot disagree
// about whether an upload was valid.
//
// An `iter.Seq[[]byte]` and not a `[]byte`: the point is that the caller never
// holds the whole object, so the signature has to be able to say that.
func SHA256HexStreaming(chunks iter.Seq[[]byte]) string {
	h := sha256.New()
	for chunk := range chunks {
		h.Write(chunk)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Chunks splits b into hash-sized chunks.
//
// SEPARATE FROM SHA256HexStreaming so the two paths are provably equivalent
// rather than merely both existing — which is the property
// `TestStreamingAndOneShotAgree` holds, and the reason a reader of that test
// knows what it is looking at.
func Chunks(b []byte) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		for i := 0; i < len(b); {
			end := min(i+HashChunkBytes, len(b))
			if !yield(b[i:end]) {
				return
			}
			i = end
		}
	}
}

// IsValidFormat reports whether a client-supplied checksum is even the right
// *shape*: exactly 64 hexadecimal characters, either case.
//
// Checked at `POST /v1/uploads` so a client that sends `"checksum": "abc"` gets
// a 422 naming the field rather than a 422 twenty minutes later at complete
// time, after it has already uploaded a gigabyte.
func IsValidFormat(candidate string) bool {
	if len(candidate) != 64 {
		return false
	}
	return strings.IndexFunc(candidate, notHexDigit) < 0
}

// Match compares a claimed checksum against a computed one, case-insensitively.
//
// A hex digest is conventionally lowercase, but a client computing it with
// `hex.EncodeToString` in Go gets lowercase and one using a base-16 encoder
// with an uppercase alphabet gets uppercase. Rejecting the second for its
// casing would be a 422 that teaches the client nothing useful, so the
// comparison is case-insensitive and the *stored* value is always normalised to
// lowercase.
func Match(claimed, computed string) bool {
	return strings.EqualFold(claimed, computed)
}

func notHexDigit(r rune) bool {
	switch {
	case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		return false
	default:
		return true
	}
}
