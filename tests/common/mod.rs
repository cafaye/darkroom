//! Shared harness for the tests that need a real Postgres.
//!
//! ## Why these are `#[ignore]`d and what that costs
//!
//! Everything that needs a database lives behind `TEST_DATABASE_URL` and is
//! marked `#[ignore]`, so `cargo test` is green on a bare machine with no
//! Postgres and no Docker. The cost is real and it is worth naming: a skipped
//! test proves nothing, so CI has to run them explicitly (see `bin/prime --db`
//! and `.github/workflows/ci.yml`) or the tenant-isolation suite is decoration.
//!
//! ```sh
//! docker compose up -d postgres
//! TEST_DATABASE_URL="postgres://darkroom:darkroom@localhost:5432/darkroom_test?sslmode=disable" \
//!   cargo test -- --ignored --test-threads=1
//! ```
//!
//! `--test-threads=1` is not a workaround for a race; each test truncates the
//! tables it touches, and two tests truncating concurrently would delete each
//! other's fixtures mid-assert.
//!
//! ## What never happens here
//!
//! No test in this file, or anywhere in this crate, opens a socket to anything
//! other than the database named by the environment. Object storage is always
//! [`InMemoryObjectStore`] or a decorator around it. That is what makes "tests
//! must never hit the network" checkable rather than aspirational — see the
//! `no_network` test below, which asserts the default build cannot even name an
//! S3 client.

#![allow(dead_code)]

use std::sync::Arc;

use async_trait::async_trait;
use bytes::Bytes;
use darkroom::auth::{Principal, StaticTokenVerifier, SCOPE_ASSETS_READ, SCOPE_ASSETS_WRITE};
use darkroom::http::AppState;
use darkroom::objectstore::{
    InMemoryObjectStore, ObjectMeta, ObjectStore, ObjectStoreError, PresignedPut, StoreStats,
};
use darkroom::service::Service;
use darkroom::store::Store;
use sqlx::postgres::PgPoolOptions;
use uuid::Uuid;

/// Connect, migrate, and hand back a clean store. Panics with a message that
/// names the variable rather than a bare `unwrap` panic: a failed harness in
/// CI should read as "TEST_DATABASE_URL is not set" at a glance.
pub async fn test_store() -> Store {
    let url = std::env::var("TEST_DATABASE_URL")
        .expect("TEST_DATABASE_URL must be set to run the database tests");

    let pool = PgPoolOptions::new()
        .max_connections(5)
        .acquire_timeout(std::time::Duration::from_secs(5))
        .connect(&url)
        .await
        .unwrap_or_else(|e| panic!("could not connect to TEST_DATABASE_URL: {e}"));

    // `sqlx::migrate!` embeds the files at compile time, so a test cannot pass
    // against a schema it did not apply and there is no path where a test
    // depends on someone having run a migration by hand.
    sqlx::migrate!("./migrations")
        .run(&pool)
        .await
        .unwrap_or_else(|e| panic!("migrations failed: {e}"));

    let store = Store::from_pool(pool);
    truncate(&store).await;
    store
}

/// Empty every table this service owns. `restart identity` resets the
/// sequences so an id is never reused across tests, which would make a
/// "no second asset was created" assertion pass for the wrong reason.
pub async fn truncate(store: &Store) {
    sqlx::query(
        "truncate assets, asset_variants, outbox_events, idempotency_keys restart identity cascade",
    )
    .execute(store.pool())
    .await
    .expect("truncate");
}

/// A service wired to the in-memory object store. Never S3 — see the module
/// docs.
pub fn test_service(store: Store) -> (Service, Arc<InMemoryObjectStore>) {
    let objects = Arc::new(InMemoryObjectStore::new());
    let service = Service::with_in_memory_objects(Arc::new(store), objects.clone());
    (service, objects)
}

/// A router with a verifier that knows exactly these tokens.
pub fn test_app(
    service: Service,
    verifier: StaticTokenVerifier,
) -> (axum::Router, Arc<StaticTokenVerifier>) {
    let shared = Arc::new(verifier);
    let state = AppState {
        service,
        verifier: shared.clone(),
    };
    (darkroom::http::router(state), shared)
}

/// A principal with both scopes, in `account`.
pub fn principal(account: Uuid, user: Uuid) -> Principal {
    Principal {
        user_id: user,
        account_id: account,
        scopes: vec![SCOPE_ASSETS_READ.into(), SCOPE_ASSETS_WRITE.into()],
    }
}

/// Two accounts and their users, named `a` and `b`. The tenant-isolation suite
/// uses these for every cross-tenant case.
pub struct TwoAccounts {
    pub a_account: Uuid,
    pub a_user: Uuid,
    pub b_account: Uuid,
    pub b_user: Uuid,
}

pub fn two_accounts() -> TwoAccounts {
    TwoAccounts {
        a_account: Uuid::new_v4(),
        a_user: Uuid::new_v4(),
        b_account: Uuid::new_v4(),
        b_user: Uuid::new_v4(),
    }
}

/// A verifier with one token per account, named `token-a` and `token-b`.
pub fn verifier_for(accounts: &TwoAccounts) -> StaticTokenVerifier {
    StaticTokenVerifier::new()
        .with_token("token-a", principal(accounts.a_account, accounts.a_user))
        .with_token("token-b", principal(accounts.b_account, accounts.b_user))
}

/// A real PNG of the given size, so the variant tests exercise the decoder
/// rather than a fixture nobody remembers to regenerate.
pub fn png(width: u32, height: u32) -> bytes::Bytes {
    use image::{ImageBuffer, Rgba};
    let mut buffer = ImageBuffer::new(width, height);
    for (x, y, pixel) in buffer.enumerate_pixels_mut() {
        *pixel = Rgba([(x % 256) as u8, (y % 256) as u8, 96, 255]);
    }
    let mut out = Vec::new();
    image::DynamicImage::ImageRgba8(buffer)
        .write_to(&mut std::io::Cursor::new(&mut out), image::ImageFormat::Png)
        .expect("a PNG always encodes");
    bytes::Bytes::from(out)
}

/// What a [`TestObjectStore`] does to the bytes it hands back. The only axis the
/// object-store behaviour table varies on.
#[derive(Clone, Copy, PartialEq, Eq)]
pub enum Fault {
    /// Returns exactly what was stored.
    None,
    /// Returns the same length with one byte flipped.
    CorruptOnRead,
}

/// The object store the database suites run against: the honest in-memory fake,
/// optionally wrapped in a fault.
///
/// ## Why the fault exists
///
/// It is the shape of a bucket holding something other than what was PUT — a
/// proxy that truncated the body, a replication step that mangled a chunk, a
/// client library that reported success on a partial write. It flips one byte
/// and keeps the length, deliberately, so the failure it produces is a
/// **checksum mismatch** and not a size mismatch: a size check that caught it
/// would prove nothing about verification.
///
/// It is the R2 case by construction. A backend that cannot be asked for a
/// checksum it never computed — R2 offers `FULL_OBJECT` for CRC-64/NVME only —
/// can only be verified by reading the object, so the store under this test is
/// the only store a correct implementation can rely on. A `head`-only check sees
/// nothing wrong here: the object is present, the size is right, the content
/// type is right. Only reading the bytes finds it.
pub struct TestObjectStore {
    inner: Arc<InMemoryObjectStore>,
    fault: Fault,
}

impl TestObjectStore {
    /// The honest store, which is what every other suite in this crate uses.
    pub fn new() -> Self {
        Self {
            inner: Arc::new(InMemoryObjectStore::new()),
            fault: Fault::None,
        }
    }

    /// The same store, damaging what it hands back.
    pub fn corrupting() -> Self {
        Self {
            inner: Arc::new(InMemoryObjectStore::new()),
            fault: Fault::CorruptOnRead,
        }
    }

    /// The store underneath, so a test can PUT bytes through a presigned URL
    /// exactly the way a client does — including for the fault row, where the
    /// damage happens on the read and not on the write. A client that uploaded
    /// nothing is a different bug (409) and belongs in another file.
    pub fn inner(&self) -> &Arc<InMemoryObjectStore> {
        &self.inner
    }

    pub fn stats(&self) -> StoreStats {
        self.inner.stats()
    }
}

#[async_trait]
impl ObjectStore for TestObjectStore {
    async fn presign_put(
        &self,
        key: &str,
        content_type: &str,
        max_bytes: i64,
        ttl: std::time::Duration,
    ) -> Result<PresignedPut, ObjectStoreError> {
        self.inner
            .presign_put(key, content_type, max_bytes, ttl)
            .await
    }

    /// Honest under both faults. The point of the fault is that metadata cannot
    /// reveal the corruption, so this returns exactly what the inner store holds.
    async fn head(&self, key: &str) -> Result<ObjectMeta, ObjectStoreError> {
        self.inner.head(key).await
    }

    async fn get(&self, key: &str) -> Result<Bytes, ObjectStoreError> {
        let bytes = self.inner.get(key).await?;
        match self.fault {
            Fault::None => Ok(bytes),
            Fault::CorruptOnRead if bytes.is_empty() => Ok(bytes),
            Fault::CorruptOnRead => {
                let mut damaged = bytes.to_vec();
                damaged[0] ^= 0xff;
                Ok(Bytes::from(damaged))
            }
        }
    }

    async fn put(
        &self,
        key: &str,
        bytes: Bytes,
        content_type: &str,
    ) -> Result<(), ObjectStoreError> {
        self.inner.put(key, bytes, content_type).await
    }

    async fn delete(&self, key: &str) -> Result<(), ObjectStoreError> {
        self.inner.delete(key).await
    }
}
