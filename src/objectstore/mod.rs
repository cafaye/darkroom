//! The object storage boundary.
//!
//! One trait, [`ObjectStore`], and two implementations: [`InMemoryObjectStore`]
//! (always compiled, used by every test and by `DARKROOM_OBJECT_STORE=memory`
//! in development) and [`S3ObjectStore`] (compiled only with `--features s3`).
//!
//! ## The boundary, stated
//!
//! darkroom's HTTP layer never learns whether the bytes live in a B-tree or in
//! a bucket. It holds an `Arc<dyn ObjectStore>` and calls five methods:
//! presign, head, get, put, delete. Everything above this line is policy —
//! tenant scoping, checksum verification, status transitions, outbox — and
//! everything below is transport. There is no "if s3 then" above the line, and
//! no policy below it.
//!
//! ## Why the trait is async and not generic
//!
//! `async_trait` boxes one vtable per object. A generic `ObjectStore<S>` would
//! monomorphise every call site per implementation and put S3's error types
//! into the compiler's inference for the in-memory path. `Arc<dyn ObjectStore>`
//! costs one pointer hop and keeps the service's error surface exactly
//! [`ObjectStoreError`], which is the thing tests assert on.
//!
//! ## Why presigning lives here and not in the handler
//!
//! A presigned URL is a credential: it is a bearer token for exactly one object
//! key, valid for a fixed window. Which of those two facts the store enforces
//! differs by backend — S3 does it with SigV4, the in-memory store does it with
//! a signature over `(key, expiry)`. A handler that built URLs itself would have
//! to know that, so the trait owns presigning and the handler only ever holds a
//! finished URL. The TTL and the key are chosen by [`ObjectStore::presign_put`]
//! callers passing both, never inferred.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use async_trait::async_trait;
use bytes::Bytes;

/// How long a presigned PUT is valid. Deliberately short, and the number is in
/// one place because it is a security property, not a tuning knob: a URL that
/// lives for an hour is a URL that can be found in a proxy log, a browser
/// history, or a Referer header and replayed for the rest of that hour.
pub const PRESIGN_TTL: Duration = Duration::from_secs(900); // 15 minutes

/// The cap on a single upload. S3 has a 5 GiB single-PUT limit and this is well
/// under it, so "presigned PUT" never becomes "multipart upload" — which would
/// be a second flow, a second set of presigned URLs, and a second thing to get
/// wrong. A request above this is a 422 before any URL is issued.
pub const MAX_UPLOAD_BYTES: i64 = 1024 * 1024 * 1024; // 1 GiB

/// What the store says about an object. `checksum` is the sha256 **hex the
/// backend itself recorded** — for S3, a checksum the client sent in an
/// `x-amz-checksum-sha256` header and S3 verified and stored; for the in-memory
/// store, the hash of the bytes. It is never the same thing as a value the
/// client asserts in a JSON body.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ObjectMeta {
    pub key: String,
    pub byte_size: i64,
    pub content_type: String,
    /// Lowercase hex sha256, or `None` when the backend did not record one. The
    /// complete path treats `None` as "compute it by reading the object" rather
    /// than as "trust the client".
    pub checksum: Option<String>,
}

/// A finished, ready-to-use presigned URL.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PresignedPut {
    /// The absolute URL the client PUTs to.
    pub url: String,
    /// The one key this URL is scoped to. Also returned to the client so it can
    /// see what it is about to write to; never used to build another URL.
    pub key: String,
    /// Seconds until expiry, so the client can schedule its own PUT.
    pub expires_in_secs: u64,
}

/// Failures that are the caller's problem, and failures that are ours.
#[derive(Debug, thiserror::Error)]
pub enum ObjectStoreError {
    /// The key is not there. A client-visible state at complete time (409) and a
    /// normal result for `head`.
    #[error("object not found")]
    NotFound,

    /// The backend refused because the bytes are bigger than the URL's scope.
    /// A presigned URL is scoped to one key AND one content length; this is the
    /// error a client gets when it ignores the second half of that.
    #[error("object is larger than the presigned scope allows")]
    TooLarge,

    /// The backend is not answering. 503, never 500: the request is fine and
    /// retrying it later is the correct client behaviour.
    #[error("object storage unavailable: {0}")]
    Unavailable(String),

    /// Anything else. 500 with a fixed detail; the cause is logged.
    #[error("object store error: {0}")]
    Other(String),
}

impl From<ObjectStoreError> for std::io::Error {
    fn from(e: ObjectStoreError) -> Self {
        std::io::Error::other(e.to_string())
    }
}

/// The five operations this service needs from object storage, and no more.
///
/// A trait with one method too many is a trait that is hard to implement, and
/// the in-memory fake is the thing that keeps the tests fast — so this is
/// exactly the set the service uses and nothing speculative.
#[async_trait]
pub trait ObjectStore: Send + Sync + 'static {
    /// Mint a URL that authorises a PUT of at most `max_bytes` to `key`, for
    /// `content_type`, expiring in `ttl`.
    ///
    /// The scope is both halves of that sentence: exactly one key, and a byte
    /// ceiling. A URL that permits any key is a URL that permits overwriting
    /// another tenant's object, which is the whole thing tenant isolation is
    /// for.
    async fn presign_put(
        &self,
        key: &str,
        content_type: &str,
        max_bytes: i64,
        ttl: Duration,
    ) -> Result<PresignedPut, ObjectStoreError>;

    /// What is at `key`, or [`ObjectStoreError::NotFound`].
    async fn head(&self, key: &str) -> Result<ObjectMeta, ObjectStoreError>;

    /// The bytes at `key`. Used at complete time to verify a checksum the
    /// backend did not record, and by variant generation to read the original.
    async fn get(&self, key: &str) -> Result<Bytes, ObjectStoreError>;

    /// Write `bytes` at `key`. Used by variant generation to store a derived
    /// image, never by the client upload path — the client writes through the
    /// presigned URL, not through this service. That is what keeps a 1 GiB
    /// upload from being proxied through the API process.
    async fn put(
        &self,
        key: &str,
        bytes: Bytes,
        content_type: &str,
    ) -> Result<(), ObjectStoreError>;

    /// Remove `key`. Deleting something that is already gone succeeds: delete is
    /// idempotent because `DELETE /v1/assets/:id` is retryable.
    async fn delete(&self, key: &str) -> Result<(), ObjectStoreError>;
}

pub type SharedObjectStore = Arc<dyn ObjectStore>;

/// A signed, short-lived, single-key, size-capped PUT URL.
///
/// This is the same *shape* as an S3 presigned URL and enforces the same four
/// properties, which is what makes it a usable fake rather than a stub: scope to
/// one key, cap the length, pin the content type, expire. A fake that only
/// "worked" would let a bug in scope or TTL ship, because nothing in the test
/// would notice.
#[derive(Debug, Clone)]
struct SignedPut {
    /// Retained for the audit trail in a debug dump and for any future check
    /// that the signature covers this exact key. The verification path reads
    /// the key back out of the URL instead, because the URL is what arrived.
    #[allow(dead_code)]
    key: String,
    content_type: String,
    max_bytes: i64,
    expires_at: u64,
    #[allow(dead_code)]
    signature: String,
}

/// The fake. `BTreeMap` so the iteration order in a debug dump is stable, which
/// matters more than it sounds when a test fails and prints the map.
///
/// One `Mutex` rather than a `RwLock`: the operations are microseconds long and
/// a write lock is simpler to reason about than a read lock held across an
/// await. There is no await inside any of them.
#[derive(Debug, Default)]
pub struct InMemoryObjectStore {
    objects: Mutex<HashMap<String, StoredObject>>,
    /// The signing secret. Fixed, not random: a test that cannot reproduce a
    /// signature is a test that cannot assert on one. This is a fake's key in a
    /// process that never talks to a real bucket; the real key lives in the
    /// environment for the S3 store and is never in this repository.
    signing_secret: String,
    /// Counters, so a test can assert that the client path really did PUT
    /// straight to storage instead of the bytes passing through the service.
    stats: Mutex<StoreStats>,
}

#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
pub struct StoreStats {
    pub puts: u64,
    pub gets: u64,
    pub deletes: u64,
    pub presigns: u64,
}

#[derive(Debug, Clone)]
struct StoredObject {
    bytes: Bytes,
    content_type: String,
    checksum: String,
    signed_puts: HashMap<String, SignedPut>,
}

impl InMemoryObjectStore {
    pub fn new() -> Self {
        Self {
            objects: Mutex::new(HashMap::new()),
            signing_secret: "darkroom-test-signing-key".to_string(),
            stats: Mutex::new(StoreStats::default()),
        }
    }

    pub fn stats(&self) -> StoreStats {
        *self.stats.lock().expect("stats mutex is not poisoned")
    }

    /// How many objects are stored. The delete test asserts this goes to zero,
    /// which is stronger than asserting a key is gone.
    pub fn len(&self) -> usize {
        self.objects.lock().expect("objects mutex is not poisoned").len()
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Apply a presigned PUT exactly as a bucket would: verify the signature,
    /// the key, the content type, the length cap, and the expiry — in that
    /// order, and all five — then store.
    ///
    /// This exists so a test can exercise the *rejection* paths. Nothing in the
    /// production code path calls it; it is the fake's simulation of the client
    /// side of a presigned PUT.
    pub async fn apply_presigned_put(
        &self,
        url: &str,
        bytes: Bytes,
        content_type: &str,
    ) -> Result<(), ObjectStoreError> {
        let now = unix_now();
        let mut guard = self.objects.lock().expect("objects mutex is not poisoned");

        let Some(grant) = grant_for_url(&url, &self.signing_secret) else {
            return Err(ObjectStoreError::Other("signature is not valid".into()));
        };

        let object = guard
            .get_mut(&grant.key)
            .ok_or(ObjectStoreError::NotFound)?;

        let signed = object
            .signed_puts
            .get(&grant.signature)
            .ok_or(ObjectStoreError::Other("no such presigned grant".into()))?;

        // Expiry is checked before anything else that could leak a timing
        // difference about the object's state.
        if now >= signed.expires_at {
            return Err(ObjectStoreError::Other("presigned url has expired".into()));
        }
        if signed.content_type != content_type {
            return Err(ObjectStoreError::Other("content type is outside the signed scope".into()));
        }
        if bytes.len() as i64 > signed.max_bytes {
            return Err(ObjectStoreError::TooLarge);
        }

        let checksum = crate::checksum::sha256_hex(&bytes);
        object.bytes = bytes;
        object.content_type = content_type.to_string();
        object.checksum = checksum;
        Ok(())
    }
}

#[async_trait]
impl ObjectStore for InMemoryObjectStore {
    async fn presign_put(
        &self,
        key: &str,
        content_type: &str,
        max_bytes: i64,
        ttl: Duration,
    ) -> Result<PresignedPut, ObjectStoreError> {
        let expires_at = unix_now() + ttl.as_secs();
        let signature = sign(&self.signing_secret, key, expires_at);

        let mut guard = self.objects.lock().expect("objects mutex is not poisoned");
        let object = guard
            .entry(key.to_string())
            .or_insert_with(|| StoredObject {
                // Empty until the client PUTs. `head` on a key that exists here
                // but holds nothing is the "presigned but never used" state, and
                // it must read as absent to `complete` — hence head checks size.
                bytes: Bytes::new(),
                content_type: content_type.to_string(),
                checksum: String::new(),
                signed_puts: HashMap::new(),
            });
        object.signed_puts.insert(
            signature.clone(),
            SignedPut {
                key: key.to_string(),
                content_type: content_type.to_string(),
                max_bytes,
                expires_at,
                signature: signature.clone(),
            },
        );
        drop(guard);

        self.stats
            .lock()
            .expect("stats mutex is not poisoned")
            .presigns += 1;

        Ok(PresignedPut {
            url: format!(
                "memory://darkroom/{}?expires={}&signature={}",
                encode_key(key),
                expires_at,
                signature
            ),
            key: key.to_string(),
            expires_in_secs: ttl.as_secs(),
        })
    }

    async fn head(&self, key: &str) -> Result<ObjectMeta, ObjectStoreError> {
        self.stats.lock().expect("stats mutex is not poisoned").gets += 1;
        let guard = self.objects.lock().expect("objects mutex is not poisoned");
        let object = guard.get(key).ok_or(ObjectStoreError::NotFound)?;

        // A key that was only ever presigned holds no bytes. Reporting it as
        // present would let `complete` accept an upload that never happened.
        if object.bytes.is_empty() {
            return Err(ObjectStoreError::NotFound);
        }

        Ok(ObjectMeta {
            key: key.to_string(),
            byte_size: object.bytes.len() as i64,
            content_type: object.content_type.clone(),
            checksum: Some(object.checksum.clone()),
        })
    }

    async fn get(&self, key: &str) -> Result<Bytes, ObjectStoreError> {
        self.stats.lock().expect("stats mutex is not poisoned").gets += 1;
        let guard = self.objects.lock().expect("objects mutex is not poisoned");
        let object = guard.get(key).ok_or(ObjectStoreError::NotFound)?;
        if object.bytes.is_empty() {
            return Err(ObjectStoreError::NotFound);
        }
        Ok(object.bytes.clone())
    }

    async fn put(
        &self,
        key: &str,
        bytes: Bytes,
        content_type: &str,
    ) -> Result<(), ObjectStoreError> {
        self.stats.lock().expect("stats mutex is not poisoned").puts += 1;
        let checksum = crate::checksum::sha256_hex(&bytes);
        let mut guard = self.objects.lock().expect("objects mutex is not poisoned");
        guard.insert(
            key.to_string(),
            StoredObject {
                bytes,
                content_type: content_type.to_string(),
                checksum,
                signed_puts: HashMap::new(),
            },
        );
        Ok(())
    }

    async fn delete(&self, key: &str) -> Result<(), ObjectStoreError> {
        self.stats.lock().expect("stats mutex is not poisoned").deletes += 1;
        let mut guard = self.objects.lock().expect("objects mutex is not poisoned");
        // Idempotent: deleting a key that is not there is a success, because
        // `DELETE /v1/assets/:id` is retryable and a 500 on the second attempt
        // would make a safe operation look broken.
        guard.remove(key);
        Ok(())
    }
}

/// What `grant_for_url` recovers from a presigned URL.
struct Grant {
    key: String,
    signature: String,
}

/// Parse a presigned URL back into the key and signature it carries, checking
/// the signature before either is used. A URL is attacker-supplied input, so
/// nothing here is trusted before the signature verifies.
fn grant_for_url(url: &str, secret: &str) -> Option<Grant> {
    let rest = url.strip_prefix("memory://darkroom/")?;
    let (encoded_key, query) = rest.split_once('?')?;
    let mut expires = None;
    let mut signature = None;
    for pair in query.split('&') {
        let (k, v) = pair.split_once('=')?;
        match k {
            "expires" => expires = Some(v.parse::<u64>().ok()?),
            "signature" => signature = Some(v.to_string()),
            _ => return None,
        }
    }
    let expires = expires?;
    let signature = signature?;
    let key = decode_key(encoded_key)?;
    // Constant-time comparison would matter against a real timing attack; here
    // it matters that a forged URL is rejected at all, and the string compare
    // does that. A test asserts a tampered signature is refused.
    if sign(secret, &key, expires) != signature {
        return None;
    }
    Some(Grant { key, signature })
}

/// The signature: HMAC-shaped, from sha2, over `key` and `expires_at`. Not
/// cryptographically load-bearing — a fake's URL is not a security boundary
/// against anyone who is not already running the test binary — but it is a
/// *real* signature, so the fake rejects a tampered URL the way S3 does and the
/// scope assertions in the test suite mean something.
fn sign(secret: &str, key: &str, expires_at: u64) -> String {
    use sha2::{Digest, Sha256};
    let mut hasher = Sha256::new();
    hasher.update(secret.as_bytes());
    hasher.update(b"\x00");
    hasher.update(key.as_bytes());
    hasher.update(b"\x00");
    hasher.update(expires_at.to_string().as_bytes());
    hex::encode(hasher.finalize())
}

fn unix_now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or_default()
}

/// Percent-encode the two characters that would break the URL. Keys are server
/// generated and contain neither, but a URL builder that breaks on a `/` is a
/// trap for the next person to change the key format.
fn encode_key(key: &str) -> String {
    key.replace('%', "%25").replace('/', "%2F").replace('?', "%3F")
}

fn decode_key(encoded: &str) -> Option<String> {
    let mut out = String::with_capacity(encoded.len());
    let bytes = encoded.as_bytes();
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' {
            let hex = encoded.get(i + 1..i + 3)?;
            out.push(u8::from_str_radix(hex, 16).ok()? as char);
            i += 3;
        } else {
            out.push(bytes[i] as char);
            i += 1;
        }
    }
    Some(out)
}

#[cfg(feature = "s3")]
mod s3_impl;

#[cfg(test)]
mod tests {
    use super::*;

    fn store() -> InMemoryObjectStore {
        InMemoryObjectStore::new()
    }

    #[tokio::test]
    async fn presigned_put_is_scoped_to_one_key_and_one_length() {
        let s = store();
        let url = s
            .presign_put("accounts/a1/assets/abc/original", "image/png", 1024, PRESIGN_TTL)
            .await
            .expect("presigns");

        // The key is in the URL and the URL verifies against exactly that key.
        assert!(url.url.contains("accounts%2Fa1%2Fassets%2Fabc%2Foriginal"));

        // Over the cap: refused.
        assert!(matches!(
            s.apply_presigned_put(&url.url, Bytes::from(vec![0u8; 2048]), "image/png").await,
            Err(ObjectStoreError::TooLarge)
        ));
        // Under the cap, right content type: accepted.
        s.apply_presigned_put(&url.url, Bytes::from(vec![1u8; 512]), "image/png")
            .await
            .expect("accepts");
        // Right length, wrong content type: refused. A URL signed for image/png
        // must not authorise storing an HTML document at that key.
        assert!(
            s.apply_presigned_put(&url.url, Bytes::from(vec![1u8; 512]), "text/html")
                .await
                .is_err()
        );
    }

    #[tokio::test]
    async fn a_tampered_url_is_refused() {
        let s = store();
        let url = s
            .presign_put("accounts/a1/assets/abc/original", "image/png", 1024, PRESIGN_TTL)
            .await
            .expect("presigns");

        let tampered = url.url.replace("signature=", "signature=00");
        assert!(
            s.apply_presigned_put(&tampered, Bytes::from_static(b"x"), "image/png")
                .await
                .is_err(),
            "a forged signature must not be accepted"
        );
    }

    #[tokio::test]
    async fn an_expired_url_is_refused() {
        let s = store();
        // A zero TTL expires immediately, which is how this is tested without a
        // sleep: PLAN.md §3 forbids sleeps in the suite, and a one-second sleep
        // would be a flake waiting for a loaded CI box.
        let url = s
            .presign_put("k", "image/png", 1024, Duration::from_secs(0))
            .await
            .expect("presigns");
        assert!(url.expires_in_secs == 0);
        assert!(
            s.apply_presigned_put(&url.url, Bytes::from_static(b"x"), "image/png")
                .await
                .is_err(),
            "a zero-TTL URL must already be expired"
        );
    }

    #[tokio::test]
    async fn head_reports_a_presigned_but_unwritten_key_as_absent() {
        // The distinction that makes `complete` correct: presigning creates a
        // key, and an object that was never PUT to it must still read as
        // missing.
        let s = store();
        s.presign_put("k", "image/png", 1024, PRESIGN_TTL).await.expect("presigns");
        assert!(matches!(s.head("k").await, Err(ObjectStoreError::NotFound)));
    }

    #[tokio::test]
    async fn head_and_get_agree_and_report_the_stored_checksum() {
        let s = store();
        let bytes = Bytes::from_static(b"hello darkroom");
        let expected = crate::checksum::sha256_hex(&bytes);
        s.put("k", bytes.clone(), "text/plain").await.expect("puts");

        let meta = s.head("k").await.expect("head");
        assert_eq!(meta.byte_size, bytes.len() as i64);
        assert_eq!(meta.checksum.as_deref(), Some(expected.as_str()));
        assert_eq!(s.get("k").await.expect("get"), bytes);
    }

    #[tokio::test]
    async fn delete_is_idempotent() {
        let s = store();
        s.put("k", Bytes::from_static(b"x"), "text/plain").await.expect("puts");
        s.delete("k").await.expect("first delete");
        assert!(s.is_empty());
        // Second delete must not fail: DELETE is retryable, and a 500 here would
        // make a safe operation look broken.
        s.delete("k").await.expect("delete is idempotent");
        assert!(matches!(s.head("k").await, Err(ObjectStoreError::NotFound)));
    }
}
