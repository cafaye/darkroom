-- 0002_outbox_events.sql — the transactional outbox.
--
-- The column list is core's, copied from core/docs/event-outbox.md verbatim
-- because core owns the contract and the implementation is darkroom's. Every
-- darkroom event is inserted into this table in the SAME transaction as the
-- domain write it describes; there is no path in this service that inserts an
-- event anywhere else.
create table if not exists outbox_events (
  id           uuid        primary key,
  event_type   text        not null,
  source       text        not null,
  subject      text        not null,
  time         timestamptz not null,
  data         jsonb       not null,
  created_at   timestamptz not null default now(),
  published_at timestamptz null,
  attempts     int         not null default 0
);

-- The publisher's only query is `where published_at is null order by created_at`.
-- A partial index makes that an index scan of the small unpublished set instead
-- of a sequential scan of every event the service has ever published, which is
-- what the table becomes after a year.
create index if not exists outbox_events_unpublished_idx
  on outbox_events (created_at)
  where published_at is null;
