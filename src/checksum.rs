//! sha256, hex, and the checksum rules.
//!
//! The checksum is the load-bearing part of the upload flow, so the rules are
//! stated here rather than in the handler:
//!
//! 1. The client sends the sha256 of the bytes it is about to upload.
//! 2. That claim is written to the `assets` row as `pending` — it is a *claim*,
//!    not a fact, and it is never what `complete` compares against.
//! 3. `complete` computes sha256 over what storage actually holds and writes
//!    *that* to the row. If the two differ, the upload is rejected with 422 and
//!    the asset is marked `failed`.
//!
//! Step 3 is the whole security property of the signed-upload pattern. A
//! service that trusted the client's claim would let a client register an asset
//! whose `checksum` describes bytes it never uploaded — and that checksum is
//! what every downstream dedupe and every future integrity check is keyed on.

use bytes::Bytes;
use sha2::{Digest, Sha256};

/// Lowercase hex sha256 of `bytes`.
pub fn sha256_hex(bytes: &[u8]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(bytes);
    hex::encode(hasher.finalize())
}

/// Whether a client-supplied checksum is even the right *shape*: exactly 64
/// lowercase-or-uppercase hex characters.
///
/// Checked at `POST /v1/uploads` so a client that sends `"checksum": "abc"` gets
/// a 422 naming the field rather than a 422 twenty minutes later at complete
/// time, after it has already uploaded a gigabyte.
pub fn is_valid_checksum_format(candidate: &str) -> bool {
    candidate.len() == 64 && candidate.chars().all(|c| c.is_ascii_hexdigit())
}

/// Compare a claimed checksum against a computed one, case-insensitively.
///
/// A hex digest is conventionally lowercase, but a client computing it with
/// `hex.EncodeToString` in Go gets lowercase and one using a base-16 encoder
/// with an uppercase alphabet gets uppercase. Rejecting the second for its
/// casing would be a 422 that teaches the client nothing useful, so the
/// comparison is case-insensitive and the *stored* value is always normalised
/// to lowercase.
pub fn checksums_match(claimed: &str, computed: &str) -> bool {
    claimed.eq_ignore_ascii_case(computed)
}

/// Read a large object in bounded chunks and hash it, so a 1 GiB upload is
/// verified in constant memory.
///
/// The streaming path exists because the naive `sha256_hex(&bytes)` on a
/// `Bytes` returned by `ObjectStore::get` is fine for the in-memory fake and
/// wrong for a real 1 GiB object: it is one contiguous allocation that is
/// already resident because `get` returned it. Streaming is what the S3
/// implementation uses, and both call the same comparison logic, so the two
/// backends cannot disagree about whether an upload was valid.
pub fn sha256_hex_streaming(chunks: impl IntoIterator<Item = Bytes>) -> String {
    let mut hasher = Sha256::new();
    for chunk in chunks {
        hasher.update(&chunk);
    }
    hex::encode(hasher.finalize())
}

/// The chunk size for streaming reads: 1 MiB. Large enough that per-chunk
/// overhead disappears, small enough that a 1 GiB verify holds 1 MiB.
pub const HASH_CHUNK_BYTES: usize = 1024 * 1024;

/// Split `bytes` into hash-sized chunks. Separate from
/// [`sha256_hex_streaming`] so the two paths are provably equivalent rather
/// than merely both existing.
pub fn chunks(bytes: &[u8]) -> Vec<Bytes> {
    bytes
        .chunks(HASH_CHUNK_BYTES)
        .map(Bytes::copy_from_slice)
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The published FIPS 180-4 vectors. Hard-coded rather than computed,
    /// because a test that hashes with the same code it is testing proves only
    /// that the code is self-consistent.
    #[test]
    fn sha256_matches_the_published_vectors() {
        assert_eq!(
            sha256_hex(b""),
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        );
        assert_eq!(
            sha256_hex(b"abc"),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
        assert_eq!(
            sha256_hex(b"abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq"),
            "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1"
        );
    }

    #[test]
    fn streaming_and_one_shot_agree() {
        // The two hash paths are used by different object-store backends. If
        // they disagreed, an upload could verify against one and fail against
        // the other, which is exactly the kind of bug that only shows up in
        // production.
        for size in [0usize, 1, 1023, 1024, HASH_CHUNK_BYTES, HASH_CHUNK_BYTES + 7] {
            let data: Vec<u8> = (0..size).map(|i| (i % 251) as u8).collect();
            assert_eq!(
                sha256_hex(&data),
                sha256_hex_streaming(chunks(&data)),
                "hash paths disagree at size {size}"
            );
        }
    }

    #[test]
    fn checksum_format_is_64_hex_and_nothing_else() {
        assert!(is_valid_checksum_format(&sha256_hex(b"x")));
        assert!(is_valid_checksum_format(&"A".repeat(64)), "uppercase hex is well-formed");
        for bad in ["", "abc", &"a".repeat(63), &"a".repeat(65), &"g".repeat(64), "  ".trim()] {
            assert!(
                !is_valid_checksum_format(bad),
                "accepted a malformed checksum: {bad:?}"
            );
        }
    }

    #[test]
    fn comparison_is_case_insensitive_but_storage_is_lowercase() {
        let lower = sha256_hex(b"payload");
        let upper = lower.to_ascii_uppercase();
        assert!(checksums_match(&lower, &upper));
        assert!(checksums_match(&upper, &lower));
        assert!(!checksums_match(&lower, &sha256_hex(b"different")));
    }
}
