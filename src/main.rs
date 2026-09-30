//! The process. Config, wires, serve, drain.
//!
//! `main` owns process lifetime and nothing else — the socket, the drain, the
//! signal handling. Everything it does is a call into the library, because a
//! signal path is not worth testing through a mock and none of the decisions
//! belong here.
//!
//! Two rules that came from `identity`'s AGENTS.md and apply to every cafaye
//! service:
//!
//! - **Liveness never touches a dependency.** `/healthz` is unconditional.
//! - **Migrations are a deploy step, not a boot step.** Nothing here applies a
//!   migration. A service that migrates itself on start is a service where two
//!   replicas racing to deploy can deadlock on `CREATE TABLE`.

use std::process::ExitCode;
use std::sync::Arc;

use darkroom::auth::{JwksVerifier, StaticTokenVerifier, TokenVerifier};
use darkroom::config::{Config, ObjectStoreConfig, ProcessEnv};
use darkroom::http::{self, AppState};
use darkroom::objectstore::{InMemoryObjectStore, SharedObjectStore};
use darkroom::service::Service;
use darkroom::store::Store;
use tokio::signal;

#[tokio::main]
async fn main() -> ExitCode {
    match run().await {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            // The logging subscriber is installed before config is read, so a
            // config failure is reported through the same structured pipeline as
            // everything else. A service that cannot log why it will not start
            // is a service someone debugs with `print`.
            darkroom::observability::init_tracing("info");
            tracing::error!(error = %e, "darkroom failed to start");
            ExitCode::FAILURE
        }
    }
}

async fn run() -> Result<(), Box<dyn std::error::Error>> {
    let config = Config::load(&ProcessEnv)?;
    darkroom::observability::init_tracing(&config.log_level);

    tracing::info!(
        version = darkroom::VERSION,
        env = %config.environment,
        object_store = config.object_store.describe(),
        "darkroom starting"
    );

    // The pool is built before the listener so a missing database fails startup
    // rather than producing a process that serves 503 to everything. Note the
    // service still does NOT ping here: a database that is down at boot shows
    // up as a failing readiness probe, which is a deployable state, rather than
    // a crash loop.
    let store = Store::connect(config.database_url()?, config.db_max_connections).await?;
    let objects: SharedObjectStore = build_object_store(&config).await?;
    let verifier = build_verifier(&config)?;

    let state = AppState {
        service: Service::new(Arc::new(store), objects),
        verifier: Arc::from(verifier),
    };

    let addr = format!("{}:{}", config.bind_addr, config.port);
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    tracing::info!(%addr, "listening");

    // Graceful shutdown. In-flight uploads are given the grace period to
    // finish; a request cut off mid-transaction is safe (the transaction rolls
    // back and the outbox row never existed) but a client that got no response
    // has to retry, so the drain is worth the wait.
    axum::serve(listener, http::router(state).into_make_service())
        .with_graceful_shutdown(shutdown_signal())
        .await?;

    tracing::info!("darkroom stopped");
    Ok(())
}

async fn build_object_store(
    config: &Config,
) -> Result<SharedObjectStore, Box<dyn std::error::Error>> {
    match &config.object_store {
        ObjectStoreConfig::Memory => {
            // Development and the whole test suite. Named in the startup log so
            // nobody discovers it by uploading into the void.
            tracing::warn!("object store is IN-MEMORY: uploads are lost on restart");
            Ok(Arc::new(InMemoryObjectStore::new()))
        }
        #[cfg(feature = "s3")]
        ObjectStoreConfig::S3 {
            bucket,
            region,
            endpoint,
            path_style,
        } => {
            let store = darkroom::objectstore::S3ObjectStore::connect(
                bucket,
                region,
                endpoint.clone(),
                *path_style,
            )
            .await?;
            Ok(Arc::new(store))
        }
    }
}

fn build_verifier(config: &Config) -> Result<Box<dyn TokenVerifier>, Box<dyn std::error::Error>> {
    // A dev-only HMAC verifier, compiled in behind a feature but gated at
    // runtime on the environment. The runtime gate is the one that matters: a
    // feature flag is a build-time switch, and the environment is the only
    // thing that knows where the binary ended up.
    #[cfg(feature = "dev-auth")]
    {
        if config.is_development() {
            // Read at runtime, not with `option_env!`: a secret baked into the
            // binary at build time is a secret in the image, in the build cache,
            // and in every layer of a `docker history`.
            if let Ok(secret) = std::env::var("DARKROOM_DEV_JWT_SECRET") {
                if !secret.is_empty() {
                    tracing::warn!(
                        "using the DEVELOPMENT HMAC token verifier; tokens are forgeable"
                    );
                    return Ok(Box::new(darkroom::auth::HmacVerifier::new(
                        secret,
                        &config.issuer,
                        &config.audience,
                    )));
                }
            }
            return Err(Box::new(darkroom::config::ConfigError::MissingDevSecret(
                "DARKROOM_DEV_JWT_SECRET",
            )));
        }
        if std::env::var("DARKROOM_DEV_JWT_SECRET").is_ok() {
            return Err(Box::new(darkroom::config::ConfigError::DevAuthInProduction));
        }
    }

    let _ = config;
    Ok(Box::new(JwksVerifier::new(
        config.jwks_url.clone(),
        config.issuer.clone(),
        config.audience.clone(),
    )))
}

async fn shutdown_signal() {
    let ctrl_c = async {
        let _ = signal::ctrl_c().await;
    };

    #[cfg(unix)]
    let terminate = async {
        match signal::unix::signal(signal::unix::SignalKind::terminate()) {
            Ok(mut s) => {
                s.recv().await;
            }
            Err(e) => {
                // A platform that cannot install SIGTERM gets Ctrl-C only. Logged
                // rather than panicked: a panic here would take down a container
                // that is otherwise healthy.
                tracing::warn!(error = %e, "could not install the SIGTERM handler");
                std::future::pending::<()>().await;
            }
        }
    };

    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();

    tokio::select! {
        () = ctrl_c => tracing::info!("received SIGINT, draining"),
        () = terminate => tracing::info!("received SIGTERM, draining"),
    }
}

/// Unused in `main`, but named here so `StaticTokenVerifier` stays a public part
/// of the library's surface for the integration tests rather than becoming dead
/// code that a future refactor deletes.
#[allow(dead_code)]
fn test_verifier() -> StaticTokenVerifier {
    StaticTokenVerifier::new()
}
