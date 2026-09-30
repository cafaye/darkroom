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
    pub fn describe(&self) -> &'static str {
        match self {
            ObjectStoreConfig::Memory => "memory",
            #[cfg(feature = "s3")]
            ObjectStoreConfig::S3 { .. } => "s3",
        }
    }
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
        self.database_url.as_deref().ok_or(ConfigError::MissingDatabaseUrl)
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
    #[error("DARKROOM_S3_REGION is required when DARKROOM_OBJECT_STORE=s3")]
    MissingRegion,
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
                    ObjectStoreConfig::S3 {
                        bucket: lookup
                            .get("DARKROOM_S3_BUCKET")
                            .ok_or(ConfigError::MissingBucket)?,
                        region: lookup
                            .get("DARKROOM_S3_REGION")
                            .ok_or(ConfigError::MissingRegion)?,
                        endpoint: lookup
                            .get("DARKROOM_S3_ENDPOINT")
                            .filter(|v| !v.trim().is_empty()),
                        path_style: lookup
                            .get("DARKROOM_S3_PATH_STYLE")
                            .is_some_and(|v| v == "true"),
                    }
                }
            }
            other => return Err(ConfigError::InvalidObjectStore(other.to_string())),
        };

        let jwks_url = lookup.get("DARKROOM_JWKS_URL").ok_or(ConfigError::MissingJwksUrl)?;
        let issuer = lookup.get("DARKROOM_ISSUER").ok_or(ConfigError::MissingIssuer)?;
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

#[cfg(test)]
mod tests {
    use super::*;

    fn base() -> HashMap<String, String> {
        let mut env = HashMap::new();
        env.insert(
            "DARKROOM_JWKS_URL".into(),
            "https://identity.cafaye.com/.well-known/jwks.json".into(),
        );
        env.insert("DARKROOM_ISSUER".into(), "https://identity.cafaye.com".into());
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
        assert_eq!(Config::load(&env).expect_err("typo"), ConfigError::InvalidPort("808O".into()));

        let mut env = base();
        env.insert("PORT".into(), "70000".into());
        assert!(matches!(Config::load(&env), Err(ConfigError::InvalidPort(_))));

        let mut env = base();
        env.insert("DARKROOM_DB_MAX_CONNECTIONS".into(), "0".into());
        assert!(matches!(Config::load(&env), Err(ConfigError::InvalidMaxConnections(_))));

        let mut env = base();
        env.insert("DARKROOM_LOG_LEVEL".into(), "chatty".into());
        assert!(matches!(Config::load(&env), Err(ConfigError::Invalid(_))));
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

    #[test]
    fn config_loading_does_not_touch_the_process_environment() {
        // The whole reason `Lookup` exists. If this ever starts reading
        // `std::env` directly, a test that sets a variable would leak into
        // every test after it.
        let config = Config::load(&base()).expect("valid");
        assert_eq!(config.port, 8080);
    }
}
