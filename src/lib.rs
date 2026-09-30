//! `darkroom` — the cafaye media service.
//!
//! Signed uploads, a tenant-scoped asset registry, and derived variants. The
//! foundation anychat's voice clips and anymark's attachments both sit on.
//!
//! ## The layer order, and why it is this order
//!
//! ```text
//!   http.rs        routes, handlers, probes, auth middleware   (the wire)
//!   service.rs     the state machine and every business rule   (the decisions)
//!   domain.rs      assets, variants, enums                     (the vocabulary)
//!   auth.rs        token verification, Principal, Tenant       (who is calling)
//!   objectstore/   the storage trait, in-memory + s3          (where bytes live)
//!   store.rs       sqlx, the outbox, the idempotency ledger    (persistence)
//!   outbox.rs      the event envelope                         (the contract)
//! ```
//!
//! Dependencies point downward only. `service.rs` knows nothing about axum;
//! `domain.rs` knows nothing about SQL; `objectstore` knows nothing about
//! assets. That is what lets the signed-upload state machine be tested without
//! a socket and the storage scope be tested without a database.
//!
//! ## The two invariants worth stating once
//!
//! **Tenant isolation.** Every query filters on `account_id`, and `account_id`
//! comes only from the verified token — [`auth::Tenant`] has no constructor that
//! takes one from a request. A cross-tenant read is an ordinary 404, never a
//! 403, so existence does not leak across a tenant boundary.
//!
//! **The checksum is verified, not trusted.** The client's claim is recorded at
//! `POST /v1/uploads` and checked against the bytes storage actually holds at
//! `POST /v1/uploads/{id}/complete`. The row ends up carrying the checksum
//! computed from storage, never the one the client sent.

pub mod auth;
pub mod checksum;
pub mod config;
pub mod domain;
pub mod error;
pub mod http;
pub mod idempotency;
pub mod objectstore;
pub mod observability;
pub mod outbox;
pub mod service;
pub mod storage_key;
pub mod store;
pub mod variants;

pub use domain::{Asset, AssetKind, AssetStatus, AssetVariant, VariantKind};
pub use error::{Error, FieldError, Problem};
pub use objectstore::{ObjectStore, SharedObjectStore};
pub use service::Service;
pub use store::Store;

/// The service's own version, from Cargo.toml. Used in the OpenAPI `info`
/// sibling fields and in the `/healthz` body, so an operator can tell which
/// build is answering without a log lookup.
pub const VERSION: &str = env!("CARGO_PKG_VERSION");
