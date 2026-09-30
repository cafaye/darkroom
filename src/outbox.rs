//! The outbox: the event envelope and the table it goes into.
//!
//! Copied from `moon/cafaye/core/docs/event-outbox.md` and
//! `event-naming.md`, which are the contract. The rule this module exists to
//! make hard to violate:
//!
//! > A service never publishes an event outside a transaction that also wrote
//! > the domain state it describes.
//!
//! So there is exactly one way to put an event in this service: [`Outbox::enqueue`],
//! which takes a `&mut Transaction`. A function that holds a transaction cannot
//! accidentally publish outside it, and a function that does not hold one has
//! no way to publish at all. The rollback test in `tests/` asserts the negative —
//! that a transaction which returns `Err` leaves zero rows in `outbox_events`.
//!
//! The publisher loop that moves rows to NATS is **not** in this packet. It is
//! called out in the README's "Not done" with the reason: it needs a broker
//! client and a NATS deployment, neither of which exists yet, and a publisher
//! loop that cannot publish is better absent than silently no-op. The table and
//! the insert path — the half that carries the correctness guarantee — are here
//! and tested.

use serde_json::Value;
use sqlx::{PgExecutor, Postgres, Transaction};
use time::OffsetDateTime;
use uuid::Uuid;

/// The service name, which is also the envelope `source` and the first segment
/// of every event type. Asserted against `cafaye.yml` in
/// `tests/manifest_matches_specs.rs`.
pub const SOURCE: &str = "darkroom";

/// A published event type. Three segments, past tense, from core's action
/// vocabulary.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum EventType {
    /// The upload completed and the bytes are in storage. This is the moment
    /// the asset becomes usable — NOT the moment the presigned URL was issued.
    /// A consumer that builds a thumbnail on `ready` and a consumer that
    /// notifies a user both need the bytes to exist; neither can work at
    /// `created`.
    AssetReady,
    /// The asset and its storage object are gone.
    AssetDeleted,
    /// A derived image exists and can be served.
    VariantCreated,
}

impl EventType {
    pub fn as_str(self) -> &'static str {
        match self {
            EventType::AssetReady => "darkroom.asset.ready",
            EventType::AssetDeleted => "darkroom.asset.deleted",
            EventType::VariantCreated => "darkroom.variant.created",
        }
    }
}

impl std::fmt::Display for EventType {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

/// An event about to be written into `outbox_events`.
///
/// The envelope's `id` is generated here, before the insert, and is the same
/// value on every retry — core: "The row and the message carry one identity, so
/// a retry publishes the same `id` and the consumer's dedupe key actually
/// dedupes."
#[derive(Debug, Clone)]
pub struct NewEvent {
    pub id: Uuid,
    pub event_type: EventType,
    /// The entity the event is about. Required by the envelope, never
    /// optional; an event with no single entity uses the literal `platform`.
    pub subject: String,
    /// When the state change happened. Not "now" by the time the row is
    /// inserted, and not "now" by the time it is published — a row that sat
    /// unpublished for an hour still reports when the change happened.
    pub time: OffsetDateTime,
    pub data: Value,
}

impl NewEvent {
    /// Build an event about `subject`, with `time` set to the moment of the
    /// state change.
    pub fn new(event_type: EventType, subject: impl Into<String>, data: Value) -> Self {
        Self {
            id: Uuid::new_v4(),
            event_type,
            subject: subject.into(),
            time: crate::observability::now(),
            data,
        }
    }

    /// The CloudEvents envelope, as the publisher would put it on the bus. The
    /// `data` here is byte-identical to what was written to the `data` column,
    /// because the payload schema validated that column.
    pub fn envelope(&self) -> Value {
        serde_json::json!({
            "specversion": "1.0",
            "id": self.id.to_string(),
            "type": self.event_type.as_str(),
            "source": SOURCE,
            "subject": self.subject,
            "time": crate::observability::rfc3339(self.time),
            "data": self.data,
        })
    }
}

/// The outbox writer. Constructed per transaction, so it cannot outlive one.
pub struct Outbox<'tx, 'conn> {
    executor: &'tx mut Transaction<'conn, Postgres>,
}

impl<'tx, 'conn> Outbox<'tx, 'conn> {
    /// Bind the outbox to a transaction. Taking `&mut Transaction` — not
    /// `&PgPool` — is the whole design: the only way to enqueue is inside a
    /// transaction that has not committed yet.
    pub fn new(executor: &'tx mut Transaction<'conn, Postgres>) -> Self {
        Self { executor }
    }

    /// Insert the event. This runs in the caller's transaction; if the caller
    /// rolls back, so does this row.
    pub async fn enqueue(&mut self, event: &NewEvent) -> Result<(), sqlx::Error> {
        sqlx::query(
            r#"
            insert into outbox_events (id, event_type, source, subject, time, data)
            values ($1, $2, $3, $4, $5, $6)
            "#,
        )
        .bind(event.id)
        .bind(event.event_type.as_str())
        .bind(SOURCE)
        .bind(&event.subject)
        .bind(event.time)
        .bind(&event.data)
        .execute(&mut **self.executor)
        .await?;
        Ok(())
    }
}

/// A row read back from `outbox_events`. Used by tests and by the future
/// publisher; the shape is core's column list.
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct OutboxRow {
    pub id: Uuid,
    pub event_type: String,
    pub source: String,
    pub subject: String,
    pub time: OffsetDateTime,
    pub data: Value,
    pub published_at: Option<OffsetDateTime>,
    pub attempts: i32,
}

/// Read unpublished rows in the order the publisher would claim them: oldest
/// first, so per-`subject` ordering is preserved, `for update skip locked` so N
/// replicas can run the loop without contending.
pub async fn claim_unpublished<'e, E>(
    executor: E,
    limit: i64,
) -> Result<Vec<OutboxRow>, sqlx::Error>
where
    E: PgExecutor<'e>,
{
    sqlx::query_as::<_, OutboxRow>(
        r#"
        select id, event_type, source, subject, time, data, published_at, attempts
          from outbox_events
         where published_at is null
         order by created_at
         limit $1
           for update skip locked
        "#,
    )
    .bind(limit)
    .fetch_all(executor)
    .await
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn every_event_type_has_three_segments_and_a_publisher_prefix() {
        // The machine-checked form of core's grammar. A fourth segment or a
        // missing prefix is a type no consumer can route, and it is the kind of
        // typo that only surfaces when something tries to subscribe.
        for event_type in [
            EventType::AssetReady,
            EventType::AssetDeleted,
            EventType::VariantCreated,
        ] {
            let name = event_type.as_str();
            let segments: Vec<&str> = name.split('.').collect();
            assert_eq!(segments.len(), 3, "{name} is not three segments");
            assert_eq!(
                segments[0], SOURCE,
                "{name} is not prefixed by its publisher"
            );
            for segment in &segments[1..] {
                assert!(
                    segment
                        .chars()
                        .next()
                        .is_some_and(|c| c.is_ascii_lowercase())
                        && segment
                            .chars()
                            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '_'),
                    "{name}: {segment} is not snake_case"
                );
            }
        }
    }

    #[test]
    fn actions_are_past_tense_and_in_core_s_action_vocabulary() {
        // core's list: created, updated, deleted, ..., completed. `ready` is
        // core's "state, never command" form and `created` is the past tense.
        for event_type in [
            EventType::AssetReady,
            EventType::AssetDeleted,
            EventType::VariantCreated,
        ] {
            let action = event_type
                .as_str()
                .rsplit('.')
                .next()
                .expect("has an action");
            assert!(
                ["ready", "deleted", "created"].contains(&action),
                "{action} is not in the v0 action vocabulary"
            );
        }
    }

    #[test]
    fn the_envelope_has_exactly_the_required_attributes() {
        // core: "Undeclared envelope attributes are rejected:
        // `additionalProperties` is `false`." So the envelope carries these
        // seven keys and no others.
        let event = NewEvent::new(
            EventType::AssetReady,
            "ast_01J9",
            json!({"asset_id": "ast_01J9"}),
        );
        let envelope = event.envelope();
        let object = envelope.as_object().expect("an envelope is an object");

        let mut keys: Vec<&str> = object.keys().map(String::as_str).collect();
        keys.sort_unstable();
        assert_eq!(
            keys,
            [
                "data",
                "id",
                "source",
                "specversion",
                "subject",
                "time",
                "type"
            ]
        );
        assert_eq!(object["specversion"], "1.0");
        assert_eq!(object["source"], SOURCE);
        assert_eq!(object["type"], "darkroom.asset.ready");
        // `subject` is required and is the entity, not the actor.
        assert_eq!(object["subject"], "ast_01J9");
    }

    #[test]
    fn the_envelope_id_is_the_dedupe_key_and_is_stable_across_reuse() {
        let event = NewEvent::new(EventType::AssetDeleted, "ast_1", json!({}));
        let first = event.envelope()["id"].clone();
        // Envelope construction is pure: calling it twice for one event yields
        // one id, which is what lets a publisher retry without minting a new
        // dedupe key.
        let second = event.envelope()["id"].clone();
        assert_eq!(first, second);

        // And two events are two ids.
        let other = NewEvent::new(EventType::AssetDeleted, "ast_1", json!({}));
        assert_ne!(event.envelope()["id"], other.envelope()["id"]);
    }

    #[test]
    fn the_envelope_time_is_rfc3339_utc() {
        let event = NewEvent::new(EventType::VariantCreated, "var_1", json!({}));
        let envelope = event.envelope();
        let time = envelope["time"].as_str().expect("time is a string");
        // Parses as RFC3339 and is a real instant.
        time::OffsetDateTime::parse(time, &time::format_description::well_known::Rfc3339)
            .unwrap_or_else(|e| panic!("{time} is not RFC3339: {e}"));
        assert!(time.ends_with('Z'), "cafaye times are UTC");
    }
}
