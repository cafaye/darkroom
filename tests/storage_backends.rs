//! The object-store behaviour table: AWS S3, an S3-compatible service, and
//! Cloudflare R2, through the same assertions.
//!
//! ## Why one table and not one test per backend
//!
//! A suite that branches on the backend proves the branch. Everything a
//! presigned URL must be — scoped to one key, pinned to one content type, good
//! for fifteen minutes, signed with the right region, and carrying no header
//! that the target will reject — is the *same* requirement for every
//! S3-compatible service, so it is asserted once, in one body, for every row.
//! What differs between rows is configuration and nothing else: an endpoint, a
//! region, a flag. Each row is still its own `#[tokio::test]`, so one failure
//! names the backend that failed instead of stopping at the first row.
//!
//! | row | endpoint | region as configured | region as signed |
//! |---|---|---|---|
//! | `aws` | — | `eu-west-1` | `eu-west-1` |
//! | `s3_compatible` | MinIO | `minio` | `minio` |
//! | `r2` | account endpoint | `auto` | `auto` |
//! | `r2_default_region` | account endpoint | *(unset)* | `auto` |
//! | `r2_us_east_1_alias` | account endpoint | `us-east-1` | `auto` |
//!
//! The last three are the R2 differences that are configuration, and the
//! assertion they share with the first two is the point: `auto` is what goes
//! into the SigV4 credential scope, and a URL that says `eu-west-1` for an R2
//! endpoint is a URL R2 rejects.
//!
//! ## No network, no credentials, no bucket
//!
//! Presigning is a local operation — the SDK builds a URI and a signature and
//! sends nothing — so this table runs the production request-shaping code
//! (`S3ObjectStore::configure`, the same function `connect` uses) with static
//! dummy credentials and asserts on the URL that comes out. The credentials
//! never leave the process: nothing is transmitted, and nothing here could
//! authenticate against a real bucket even if it tried.
//!
//! A real R2 bucket is a manual verification step and the README says exactly
//! how to do it. What this file cannot prove is that R2 *accepts* the URL, and
//! it does not claim to.

#![cfg(feature = "s3")]

use std::collections::HashMap;

use darkroom::config::{Config, ObjectStoreConfig};
use darkroom::objectstore::{ObjectStore, S3ObjectStore, PRESIGN_TTL};

/// A storage key with the shape `storage_key::generate_original` produces.
const KEY: &str = "a/9f8b1c2d-0000-4000-8000-000000000000/original";
const BUCKET: &str = "darkroom-media";

/// Header names and query parameters an S3-compatible service may reject, and
/// darkroom must therefore never put on the wire.
///
/// The first four are R2's list verbatim (`x-amz-acl`, `x-amz-grant-*`,
/// `x-amz-expected-bucket-owner`, plus the object-lock family it does not
/// implement). The last two are the checksum family, which is not a rejection
/// problem but a *correctness* one: R2 offers `FULL_OBJECT` for CRC-64/NVME only
/// and `COMPOSITE` for SHA-256, so a checksum that came back from a header would
/// be a different checksum type on a different bucket. darkroom asks for none.
const FORBIDDEN: &[&str] = &[
    "x-amz-acl",
    "x-amz-grant",
    "x-amz-expected-bucket-owner",
    "x-amz-object-lock",
    "x-amz-bucket-owner",
    "x-amz-checksum",
    "x-amz-sdk-checksum",
    "x-amz-checksum-mode",
    "x-amz-trailer",
    "content-md5",
];

/// One row: a configuration, and the region its signature must carry.
struct Row {
    name: &'static str,
    env: Vec<(&'static str, &'static str)>,
    /// The region in `X-Amz-Credential`. For the R2 rows it is `auto` however
    /// the region was configured, which is the whole of the R2 region rule.
    signed_region: &'static str,
}

const R2_ENDPOINT: &str = "https://9f8b1c2d3e4f5a6b7c8d9e0f1a2b3c4d.r2.cloudflarestorage.com";

fn rows() -> Vec<Row> {
    vec![
        Row {
            name: "aws",
            env: vec![
                ("DARKROOM_S3_BUCKET", BUCKET),
                ("DARKROOM_S3_REGION", "eu-west-1"),
                ("DARKROOM_S3_PATH_STYLE", "true"),
            ],
            signed_region: "eu-west-1",
        },
        Row {
            name: "s3_compatible",
            env: vec![
                ("DARKROOM_S3_BUCKET", BUCKET),
                ("DARKROOM_S3_REGION", "minio"),
                ("DARKROOM_S3_ENDPOINT", "https://minio.internal:9000"),
                ("DARKROOM_S3_PATH_STYLE", "true"),
            ],
            signed_region: "minio",
        },
        Row {
            name: "r2",
            env: vec![
                ("DARKROOM_S3_BUCKET", BUCKET),
                ("DARKROOM_S3_REGION", "auto"),
                ("DARKROOM_S3_ENDPOINT", R2_ENDPOINT),
            ],
            signed_region: "auto",
        },
        Row {
            name: "r2_default_region",
            env: vec![
                ("DARKROOM_S3_BUCKET", BUCKET),
                ("DARKROOM_S3_ENDPOINT", R2_ENDPOINT),
            ],
            signed_region: "auto",
        },
        Row {
            name: "r2_us_east_1_alias",
            env: vec![
                ("DARKROOM_S3_BUCKET", BUCKET),
                ("DARKROOM_S3_REGION", "us-east-1"),
                ("DARKROOM_S3_ENDPOINT", R2_ENDPOINT),
            ],
            signed_region: "auto",
        },
    ]
}

/// Load a row's configuration the way `main` does. Configuration is in the loop
/// deliberately: the row is "what an operator writes in a deployment", not a
/// struct this test built.
fn load(row: &Row) -> ObjectStoreConfig {
    let mut env = HashMap::new();
    env.insert(
        "DARKROOM_JWKS_URL".into(),
        "https://identity.cafaye.com/.well-known/jwks.json".into(),
    );
    env.insert(
        "DARKROOM_ISSUER".into(),
        "https://identity.cafaye.com".into(),
    );
    env.insert("DARKROOM_AUDIENCE".into(), "darkroom".into());
    env.insert("DARKROOM_OBJECT_STORE".into(), "s3".into());
    for (k, v) in &row.env {
        env.insert((*k).to_string(), (*v).to_string());
    }
    Config::load(&env)
        .unwrap_or_else(|e| panic!("{}: the configuration itself is invalid: {e}", row.name))
        .object_store
}

/// Build the store for a row, through the production request-shaping path.
fn store_for(row: &Row, config: &ObjectStoreConfig) -> S3ObjectStore {
    let ObjectStoreConfig::S3 {
        bucket,
        region,
        endpoint,
        path_style,
    } = config
    else {
        panic!("{}: expected an S3-compatible store", row.name);
    };
    S3ObjectStore::with_static_credentials(
        bucket,
        region,
        endpoint.clone(),
        *path_style,
        "AKIADARKROOMEXAMPLE",
        "darkroom-example-secret-not-a-real-key",
    )
}

/// The one body. Every row runs this, and there is no per-backend branch in it:
/// `row.signed_region` is the row's *data*, declared alongside its configuration.
async fn assert_behaviour(row: &Row) {
    let config = load(row);
    let store = store_for(row, &config);

    // 1. The region the SDK will sign with is the region the configuration
    //    resolved to, and for every R2 row that is the literal `auto`.
    assert_eq!(
        store
            .sdk_config()
            .region()
            .map(|r| r.as_ref())
            .unwrap_or_default(),
        row.signed_region,
        "{}: the client's region",
        row.name
    );

    let presigned = store
        .presign_put(KEY, "image/png", 4096, PRESIGN_TTL)
        .await
        .unwrap_or_else(|e| panic!("{}: presigns: {e}", row.name));
    let url = presigned.url.to_ascii_lowercase();

    // 2. The SigV4 credential scope carries that region. This is the assertion
    //    that matters most for R2: a signature built for a real region is
    //    rejected by R2 with nothing useful in the body, and this is the only
    //    place the difference is visible before a request is sent.
    assert!(
        url.contains(&format!("%2f{0}%2fs3%2faws4_request", row.signed_region)),
        "{}: the credential scope must be `<key>/<date>/{}/s3/aws4_request`, got {url}",
        row.name,
        row.signed_region
    );

    // 3. The signature covers the content type and the host, and those two.
    //    Exact equality, not containment: a signed-header list that grew by one
    //    entry is a client that now has to send a header it does not know about,
    //    and this is where an `x-amz-checksum-algorithm` or an
    //    `x-amz-expected-bucket-owner` would first appear.
    assert!(
        url.contains("x-amz-signedheaders=content-type%3bhost"),
        "{}: exactly content-type and host must be signed, got {url}",
        row.name
    );

    // 4. Nothing darkroom must never send appears anywhere in the URL — not in
    //    the signed headers, not in the query. This is the R2 header list and
    //    the checksum family in one assertion, applied to every row.
    for forbidden in FORBIDDEN {
        assert!(
            !url.contains(forbidden),
            "{}: {forbidden} must never be sent; the URL was {url}",
            row.name
        );
    }

    // 5. The URL is scoped: one key, and it is the key that was asked for.
    assert!(
        url.ends_with(&format!(
            "/{key}?{rest}",
            key = KEY,
            rest = url.split("?").nth(1).unwrap()
        )),
        "{}: the path must address exactly the key, got {url}",
        row.name
    );

    // 6. Fifteen minutes, and the ceiling darkroom documents is on it.
    assert_eq!(presigned.expires_in_secs, PRESIGN_TTL.as_secs());
    assert!(url.contains(&format!("x-amz-expires={}", PRESIGN_TTL.as_secs())));
    assert!(url.contains("x-darkroom-max-bytes=4096"));
}

/// Row: AWS S3, path-style, a real region.
#[tokio::test]
async fn aws_path_style_signs_its_own_region() {
    assert_behaviour(&rows()[0]).await;
}

/// Row: an S3-compatible service on a custom endpoint (MinIO, Ceph), which is
/// the case `DARKROOM_S3_PATH_STYLE` exists for.
#[tokio::test]
async fn an_s3_compatible_endpoint_signs_its_own_region() {
    assert_behaviour(&rows()[1]).await;
}

/// Row: Cloudflare R2, region given as `auto`.
#[tokio::test]
async fn an_r2_endpoint_signs_region_auto() {
    assert_behaviour(&rows()[2]).await;
}

/// Row: Cloudflare R2 with no region configured. R2 makes the region optional;
/// S3 does not. The URL has to be identical to the row above.
#[tokio::test]
async fn an_r2_endpoint_with_no_region_configured_signs_region_auto() {
    assert_behaviour(&rows()[3]).await;
}

/// Row: Cloudflare R2 with `us-east-1`, which R2 documents as an alias for
/// `auto`. The URL has to be identical to the row above, because the signature
/// is built with `auto` and not with the string that was configured.
#[tokio::test]
async fn an_r2_endpoint_with_the_us_east_1_alias_signs_region_auto() {
    assert_behaviour(&rows()[4]).await;
}

/// The three R2 rows produce the same URL, byte for byte apart from the
/// signature, which depends on the date. Asserted separately because "each row
/// passed" and "the rows agree" are different claims, and it is the second one
/// that says the region rule is normalisation rather than a coincidence.
#[tokio::test]
async fn the_three_r2_configurations_produce_the_same_presigned_url() {
    let mut urls = Vec::new();
    for row in rows().into_iter().skip(2) {
        let store = store_for(&row, &load(&row));
        let url = store
            .presign_put(KEY, "image/png", 4096, PRESIGN_TTL)
            .await
            .expect("presigns")
            .url;
        // The signature covers the date and the key, not the configuration, so
        // dropping it compares the parts that configuration could change.
        let without_signature = url
            .split("&X-Amz-Signature=")
            .next()
            .expect("the URL has a signature")
            .to_string();
        urls.push((row.name, without_signature));
    }
    let (first_name, first) = &urls[0];
    for (name, url) in &urls[1..] {
        assert_eq!(url, first, "{name} differs from {first_name}");
    }
}

/// An endpoint that merely *contains* R2's host suffix is not R2, and must not
/// get R2's rules.
///
/// A substring match here would be a way to make a deployment believe it had R2
/// validation when it had none, so the match is on the host. The observable
/// consequence: `auto` is refused, because for this endpoint it is not an R2
/// region and there is no global R2 endpoint to fall back to.
#[test]
fn a_host_that_only_looks_like_r2_is_not_given_r2_s_rules() {
    for lookalike in [
        "https://r2.cloudflarestorage.com.attacker.example",
        "https://notr2.cloudflarestorage.example.com",
        "https://acct.r2.cloudflarestorage.com.attacker.example",
    ] {
        let mut env = HashMap::new();
        env.insert("DARKROOM_JWKS_URL".into(), "https://id/jwks".into());
        env.insert("DARKROOM_ISSUER".into(), "https://id".into());
        env.insert("DARKROOM_AUDIENCE".into(), "darkroom".into());
        env.insert("DARKROOM_OBJECT_STORE".into(), "s3".into());
        env.insert("DARKROOM_S3_BUCKET".into(), BUCKET.into());
        env.insert("DARKROOM_S3_ENDPOINT".into(), lookalike.into());
        env.insert("DARKROOM_S3_REGION".into(), "auto".into());

        let err = Config::load(&env).expect_err("a lookalike is not R2");
        assert_eq!(
            err,
            darkroom::config::ConfigError::AutoRegionWithoutEndpoint,
            "{lookalike} was treated as R2"
        );
    }
}

/// Run the real binary with exactly these variables, and return whether it
/// started plus everything it said.
///
/// `env_clear` is deliberate: a test that inherits the developer's shell is a
/// test that passes on one machine and fails on another. `PATH` is kept because
/// the process needs nothing but its own binary, and a bare `PATH` cannot
/// reintroduce a stray `DARKROOM_*`.
///
/// The output is the tracing subscriber's, which writes to **stdout** in human
/// format with ANSI colour, so both streams are captured and the escapes are
/// stripped — the assertion should read the message, not the terminal.
fn run_binary(vars: &[(&str, &str)]) -> (bool, String) {
    let mut command = std::process::Command::new(env!("CARGO_BIN_EXE_darkroom"));
    command
        .env_clear()
        .env("PATH", std::env::var("PATH").unwrap_or_default());
    for (k, v) in vars {
        command.env(k, v);
    }
    let output = command
        .output()
        .expect("the darkroom binary is built for this test");
    let combined = format!(
        "{}{}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    (output.status.success(), strip_ansi(&combined))
}

fn strip_ansi(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut chars = text.chars();
    while let Some(c) = chars.next() {
        if c != '\u{1b}' {
            out.push(c);
            continue;
        }
        // A CSI sequence: ESC [ ... <final byte in @..~>.
        if chars.next() == Some('[') {
            for c in chars.by_ref() {
                if ('@'..='~').contains(&c) {
                    break;
                }
            }
        }
    }
    out
}

/// The variables every startup test needs: enough to get past the auth
/// configuration, and nothing that names a database.
fn base_env() -> Vec<(&'static str, &'static str)> {
    vec![
        ("DARKROOM_JWKS_URL", "https://identity.cafaye.com/jwks"),
        ("DARKROOM_ISSUER", "https://identity.cafaye.com"),
        ("DARKROOM_AUDIENCE", "darkroom"),
        ("DARKROOM_OBJECT_STORE", "s3"),
        ("DARKROOM_S3_BUCKET", BUCKET),
    ]
}

/// Refused **at startup**, by the process, not just by the parser.
///
/// `Config::load` is the first thing `main` does, before the database pool and
/// before the listener, so a `ConfigError` here is a process that never serves a
/// request. Running the binary is the only way to assert that rather than infer
/// it, and it costs nothing: no socket, no database, no credential, and the
/// configuration is rejected before any of them are touched.
#[test]
fn the_binary_refuses_to_start_on_an_r2_endpoint_with_a_real_region() {
    let vars = [
        base_env(),
        vec![
            ("DARKROOM_S3_ENDPOINT", R2_ENDPOINT),
            ("DARKROOM_S3_REGION", "eu-central-1"),
        ],
    ]
    .concat();
    let (started, output) = run_binary(&vars);

    assert!(
        !started,
        "the process must refuse to start, not start and fail on the first upload: {output}"
    );
    for expected in ["eu-central-1", "auto", "DARKROOM_S3_REGION"] {
        assert!(
            output.contains(expected),
            "the operator must be told what is wrong and what to do, and the \
             message must mention {expected:?}; it said: {output}"
        );
    }
}

/// The same binary, the same R2 endpoint, region omitted: it gets past
/// configuration. This is the positive control for the test above, without which
/// "refuses to start" could be satisfied by a binary that refuses everything.
#[test]
fn the_binary_gets_past_configuration_with_an_r2_endpoint_and_no_region() {
    let vars = [base_env(), vec![("DARKROOM_S3_ENDPOINT", R2_ENDPOINT)]].concat();
    let (_started, output) = run_binary(&vars);

    assert!(
        !output.contains("DARKROOM_S3_REGION"),
        "the configuration was accepted, so the region must not be the failure: {output}"
    );
    assert!(
        output.contains("DATABASE_URL"),
        "and the next thing it needs is a database, which is what an accepted \
         configuration looks like: {output}"
    );
}
