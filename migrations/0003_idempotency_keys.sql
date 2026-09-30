-- 0003_idempotency_keys.sql — the Idempotency-Key ledger.
--
-- core/docs/openapi-conventions.md "Idempotency":
--   - Scope: the `(endpoint, principal, key)` triple. The same key on a
--     different endpoint or principal is a different key.
--   - Retention: 24 hours, stored with the response.
--   - Replay with the same key and the same body returns the original response.
--   - Replay with the same key and a different body returns 409
--     `idempotency_key_reused`.
--
-- A table and not an in-memory set, because the scope includes the principal
-- and the request may be served by any replica. Two instances with two caches
-- is a service where "idempotent" is true for a retry that lands on the same
-- box and false for one that does not — which is the worst possible property
-- for something whose entire purpose is surviving a timeout.
create table if not exists idempotency_keys (
  key           text        not null,
  -- The route, e.g. `POST /v1/uploads`. Part of the primary key so the same key
  -- on two endpoints does not collide.
  endpoint      text        not null,
  -- The authenticated principal, as `user_id:account_id`. Also part of the key:
  -- one tenant's key must never replay another tenant's response, which would
  -- hand a caller data they are not entitled to.
  principal_key text        not null,
  -- sha256 of the canonical request body. A replay with a different body under
  -- the same key is a client bug and gets 409 `idempotency_key_reused`.
  request_hash  text        not null,
  -- 0 means "reserved, handler still running". A non-zero value is the stored
  -- response and is what a replay returns verbatim.
  status_code   int         not null default 0,
  response_body jsonb       not null default '{}'::jsonb,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now(),

  constraint idempotency_keys_pk primary key (key, endpoint, principal_key)
);

-- Retention is 24 hours, enforced by a sweep rather than by a cron the service
-- does not run yet. The index makes "what expired" a range scan rather than a
-- full table scan, so the sweep is cheap when it is written.
create index if not exists idempotency_keys_created_idx
  on idempotency_keys (created_at);
