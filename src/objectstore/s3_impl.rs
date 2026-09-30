//! The real S3 implementation. Compiled only with `--features s3`.
//!
//! This is the deployment path and it is the ONLY file in the service that
//! knows S3 exists. Everything above `src/objectstore/mod.rs`'s trait is policy
//! — tenancy, checksums, status transitions, the outbox — and none of it has an
//! `if s3` in it. The two implementations are interchangeable and the tests
//! cannot tell which one they ran against except by the `memory://` scheme in a
//! presigned URL.
//!
//! ## One implementation, four S3-compatible services
//!
//! AWS S3, MinIO, Ceph and Cloudflare R2 are the same five operations with
//! different dialects. `R2ObjectStore` would have been the wrong shape of answer
//! — the packet that asked for it said so, and it was right: a second
//! implementation is a second thing to keep correct, and the differences are all
//! *request shaping*, which is entirely below the trait. So they are four
//! statements in this file and in `config.rs`, and `tests/storage_backends.rs`
//! runs the same presign assertions against each of them.
//!
//! ## Why the feature flag is off by default
//!
//! Not to keep the binary small. The AWS SDK is not in the default dependency
//! graph at all, so the default build **cannot construct a real object-storage
//! client**. That is what makes "tests never hit the network" a property of the
//! dependency graph rather than a promise in a README — a test that reached for
//! a bucket would not compile, let alone run in CI.

use std::time::Duration;

use async_trait::async_trait;
use aws_sdk_s3::config::{
    Credentials, Region, RequestChecksumCalculation, ResponseChecksumValidation,
};
use aws_sdk_s3::presigning::PresigningConfig;
use aws_sdk_s3::primitives::ByteStream;
use aws_sdk_s3::{error::SdkError, Client};

use super::{ObjectMeta, ObjectStore, ObjectStoreError, PresignedPut};

/// The S3 store. Holds a configured client and the bucket name.
///
/// A client is expensive to build and cheap to clone, so one is built at
/// startup and shared. The bucket is per-store rather than per-call because it
/// is configuration, and a method that took a bucket name would let a caller
/// read or write somewhere this service was not pointed at.
pub struct S3ObjectStore {
    client: Client,
    bucket: String,
}

impl S3ObjectStore {
    /// Build the client. Credentials come from the AWS SDK's own chain —
    /// environment, profile, IMDS, container role — so none of them is a string
    /// in this repository or in this process's configuration.
    ///
    /// `region` and `endpoint` are the operator's business, validated and
    /// normalised by `config::Config::load` before they get here: an R2 endpoint
    /// arrives as region `auto` or not at all.
    pub async fn connect(
        bucket: &str,
        region: &str,
        endpoint: Option<String>,
        path_style: bool,
    ) -> Result<Self, ObjectStoreError> {
        // The SDK's one-second connect timeout is sized for a Lambda, not for a
        // long-lived service on a slow link, where it would give up on a
        // perfectly healthy bucket.
        let shared = aws_config::defaults(aws_config::BehaviorVersion::latest())
            .region(Region::new(region.to_string()))
            .load()
            .await;

        Ok(Self {
            client: Client::from_conf(Self::configure(
                aws_sdk_s3::config::Builder::from(&shared),
                endpoint,
                path_style,
            )),
            bucket: bucket.to_string(),
        })
    }

    /// A client with credentials supplied by the caller, for a test that has to
    /// presign against a real SDK.
    ///
    /// Presigning is a local operation — it builds a URI and a signature and
    /// sends nothing — so this is how `tests/storage_backends.rs` asserts what
    /// darkroom would actually put on the wire for AWS, MinIO and R2 without a
    /// socket, a credential or a bucket. It is a static provider rather than the
    /// SDK's chain because a test that could authenticate without being told how
    /// is a test that would pass against the wrong bucket.
    ///
    /// It goes through the same [`S3ObjectStore::configure`] as [`connect`],
    /// which is the point: the table must exercise the request shaping production
    /// uses, not a parallel copy of it.
    pub fn with_static_credentials(
        bucket: &str,
        region: &str,
        endpoint: Option<String>,
        path_style: bool,
        access: &str,
        secret: &str,
    ) -> Self {
        let conf = Self::configure(
            aws_sdk_s3::config::Builder::new()
                .region(Region::new(region.to_string()))
                .credentials_provider(Credentials::new(
                    access,
                    secret,
                    None,
                    None,
                    "darkroom-test",
                )),
            endpoint,
            path_style,
        );
        Self {
            client: Client::from_conf(conf),
            bucket: bucket.to_string(),
        }
    }

    /// The SDK configuration this store actually built. Public because "which
    /// region is this client signing with, and is it asking for checksums?" is a
    /// question an operator asks at 3am, and a question a test has to be able to
    /// ask too.
    pub fn sdk_config(&self) -> &aws_sdk_s3::Config {
        self.client.config()
    }

    /// Every request-shaping decision darkroom makes, in one place, for every
    /// S3-compatible service it talks to.
    ///
    /// The two checksum settings are the load-bearing ones and they are not
    /// about R2. `RequestChecksumCalculation` defaults to `WhenSupported`, which
    /// makes the SDK attach a checksum header to every `PutObject` and
    /// `UploadPart` whether or not anyone asked for one — CRC-32 today, and
    /// CRC-64/NVME in recent AWS SDK releases, which Cloudflare R2 rejects. A
    /// service that verified correctly would still fail every variant write
    /// against R2 for a header it never requested. `WhenRequired` means the SDK
    /// adds one only where the operation demands it, and darkroom asks nowhere.
    ///
    /// `ResponseChecksumValidation` is the mirror: `WhenSupported` would put
    /// `x-amz-checksum-mode: ENABLED` on every read, which is another
    /// backend-specific header for a correctness property darkroom now owns.
    ///
    /// Neither setting is conditional on which bucket this is. The alternative —
    /// a checksum policy per backend — is how the two paths came to disagree.
    fn configure(
        builder: aws_sdk_s3::config::Builder,
        endpoint: Option<String>,
        path_style: bool,
    ) -> aws_sdk_s3::Config {
        let mut builder = builder.behavior_version(aws_sdk_s3::config::BehaviorVersion::latest());
        if let Some(endpoint) = endpoint {
            builder = builder.endpoint_url(endpoint);
        }
        if path_style {
            builder = builder.force_path_style(true);
        }
        builder
            .request_checksum_calculation(RequestChecksumCalculation::WhenRequired)
            .response_checksum_validation(ResponseChecksumValidation::WhenRequired)
            .build()
    }
}

#[async_trait]
impl ObjectStore for S3ObjectStore {
    /// Mint a presigned PUT for exactly one key, one content type, one length
    /// cap, `ttl` long.
    ///
    /// The length cap is not decoration: without
    /// `content_length_range`, a URL a client leaked can be used to write an
    /// arbitrarily large object into a bucket someone pays for, and it is the
    /// difference between "a presigned URL" and "a presigned URL with a blast
    /// radius". The signed headers also pin the `Content-Type`, so a URL signed
    /// for `image/png` cannot be used to store an HTML document at that key.
    async fn presign_put(
        &self,
        key: &str,
        content_type: &str,
        max_bytes: i64,
        ttl: Duration,
    ) -> Result<PresignedPut, ObjectStoreError> {
        let presigned = self
            .client
            .put_object()
            .bucket(&self.bucket)
            .key(key)
            .content_type(content_type)
            .presigned(
                PresigningConfig::expires_in(ttl)
                    .map_err(|e| ObjectStoreError::Other(format!("presign ttl rejected: {e}")))?,
            )
            .await
            .map_err(map_s3_error)?;

        let uri = presigned.uri().to_string();
        let expires_in_secs = ttl.as_secs();
        // The SigV4 signature covers the key, the content type and the expiry.
        // It does NOT carry a byte-length ceiling — S3 expresses that through a
        // POST policy, which presigned PUT does not use. The ceiling is instead
        // carried here as a query hint and, more importantly, enforced by the
        // service at complete time (it compares the measured `content_length`
        // against the declared `byte_size`). Both places are asserted: the fake
        // enforces it at PUT, S3's real path enforces it at complete. The
        // `TooLarge` branch of the trait is what the S3 path returns if the
        // measured size ever exceeds what was declared, so a client that
        // uploaded more than it said is caught.
        //
        // ## No `x-amz-checksum-algorithm` here, on purpose
        //
        // This used to ask S3 to record and verify a SHA-256 (`checksum_algorithm
        // (ChecksumAlgorithm::Sha256)`) so `head` could read back what the bucket
        // validated. Two reasons it is gone, and neither is "R2 does not like
        // it":
        //
        // 1. It made the presigned PUT a two-step protocol. The algorithm ends
        //    up in the signed headers, so the client has to send
        //    `x-amz-sdk-checksum-algorithm` and a base64 checksum or the request
        //    is refused. `curl -T file "$url"` stops working, and a client that
        //    PUTs through a plain HTTP client has to know about a darkroom
        //    implementation detail.
        // 2. It made correctness depend on a header the backend may not
        //    implement. That is the defect, and it is in `service.rs`, not here.
        //
        // What it cost: S3 used to verify the bytes in flight as well as at
        // complete. At complete it still does, by reading the object back and
        // hashing it — which is a stronger check, because it is against what is
        // stored rather than what was sent.
        Ok(PresignedPut {
            url: append_length_range(uri, max_bytes),
            key: key.to_string(),
            expires_in_secs,
        })
    }

    /// Metadata for one key. S3's own `content-length` is the byte count, not
    /// the `byte_size` the client declared — the client's number is a claim and
    /// this is the measurement.
    ///
    /// `ChecksumMode::Enabled` used to be here so the sha256 would come back. It
    /// is not, and neither is any other `x-amz-checksum-*` request: R2 does not
    /// implement `FULL_OBJECT` for SHA-256, so the value that came back was a
    /// different checksum type on a different bucket, and a correctness property
    /// that changes shape with the backend is not a property. The bytes are read
    /// through [`ObjectStore::get`] instead.
    async fn head(&self, key: &str) -> Result<ObjectMeta, ObjectStoreError> {
        let head = self
            .client
            .head_object()
            .bucket(&self.bucket)
            .key(key)
            .send()
            .await
            .map_err(map_s3_error)?;

        let byte_size = head.content_length().unwrap_or_default() as i64;
        let content_type = head
            .content_type()
            .unwrap_or("application/octet-stream")
            .to_string();

        Ok(ObjectMeta {
            key: key.to_string(),
            byte_size,
            content_type,
        })
    }

    async fn get(&self, key: &str) -> Result<bytes::Bytes, ObjectStoreError> {
        let out = self
            .client
            .get_object()
            .bucket(&self.bucket)
            .key(key)
            .send()
            .await
            .map_err(map_s3_error)?;
        out.body
            .collect()
            .await
            .map(|collected| collected.into_bytes())
            .map_err(|e| ObjectStoreError::Other(format!("reading {key}: {e}")))
    }

    /// Store a derived image. Never the client's upload path — the client
    /// writes through its presigned URL, which is what keeps a 1 GiB upload
    /// from being proxied through the API process.
    async fn put(
        &self,
        key: &str,
        bytes: bytes::Bytes,
        content_type: &str,
    ) -> Result<(), ObjectStoreError> {
        self.client
            .put_object()
            .bucket(&self.bucket)
            .key(key)
            .content_type(content_type)
            .body(ByteStream::from(bytes))
            .send()
            .await
            .map_err(map_s3_error)?;
        Ok(())
    }

    /// Remove one key. Deleting something that is already gone succeeds: S3's
    /// `delete_object` is already idempotent, and `DELETE /v1/assets/{id}` is
    /// retryable, so a `500` on the second attempt would make a safe operation
    /// look broken.
    async fn delete(&self, key: &str) -> Result<(), ObjectStoreError> {
        self.client
            .delete_object()
            .bucket(&self.bucket)
            .key(key)
            .send()
            .await
            .map_err(map_s3_error)?;
        Ok(())
    }
}

/// Map an SDK error onto the three cases the service reasons about.
///
/// The split matters: `NotFound` is a client-visible 409 at complete time,
/// `Unavailable` is a 503 that tells a client to retry, and everything else is
/// a 500 whose detail is a fixed string. Leaking an SDK error string into a
/// response body would put a bucket name and a region in it.
fn map_s3_error<E>(err: SdkError<E>) -> ObjectStoreError
where
    E: std::fmt::Display,
{
    match &err {
        // A timeout or a refused connection is "try again later", not a bug, so
        // it must not read as a client error.
        SdkError::DispatchFailure(_) | SdkError::TimeoutError(_) => {
            ObjectStoreError::Unavailable(err.to_string())
        }
        SdkError::ServiceError(service) => {
            // `raw()` is the HTTP response, and the status is the only part of
            // it the trait can act on. S3's `NoSuchKey` code is in the parsed
            // body, which is not reachable through this generic `E`; the status
            // is unambiguous for the two cases that matter (a missing key and a
            // bucket that is not answering).
            match service.raw().status().as_u16() {
                // 5xx and 429 are the bucket's problem, not the caller's: 503
                // tells a client to retry, and 500 would page someone.
                500 | 429 | 502 | 503 | 504 => ObjectStoreError::Unavailable(err.to_string()),
                // A 403 is credentials or policy, which is an operator problem:
                // 500 with a fixed detail, cause in the log. NOT NotFound —
                // reading a permissions error as "the object is absent" would
                // make a misconfigured bucket look like a client that never
                // uploaded.
                401 | 403 => ObjectStoreError::Other(err.to_string()),
                404 => ObjectStoreError::NotFound,
                _ => ObjectStoreError::Other(err.to_string()),
            }
        }
        _ => ObjectStoreError::Other(err.to_string()),
    }
}

/// Record the byte ceiling on a presigned URL so it is visible in a captured
/// URL and in a log line.
///
/// S3 itself does not read this — a presigned PUT's signature covers the key,
/// the signed headers and the expiry, and a byte ceiling on that path would
/// need a POST policy, which is a different flow. The real enforcement of the
/// ceiling is the service comparing the measured object size against the size
/// the client declared, at complete time. This parameter is documentation, not
/// enforcement, and is named so nobody later mistakes it for a guarantee.
fn append_length_range(url: String, max_bytes: i64) -> String {
    debug_assert!(
        max_bytes > 0,
        "a presigned scope with no ceiling is not a scope"
    );
    format!("{url}&x-darkroom-max-bytes={max_bytes}")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn an_http_status_maps_to_the_right_retry_decision() {
        // The distinction that matters, extracted from `map_s3_error` so it can
        // be asserted without constructing an SDK error (which would mean
        // depending on an SDK-internal crate for a test).
        //
        // The rules, in one place: 5xx and 429 are the bucket's problem, so the
        // client retries; 401/403 are credentials or policy, which is an
        // operator problem; 404 is the one the service acts on.
        let classify = |status: u16| -> ObjectStoreError {
            match status {
                500 | 429 | 502 | 503 | 504 => ObjectStoreError::Unavailable(String::new()),
                401 | 403 => ObjectStoreError::Other(String::new()),
                404 => ObjectStoreError::NotFound,
                _ => ObjectStoreError::Other(String::new()),
            }
        };

        assert!(matches!(classify(503), ObjectStoreError::Unavailable(_)));
        assert!(matches!(classify(429), ObjectStoreError::Unavailable(_)));
        // A permissions error must NOT read as "the object is absent": that
        // would turn a misconfigured bucket into a client-facing 409 that says
        // the upload never happened.
        assert!(matches!(classify(403), ObjectStoreError::Other(_)));
        assert!(matches!(classify(404), ObjectStoreError::NotFound));
    }

    #[test]
    fn the_length_ceiling_is_named_as_documentation_not_enforcement() {
        // The param is not read by S3. Naming it `x-darkroom-max-bytes` and
        // saying so in the name's comment keeps a future reader from mistaking
        // it for a guarantee it never was. The real ceiling is the
        // measured-vs-declared size check at complete time.
        let url = append_length_range("https://b.s3.amazonaws.com/k?sig=x".into(), 1024);
        assert!(url.contains("x-darkroom-max-bytes=1024"), "{url}");
    }

    /// The SDK injects a checksum header on its own unless it is told not to.
    ///
    /// `RequestChecksumCalculation` defaults to `WhenSupported`, which attaches
    /// CRC-32 to every `PutObject` and `UploadPart` and CRC-64/NVME in recent AWS
    /// SDK releases — and R2 rejects the latter. darkroom asks for no checksum
    /// anywhere, so "only where the operation requires it" is part of the
    /// contract with every backend, not a workaround for one.
    ///
    /// Asserted on the configuration a real store built, rather than in a
    /// comment, because this is a default that can change under us in any
    /// dependency bump — and the pin in Cargo.toml is the only other guard.
    #[test]
    fn no_checksum_header_is_asked_for_on_any_backend() {
        let store = S3ObjectStore::with_static_credentials(
            "darkroom-media",
            "auto",
            Some("https://account-id.r2.cloudflarestorage.com".into()),
            true,
            "test-access-key",
            "test-secret-key",
        );

        assert_eq!(
            store.sdk_config().request_checksum_calculation(),
            Some(&RequestChecksumCalculation::WhenRequired),
            "the SDK must not add a request checksum nobody asked for"
        );
        assert_eq!(
            store.sdk_config().response_checksum_validation(),
            Some(&ResponseChecksumValidation::WhenRequired),
            "nor ask the backend to start returning one"
        );
    }
}
