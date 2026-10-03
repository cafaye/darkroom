//! Per-test schema isolation — the property the database suite's ability to run
//! in parallel rests on.
//!
//! ## Why this file exists at all
//!
//! `tests/common/mod.rs` gives every test a schema of its own, applies the
//! migrations into it and puts that schema on the pool's `search_path`, and
//! `truncate` then empties this test's tables because the names it truncates
//! are unqualified. Read that description and it is a plausible design; the
//! suite running green is not evidence, because the same design with
//! `t_<hex>, public` on the path fails as the bug it replaced — the migrations
//! become no-ops, every query resolves back to the shared tables, and nothing
//! says so.
//!
//! So this file states the property three ways, and the split is the point:
//!
//!   - the NAME, its shape, the stamp and the janitor's arithmetic, in the
//!     DEFAULT tier, because none of them needs a database and a bare machine
//!     should still check them;
//!   - the SEARCH PATH, and whether `_sqlx_migrations` landed in this test's
//!     schema or in a shared one, in the database tier — because that is a
//!     question about what sqlx's `create table if not exists` resolved to, and
//!     it was measured rather than reasoned about (see the packet report);
//!   - the PROPERTY, one test's `truncate` against another test's rows, with
//!     the interleaving written out rather than waited for.
//!
//! ## What a planted divergence here proves
//!
//! Three of these guards can pass on a broken harness, and all three were
//! checked by breaking them on purpose rather than by reading them:
//!
//!   - the janitor filtering on the name alone: it would `drop schema cascade` a
//!     developer's own schema on a database that happens to contain a `t_`
//!     something. The stamp assertions are what hold that.
//!   - `set search_path` once on ONE connection instead of in `after_connect`:
//!     a small suite passes, because most tests never open a second connection
//!     and the one that does may get the same one back. Asserting the path on
//!     every fetch is what makes it visible at all.
//!   - `truncate` qualified to `public.assets` "to be explicit": every test in
//!     this file that asserts on its own schema still passes, and the whole
//!     suite goes back to sharing tables. That one is caught by
//!     `one_tests_truncate_cannot_delete_anothers_rows` and by nothing else in
//!     the tree, which is why it is here rather than in the suite that broke.
//!
//! ## And one thing that was believed and then measured false
//!
//! `search_path = t_x, public` was going to be the interesting hazard: the story
//! was that `create table if not exists assets` would be satisfied by the table
//! already in `public`, the per-test schema would end up empty, and every query
//! would resolve back to the shared tables. It is not what happens. The target
//! of unqualified DDL is the FIRST schema on the path and `if not exists` is
//! checked there, so the tables and the ledger land in `t_x` either way, and a
//! full `sqlx::migrate!` run against `t_x, public` left `public` untouched.
//!
//! The fallback is still not wanted, for a quieter reason and a measured one:
//! a table the test's schema does not have but `public` does resolves to
//! `public`'s copy. Without the fallback that query is
//! `relation "zz_probe" does not exist`; with it, it is another test's rows
//! read as this test's. The path stays one schema, and the assertion is the
//! length of `current_schemas(false)`.

mod common;

use common::*;
use darkroom::auth::Tenant;
use darkroom::checksum::sha256_hex;
use darkroom::service::CreateUpload;
use darkroom::store::Store;
use uuid::Uuid;

// ------------------------------------------------- the name, with no database

/// Every schema name this harness generates is one it would also accept, and
/// sixty-four of them are distinct.
///
/// Sixty-four rather than a thousand, because the property worth asserting here
/// is distinctness and a thousand samples collide roughly one time in ten
/// thousand: a test that fails for the right reason at the wrong rate is a
/// flake, not a check. The distinctness that matters at run time is fifty-odd
/// schemas against 2^32, which is the arithmetic in `fresh_schema`'s own
/// documentation.
#[test]
fn every_generated_schema_name_is_a_plain_sql_identifier() {
    let mut seen = std::collections::HashSet::new();
    for _ in 0..64 {
        let name = fresh_schema();
        assert!(is_plain_identifier(&name), "{name:?} is not one of ours");
        assert_eq!(name.len(), SCHEMA_PREFIX.len() + SCHEMA_HEX_CHARS);
        assert!(seen.insert(name.clone()), "{name:?} came up twice");
    }
}

/// The check on the two statements that interpolate an identifier is a check
/// and not a rubber stamp.
///
/// Every name below is one this harness would never generate, and every one of
/// them is refused. The last paragraph is the case that is easy to get wrong in
/// the other direction: `t_0123abcd` was NOT generated by this process and is
/// accepted anyway, because what has to be established is that the name can only
/// be twelve characters from a fixed alphabet — not that some earlier line of
/// code vouched for it.
#[test]
fn the_identifier_check_refuses_anything_it_would_not_generate() {
    for name in [
        "",
        "t_",
        "t_0123abc",
        "t_0123abcde",
        "t_0123ABCD",
        "t_0123abcg",
        "t_0123-abc",
        "t_0123 abc",
        "t_0123abc'",
        "t_0123abc\"",
        "t_0123abc; drop schema public cascade --",
        "t_0123abc\0",
        "public",
        "\"t_0123abc\"",
    ] {
        assert!(
            !is_plain_identifier(name),
            "{name:?} should have been refused and was not"
        );
    }
    for name in ["t_0123abcd", "t_00000000", "t_ffffffff"] {
        assert!(
            is_plain_identifier(name),
            "{name:?} is a name this harness would generate"
        );
    }
}

/// The double quotes are not the check, and this is what says so.
///
/// `quoted` asserts and then quotes, which is belt and braces on purpose: if
/// someone deletes the assertion the quoting still cannot be escaped out of,
/// and if someone deletes the quoting the assertion still refuses. The test that
/// holds the assertion in place is this one — a `quoted` that returned a string
/// instead of panicking would fail here, and a `quoted` that had no assertion
/// in it at all would leave every other test in this file green.
#[test]
#[should_panic(expected = "is not a test schema name")]
fn quoting_a_name_this_harness_would_not_generate_panics_rather_than_quoting_it() {
    let quoted = quoted("public");
    // Unreachable, and the point: `quoted` refuses before it can produce this.
    assert_eq!(quoted, "\"public\"");
}

/// The janitor's clock, in both directions.
///
/// A comment this cannot read is not a schema this harness made, and the janitor
/// leaves it alone. That is a claim about a `drop schema`, so it is checked
/// against the shapes it has to refuse rather than against one shape it happens
/// to accept.
#[test]
fn a_stamp_round_trips_and_an_unfamiliar_comment_is_not_a_stamp() {
    for millis in [0u64, 1, 1_700_000_000_000, u64::MAX] {
        assert_eq!(
            stamp_millis(&schema_stamp(millis)),
            Some(millis),
            "a stamp we wrote has to read back as the number we wrote"
        );
    }
    assert_eq!(
        schema_stamp(1_700_000_000_000),
        format!("{SCHEMA_STAMP_PREFIX}1700000000000")
    );
    for comment in [
        "",
        "darkroom",
        "darkroom-test",
        "darkroom-test ",
        "darkroom-test 12x",
        "darkroom-test 12 34",
        "darkroom-test -1",
        "darkroom-test 1.0",
        " darkroom-test 12",
        "public",
        "v123 darkroom-test 12",
    ] {
        assert_eq!(
            stamp_millis(comment),
            None,
            "{comment:?} must not parse as a stamp, or the janitor would treat \
             a schema it does not own as one of ours"
        );
    }
}

// ---------------------------------------------- the search path, with a database

/// Two stores, two schemas, and a path with nowhere to fall out of.
///
/// The `current_schemas(false)` assertion is the one in this file that matters
/// most. `t_x, public` is a reasonable-looking `search_path`, and it does NOT
/// break the migrations — measured, because it was the obvious thing to believe:
/// `create table if not exists` resolves its target to the FIRST schema on the
/// path and checks existence there, so the tables and `_sqlx_migrations` both
/// still land in `t_x`.
///
/// What the fallback would cost is quieter. A table this test's schema does not
/// have, but `public` does, resolves to `public`'s copy instead of failing:
/// `relation "zz_probe" does not exist` is what the same query returns without
/// the fallback, and `Ok(0)` off the shared table is what it returns with one.
/// That is another test's rows arriving as this test's, and no assertion in the
/// suite would see it happen. One schema and nothing else turns that class of
/// mistake into an error at the moment it is made.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn two_stores_get_two_schemas_and_neither_can_fall_out_of_one() {
    let a = test_store().await;
    let b = test_store().await;

    let a_schema = current_schema(&a).await;
    let b_schema = current_schema(&b).await;
    assert!(is_plain_identifier(&a_schema), "{a_schema:?} is not ours");
    assert!(is_plain_identifier(&b_schema), "{b_schema:?} is not ours");
    assert_ne!(
        a_schema, b_schema,
        "two stores resolved to one schema, so two tests are sharing tables"
    );

    for (store, schema) in [(&a, &a_schema), (&b, &b_schema)] {
        let path: Vec<String> = sqlx::query_scalar("select unnest(current_schemas(false)::text[])")
            .fetch_all(store.pool())
            .await
            .expect("reads the effective search path");
        assert_eq!(
            path,
            vec![schema.clone()],
            "the search path has to be this test's schema and nothing else: a \
             fallback to public resolves a table this schema does not have to \
             public's copy, which is another test's rows arriving as this \
             test's"
        );
    }
}

/// Where `_sqlx_migrations` actually landed.
///
/// sqlx records applied migrations with an unqualified
/// `create table if not exists _sqlx_migrations`, so whether the ledger is
/// per-test or shared is decided entirely by the search path and not by sqlx.
/// This asserts both halves: each store reads three applied migrations through
/// its own unqualified name, and the catalog says there are two ledgers in two
/// schemas rather than one ledger read twice.
///
/// The descriptions are sqlx's, not the filenames': it turns `0002_outbox_events`
/// into "outbox events". The assertion below was written with the filenames and
/// measured into this shape, which is the second time in this file that a
/// measurement corrected a plausible assumption.
///
/// The catalog is queried through [`unscoped_pool`] rather than through a store,
/// because a query issued on a store's pool cannot see anything outside that
/// store's schema — which is the property being measured, and would otherwise
/// make the measurement circular.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn each_test_schema_carries_its_own_migration_ledger() {
    let a = test_store().await;
    let b = test_store().await;
    let expected = vec![
        (1i64, "assets".to_string()),
        (2, "outbox events".to_string()),
        (3, "idempotency keys".to_string()),
    ];

    for store in [&a, &b] {
        let applied: Vec<(i64, String)> =
            sqlx::query_as("select version, description from _sqlx_migrations order by version")
                .fetch_all(store.pool())
                .await
                .expect("reads the ledger through the unqualified name");
        assert_eq!(
            applied, expected,
            "every test applies every migration into its own schema"
        );
    }

    let owners: Vec<String> = sqlx::query_scalar(
        "select n.nspname
           from pg_class c
           join pg_namespace n on n.oid = c.relnamespace
          where c.relname = '_sqlx_migrations'",
    )
    .fetch_all(&unscoped_pool().await)
    .await
    .expect("reads the catalog");
    for schema in [current_schema(&a).await, current_schema(&b).await] {
        assert!(
            owners.contains(&schema),
            "no ledger in {schema}: the two stores are sharing one, which is \
             what happens when the path falls back to a schema that already \
             holds the tables. owners: {owners:?}"
        );
    }
}

/// The property, with the interleaving written out instead of waited for.
///
/// Before the schema existed this is the assertion that could not be made at
/// all: `truncate` emptied the tables, so B's truncate deleted the row A had
/// written and A's `count` read zero. Asserting it here means a regression in
/// the harness is a red test in the suite that owns the harness, not a
/// flake that shows up as `NotFound` in an unrelated suite.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn one_tests_truncate_cannot_delete_anothers_rows() {
    let a = test_store().await;
    let b = test_store().await;

    seed_one_ready_asset(&a).await;
    // Exactly what every test in this tree does on entry, run while A is
    // holding rows it is about to assert on.
    truncate(&b).await;
    assert_eq!(
        asset_rows(&a).await,
        1,
        "another test's truncate deleted this test's fixture, which is the \
         failure mode that made --test-threads=1 load-bearing"
    );

    // And the other direction, because isolation that only holds one way is not
    // isolation.
    seed_one_ready_asset(&b).await;
    truncate(&a).await;
    assert_eq!(
        asset_rows(&b).await,
        1,
        "the same in reverse: A's truncate reached into B's schema"
    );
}

/// `truncate` points at THIS test's schema — the half of the property that is
/// easy to lose and that nothing else in the tree was holding.
///
/// Found by planting the divergence, not by reading this file. With `truncate`
/// rewritten to name `public.assets` and its three siblings, **the entire
/// database tier stayed green**: the cross-test guard above still passes (a
/// `public` truncate cannot reach into another schema), every test gets a fresh
/// empty schema anyway, and `test_store`'s own truncate deletes nothing because
/// the schema it is called on was created empty a line earlier. A `truncate`
/// that quietly empties the SERVICE's tables is the sharpest edge in this test
/// suite and it had no guard at all.
///
/// So: A writes, B writes, `truncate(&a)`, and A's rows are gone while B's are
/// not. Both directions, because the first half alone cannot tell "truncate does
/// nothing" from "truncate empties the right schema" — a `truncate` that
/// silently did nothing would satisfy it too.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn truncate_empties_this_tests_schema_and_only_that() {
    let a = test_store().await;
    let b = test_store().await;
    seed_one_ready_asset(&a).await;
    seed_one_ready_asset(&b).await;
    assert_eq!(asset_rows(&a).await, 1, "A wrote a row");
    assert_eq!(asset_rows(&b).await, 1, "B wrote a row");

    truncate(&a).await;

    assert_eq!(
        asset_rows(&a).await,
        0,
        "truncate did not empty this test's own schema. Either it names \
         something other than this test's tables — public.assets being the one \
         that matters, since that is the service's own database — or it does \
         nothing, which leaves a test that reuses its store across steps reading \
         the rows it wrote in the previous step."
    );
    assert_eq!(
        asset_rows(&b).await,
        1,
        "truncate reached into a schema it does not own"
    );
}

/// What the janitor drops and what it leaves.
///
/// Four schemas, four answers. The one with no comment and the one whose name
/// only looks like ours are the interesting half: a janitor that filtered on the
/// name alone would `drop schema cascade` both of them, and one of them is a
/// developer's own schema on a database that happens to contain a `t_`
/// something. The clock is [`STALE_AFTER`]'s, and the planted stamp is well
/// past it, so this is the production threshold and not a threshold chosen to
/// make the test pass.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn the_janitor_drops_a_schema_no_run_can_be_using_and_leaves_everything_else() {
    let mine = test_store().await;
    let mine_schema = current_schema(&mine).await;
    let control = unscoped_pool().await;

    let stale = fresh_schema();
    let uncommented = fresh_schema();
    // Matches the janitor's candidate query (`^t_`) and is refused by
    // `is_plain_identifier`, because `z` is not a hex digit. Quoted here by hand
    // because `quoted` would panic — which is the point: the harness could never
    // have created this one, so the janitor has to be the thing that notices.
    //
    // Unique per run even though the prefix is not: a fixed name here is a
    // planted schema that outlives a FAILED run of this test, because the
    // cleanup below it never executes, and the next run then dies on
    // `42P06 schema already exists` — which is how this was found.
    let not_ours = format!(
        "{SCHEMA_PREFIX}zzzz{}",
        &Uuid::new_v4().simple().to_string()[..4]
    );
    let eight_hours_ago = now_millis().saturating_sub(8 * 60 * 60 * 1000);

    plant(
        &control,
        &quoted(&stale),
        Some(schema_stamp(eight_hours_ago)),
    )
    .await;
    plant(&control, &quoted(&uncommented), None).await;
    plant(
        &control,
        &format!("\"{not_ours}\""),
        Some(schema_stamp(eight_hours_ago)),
    )
    .await;

    reap_stale_schemas(&control, STALE_AFTER).await;

    let left = schema_names(&control).await;
    assert!(
        !left.contains(&stale),
        "an eight-hour-old test schema is still there, so every run of the \
         suite leaks one"
    );
    for kept in [&uncommented, &not_ours, &mine_schema] {
        assert!(
            left.contains(kept),
            "{kept} was dropped by the janitor and should not have been"
        );
    }
    // And this test's own schema is not merely present but usable, since a
    // janitor that truncated the live one would pass the assertions above.
    assert_eq!(asset_rows(&mine).await, 0, "the live schema still works");

    for planted in [&stale, &uncommented, &format!("\"{not_ours}\"")] {
        sqlx::query(&format!("drop schema if exists {planted} cascade"))
            .execute(&control)
            .await
            .expect("cleans up after itself");
    }
}

// ------------------------------------------------------------------- helpers

async fn current_schema(store: &Store) -> String {
    sqlx::query_scalar("select current_schema()")
        .fetch_one(store.pool())
        .await
        .expect("reads current_schema()")
}

async fn asset_rows(store: &Store) -> i64 {
    sqlx::query_scalar("select count(*) from assets")
        .fetch_one(store.pool())
        .await
        .expect("counts assets")
}

/// One asset, through the service, so the row is a real one. A hand-written
/// `insert into assets` would prove the schema isolation just as well and would
/// also stop caring whether the harness produces rows the service can read.
async fn seed_one_ready_asset(store: &Store) {
    let (service, objects) = test_service(store.clone());
    let tenant = Tenant::from_principal(&principal(Uuid::new_v4(), Uuid::new_v4()));
    let payload = png(8, 8);
    let checksum = sha256_hex(&payload);

    let upload = service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "seed.png".into(),
                content_type: "image/png".into(),
                byte_size: payload.len() as i64,
                checksum: checksum.clone(),
            },
        )
        .await
        .expect("creates an upload");
    objects
        .apply_presigned_put(&upload.presigned.url, payload, "image/png")
        .await
        .expect("puts the bytes");
    service
        .complete_upload(&tenant, upload.asset.id, &checksum)
        .await
        .expect("completes it");
}

/// Create a schema named by an ALREADY-QUOTED identifier, optionally stamping
/// it. Takes the quoted form because one case plants a name `quoted` refuses.
async fn plant(pool: &sqlx::PgPool, quoted_name: &str, stamp: Option<String>) {
    sqlx::query(&format!("create schema {quoted_name}"))
        .execute(pool)
        .await
        .expect("plants a schema");
    if let Some(stamp) = stamp {
        sqlx::query(&format!("comment on schema {quoted_name} is '{stamp}'"))
            .execute(pool)
            .await
            .expect("stamps a schema");
    }
}

/// Every schema the janitor's own candidate query would consider.
async fn schema_names(pool: &sqlx::PgPool) -> Vec<String> {
    sqlx::query_scalar("select n.nspname from pg_namespace n where n.nspname ~ '^t_'")
        .fetch_all(pool)
        .await
        .expect("lists candidate schemas")
}
