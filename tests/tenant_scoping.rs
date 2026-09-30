//! The query layer's tenant scoping, checked **as a property of the source**.
//!
//! ## What this file is for
//!
//! `src/store.rs` was correct by construction: `assets` and `asset_variants`
//! both carry `account_id uuid not null`, and every query constrains on it —
//!
//! ```text
//!   where id = $1 and account_id = $2
//!   update assets where id = $1 and account_id = $2 and status = 'pending'
//!   delete from assets where id = $1 and account_id = $2
//! ```
//!
//! — and held in place by **nothing**. Every other test in this repository
//! exercises those queries *through* a `Tenant`, so all of them stay green if a
//! future edit drops `and account_id = $2` from one line. "Correct by
//! construction" is not a property if the construction is never checked, and
//! what is checked here is the construction.
//!
//! ## Why a test and not a review rule
//!
//! Because a review rule is a comment. This file reads `src/store.rs` with
//! `include_str!` — so a failure is a fact about the source that built the
//! binary, not a grep a human has to remember to run — and it derives its
//! expectations from the code rather than from a list somebody maintains. Four
//! properties, each load-bearing on its own:
//!
//! 1. **Every query that takes a `&Tenant` constrains `account_id`.** Derived
//!    from the *signature*, not from an allow-list, so dropping the clause from
//!    `delete_asset` fails without its signature changing at all.
//! 2. **The set of such queries is exactly the ten named below.** A new
//!    tenant-scoped query that forgot `account_id` is a new name and fails. So
//!    does a removal, because a query nobody can find is a query nobody has
//!    ever run — the `find_stale_pending` doc comment says that in prose.
//! 3. **Every mutating scoped query names one row.** `where account_id = $2`
//!    alone would rewrite an entire account: the scoping would be present and
//!    the outcome would still be a defect. This is the check that a "let me
//!    simplify this update" edit has to fail.
//! 4. **Exactly one query reads across accounts, and no request path can reach
//!    it.** `find_stale_pending` is the sweeper. A derived set means a *second*
//!    unscoped read fails; the reachability check means the sweeper cannot be
//!    wired into a handler without going red.
//!
//! ## Why it is in the default tier
//!
//! It needs no database, no Docker and no network — it is string analysis of a
//! file already in the binary. The behavioural half (a two-account fixture,
//! every query, read/list/update/delete) is `tests/query_scoping.rs` and does
//! need Postgres. Two files because they answer different questions: *is the
//! scoping still written down* is a property of the source and belongs on a
//! bare machine; *does the scoping actually hold* is a property of Postgres and
//! cannot be checked without it.
//!
//! They are not substitutes and this file does not pretend to be the second
//! one. A `where` clause that names `account_id` and a `where` clause that is
//! correct are the same string here and different queries in the database, and
//! only `tests/query_scoping.rs` can tell them apart.

// `include_str!` rather than reading the working tree at runtime: the source is
// embedded at compile time, so this cannot pass by reading a file that differs
// from the one that built the binary, and renaming `src/store.rs` breaks the
// build here instead of failing at runtime on someone else's machine.
use std::collections::BTreeMap;

const STORE_SRC: &str = include_str!("../src/store.rs");
const SERVICE_SRC: &str = include_str!("../src/service.rs");
const HTTP_SRC: &str = include_str!("../src/http.rs");
const IDEMPOTENCY_SRC: &str = include_str!("../src/idempotency.rs");
const QUERY_SCOPING_SRC: &str = include_str!("../tests/query_scoping.rs");

/// One `pub async fn` in `store.rs`, with the text that follows it.
struct QueryFn<'a> {
    name: &'a str,
    /// From the opening brace of the body to the next `pub async fn`, or to the
    /// end of the file. Slicing rather than parsing because the question is
    /// "does this function's SQL name the column", and a body slice answers it
    /// without a parser that would have to understand raw string literals. The
    /// tail is taken when there is no next function so the *last* function in
    /// the file is included — a guard with a hole in the one place nobody
    /// re-reads is not a guard.
    body: &'a str,
}

/// Every `pub async fn` in `store.rs`, in source order.
///
/// The unit is the **whole signature and body**, not just the body: the
/// predicate that decides whether a function is tenant-scoped is `tenant:
/// &Tenant`, which lives in the parameter list. Slicing from the opening brace
/// would drop the very thing being filtered on, and every assertion below would
/// quietly pass on an empty set.
///
/// A function's unit therefore also contains the *next* function's doc comment,
/// since the end of one is the start of the next marker. That is harmless and
/// worth saying out loud: nothing here reads prose, only string literals.
fn query_fns(source: &str) -> Vec<QueryFn<'_>> {
    const MARKER: &str = "pub async fn ";
    let mut out = Vec::new();
    let mut rest = source;
    while let Some(at) = rest.find(MARKER) {
        let after = &rest[at + MARKER.len()..];
        // The name is the identifier, so generic parameters (`<'e, E>`) do not
        // end up in it. Without this the expectations below would all be
        // spelled `find_asset<'e, E>` and nobody would notice, because a
        // reviewer would read past it.
        let name_end = after
            .find(|c: char| !(c.is_alphanumeric() || c == '_'))
            .expect("`pub async fn` is followed by a name");
        let unit = match after[name_end..].find(MARKER) {
            Some(next) => &after[name_end..name_end + next],
            // The tail is taken when there is no next function, so the *last*
            // function in the file is included. A guard with a hole in the one
            // place nobody re-reads is not a guard.
            None => &after[name_end..],
        };
        out.push(QueryFn {
            name: &after[..name_end],
            body: unit,
        });
        rest = &after[name_end..];
    }
    out
}

/// Every string literal in a unit, as a slice of it.
///
/// Walked by **character**, not by byte. A byte walk steps one byte at a time and
/// lands inside the multi-byte `—` in a comment, and slicing there panics — a
/// crash in a security guard is a guard that gets deleted rather than fixed.
///
/// Escapes are handled by skipping the escaped character, so `\"` does not end
/// the literal early. A `where` clause spans multiple lines and cannot be forged
/// by a shortened scan, so nothing here is load-bearing on the exactness of the
/// lexer — and a hand-written parser would need its own tests, which is the
/// wrong shape for a guard.
///
/// ## The cursor must consume the closing quote, and it does
///
/// The obvious version of the loop below left the cursor **on** the closing
/// quote rather than past it: `rest` begins after the opening quote and `len` is
/// the offset of the closing quote within `rest`, so `&rest[len..]` starts with
/// that same quote. The next iteration took the same branch again and scanned
/// forward to the *next* quote, so every literal in the text bought a fresh
/// forward scan of everything after it.
///
/// That is quadratic, and it was not slow — it was unbounded. `src/store.rs`
/// carries doc comments containing `"` characters ("`not found` and `not
/// yours`", `"have I seen this key"`), and because a unit runs to the next
/// `pub async fn` it includes the following function's prose. Two tests in this
/// file spent **eleven minutes** of CPU inside `sql_statements` and were still
/// running when the gate was killed, so the guard that is supposed to run on a
/// bare machine in the default tier could not be run at all. The single
/// character `+ 1` is the difference between a security check that executes and
/// one that hangs the gate.
fn sql_statements(body: &str) -> Vec<&str> {
    let mut out = Vec::new();
    let mut cursor = body;
    while !cursor.is_empty() {
        if cursor.starts_with("r#\"") {
            if let Some(end) = cursor[3..].find("\"#") {
                out.push(&cursor[3..3 + end]);
                cursor = &cursor[3 + end + 2..];
                continue;
            }
        }
        if let Some(rest) = cursor.strip_prefix('"') {
            let mut chars = rest.char_indices();
            let mut len = rest.len();
            let mut closed = false;
            while let Some((i, c)) = chars.next() {
                if c == '\\' {
                    // Skip the escaped character. `char_indices` resumes at the
                    // next one, which is exactly the intent.
                    let _ = chars.next();
                    continue;
                }
                if c == '"' {
                    len = i;
                    closed = true;
                    break;
                }
            }
            out.push(&rest[..len]);
            // Step over the closing quote. Leaving the cursor sitting on it is
            // the quadratic bug described above. An unterminated literal — a
            // stray quote in prose, which these units do contain — consumes the
            // rest of the unit rather than indexing one past its end.
            cursor = if closed {
                &rest[len + 1..]
            } else {
                &rest[len..]
            };
            continue;
        }
        // Advance one whole character, so a cursor never lands mid-codepoint.
        // `len_utf8` of the consumed char, **not** `Chars::as_str().len()`:
        // `as_str` is the *remaining* string, so using its length indexes to 0
        // and the loop never advances — which is a guard that hangs rather than
        // one that fails, and a hanging guard gets deleted rather than fixed.
        match cursor.chars().next() {
            Some(c) => cursor = &cursor[c.len_utf8()..],
            None => break,
        }
    }
    out
}

/// The property. `account_id = $N` is the shape every scoped query here uses, so
/// the assertion names the shape rather than "mentions account_id" — a query
/// that named the column in a comment, a `select` list or a bind name would
/// satisfy the looser version and prove nothing.
fn constrains_account_id(body: &str) -> bool {
    sql_statements(body)
        .iter()
        .any(|sql| sql.contains("account_id = $"))
}

/// The statement narrows to a **row** — one asset, or one variant of it — and
/// not merely to an account.
///
/// ## The word boundary is the whole test, and it is not decoration
///
/// The obvious spelling is `sql.contains("id = $") || sql.contains("asset_id =
/// $")`, and that is **true for every statement naming `account_id = $2`**: the
/// substring `id = $` occurs inside `account_id = $2`, starting one character in.
/// So `update assets set status = 'failed' where account_id = $2 and status =
/// 'pending'` — an update that rewrites *every* pending row an account has ever
/// uploaded, the exact defect this check exists to catch — satisfied it.
///
/// That is the shape of a guard that proves the opposite of what it says: it
/// passed the whole-account update, and could only ever have fired on a mutation
/// that also dropped `account_id`, which the sibling check already catches.
/// Measured by mutation rather than by argument: the `where account_id = $2`
/// variant of `mark_failed` left all eleven tests green.
///
/// So the match is anchored at **both** ends of the identifier. A match counts
/// only where `id` is a whole word — the character before it is not a word
/// character, so `account_id` and `owner_id` do not qualify — and it is one of
/// the two column names a row of this schema is addressed by. `asset_id` is
/// deliberately *not* accepted: variants are addressed through their parent
/// asset, and a mutation scoped to `asset_id` without `id` is a different
/// question, asked of no statement in `store.rs` today.
fn names_a_row(sql: &str) -> bool {
    sql.match_indices("id = $").any(|(at, _)| {
        // `at` is the offset of the `i`, so the identifier *ends* two bytes
        // later and *starts* at the last non-word byte before it plus one. The
        // bytes are ASCII in a SQL predicate, so byte-wise is safe and needs no
        // UTF-8 boundary reasoning: every index walked here lands on an ASCII
        // byte.
        let start = sql.as_bytes()[..at]
            .iter()
            .rposition(|b| !is_word_byte(*b))
            .map_or(0, |i| i + 1);
        let column = &sql[start..at + 2];
        // `id` is the primary key; `asset_id` addresses a variant's parent. Both
        // name a row. `account_id` names an account, and is the whole point.
        matches!(column, "id" | "asset_id")
    })
}

fn is_word_byte(b: u8) -> bool {
    b.is_ascii_alphanumeric() || b == b'_'
}

/// Reads the tenant tables at all.
fn reads_tenant_tables(body: &str) -> bool {
    sql_statements(body)
        .iter()
        .any(|sql| sql.contains("from assets") || sql.contains("from asset_variants"))
}

/// The ten, written out because the point of a constant is that somebody has to
/// look at it: a reader checks each name against `src/store.rs` and sees a list
/// of what exists rather than a wish list.
///
/// Four reads, three lists, two updates, one delete. The delete and the two
/// updates are the ones a service that scoped its reads and forgot its writes
/// would be missing, and the count is asserted in a separate test for that
/// reason — a service with four reads and no delete would satisfy "every scoped
/// query is scoped" perfectly.
const TENANT_SCOPED_QUERIES: &[&str] = &[
    // read
    "find_asset",
    "find_asset_by_checksum",
    "find_storage_key",
    "find_variant_storage_key",
    // list
    "list_assets",
    "list_variants",
    "list_storage_keys",
    // update
    "mark_ready",
    "mark_failed",
    // delete
    "delete_asset",
];

/// The predicate above, tested directly — because a guard with a hole in it
/// that nothing exercises is a hole that ships.
///
/// Three cases, and the middle one is the bug this file found in itself: the
/// mutation that turned `where id = $1 and account_id = $2` into `where
/// account_id = $2` passed every check in the file while rewriting every pending
/// row an account had. `where account_id = $2` contains the text `id = $` one
/// character in, so a substring test could not tell the two apart. A check that
/// has never been seen to fail has not been shown to work.
#[test]
fn naming_a_row_is_anchored_to_a_whole_identifier() {
    // The mutation that slipped through: scoped to the account, naming no row.
    assert!(
        !names_a_row("update assets set status = 'failed' where account_id = $2"),
        "`account_id = $2` contains `id = $` as a substring, so an unanchored \
         test calls a whole-account update row-scoped. This is the assertion that \
         makes the anchoring a rule rather than a guess."
    );
    // The other lookalike, for the same reason.
    assert!(
        !names_a_row("delete from assets where owner_id = $1"),
        "the anchoring is about identifiers generally, not about one column"
    );
    // And the shapes that genuinely do name a row.
    for real in [
        "delete from assets where id = $1 and account_id = $2",
        "update assets set status = 'ready' where id = $1 and account_id = $2 and status = 'pending'",
        "select storage_key from asset_variants where asset_id = $1 and account_id = $2",
        "where id = $1",
    ] {
        assert!(names_a_row(real), "a genuine row predicate was rejected: {real}");
    }
}

/// The functions that take a tenant and therefore must scope on it.
fn tenant_scoped() -> Vec<QueryFn<'static>> {
    query_fns(STORE_SRC)
        .into_iter()
        .filter(|f| f.body.contains("tenant: &Tenant"))
        .collect()
}

#[test]
fn every_tenant_scoped_query_constrains_account_id() {
    let scoped = tenant_scoped();
    let mut names: Vec<&str> = scoped.iter().map(|f| f.name).collect();
    let mut expected: Vec<&str> = TENANT_SCOPED_QUERIES.to_vec();
    names.sort_unstable();
    expected.sort_unstable();

    // Sorted, and **deliberately not** in source order. The derived side is the
    // order `src/store.rs` happens to declare its functions in, and the constant
    // is grouped by operation kind, so a sequence comparison asserts a fact
    // about file layout that has nothing to do with tenancy — and it did: this
    // assertion failed on a tree where the two lists held *the same ten names*,
    // because `find_asset_by_checksum` precedes `find_asset` in the source and
    // follows it in the constant.
    //
    // A guard that fails for a reason unrelated to the property is a guard
    // somebody learns to re-run without reading, and a guard that is
    // habitually red is a guard that is not protecting anything. The kind
    // grouping the constant exists to document is asserted on its own, by
    // `the_operation_kinds_are_what_the_comment_claims`, where a change to it
    // is a real change rather than an accident of ordering.
    assert_eq!(
        names, expected,
        "the set of store.rs functions taking a &Tenant moved. If one was added \
         it must constrain account_id like its neighbours. If one was removed, \
         find out who was calling it before removing it from this list: a query \
         with no caller is a query nobody has ever run."
    );

    for f in &scoped {
        assert!(
            constrains_account_id(f.body),
            "store::{} takes a &Tenant but its SQL does not constrain \
             account_id, so account A could read, update or delete account B's \
             rows. This file exists for exactly that line.",
            f.name
        );
    }
}

/// The kind breakdown in `TENANT_SCOPED_QUERIES`' doc comment is a claim about
/// coverage, so it is asserted rather than described.
///
/// "Four reads, three lists, two updates, one delete" is not decoration: it is
/// the sentence that says the suite is not four reads and a shrug, and the
/// delete and the two updates are the operations a service that scoped its reads
/// and forgot its writes would be missing. A grouping that silently changed to
/// four reads and six updates would still pass every other test in this file, so
/// without this the comment is the only thing holding the claim up.
#[test]
fn the_operation_kinds_are_what_the_comment_claims() {
    // The grouping is the constant's own comments, read rather than restated, so
    // a name cannot be moved between kinds by editing one list and not the
    // other. `// read`, `// list`, `// update`, `// delete` are the section
    // headers in the constant below.
    let source = include_str!("tenant_scoping.rs");
    let constant = source
        .split("const TENANT_SCOPED_QUERIES")
        .nth(1)
        .expect("the constant is in this file")
        .split("];")
        .next()
        .expect("the constant is terminated");

    let kind_of = |name: &str| -> &'static str {
        let mut current = "";
        for line in constant.lines() {
            let trimmed = line.trim();
            for kind in ["read", "list", "update", "delete"] {
                if trimmed == format!("// {kind}") {
                    current = match kind {
                        "read" => "read",
                        "list" => "list",
                        "update" => "update",
                        _ => "delete",
                    };
                }
            }
            if trimmed.starts_with('"') && trimmed.contains(&format!("\"{name}\"")) {
                return current;
            }
        }
        panic!("{name} is not in TENANT_SCOPED_QUERIES");
    };

    let mut counts = std::collections::BTreeMap::new();
    for name in TENANT_SCOPED_QUERIES {
        *counts.entry(kind_of(name)).or_insert(0usize) += 1;
    }
    assert_eq!(
        counts,
        BTreeMap::from([("read", 4), ("list", 3), ("update", 2), ("delete", 1)]),
        "TENANT_SCOPED_QUERIES is documented as four reads, three lists, two \
         updates and one delete. A different breakdown is a coverage change and \
         needs the comment changed with it, deliberately."
    );
}

#[test]
fn a_mutating_scoped_query_names_one_row_and_never_a_whole_account() {
    // Scoping an `update` or a `delete` on `account_id` alone is scoped and
    // still a defect: `update assets set status = 'failed' where account_id =
    // $2` rewrites every row an account has ever uploaded. The tenant predicate
    // is necessary and not sufficient — a mutation additionally has to name the
    // row it means.
    let mut checked = 0;
    for f in tenant_scoped() {
        for sql in sql_statements(f.body) {
            let mutating = sql.contains("update ") || sql.contains("delete from");
            if !mutating || !sql.contains("assets") {
                continue;
            }
            checked += 1;
            assert!(
                names_a_row(sql),
                "store::{} mutates the tenant tables without naming a row: \
                 `{sql}`. Scoped to one account, it is still every row in that \
                 account.",
                f.name
            );
        }
    }
    assert_eq!(
        checked, 3,
        "expected the two updates and the one delete, found {checked}. A \
         mutation that is neither an update nor a delete has not been reviewed \
         against this rule."
    );
}

#[test]
fn exactly_one_query_reads_across_accounts_and_it_is_the_sweeper() {
    // Derived rather than listed: every function whose SQL reads the tenant
    // tables without constraining the tenant. Exactly this one name, so a
    // second unscoped read fails here without anybody having decided that is
    // acceptable.
    let unscoped: Vec<&str> = query_fns(STORE_SRC)
        .into_iter()
        .filter(|f| reads_tenant_tables(f.body) && !constrains_account_id(f.body))
        .map(|f| f.name)
        .collect();

    assert_eq!(
        unscoped,
        vec!["find_stale_pending"],
        "these read tenant data with no account_id predicate: {unscoped:?}. That \
         is legitimate for the stale-upload sweeper and for nothing else."
    );
}

#[test]
fn the_sweeper_cannot_be_reached_from_a_request() {
    // The one query allowed to read across accounts is safe because it has no
    // `Tenant` — there is nothing for a request to supply — and because no
    // handler names it. Both halves are asserted. The first is the signature;
    // the second is the wiring, which is the half a code change could break
    // without any type error.
    let sweeper = query_fns(STORE_SRC)
        .into_iter()
        .find(|f| f.name == "find_stale_pending")
        .expect("find_stale_pending is declared in store.rs");
    assert!(
        !sweeper.body.contains("tenant: &Tenant"),
        "the sweeper takes no Tenant and never will: a sweeper scoped to one \
         account is a sweeper that leaves every other account's abandoned \
         uploads in `pending` forever"
    );

    for (module, source) in [("service.rs", SERVICE_SRC), ("http.rs", HTTP_SRC)] {
        assert!(
            !source.contains("find_stale_pending"),
            "src/{module} names find_stale_pending. It reads every account's \
             rows; the day it becomes callable from a request is the day \
             'cross-tenant is 404, never 403' has an exception, and this goes \
             red first."
        );
    }
}

#[test]
fn the_idempotency_ledger_is_scoped_by_tenant_too() {
    // The ledger is tenant data, and unlike every other table here it holds
    // whole HTTP response *bodies*. `reserve_idempotency_key` hands the stored
    // body back on a replay, so an unscoped replay is not a status-code leak —
    // it is another account's response, byte for byte. The only thing between
    // account B and account A's recorded response is that `principal_scope`
    // puts `account_id` in the key.
    //
    // What is asserted here is the structural half, in the default tier, on a
    // bare machine. The behavioural half is
    // `tests/idempotency.rs::one_accounts_key_cannot_replay_another_accounts_response`,
    // which needs Postgres. This one fails on a refactor that trims the scope
    // string, without waiting for anybody to have a database.
    // The unit is up to the next item, not up to the first `}`: the function is
    // one `format!` whose *arguments* contain braces, so splitting on `}`
    // truncates before the very expression being checked.
    let unit = IDEMPOTENCY_SRC
        .split("pub fn principal_scope")
        .nth(1)
        .expect("idempotency.rs declares principal_scope")
        .split("pub fn ")
        .next()
        .expect("principal_scope has a body");
    assert!(
        unit.contains("principal.account_id"),
        "principal_scope must include account_id. The ledger is keyed by \
         (endpoint, principal, key) and its rows hold response bodies, so a \
         scope without the account is a cross-tenant read."
    );
    assert!(
        unit.contains("principal.user_id"),
        "principal_scope must include user_id too: a key shared by every token \
         of one account is one tenant's token able to replay another's upload \
         response"
    );
}

#[test]
fn every_scoped_query_has_a_case_in_the_database_suite() {
    // The closure between "the query is scoped" and "the scoping was exercised".
    // This file asserts the first for all ten; `tests/query_scoping.rs` asserts
    // the second against a real Postgres. Neither can tell that the other fell
    // behind, so this one does — by looking, for each scoped query derived from
    // `src/store.rs`, whether the database suite calls it.
    //
    // Derived on both sides, deliberately. A list of ten names written here
    // would be a list that goes stale silently, which is the exact failure this
    // file exists to prevent elsewhere.
    for f in tenant_scoped() {
        let call = format!("store::{}(", f.name);
        assert!(
            QUERY_SCOPING_SRC.contains(&call),
            "store::{} is tenant-scoped and no case in tests/query_scoping.rs \
             calls it. A scoped query nobody exercises is scoped on paper: drop \
             `and account_id = $2` from it and the whole suite stays green.",
            f.name
        );
    }
}

#[test]
fn every_scoped_query_name_the_database_suite_calls_still_exists() {
    // The other direction, and the reason it is not simply the first test
    // mirrored. A name in `tests/query_scoping.rs` that no longer names a
    // function in `src/store.rs` means one of three things: the query was
    // renamed, it was removed, or the assertion in the database suite was
    // quietly narrowed to something weaker while keeping the old call. `cargo`
    // catches the first two. Only this catches the third, and the third is the
    // one that leaves a green badge over a hole.
    let source: Vec<&str> = query_fns(STORE_SRC).into_iter().map(|f| f.name).collect();
    for name in [
        "find_asset",
        "find_asset_by_checksum",
        "find_storage_key",
        "find_variant_storage_key",
        "list_assets",
        "list_variants",
        "list_storage_keys",
        "mark_ready",
        "mark_failed",
        "delete_asset",
    ] {
        assert!(
            QUERY_SCOPING_SRC.contains(&format!("store::{name}(")),
            "tests/query_scoping.rs no longer calls store::{name}"
        );
        assert!(
            source.contains(&name),
            "tests/query_scoping.rs calls store::{name}, which src/store.rs no \
             longer declares. Either the query was renamed or it was removed, and \
             both need a decision rather than a stale test."
        );
    }
}

// ------------------------------------------------------- the route surface
//
// The other half of the enumeration. `store.rs` decides what a query can reach;
// `http::OPERATIONS` decides what a *request* can reach. Both are derived, and
// the negative-case list below is the one thing that has to be written by hand
// — so it is checked from both sides, and the names it claims are checked
// against the file that has to contain them.

const TENANT_ISOLATION_SRC: &str = include_str!("../tests/tenant_isolation.rs");

/// Every tenant-scoped operation `router()` serves, and the case in
/// `tests/tenant_isolation.rs` that refuses a cross-tenant caller.
///
/// Keyed by `(method, path)` in the exact spelling `OPERATIONS` uses, including
/// the `{id}` placeholder — so the left-hand side below is compared against the
/// router's own table rather than a paraphrase of it.
const NEGATIVE_CASES: &[(&str, &str)] = &[
    ("POST /v1/uploads", "a_create_body_cannot_name_an_account"),
    (
        "POST /v1/uploads/{id}/complete",
        "a_cross_tenant_complete_does_not_fail_another_accounts_upload",
    ),
    (
        "GET /v1/assets",
        "a_listing_returns_only_the_callers_own_assets",
    ),
    (
        "GET /v1/assets/{id}",
        "a_cross_tenant_404_is_indistinguishable_from_a_missing_one",
    ),
    (
        "DELETE /v1/assets/{id}",
        "a_cross_tenant_delete_is_404_and_the_asset_survives",
    ),
    (
        "POST /v1/assets/{id}/variants",
        "a_cross_tenant_variant_write_touches_nothing",
    ),
    (
        "GET /v1/assets/{id}/variants",
        "a_cross_tenant_variant_list_is_404_not_an_empty_list",
    ),
    (
        "GET /v1/assets (cursor)",
        "a_cursor_from_another_account_pages_only_the_callers_own_rows",
    ),
    (
        "POST /v1/uploads (checksum)",
        "the_same_bytes_in_two_accounts_are_two_assets_and_never_a_credential",
    ),
];

/// The tenant-scoped operations, derived from the table `router()` is built
/// from. `/v1` is the whole tenant surface: `/healthz` and `/readyz` are the
/// unauthenticated probes and hold nobody's data.
fn tenant_scoped_operations() -> Vec<String> {
    let mut out: Vec<String> = darkroom::http::OPERATIONS
        .iter()
        .filter(|op| op.path.starts_with("/v1/"))
        .map(|op| format!("{} {}", op.method.as_str(), op.path))
        .collect();
    out.sort();
    out
}

#[test]
fn every_tenant_scoped_route_has_a_negative_case() {
    // The closure that makes the count in the report checkable. A route added
    // to `OPERATIONS` without a case here fails, and a case here without a
    // route fails — so the enumeration cannot drift from the surface in either
    // direction, which is the whole point of deriving one side.
    let mut declared: Vec<String> = NEGATIVE_CASES
        .iter()
        .map(|(op, _)| op.to_string())
        .collect();
    declared.sort();
    declared.dedup();

    let mut routes = tenant_scoped_operations();
    routes.dedup();

    // The two extra rows cover inputs *into* a route rather than routes of
    // their own, so they are checked to name a real route and then accounted
    // for. Everything else must be the route list exactly.
    let (extras, operations): (Vec<&String>, Vec<&String>) = declared
        .iter()
        .partition(|op| op.contains('(') || op.contains(" ("));

    for op in &extras {
        let route = op.split(" (").next().expect("an extra names its route");
        assert!(
            routes.iter().any(|r| r == route),
            "{op} names route {route}, which is not in OPERATIONS"
        );
    }
    let operations: Vec<String> = operations.into_iter().cloned().collect();
    assert_eq!(
        operations, routes,
        "the negative-case list and http::OPERATIONS disagree. Every \
         tenant-scoped route needs a case that refuses another account, and \
         every case needs a route."
    );
}

#[test]
fn the_named_negative_cases_exist_where_the_table_says() {
    // A table of names is a claim about code that can be renamed. Resolved
    // against the file itself, so a rename of any of these fails here rather
    // than leaving the table quietly pointing at nothing.
    for (op, case) in NEGATIVE_CASES {
        let needle = format!("async fn {case}(");
        assert!(
            TENANT_ISOLATION_SRC.contains(&needle),
            "{op} claims tests/tenant_isolation.rs::{case}, which is not there"
        );
    }
}

#[test]
fn the_cross_tenant_matrix_covers_five_routes_and_the_table_covers_seven() {
    // The two numbers, asserted so neither drifts. Five operations take an
    // `{id}` and therefore answer 404 to a cross-tenant caller; seven are
    // tenant-scoped in total, the other two being a create and a list whose
    // negative case is about ownership and about membership respectively.
    //
    // The 404 count is a property of the router's path shapes, not of a list,
    // so it is derived: an operation is id-scoped exactly when its path
    // contains the placeholder.
    let id_scoped: Vec<String> = tenant_scoped_operations()
        .into_iter()
        .filter(|op| op.contains("{id}"))
        .collect();
    assert_eq!(
        id_scoped.len(),
        5,
        "the byte-identical-404 case in tenant_isolation.rs covers five routes: \
         {id_scoped:?}"
    );
    assert_eq!(
        tenant_scoped_operations().len(),
        7,
        "seven tenant-scoped operations: {:?}",
        tenant_scoped_operations()
    );
    assert_eq!(
        NEGATIVE_CASES.len(),
        9,
        "seven routes plus the cursor and the checksum, which are inputs to a \
         route rather than routes of their own"
    );
}
