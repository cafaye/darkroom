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
//! # the shared cluster, brought up with darkroom's compose file merged over
//! # kit's pinned stack. See README "Running it" and .env.example.
//! KIT_COMPOSE_DIR=<kit>/templates/compose \
//! docker compose --project-directory . \
//!   -f <kit>/templates/compose/docker-compose.yml \
//!   -f ./docker-compose.yml up -d --wait
//! cargo test -- --ignored
//! ```
//!
//! `TEST_DATABASE_URL` is the HOST-side URL — `localhost` and
//! `KIT_POSTGRES_PORT` (15500, not 5432). It names the same `darkroom` database
//! the service uses and the CLUSTER's password rather than a per-service one,
//! because both changed when this repository joined kit's shared cluster.
//!
//! ## Why a schema and not a database, and why not `--test-threads=1`
//!
//! Every test in this tree calls [`test_store`], and every one of them used to
//! get the same tables in the same database and then `truncate` them. That is a
//! real constraint and it was the reason the suite ran one test at a time: two
//! concurrent tests each delete the rows the other just wrote, and the symptom
//! is an assertion failing about rows that genuinely were not created.
//! `NotFound { detail: "asset not found" }` on a test that created the asset two
//! lines earlier is the worst kind of test failure to debug and the easiest to
//! misread as a product defect.
//!
//! So each test gets its own **schema** inside that one database: `t_` plus
//! eight hex characters of a fresh uuid, the migrations applied into it, and
//! the pool's `search_path` pointed at it. `truncate()` still runs, because a
//! test that reuses its own store across steps needs it, but the names it
//! truncates are unqualified and therefore resolve through that `search_path` —
//! it can empty this test's tables and nothing else. Nothing in the other
//! suites changed, because [`Store::from_pool`] already took a pool and the
//! schema is chosen before the pool is built.
//!
//! Three alternatives were rejected for stated reasons. A database per test is
//! the textbook answer and the wrong one here: `TEST_DATABASE_URL` names one
//! database and creating fifty-four of them changes its shape, which is out of
//! scope and slower. A transaction rolled back at the end of each test cannot
//! work at all, because [`Service`] holds a transaction open across an `await`
//! on object storage (that is the outbox rule), so the test's own writes are not
//! visible to it. And a global mutex is `--test-threads=1` wearing a different
//! hat.
//!
//! ## `search_path` is `t_<hex>` and NOT `t_<hex>, public`
//!
//! `sqlx` records applied migrations in `_sqlx_migrations`, and whether that
//! lands in this test's schema or in whatever the path falls back to is a
//! property of how the path is set, not of sqlx: it creates the table with an
//! unqualified `create table if not exists`. **Measured, from two schemas at
//! once:** it lands in this test's schema, and the catalog holds one ledger per
//! schema rather than one shared ledger. `tests/schema_isolation.rs` asserts
//! that, because this paragraph is a claim and a claim about a `drop schema`
//! wants a check.
//!
//! The fallback is the part worth being careful about, and the obvious reason
//! for it is wrong. `t_<hex>, public` does NOT make the migrations no-ops: the
//! target of unqualified DDL is the first schema on the path and `if not exists`
//! is checked there, so the tables and the ledger still land in `t_<hex>` (also
//! measured — see the report). What the fallback buys is worse than a build
//! failure: a table this test's schema does not have but `public` does resolves
//! to `public`'s copy. The same query returns `relation "zz_probe" does not
//! exist` without the fallback and `Ok(0)` off the shared table with one, and
//! nothing in the suite would report that as anything other than a passing test.
//! So the path is the one schema and nothing else, `current_schemas(false)` has
//! length one, and it is asserted on every fetch rather than sampled.
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

/// The schema a test owns: `SCHEMA_PREFIX` and the first
/// [`SCHEMA_HEX_CHARS`] hex characters of a fresh v4 uuid.
///
/// Thirty-two bits of name for roughly fifty-four schemas a run. The chance of
/// two tests picking the same one is about 3e-7 a run, and it is worth saying
/// which way that failure points, because the answer is the reason the name is
/// allowed to be this short: two tests sharing a schema is exactly the
/// arrangement this harness exists to remove, so they would truncate each
/// other and the suite would go RED. It cannot make a test pass that should
/// have failed.
pub fn fresh_schema() -> String {
    let hex = Uuid::new_v4().simple().to_string();
    format!("{SCHEMA_PREFIX}{}", &hex[..SCHEMA_HEX_CHARS])
}

/// The prefix every test schema carries, and the only thing the janitor's
/// candidate query filters on. `darkroom` names no schema like this, so a
/// collision with a real one would be visible in that name.
pub const SCHEMA_PREFIX: &str = "t_";

/// Eight hex characters, i.e. 32 bits. See [`fresh_schema`] for why that is
/// enough and why a collision would be a red test rather than a green one.
pub const SCHEMA_HEX_CHARS: usize = 8;

/// Whether `name` is one this harness would generate, and therefore safe to
/// interpolate into SQL as an identifier.
///
/// This is the gate on the only two places a name reaches SQL as an identifier
/// rather than as a bind parameter — `create schema` and `set search_path` take
/// identifiers, and Postgres has no parameterised form of either. It answers a
/// question about the NAME, not about its provenance: `t_0123abcd` was not
/// generated by this process and is accepted, because what matters is that it
/// can only be those twelve characters. Upper case, quotes, spaces, semicolons,
/// dashes and the wrong length are all refused.
pub fn is_plain_identifier(name: &str) -> bool {
    let Some(rest) = name.strip_prefix(SCHEMA_PREFIX) else {
        return false;
    };
    rest.len() == SCHEMA_HEX_CHARS
        && rest
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

/// `name` as a SQL identifier, double-quoted.
///
/// Quoting AND [`is_plain_identifier`], which is belt and braces on purpose and
/// says so: the quoting means a name this harness did not generate could not
/// break out of the statement even if the assertion were deleted, and the
/// assertion means the shape of a generated name is a claim somebody can check
/// without a database. `tests/schema_isolation.rs` checks both halves, the
/// second one in the default tier.
pub fn quoted(name: &str) -> String {
    assert!(
        is_plain_identifier(name),
        "{name:?} is not a test schema name, and this is the only place one is \
         interpolated into SQL"
    );
    format!("\"{name}\"")
}

/// The comment every test schema carries: [`SCHEMA_STAMP_PREFIX`] and the
/// millisecond clock, so the janitor can tell a schema this harness made
/// minutes ago from one a run left behind days ago.
///
/// A comment rather than something in the name, because the packet's name has
/// eight hex characters and an eight-character unix timestamp does not exist.
/// It also means a schema the janitor does not recognise — a developer's own,
/// or a `CREATE SCHEMA` from a psql session — is never a candidate, which is
/// the property [`stamp_millis`] returning `None` is what asserts.
pub fn schema_stamp(millis: u64) -> String {
    format!("{SCHEMA_STAMP_PREFIX}{millis}")
}

/// The prefix [`schema_stamp`] writes and [`stamp_millis`] insists on.
pub const SCHEMA_STAMP_PREFIX: &str = "darkroom-test ";

/// The millisecond clock out of a schema comment, or `None` for anything that
/// is not one of ours.
///
/// Two parts, the fixed prefix, and then digits and nothing else: a comment
/// this cannot read is not a schema this harness made, and the janitor leaves
/// it alone. `darkroom-test 12x`, `darkroom-test ` and `darkroom-test 1 2` are
/// all `None`, and none of them is a reason to drop anything.
pub fn stamp_millis(comment: &str) -> Option<u64> {
    let millis = comment.strip_prefix(SCHEMA_STAMP_PREFIX)?;
    if millis.is_empty() || !millis.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    millis.parse().ok()
}

/// How old a test schema has to be before a run drops it.
///
/// Six hours is two claims. A suite takes seconds to minutes, so no run that is
/// still going has a schema this old, and the janitor cannot pull the schema out
/// from under a live test. And a database that is used a few times a week still
/// does not accumulate schemas forever, which matters because every test in
/// every run leaves one behind and nothing else in the harness can remove it:
/// [`test_store`] hands back a [`Store`], not a guard, and a schema cannot be
/// dropped from a `Drop` impl on production code.
pub const STALE_AFTER: std::time::Duration = std::time::Duration::from_secs(6 * 60 * 60);

/// The wall clock, in milliseconds. Public because the janitor's test has to
/// plant a stamp that is genuinely old rather than assert against a mock.
pub fn now_millis() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        // Before 1970 is not a thing that happens on a developer's machine, and
        // a zero stamp makes a schema look ancient rather than making this panic
        // in the middle of a test.
        .unwrap_or(0)
}

/// The URL the database suites run against. Panics by name, because a failed
/// harness in CI should read as "TEST_DATABASE_URL is not set" at a glance
/// rather than as a bare `unwrap` panic.
pub fn test_database_url() -> String {
    std::env::var("TEST_DATABASE_URL")
        .expect("TEST_DATABASE_URL must be set to run the database tests")
}

/// A pool with NOTHING on its `search_path`, for the tests that have to ask the
/// database about the per-test schemas rather than through one. The harness
/// itself never uses it: an `unset` search path is the default and the whole
/// design is that nothing resolves out of it.
pub async fn unscoped_pool() -> sqlx::PgPool {
    PgPoolOptions::new()
        .max_connections(2)
        .acquire_timeout(std::time::Duration::from_secs(5))
        .connect(&test_database_url())
        .await
        .unwrap_or_else(|e| panic!("could not connect to TEST_DATABASE_URL: {e}"))
}

/// Create the test's schema and stamp it, from a pool whose `search_path`
/// already names it.
///
/// That order looks wrong and is not: `set search_path` to a schema that does
/// not exist is not an error in Postgres, it just leaves `current_schema()`
/// null, and `create schema` names its target explicitly rather than resolving
/// it — so this works on one pool, and the connection that runs the migrations
/// afterwards is the one with the path already set. Measured, not assumed; see
/// the report.
async fn stamp_schema(pool: &sqlx::PgPool, schema: &str) {
    let name = quoted(schema);
    sqlx::query(&format!("create schema {name}"))
        .execute(pool)
        .await
        .unwrap_or_else(|e| panic!("could not create schema {schema}: {e}"));
    // The value is `u64` milliseconds formatted as decimal, so it cannot contain
    // a quote; the janitor's own parser is the second thing standing between
    // this string and a `drop schema`.
    sqlx::query(&format!(
        "comment on schema {name} is '{}'",
        schema_stamp(now_millis())
    ))
    .execute(pool)
    .await
    .unwrap_or_else(|e| panic!("could not stamp schema {schema}: {e}"));
}

/// Drop the test schemas no run can still be using — see [`STALE_AFTER`].
///
/// Called on every [`test_store`] rather than once per process, because `Once`
/// around an async body needs a runtime handle this module does not have, and
/// because the query is one indexed catalog lookup that returns nothing on a
/// clean database. The decision of what to drop is made HERE, in Rust, not in
/// the SQL: the query only offers up schemas whose name starts with
/// `SCHEMA_PREFIX`, and everything after that — the name's shape, the stamp's
/// shape, its age — is [`is_plain_identifier`] and [`stamp_millis`]. So the
/// rules are unit-testable on a machine with no Postgres, which is where they
/// are tested.
pub async fn reap_stale_schemas(pool: &sqlx::PgPool, older_than: std::time::Duration) {
    let cutoff = now_millis().saturating_sub(older_than.as_millis() as u64);
    let candidates: Vec<(String, Option<String>)> = sqlx::query_as(
        "select n.nspname, obj_description(n.oid, 'pg_namespace') \
           from pg_namespace n \
          where n.nspname ~ '^t_'",
    )
    .fetch_all(pool)
    .await
    .unwrap_or_else(|e| panic!("could not list schemas to reap: {e}"));

    for (name, comment) in candidates {
        if !is_plain_identifier(&name) {
            continue;
        }
        let Some(millis) = comment.as_deref().and_then(stamp_millis) else {
            continue;
        };
        if millis >= cutoff {
            continue;
        }
        sqlx::query(&format!("drop schema if exists {} cascade", quoted(&name)))
            .execute(pool)
            .await
            .unwrap_or_else(|e| panic!("could not drop the stale schema {name}: {e}"));
    }
}

/// Connect, migrate into a schema of this test's own, and hand back a clean
/// store. Panics with a message that names the variable rather than a bare
/// `unwrap` panic: a failed harness in CI should read as "TEST_DATABASE_URL is
/// not set" at a glance.
pub async fn test_store() -> Store {
    let schema = fresh_schema();
    // Set on every connection in the pool, not once on one of them. A pool
    // hands out any of its connections to any statement, so a `search_path`
    // applied to a single connection would make this test's isolation hold for
    // whichever query happened to get that one — the shape of bug that passes
    // in a small suite and fails under load.
    let search_path = format!("set search_path to {}", quoted(&schema));

    let pool = PgPoolOptions::new()
        .max_connections(5)
        .acquire_timeout(std::time::Duration::from_secs(5))
        .after_connect(move |conn, _meta| {
            let search_path = search_path.clone();
            Box::pin(async move {
                sqlx::query(&search_path).execute(&mut *conn).await?;
                Ok(())
            })
        })
        .connect(&test_database_url())
        .await
        .unwrap_or_else(|e| panic!("could not connect to TEST_DATABASE_URL: {e}"));

    reap_stale_schemas(&pool, STALE_AFTER).await;
    stamp_schema(&pool, &schema).await;

    // `sqlx::migrate!` embeds the files at compile time, so a test cannot pass
    // against a schema it did not apply and there is no path where a test
    // depends on someone having run a migration by hand.
    //
    // It runs on the pool rather than on one connection, and it therefore runs
    // with this test's schema as the first entry on the path — which is the
    // whole reason `_sqlx_migrations` is per-test rather than shared. sqlx
    // takes a database-wide advisory lock around this, so the DDL of concurrent
    // tests serialises; that is a wall-clock cost of milliseconds per test and
    // it buys the guarantee that two migrators can never be in one schema, so
    // it is left on.
    sqlx::migrate!("./migrations")
        .run(&pool)
        .await
        .unwrap_or_else(|e| panic!("migrations failed: {e}"));

    let store = Store::from_pool(pool);
    truncate(&store).await;
    store
}

/// Empty every table this service owns, in THIS test's schema.
///
/// `restart identity` resets the sequences so an id is never reused across
/// tests, which would make a "no second asset was created" assertion pass for
/// the wrong reason.
///
/// The names are unqualified and that is the isolation, not an oversight:
/// `truncate assets` resolves through the pool's `search_path`, which
/// [`test_store`] pointed at a schema nobody else knows the name of. Writing
/// `public.assets` here would restore the exact bug this harness was built to
/// remove, and the fastest way to do that by accident is a "harmless" edit that
/// qualifies the names to be explicit.
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
