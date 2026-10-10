# REPORT — BRK-5, darkroom's outbox publisher

Branch `broker/darkroom-brk5`, worktree `/Users/kaka/Code/any/moon/wt/darkroom-brk5`.
Four commits, nothing merged and nothing pushed.

```
55a2c58 docs: the two outbox dependencies, and what each one buys
e1dcb4e gate: the service identity had been drifting for two packets, and it is a floor
77c19ca outbox: the proofs, the gate floors, and the operational half
addd889 outbox: the publisher, the transport, and the claim that was never ported
```

## The question the brief asked first: did `internal/service/outbox.go` have the claim side?

**No. It did not, and it said so at length.** The file holds the WRITE half and
nothing else:

- `enqueue(ctx, tx, event)` — the only way to write a row, taking a `pgx.Tx`.
- `NewEvent`, `Event`, `Event.Envelope()` — the envelope construction.
- `countOutbox` — a test-only `select count(*)`, used by
  `TestARolledBackTransactionLeavesZeroOutboxRows` and by nobody else.
- `eventTypes` and `eventTypesForTest()` — the guard list.

There is no read path, no `Claim`, no `Batch`, and no `Store`. The file's own
header records this twice, once as a decision ("a publisher that cannot publish
is worse than an absent one, so there is none") and once as an admission of loss:

> The Rust tree DID have it, and losing it is worth recording rather than quietly
> leaving as a gap: `src/outbox.rs` carried a real `claim_unpublished` with
> `OutboxRow`, `where published_at is null order by created_at limit $1 for
> `update skip locked`, and the Go rewrite did not port it.

So the packet was **larger than "from zero" in the sense that the design was
already written and already correct, and smaller in the sense that nothing had to
be invented**. I ported `claim_unpublished` rather than designing a claim, and the
four properties the file named in advance are exactly the four the port carries —
see "The claim" below. The same header also explained why it had NOT been written
again at the time ("every function in this tree is reached by something […] a read
path with no caller would be the first one whose coverage depended on a reader of
this comment rather than on a call site"), which is the reason the loop in
`cmd/darkroom/main.go` and the 19 tests in `internal/outbox` are part of this
packet and not a follow-up: they are what makes the read path reachable.

**So the packet is the whole read half and its wiring, and none of the write
half was touched except to remove a tenant id.** No claim store was duplicated —
there was nothing to duplicate.

## Every event type darkroom publishes

Three, all in `internal/service/outbox.go` and all declared in `cafaye.yml`:

| type | subject | when |
|---|---|---|
| `darkroom.asset.ready` | the asset | `Complete` verified the bytes. Not at create. |
| `darkroom.asset.deleted` | the asset | The row and the storage objects are gone. |
| `darkroom.variant.created` | the **variant** | A derived image is readable. |

Nothing else: no event for a `pending` asset and no event for a failure, both
stated as rules in `AGENTS.md` and both load-bearing. The carrier subject is
therefore **`darkroom.>`**, and that is the one fleet-level fact this packet
imposes on kit's stack.

## What is built

| file | what |
|---|---|
| `internal/outbox/outbox.go` | `Envelope` (core's seven attributes, JSON tags as the wire contract) and `Event` read off a row |
| `internal/outbox/publisher.go` | the loop: `Claimer`, `Publisher`, `NoopPublisher`, `NewService`, `RunOnce`, `Run`, the two constants |
| `internal/outbox/store.go` | `Store.Claim` — the ported claim query, and `pgBatch` (Commit / Release / MarkPublished / MarkFailed) |
| `internal/outbox/nats.go` | `NATSPublisher`, JetStream, subject = event type |
| `internal/outbox/validate.go` | core's patterns, and the capability guard |
| `internal/darkroomdb/darkroomdb.go` | `BeginTx` — the identity-free transaction the claim needs |
| `cmd/darkroom/config.go` | `NATSURL`, `NATSEnabled`, `natsURLVar` |
| `cmd/darkroom/main.go` | the wiring |
| `README.md`, `AGENTS.md`, `.env.example`, `docker-compose.yml`, `bin/prime`, `bin/tier-counts` | the operational and gate halves |

Batch 100. A fixed one-second poll. `NoopPublisher` present and never wired.
The loop runs on the process's shutdown context.

## The claim, and the tenant axis

`claimQuery` is the ported one, verbatim in its four properties:

```sql
SELECT id, event_type, source, subject, time, data
FROM outbox_events
WHERE published_at IS NULL
ORDER BY created_at, id
LIMIT $1
FOR UPDATE SKIP LOCKED
```

**It sets no account, and that is the tenant axis rather than a shortcut.**
`outbox_events` has no `account_id` column and no policy; `migrations/0004` grants
it DML outside `cafaye.protect_table`; `tenancy.yml` records it beside
`idempotency_keys` as deliberately unscoped. So there is no tenant to scope the
claim BY — and the risk runs the other way: wrapping it in `InAccountTx` would put
an identity on a transaction that reads a table no policy guards, which is a claim
that the rows are this account's when nothing in the database agrees. If the table
ever grew a policy, the publisher would read one tenant's events while every
asset still completed.

It goes through the new `darkroomdb.BeginTx`, which opens the same transaction
`InTx` already documents `outbox_events` as being for and hands it back — needed
because the claim's transaction must stay OPEN across a batch of publishes, and
every other starter in the package is a callback that commits when it returns.

What actually holds the axis is that the claim names ONE table and it is that one.
`internal/outbox/store_test.go` reads `claimQuery` as text and holds it, because a
join to carry a checksum onto the bus is exactly the edit that reads as an
improvement and would read another tenant's rows under a transaction carrying no
identity at all. That test also holds the four properties above, and reads
`store.go`'s `Claim` body to hold that no `begin_account` appears in it.

`tenancy-check .` → **0 failures, 2 warnings**, both of which are the honest
"this scanner cannot read my scoping" warnings `tenancy.yml` already declares.
`tenancy.yml` was not edited.

## The capability guard, and the one payload change

**`account_id` was removed from all three payloads.** The brief's rule was "no
tenant ids in the subject"; the payloads were carrying one in `data`, and the bus
is a broadcast surface, so what reached every consumer on the platform was the
identifier of whose asset it was. Two tests now hold that, one on each side:

- **`internal/outbox/validate.go`** — `RejectCapabilities`, called from
  `NATSPublisher.Publish` BEFORE it marshals. Refuses a forbidden key name
  (`account_id`, `tenant_id`, `storage_key`, `bucket`, `object_key`, `key`,
  `presigned*`, `url`, `uri`, `signature`, `secret`, …) AND any string value that
  is an absolute http(s) address, because a name list cannot see a capability
  under an innocent key. A refusal is a FAILED publish: the row stays unpublished
  with `attempts` climbing.
- **`internal/service/payload_guard_test.go`** — reads the three map literals out
  of `service.go` (walking balanced braces, so a payload that gained a field stays
  inside the window) and asserts none carries a banned key, and that they DO carry
  the fact a consumer needs — a guard that only checked for banned keys would pass
  on a literal that said nothing.

**The guard was planted.** `"account_id": "x"` added to the deleted payload turned
it red on the right line; the plant was removed and the test is green again.

`owner_user_id` is deliberately KEPT on `darkroom.asset.ready`. It is not a tenant
id and identity's own `identity.user.created` carries a `user_id`, so the
precedent is core's.

## The proofs, and which one needs a database

None of them needs one. That was the brief's constraint and it shaped the split:

**`internal/outbox/publisher_test.go`** — the loop, driven by a fake `Claimer` and
a `capturingPublisher`. Nine cases:

- the whole shape in one test (three claimed → three envelopes → three marks → one
  commit, no release),
- a refused publish does not fail the pass, leaves the event claimable, and **does
  not block the batch** — the event after the refused one is still published, which
  is the half a test that only counted marks would miss,
- a claim failure is reported and wrapped,
- the carried envelope is core's seven attributes, asserted on the MARSHALLED
  form rather than by reflection,
- the capability guard across nine poison payloads, each proved through the
  publisher and not only the helper,
- darkroom's own three payloads pass the same guard,
- the batch and the poll are 100 and 1s,
- `Run` returns nil and promptly on cancellation,
- `Run` keeps polling while the queue is dry (three claims in 4s).

**`internal/outbox/nats_test.go`** — the transport, against a **real in-process
JetStream broker** on a random loopback port. Ten cases, and the important one is
`TestAnUnpublishedEventReachesTheStreamUnderADarkroomSubject`, which **reads the
message back out of the stream** rather than counting the publisher's return
value: a test that counted the return would prove the transport believed it
delivered, and this one makes the message being there the broker's statement. The
twin with no stream declared is what makes that test mean something — the broker
refuses, `Publish` returns an error naming the subject, and nothing is stored.

`internal/outbox/store_test.go` — the structural guards, as above.

`cmd/darkroom/outbox_wiring_test.go` — the condition and the lifetime. Six cases,
all no-database and no-broker, because a condition test that needed a PostgreSQL
would skip on a bare machine, which is exactly how a publisher that started with
no broker got to production.

**The database-test skip is intact and untouched.** Measured on a bare machine:
`internal/service` is 18 PASS and 31 SKIP (the two payload guards are among the
PASS — they need no database), `pgtest` skips with its named reason. Measured with
`--db`: **0 SKIPs in any database tier**, and the identity holds — 49 enumerated
without a database equals 49 passed with one, checked in both directions.

## A finding: the service identity had been drifting for two packets

`bin/tier-counts` reported **`expected 39, log says 47` on a run every test
passed**, and `bin/prime` reported 47 out of a floor of 39 as fine. Both green on
the same run, because the two figures are read by two different comparisons and
only one is an equality:

```
bin/prime       -lt   — a count that is too HIGH passes
bin/tier-counts ==    — and only this one can see the drift
```

The tenancy packet added `internal/service/tenancy_test.go`'s eight tests and did
not raise either number. Found by running `bin/tier-counts` on a `--db` log rather
than by reading it — the same lesson the planted divergences in `pgtest_test.go`
teach, and a guard that had been passing on a broken source for two packets.

Measured and raised to 49 (the tenancy eight plus this packet's two payload
guards), `cmd/darkroom` to 16, `outbox` added at 19. `AGENTS.md` carries the same
figures plus the asymmetry, so the next reader does not have to re-measure it.

## The two dependencies, and what each buys

`AGENTS.md` requires a cause next to each, and both are in README with `go mod
why -m` output:

- **`nats.go`** — and specifically JetStream's `PublishMsg`, not core NATS'
  `Publish`. Core NATS returns when the message is on the wire, so marking a row
  published on that return would mark rows the broker never saw.
- **`nats-server/v2`** — TEST-ONLY, and it buys the one proof the fakes cannot
  make. A capturing publisher can prove the loop hands the transport an envelope;
  it cannot prove the transport puts anything on a bus, because from the loop's
  point of view the fake IS the bus.

## Verification

```
mise run prime            ./bin/prime --db     → exit 0
./bin/tier-counts <log>                          → ok, 24 variants, 17 harness, 49 service, 34 httpapi, 19 outbox
tenancy-check .                                  → 0 failures, 2 warnings (both pre-existing, declared in tenancy.yml)
go test -v ./internal/service/ ./internal/outbox/ -run <the case>   → the NARROW loop, no database
gofmt -l .   go build ./...   go vet ./...        → clean
```

No skips in any database tier. No skip was removed, weakened or added to get
coverage.

## What is deliberately NOT done

- **No stream is declared.** kit's compose runs the broker with `-js` and defines
  none. Until somebody owning the broker adds `darkroom.>`, every publish is
  refused, the row stays unpublished, and `attempts` climbs. That is the loud
  direction, and it is visible with
  `select count(*) from outbox_events where published_at is null`. This is
  operational, not structural, and it is stated in README's "Not done", in
  `.env.example` and in `docker-compose.yml` — where the variable is DELIBERATELY
  NOT SET, because pointing the service at a broker with no stream buys nothing
  and hides the misconfiguration inside a loop that retries.
- **`docker-compose.yml` gains no broker service.** kit owns the broker; a second
  `nats:` block here would be this repository choosing a stream name, a retention
  policy and a replication factor for the fleet's bus.
- **No `last_error` column.** `migrations/0002` never added one and identity's did,
  so the reason lives in the log and the row carries `attempts`. Adding it is a
  migration and a new one, not an edit to an applied one.
- **No presign-and-drain test through the real HTTP handler.** The loop's proof is
  the in-process broker plus the fake claimer; the write half was already proved.
- **The `DB` tier was not run by the previous attempt** — the worktree was clean at
  dispatch (`git status --porcelain=v1` was empty), so there was no partial work to
  judge, keep or discard. Two commits of structure landed in the first half-hour,
  which is what the quarter shape asked for.
