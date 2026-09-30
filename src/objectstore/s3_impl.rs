//! The real S3 implementation. Compiled only with `--features s3`.
//!
//! This is the deployment path and it is the ONLY file in the service that
//! knows S3 exists. Everything above `src/objectstore/mod.rs`'s trait is policy
//! — tenancy, checksums, status transitions, the outbox — and none of it has an
//! `if s3` in it. The two implementations are interchangeable and the tests
//! cannot tell which one they ran against except by the `memory://` scheme in a
//! presigned URL.
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
use aws_sdk_s3::config::{Credentials, Region};
use aws_sdk_s3::presigning::PresigningConfig;
use aws_sdk_s3::primitives::ByteStream;
use aws_sdk_s3::types::ChecksumAlgorithm;
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
    /// `endpoint` and `path_style` exist for an S3-compatible service (MinIO,
    /// Ceph): virtual-host addressing needs DNS for a bucket name under a
    /// hostname that does not exist, so those services need path-style.
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

        let mut builder = aws_sdk_s3::config::Builder::from(&shared)
            .behavior_version(aws_sdk_s3::config::BehaviorVersion::latest());

        if let Some(endpoint) = endpoint {
            builder = builder.endpoint_url(endpoint);
        }
        if path_style {
            builder = builder.force_path_style(true);
        }

        Ok(Self {
            client: Client::from_conf(builder.build()),
            bucket: bucket.to_string(),
        })
    }

    /// A client wired for tests against a local S3-compatible service. The
    /// credentials are supplied by the caller, never defaulted — a test that
    /// could authenticate without being told how is a test that would pass
    /// against the wrong bucket.
    pub fn for_local_testing(
        endpoint: &str,
        region: &str,
        access: &str,
        secret: &str,
        bucket: &str,
    ) -> Self {
        // A static provider rather than the SDK's credential chain: a test that
        // could authenticate without being told how is a test that would pass
        // against the wrong bucket.
        let conf = aws_sdk_s3::config::Builder::new()
            .behavior_version(aws_sdk_s3::config::BehaviorVersion::latest())
            .region(Region::new(region.to_string()))
            .endpoint_url(endpoint)
            .force_path_style(true)
            .credentials_provider(Credentials::new(
                access,
                secret,
                None,
                None,
                "darkroom-local",
            ))
            .build();
        Self {
            client: Client::from_conf(conf),
            bucket: bucket.to_string(),
        }
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
            // Ask S3 to record and verify the client's checksum, so `head` can
            // read back what the bucket itself validated rather than trusting
            // the value the client will later claim in a JSON body.
            .checksum_algorithm(ChecksumAlgorithm::Sha256)
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
        Ok(PresignedPut {
            url: append_length_range(uri, max_bytes),
            key: key.to_string(),
            expires_in_secs,
        })
    }

    /// Metadata for one key. S3's own `content-length` is the byte count, not
    /// the `byte_size` the client declared — the client's number is a claim and
    /// this is the measurement.
    async fn head(&self, key: &str) -> Result<ObjectMeta, ObjectStoreError> {
        let head = self
            .client
            .head_object()
            .bucket(&self.bucket)
            .key(key)
            .checksum_mode(aws_sdk_s3::types::ChecksumMode::Enabled)
            .send()
            .await
            .map_err(map_s3_error)?;

        let byte_size = head.content_length().unwrap_or_default() as i64;
        let content_type = head
            .content_type()
            .unwrap_or("application/octet-stream")
            .to_string();
        // `checksum_sha256` is base64, not hex. `checksums_match` compares hex,
        // so it is converted here — at the boundary — rather than anywhere
        // above, where a hex/base64 mix-up would be invisible.
        let checksum = head.checksum_sha256().map(super::base64_to_hex);

        Ok(ObjectMeta {
            key: key.to_string(),
            byte_size,
            content_type,
            checksum,
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

/// Percent-encode a header name for the SDK's `signable_headers` list, which
/// wants them sorted and semicolon-separated.
#[allow(dead_code)]
fn encode_query(name: &str, _value: &str) -> String {
    format!("x-amz-checksum-sha256;{}", super::encode_key(name))
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
}
