# HANDOFF — darkroom-go-08, `order` on the asset list

Branch `wt-darkroom-order`. Four commits, and **no test in the packet is
unverified** — but the full gate was not run, because the packet forbids it and
because it is the manager's job on the merged result. Read that first; it is the one
thing here that is not closed.

## What is done, and what proves it

The packet's goal: `GET /v1/assets?order=asc` returns oldest-first instead of
silently returning newest-first. `internal/service/order.go` (new) holds the decision;
`internal/service/cursor.go` carries it in the token; `internal/service/store.go`
builds SQL from it; the `ListAssets` handler in `internal/httpapi/handlers.go`
translates and decides nothing.

| done-means | test | where |
|---|---|---|
| 1. omitting `order` = newest-first | `TestBothDirectionsReturnTheSamePageInOppositeOrder` | `service/listing_test.go` |
| 2. `?order=desc` = unchanged | same test, the `explicit` half | `service/listing_test.go` |
| 3. `?order=asc` = same page, opposite order | same test, the `ascending` half | `service/listing_test.go` |
| 4. a walk visits every asset exactly once, **both directions** | `TestPaginationWalksEveryRowExactlyOnceInEitherDirection` | `service/listing_test.go` |
| 5. a cursor from the other direction is 422 naming `cursor` | `TestACursorFromTheOtherDirectionIsRefused` (DB) + `TestListAssetsRefusesACursorFromTheOtherDirection` (wire) | `service/`, `httpapi/` |
| 6. `?order=anything-else` is 422 naming `order` | `TestAnOrderTheDocumentDoesNotEnumerateIs422NamingTheField` + `TestListAssetsRefusesAnOrderTheDocumentDoesNotEnumerate` | `service/`, `httpapi/` |
| 7. tenant isolation unchanged | `TestTheListQueryNeverNamesTheTenantItself` (structural) + `TestListAssetsSeesOnlyTheCallersAccount` (behavioural) | `service/listing_test.go` |

Expected order in every ordering assertion is built from **the order the assets were
created in**, never from a response. "asc is the reverse of desc" would pass against
an implementation that got both wrong and reversed one of them in Go.

## The plants, because a green test is not the claim `AGENTS.md` asks for

The recovery commit (`8172cad`) landed two test files caught mid-`sed`, "expected to be
wrong in detail". They compile, run against Postgres and pass — but that is not
evidence they *hold* anything. Five divergences were planted; all five fire.

1. **`SortOrder.comparison` never flips** (always `<`). Catches the walk:
   `order=asc visited 3 distinct assets, want 7`, two of them `2 times, want once`.
   **The first page is CORRECT under this plant**, so an assertion about page one
   alone would have stayed green — the failure is only visible because the walk is
   followed to the end. `desc` is unaffected, so this plant is invisible to every
   other test in the package.
2. **`Cursor.AgreesWith` always nil** (the refusal deleted). Three tests red across
   both packages.
3. **`ParseSortOrder` admits nothing.** Six red, including the cursor round trip —
   the token's third segment refusing to survive its own removal.
4. **`listAssets` gains `where account_id = $1 and id = $2`.** Caught by the
   structural guard naming the line, plus two behavioural tests.
5. **The one that closed a gap in my own review:** `handlers.go` parsing `order`,
   validating it, then passing the default anyway. **Both `httpapi` tests assert
   refusals and neither would have noticed** — the handler would agree with the
   cursor it was handed, fall through `AgreesWith`, and reach a nil store. It dies
   with a SIGSEGV in `darkroomdb.(*Store).InTx`. The wiring is held by that panic,
   not by the status codes.

I did not believe the recovered file's own claim that the nil store makes a missing
refusal fail loudly until plant 5 made it panic. The headers now carry measurements
rather than assurances.

## Commands, and their real output

```
go test ./internal/service/ -run 'Order|Cursor|List|Paginat|Page'   ok
go test ./internal/service/                                           ok
go test ./internal/httpapi/ -run 'List' -v                            2 PASS
```

With `TEST_DATABASE_URL` and `DARKROOM_DATABASE_URL` exported. Also run and green:
`gofmt -l .` (empty), `go build ./...`, `go build -o bin/darkroom ./cmd/darkroom`,
`go vet ./...`, `go test -count=1 ./internal/service/ ./internal/httpapi/`.
Counts with the database: **service 39, httpapi 21, pgtest 15, variants 24, auth 12,
0 skips.**

**Nothing is failing.** There is no red command to hand over.

## Not done, and why

- **`./bin/prime --db` was not run.** The packet says the full gate is the manager's
  job on the merged result. This is the one open item and it is not optional
  bookkeeping — see below.
- **The count floors are now coupled to another worker.** This packet added 12 tests,
  so `bin/tier-counts` went to `service 39 / httpapi 21` and `bin/prime`'s floors
  moved with it. `tier-counts` asserts **equality** on those two
  (`got != want` → fail), not a floor. The other worker in this wave is editing
  `internal/httpapi/`; **whoever merges second must re-measure and re-bump**, or the
  identity fails against a tree neither of us ran.
- **No `order` entry was removed from README "Not done"**, because there was none —
  the old handler comment claimed the gap "is recorded in … README 'Not done'" and it
  was not. Pre-existing inaccuracy in a comment, not a defect in behaviour.
- **`info.version` was left at 1.0.0.** A `description` is documentation, not a
  contract change; nothing in `bin/` or the tests reads it, and a bump here would be a
  release decision wearing a documentation commit's clothes. Flagged, not decided.
- **No compatibility shim for the two-segment cursor.** Deliberate: the document says
  clients must not parse it and nothing has ever been served from this
  implementation. The old shape is now one of the refusals.

## Wrong but outside this packet — named, not fixed

- **`internal/httpapi` has no test that a successful `GET /v1/assets` returns a
  200 body**, because that package's service is `service.New(nil, nil)` and a body
  decoded from a nil store is not an answer. `tier-counts` itself calls this "a
  FINDING packet 05 recorded". A 200-shape assertion here needs a seeded database and
  belongs to whoever owns the HTTP package, not to this packet.
- **`bin/tier-counts`'s `pgtest` count is environment-dependent**, which is easy to
  misread as a broken gate: `TestTheLoginRoleCanDoTheWork` reads
  `DARKROOM_DATABASE_URL`, not `TEST_DATABASE_URL`, so with only the latter exported
  it SKIPs and the tier reads 14 of 15. With both exported it is 15/15. Recorded in
  the script's header rather than papered over.

## The successor's first move

Run the gate, from a merged tree with both workers present:

```
export TEST_DATABASE_URL="postgres://darkroom:cafaye@localhost:15500/darkroom?sslmode=disable"
export DARKROOM_DATABASE_URL="postgres://darkroom_app:cafaye@localhost:15500/darkroom?sslmode=disable"
./bin/prime --db 2>&1 | tee /tmp/prime.log
./bin/tier-counts /tmp/prime.log
```

Expect one of exactly two outcomes. `tier-counts` green means the packet and the gate
agree. A red identity line naming `internal/service` or `internal/httpapi` means the
other worker's tests moved the count — re-measure with `go test -list '.*' <pkg>` and
bump `SERVICE_DB_TESTS` / `HTTPAPI_TESTS` in `bin/tier-counts` **and** the matching
`assert_go_suite` line in `bin/prime` together, since the identity compares them.