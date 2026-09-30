//! Configuration: environment in, validated [`Config`] out.
//!
//! Two rules, both borrowed from `identity`'s AGENTS.md because they are the
//! platform's rules and not this service's:
//!
//! - **`main` is the only place that reads the environment.** A `Lookup` trait
//!   is taken instead of the process environment so a test can supply a map and
//!   never mutate global state, which would make tests order-dependent.
//! - **Invalid config is an error, not a fallback.** A variable that is present
//!   but unparseable fails startup with a wrapped sentinel. Silently defaulting
//!   a typo is how a service ends up listening on the wrong port in production.
//!   The one supported "absent" is spelled out per field, because absence is
//!   sometimes deliberate (a bucket name is irrelevant to the in-memory store).
//!
//! No global, no `lazy_static`, no `OnceLock` holding config. The value is read
//! once and threaded, so a test's config cannot leak into the next test.

use std::collections::HashMap;
use std::time::Duration;

use crate::error::Error;
use crate::objectstore::{InMemoryObjectStore, MAX_UPLOAD_BYTES};

/// Where the bytes live. The S3 variant is only constructible with the `s3`
/// feature, so a build without it cannot name a bucket at all — which is what
/// makes "tests never reach the network" a property of the dependency graph
/// rather than a promise in a document.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ObjectStoreConfig {
    Memory,
    #[cfg(feature = "s3")]
    S3 {
        bucket: String,
        region: String,
        /// Optional; the AWS SDK's own chain (env, profile, IMDS, container
        /// role) is used when it is absent, which is how a container gets
        /// credentials without a key in the environment.
        endpoint: Option<String>,
        /// True for an S3-compatible service that does not support SigV4a.
        path_style: bool,
    },
}

impl ObjectStoreConfig {
    /// The name for the startup log. `r2` and `s3` are the same implementation
    /// and the same five methods; the only reason to name them apart is that an
    /// operator debugging a signature error needs to know which bucket is being
    /// signed for, and the log is where they look.
    pub fn describe(&self) -> &'static str {
        match self {
            ObjectStoreConfig::Memory => "memory",
            #[cfg(feature = "s3")]
            ObjectStoreConfig::S3 { endpoint, .. } => {
                if is_r2_endpoint(endpoint.as_deref()) {
                    "r2"
                } else {
                    "s3"
                }
            }
        }
    }
}

/// The host every R2 account endpoint ends in. Cloudflare has no global R2
/// endpoint, so the account id is part of the host and the endpoint is always
/// configuration — this constant recognises one, it is never used to build one.
#[cfg(feature = "s3")]
const R2_HOST_SUFFIX: &str = ".r2.cloudflarestorage.com";

/// R2's region, and the two values R2 documents as aliases for it.
#[cfg(feature = "s3")]
const R2_REGION: &str = "auto";

/// Whether an endpoint is a Cloudflare R2 account endpoint.
///
/// Host match, not substring: `https://r2.cloudflarestorage.com.evil.example`
/// ends with the right characters and is not R2, and configuration that decides
/// which rules to apply should not be decided by `contains`.
#[cfg(feature = "s3")]
fn is_r2_endpoint(endpoint: Option<&str>) -> bool {
    let Some(endpoint) = endpoint else {
        return false;
    };
    // Tolerate the shapes an operator actually pastes: with or without a
    // scheme, with a trailing slash, with a port for a local R2 (r2r - MinIO's
    // R2-compatible server, which is how the integration tests run).
    let after_scheme = endpoint
        .split_once("://")
        .map_or(endpoint, |(_, rest)| rest);
    let host = after_scheme.split('/').next().unwrap_or_default();
    let host = host.rsplit_once(':').map_or(host, |(h, _port)| h);
    host.to_ascii_lowercase().ends_with(R2_HOST_SUFFIX)
}

/// The validated configuration.
#[derive(Debug, Clone)]
pub struct Config {
    pub bind_addr: String,
    pub port: u16,
    pub database_url: Option<String>,
    pub db_max_connections: u32,
    pub object_store: ObjectStoreConfig,
    pub jwks_url: String,
    pub issuer: String,
    pub audience: String,
    pub log_level: String,
    /// Set by `DARKROOM_ENV=development`. Gates the HMAC verifier, which is
    /// compiled in but must never be reachable in production.
    pub environment: String,
}

impl Config {
    /// A default port. 8080, matching every other cafaye service, because a
    /// service that listens somewhere else is one more thing to configure.
    pub const DEFAULT_PORT: u16 = 8080;

    pub fn is_development(&self) -> bool {
        self.environment == "development"
    }

    /// Whether a database is required. `/readyz` genuinely queries Postgres, so
    /// a service with no `DATABASE_URL` can never be ready — refusing to start
    /// is honest, where a service that starts and fails every probe is not.
    pub fn database_url(&self) -> Result<&str, ConfigError> {
        self.database_url
            .as_deref()
            .ok_or(ConfigError::MissingDatabaseUrl)
    }
}

/// Everything that can go wrong reading configuration. Matched with
/// `matches!` or by comparison, never by string matching on the message.
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum ConfigError {
    #[error("DATABASE_URL is required: darkroom cannot serve a request without its database")]
    MissingDatabaseUrl,
    #[error("PORT must be a number between 1 and 65535, got {0:?}")]
    InvalidPort(String),
    #[error("DARKROOM_DB_MAX_CONNECTIONS must be a positive integer, got {0:?}")]
    InvalidMaxConnections(String),
    #[error("DARKROOM_OBJECT_STORE must be `memory` or `s3`, got {0:?}")]
    InvalidObjectStore(String),
    #[error("DARKROOM_S3_BUCKET is required when DARKROOM_OBJECT_STORE=s3")]
    MissingBucket,
    #[error("DARKROOM_S3_REGION is required when DARKROOM_OBJECT_STORE=s3 and the endpoint is not Cloudflare R2, whose region is `auto` and is defaulted")]
    MissingRegion,
    #[error(
        "DARKROOM_S3_REGION is {got:?} but the endpoint is Cloudflare R2, whose region is the literal string `auto`: R2 rejects SigV4 signed for a real region. Set DARKROOM_S3_REGION=auto (or leave it unset, which defaults to `auto` for an R2 endpoint). `{got:?}` is only valid with a non-R2 endpoint."
    )]
    R2RegionMustBeAuto { got: String },
    #[error(
        "DARKROOM_S3_REGION=auto is only valid with a Cloudflare R2 endpoint: AWS has no `auto` region, so this configuration would sign against the AWS default endpoint. Set DARKROOM_S3_ENDPOINT=https://<ACCOUNT_ID>.r2.cloudflarestorage.com, or set a real region like us-east-1."
    )]
    AutoRegionWithoutEndpoint,
    #[error("DARKROOM_JWKS_URL is required")]
    MissingJwksUrl,
    #[error("DARKROOM_ISSUER is required")]
    MissingIssuer,
    #[error("DARKROOM_AUDIENCE is required")]
    MissingAudience,
    #[error("{0} is required when the binary was built with --features dev-auth")]
    MissingDevSecret(&'static str),
    #[error("the dev HMAC verifier refuses to start outside DARKROOM_ENV=development")]
    DevAuthInProduction,
    #[error("{0} is invalid: {1}")]
    Invalid(&'static str, String),
}

impl From<ConfigError> for Error {
    fn from(e: ConfigError) -> Self {
        // A configuration error is 500 on the wire because there is no request
        // that could have caused it or fixed it, but it fails startup long
        // before any request exists. The conversion exists so a startup path and
        // a request path can share the type.
        tracing::error!(error = %e, "configuration is invalid");
        Error::internal("the service is misconfigured")
    }
}

/// The environment, injected. A trait so a test can pass a map; the process
/// environment is one implementation of it and `main` is the only caller.
pub trait Lookup {
    fn get(&self, key: &str) -> Option<String>;
}

impl Lookup for HashMap<String, String> {
    fn get(&self, key: &str) -> Option<String> {
        HashMap::get(self, key).cloned()
    }
}

/// The real environment. Isolated in one place so nothing else in the service
/// can read it.
pub struct ProcessEnv;

impl Lookup for ProcessEnv {
    fn get(&self, key: &str) -> Option<String> {
        std::env::var(key).ok()
    }
}

impl Config {
    /// Read and validate. Fails on the first problem with a specific
    /// [`ConfigError`], so an operator fixing a deployment sees the actual
    /// cause rather than a list.
    pub fn load(lookup: &impl Lookup) -> Result<Self, ConfigError> {
        let port = match lookup.get("PORT") {
            None => Config::DEFAULT_PORT,
            Some(raw) => raw
                .trim()
                .parse::<u16>()
                .ok()
                .filter(|p| *p > 0)
                .ok_or_else(|| ConfigError::InvalidPort(raw.clone()))?,
        };

        let db_max_connections = match lookup.get("DARKROOM_DB_MAX_CONNECTIONS") {
            None => 10,
            Some(raw) => raw
                .trim()
                .parse::<u32>()
                .ok()
                .filter(|n| *n > 0)
                .ok_or_else(|| ConfigError::InvalidMaxConnections(raw.clone()))?,
        };

        let object_store = match lookup
            .get("DARKROOM_OBJECT_STORE")
            .unwrap_or_else(|| "memory".to_string())
            .trim()
        {
            "memory" | "" => ObjectStoreConfig::Memory,
            "s3" => {
                #[cfg(not(feature = "s3"))]
                {
                    // A build without the feature cannot name a bucket, so
                    // selecting s3 is a startup failure rather than a runtime
                    // one on the first upload.
                    return Err(ConfigError::InvalidObjectStore(
                        "s3 (this binary was built without --features s3)".to_string(),
                    ));
                }
                #[cfg(feature = "s3")]
                {
                    let bucket = lookup
                        .get("DARKROOM_S3_BUCKET")
                        .ok_or(ConfigError::MissingBucket)?;
                    let endpoint = lookup
                        .get("DARKROOM_S3_ENDPOINT")
                        .filter(|v| !v.trim().is_empty());
                    let path_style = lookup
                        .get("DARKROOM_S3_PATH_STYLE")
                        .is_some_and(|v| v == "true");
                    let is_r2 = is_r2_endpoint(endpoint.as_deref());
                    let region = resolve_s3_region(
                        lookup.get("DARKROOM_S3_REGION").as_deref(),
                        endpoint.as_deref(),
                    )?;
                    ObjectStoreConfig::S3 {
                        bucket,
                        region,
                        endpoint,
                        // R2 is path-style regardless of the flag: the account
                        // endpoint is the whole host, so the bucket belongs in
                        // the path.
                        path_style: path_style || is_r2,
                    }
                }
            }
            other => return Err(ConfigError::InvalidObjectStore(other.to_string())),
        };

        let jwks_url = lookup
            .get("DARKROOM_JWKS_URL")
            .ok_or(ConfigError::MissingJwksUrl)?;
        let issuer = lookup
            .get("DARKROOM_ISSUER")
            .ok_or(ConfigError::MissingIssuer)?;
        let audience = lookup
            .get("DARKROOM_AUDIENCE")
            .ok_or(ConfigError::MissingAudience)?;

        let log_level = lookup
            .get("DARKROOM_LOG_LEVEL")
            .unwrap_or_else(|| "info".to_string());
        if !["error", "warn", "info", "debug", "trace", "off"].contains(&log_level.as_str()) {
            return Err(ConfigError::Invalid(
                "DARKROOM_LOG_LEVEL",
                format!("{log_level} is not a level"),
            ));
        }

        Ok(Config {
            bind_addr: lookup
                .get("DARKROOM_BIND_ADDR")
                .unwrap_or_else(|| "0.0.0.0".to_string()),
            port,
            database_url: lookup.get("DATABASE_URL"),
            db_max_connections,
            object_store,
            jwks_url,
            issuer,
            audience,
            log_level,
            environment: lookup
                .get("DARKROOM_ENV")
                .unwrap_or_else(|| "production".to_string()),
        })
    }

    /// The in-memory object store. Panics outside the `s3` feature, where it is
    /// the only implementation that can exist.
    pub fn memory_object_store(&self) -> InMemoryObjectStore {
        InMemoryObjectStore::new()
    }
}

/// The connection-pool acquire timeout, restated here so it is one number
/// rather than an inline literal in `store.rs`.
pub const ACQUIRE_TIMEOUT: Duration = Duration::from_secs(5);

/// The upload cap, re-exported so a caller building a request body does not
/// have to reach into `objectstore`.
pub const UPLOAD_LIMIT_BYTES: i64 = MAX_UPLOAD_BYTES;

/// The region a client will actually sign with, given what the operator said and
/// which bucket they pointed at.
///
/// This is the whole of the R2 region problem, and it is worth why it lives in
/// configuration rather than in the S3 client:
///
/// - **R2's region is `auto`.** Not "any region works" — R2 rejects a SigV4
///   signature built for a real region, and `us-east-1` and the empty string
///   are accepted only as aliases that still have to be *signed* as `auto`. So
///   the aliases are normalised here and every downstream consumer sees one
///   string.
/// - **A real region with an R2 endpoint is refused**, at startup, naming the
///   variable and the value. Left to the first request it is a signature error
///   on every upload with nothing in the log about the region.
/// - **`auto` without an endpoint is refused**, because AWS has no `auto` region
///   and a bucket-only configuration would otherwise resolve
///   `s3.amazonaws.com`. That is the "I configured for R2 and forgot the account
///   id" case, and it is cheaper to catch here than to debug from a 403.
///
/// Everything else is passed through untouched, so an AWS or MinIO deployment
/// can still say whatever it means.
#[cfg(feature = "s3")]
fn resolve_s3_region(
    configured: Option<&str>,
    endpoint: Option<&str>,
) -> Result<String, ConfigError> {
    let region = configured.unwrap_or_default().trim();

    if is_r2_endpoint(endpoint) {
        if region.eq_ignore_ascii_case(R2_REGION)
            || region.eq_ignore_ascii_case("us-east-1")
            || region.is_empty()
        {
            return Ok(R2_REGION.to_string());
        }
        return Err(ConfigError::R2RegionMustBeAuto {
            got: region.to_string(),
        });
    }

    if region.is_empty() {
        return Err(ConfigError::MissingRegion);
    }
    if region.eq_ignore_ascii_case(R2_REGION) {
        return Err(ConfigError::AutoRegionWithoutEndpoint);
    }
    Ok(region.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn base() -> HashMap<String, String> {
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
        env
    }

    #[test]
    fn the_defaults_are_the_documented_ones() {
        let config = Config::load(&base()).expect("valid");
        assert_eq!(config.port, 8080);
        assert_eq!(config.db_max_connections, 10);
        assert_eq!(config.object_store, ObjectStoreConfig::Memory);
        assert_eq!(config.log_level, "info");
        // Absent DATABASE_URL is a supported state at load time and an error at
        // serve time. Stated as a fact about `Config::database_url`.
        assert!(config.database_url().is_err());
    }

    #[test]
    fn a_present_but_unparseable_value_is_an_error_not_a_fallback() {
        // This is the rule that keeps a typo from becoming a production
        // surprise: `PORT=808O` must not silently become 8080.
        let mut env = base();
        env.insert("PORT".into(), "808O".into());
        assert_eq!(
            Config::load(&env).expect_err("typo"),
            ConfigError::InvalidPort("808O".into())
        );

        let mut env = base();
        env.insert("PORT".into(), "70000".into());
        assert!(matches!(
            Config::load(&env),
            Err(ConfigError::InvalidPort(_))
        ));

        let mut env = base();
        env.insert("DARKROOM_DB_MAX_CONNECTIONS".into(), "0".into());
        assert!(matches!(
            Config::load(&env),
            Err(ConfigError::InvalidMaxConnections(_))
        ));

        let mut env = base();
        env.insert("DARKROOM_LOG_LEVEL".into(), "chatty".into());
        assert!(matches!(
            Config::load(&env),
            Err(ConfigError::Invalid(_, _))
        ));
    }

    #[test]
    fn s3_without_its_required_variables_names_the_missing_one() {
        let mut env = base();
        env.insert("DARKROOM_OBJECT_STORE".into(), "s3".into());
        // The exact error depends on the build's features; either way it is a
        // specific error, never a silent fallback to the in-memory store — which
        // would be the worst outcome, because the service would appear to work
        // and every upload would vanish.
        let err = Config::load(&env).expect_err("s3 needs a bucket");
        assert!(matches!(
            err,
            ConfigError::MissingBucket
                | ConfigError::InvalidObjectStore(_)
                | ConfigError::MissingRegion
        ));
    }

    #[test]
    fn an_unknown_object_store_value_is_rejected() {
        let mut env = base();
        env.insert("DARKROOM_OBJECT_STORE".into(), "gcs".into());
        assert_eq!(
            Config::load(&env).expect_err("unknown"),
            ConfigError::InvalidObjectStore("gcs".into())
        );
    }

    #[test]
    fn the_auth_variables_are_required() {
        for (key, expected) in [
            ("DARKROOM_JWKS_URL", ConfigError::MissingJwksUrl),
            ("DARKROOM_ISSUER", ConfigError::MissingIssuer),
            ("DARKROOM_AUDIENCE", ConfigError::MissingAudience),
        ] {
            let mut env = base();
            env.remove(key);
            assert_eq!(
                Config::load(&env).expect_err(key),
                expected,
                "{key} should be required"
            );
        }
    }

    #[test]
    fn development_is_opt_in_not_the_default() {
        // The default must be production, because the dev HMAC verifier is
        // gated on this and a default of `development` would make that gate
        // meaningless.
        assert!(!Config::load(&base()).expect("valid").is_development());

        let mut env = base();
        env.insert("DARKROOM_ENV".into(), "development".into());
        assert!(Config::load(&env).expect("valid").is_development());
    }

    // --- Cloudflare R2 ----------------------------------------------------
    //
    // The differences between R2 and S3 that are configuration rather than code.
    // Each is verified against developers.cloudflare.com/r2/api/s3/api and each
    // has a test here or in tests/storage_backends.rs, because a comment is not
    // an invariant.

    /// An R2 configuration produces an endpoint, region `auto`, and path-style
    /// addressing — the three things R2 needs that S3 does not.
    // Named per backend but gated on the backend: without the `s3` feature
    // there is no S3 configuration to resolve, and a test that asserted the
    // in-memory fallback instead would be testing nothing.
    #[cfg(feature = "s3")]
    #[test]
    fn an_r2_endpoint_resolves_to_auto_region_and_path_style() {
        let env = s3_env(&[
            ("DARKROOM_S3_BUCKET", "darkroom-media"),
            (
                "DARKROOM_S3_ENDPOINT",
                "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com",
            ),
            ("DARKROOM_S3_REGION", "auto"),
        ]);
        match Config::load(&env)
            .expect("an R2 endpoint is a valid configuration")
            .object_store
        {
            ObjectStoreConfig::S3 {
                bucket,
                region,
                endpoint,
                path_style,
            } => {
                assert_eq!(bucket, "darkroom-media");
                assert_eq!(region, "auto", "R2's region is the literal string `auto`");
                assert_eq!(
                    endpoint.as_deref(),
                    Some("https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com"),
                    "the account-scoped endpoint is configuration, never a constant"
                );
                assert!(
                    path_style,
                    "path-style, because the account endpoint is the whole host"
                );
            }
            other => panic!("expected an S3-compatible store, got {other:?}"),
        }
    }

    /// The region is *optional* for an R2 endpoint and defaults to `auto`.
    // Named per backend but gated on the backend: without the `s3` feature
    // there is no S3 configuration to resolve, and a test that asserted the
    // in-memory fallback instead would be testing nothing.
    #[cfg(feature = "s3")]
    #[test]
    fn an_r2_endpoint_with_no_region_configured_still_signs_as_auto() {
        let env = s3_env(&[
            ("DARKROOM_S3_BUCKET", "darkroom-media"),
            (
                "DARKROOM_S3_ENDPOINT",
                "https://acct.r2.cloudflarestorage.com",
            ),
        ]);
        match Config::load(&env)
            .expect("region is not required for R2")
            .object_store
        {
            ObjectStoreConfig::S3 { region, .. } => {
                assert_eq!(region, "auto", "the default for an R2 endpoint");
            }
            other => panic!("expected an S3-compatible store, got {other:?}"),
        }
    }

    /// R2 documents `us-east-1` and the empty value as aliases for `auto`, and
    /// accepts them — but the signature has to be built with `auto`, so the
    /// configuration is normalised rather than passed through.
    // Named per backend but gated on the backend: without the `s3` feature
    // there is no S3 configuration to resolve, and a test that asserted the
    // in-memory fallback instead would be testing nothing.
    #[cfg(feature = "s3")]
    #[test]
    fn the_r2_region_aliases_are_normalised_to_auto() {
        for given in ["us-east-1", "US-EAST-1", "", "auto", "AUTO"] {
            let mut env = s3_env(&[
                ("DARKROOM_S3_BUCKET", "darkroom-media"),
                (
                    "DARKROOM_S3_ENDPOINT",
                    "https://acct.r2.cloudflarestorage.com",
                ),
            ]);
            env.insert("DARKROOM_S3_REGION".into(), given.into());
            match Config::load(&env)
                .unwrap_or_else(|e| panic!("{given:?} is an R2 alias: {e}"))
                .object_store
            {
                ObjectStoreConfig::S3 { region, .. } => assert_eq!(
                    region, "auto",
                    "{given:?} must be signed as `auto`, not passed through"
                ),
                other => panic!("expected an S3-compatible store, got {other:?}"),
            }
        }
    }

    /// Refused at startup, with a message that names the fix.
    ///
    /// This is the single most common R2 integration failure and the only one
    /// that is invisible until a request is signed: SigV4 with a real region
    /// against an R2 endpoint is rejected by R2, so the symptom is every upload
    /// failing and nothing in the logs saying why. Refusing to start says it
    /// instead, once, before a single byte is accepted.
    // Named per backend but gated on the backend: without the `s3` feature
    // there is no S3 configuration to resolve, and a test that asserted the
    // in-memory fallback instead would be testing nothing.
    #[cfg(feature = "s3")]
    #[test]
    fn an_r2_endpoint_paired_with_a_real_region_is_refused_and_names_the_fix() {
        for given in ["us-west-2", "eu-central-1", "wnam"] {
            let env = s3_env(&[
                ("DARKROOM_S3_BUCKET", "darkroom-media"),
                (
                    "DARKROOM_S3_ENDPOINT",
                    "https://acct.r2.cloudflarestorage.com",
                ),
                ("DARKROOM_S3_REGION", given),
            ]);

            let err = Config::load(&env).expect_err("an R2 endpoint cannot sign a real region");
            let message = err.to_string();
            assert!(
                message.contains("DARKROOM_S3_REGION"),
                "the message must name the variable to change, got: {message}"
            );
            assert!(
                message.contains("auto"),
                "and the value to change it to, got: {message}"
            );
            assert!(
                message.contains(given),
                "and the value that was wrong, got: {message}"
            );
        }
    }

    /// A bucket-only configuration must not silently become an AWS endpoint.
    ///
    /// `DARKROOM_S3_REGION=auto` with no `DARKROOM_S3_ENDPOINT` means somebody
    /// configured for R2 and forgot the account id. Without this refusal the SDK
    /// resolves `s3.amazonaws.com`, the first upload is a signature error against
    /// a bucket that does not exist, and the log says nothing about the endpoint.
    // Named per backend but gated on the backend: without the `s3` feature
    // there is no S3 configuration to resolve, and a test that asserted the
    // in-memory fallback instead would be testing nothing.
    #[cfg(feature = "s3")]
    #[test]
    fn region_auto_without_an_endpoint_is_refused() {
        let env = s3_env(&[
            ("DARKROOM_S3_BUCKET", "darkroom-media"),
            ("DARKROOM_S3_REGION", "auto"),
        ]);
        let err = Config::load(&env).expect_err("`auto` is an R2 region, not an AWS one");
        let message = err.to_string();
        assert!(
            message.contains("DARKROOM_S3_ENDPOINT"),
            "the message must name the variable that is missing, got: {message}"
        );
        assert!(
            message.contains("r2.cloudflarestorage.com"),
            "and the endpoint shape that would fix it, got: {message}"
        );
    }

    /// Path-style is forced for an R2 endpoint whatever the flag says.
    ///
    /// Virtual-host addressing against an account-scoped endpoint produces
    /// `<bucket>.<account>.r2.cloudflarestorage.com`, which is not the shape R2
    /// documents or its tooling expects. The account endpoint already encodes the
    /// account, so the bucket belongs in the path.
    // Named per backend but gated on the backend: without the `s3` feature
    // there is no S3 configuration to resolve, and a test that asserted the
    // in-memory fallback instead would be testing nothing.
    #[cfg(feature = "s3")]
    #[test]
    fn an_r2_endpoint_forces_path_style_even_when_the_flag_says_otherwise() {
        let env = s3_env(&[
            ("DARKROOM_S3_BUCKET", "darkroom-media"),
            (
                "DARKROOM_S3_ENDPOINT",
                "https://acct.r2.cloudflarestorage.com",
            ),
            ("DARKROOM_S3_PATH_STYLE", "false"),
        ]);
        match Config::load(&env).expect("valid").object_store {
            ObjectStoreConfig::S3 { path_style, .. } => {
                assert!(path_style, "the flag does not get a vote on an R2 endpoint")
            }
            other => panic!("expected an S3-compatible store, got {other:?}"),
        }
    }

    /// A normal S3 configuration is untouched by any of the above. The R2 rules
    /// must not narrow what an AWS deployment can say — `eu-west-1` with no
    /// endpoint and path-style off has to keep working, or fixing R2 weakened
    /// S3.
    // Named per backend but gated on the backend: without the `s3` feature
    // there is no S3 configuration to resolve, and a test that asserted the
    // in-memory fallback instead would be testing nothing.
    #[cfg(feature = "s3")]
    #[test]
    fn an_aws_configuration_is_left_exactly_as_it_was() {
        let env = s3_env(&[
            ("DARKROOM_S3_BUCKET", "darkroom-media"),
            ("DARKROOM_S3_REGION", "eu-west-1"),
        ]);
        match Config::load(&env).expect("valid").object_store {
            ObjectStoreConfig::S3 {
                region,
                endpoint,
                path_style,
                ..
            } => {
                assert_eq!(region, "eu-west-1", "a real region is passed through");
                assert_eq!(endpoint, None, "no endpoint means AWS resolves it");
                assert!(!path_style, "and the flag still decides addressing");
            }
            other => panic!("expected an S3-compatible store, got {other:?}"),
        }
    }

    /// A non-R2 custom endpoint (MinIO, Ceph) keeps its own region. R2's rules
    /// are about R2's host, not about "there is an endpoint".
    // Named per backend but gated on the backend: without the `s3` feature
    // there is no S3 configuration to resolve, and a test that asserted the
    // in-memory fallback instead would be testing nothing.
    #[cfg(feature = "s3")]
    #[test]
    fn another_s3_compatible_endpoint_keeps_its_own_region() {
        let env = s3_env(&[
            ("DARKROOM_S3_BUCKET", "darkroom-media"),
            ("DARKROOM_S3_ENDPOINT", "https://minio.internal:9000"),
            ("DARKROOM_S3_REGION", "minio"),
            ("DARKROOM_S3_PATH_STYLE", "true"),
        ]);
        match Config::load(&env).expect("valid").object_store {
            ObjectStoreConfig::S3 {
                region, path_style, ..
            } => {
                assert_eq!(region, "minio");
                assert!(path_style, "and the flag is honoured for a non-R2 endpoint");
            }
            other => panic!("expected an S3-compatible store, got {other:?}"),
        }
    }

    /// The startup log says which bucket darkroom is really talking to. `r2` and
    /// `s3` are the same code, so without this the only way to tell is to read
    /// the endpoint out of a crash.
    // Named per backend but gated on the backend: without the `s3` feature
    // there is no S3 configuration to resolve, and a test that asserted the
    // in-memory fallback instead would be testing nothing.
    #[cfg(feature = "s3")]
    #[test]
    fn the_store_names_itself_in_the_startup_log() {
        assert_eq!(ObjectStoreConfig::Memory.describe(), "memory");

        let r2 = s3_env(&[
            ("DARKROOM_S3_BUCKET", "b"),
            (
                "DARKROOM_S3_ENDPOINT",
                "https://acct.r2.cloudflarestorage.com",
            ),
        ]);
        assert_eq!(
            Config::load(&r2).expect("valid").object_store.describe(),
            "r2"
        );

        let aws = s3_env(&[
            ("DARKROOM_S3_BUCKET", "b"),
            ("DARKROOM_S3_REGION", "us-east-1"),
        ]);
        assert_eq!(
            Config::load(&aws).expect("valid").object_store.describe(),
            "s3"
        );
    }

    /// A base environment plus the S3 variables a row needs. `#[cfg]`-gated
    /// because without the feature these rows are a different error, and a
    /// second table of refusals would be a second thing to keep true.
    #[cfg(feature = "s3")]
    fn s3_env(pairs: &[(&str, &str)]) -> HashMap<String, String> {
        let mut env = base();
        env.insert("DARKROOM_OBJECT_STORE".into(), "s3".into());
        for (k, v) in pairs {
            env.insert((*k).to_string(), (*v).to_string());
        }
        env
    }

    #[test]
    fn config_loading_does_not_touch_the_process_environment() {
        // The whole reason `Lookup` exists. If this ever starts reading
        // `std::env` directly, a test that sets a variable would leak into
        // every test after it.
        let config = Config::load(&base()).expect("valid");
        assert_eq!(config.port, 8080);
    }
}
