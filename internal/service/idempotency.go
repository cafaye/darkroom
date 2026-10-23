// The `Idempotency-Key` ledger: reservation, replay, release.
//
// # WHAT IS HERE AND WHAT IS NOT
//
// The DATABASE half of core's "Idempotency" section lives in this file: reserve,
// record, release, and the request hash they are keyed on. The HTTP half — reading
// the header, wrapping the handler, writing `Idempotency-Replayed` — is packet 05's,
// because it is a router concern and this package holds no HTTP.
//
// # WHY THE TRANSACTION IS `InTx` AND NOT `InAccountTx`
//
// `idempotency_keys` has NO `account_id` column and therefore no policy — the
// migration says so, and `migrations/0004_account_isolation.sql` grants it DML
// explicitly for exactly this reason. The scope is the PRIMARY KEY:
// `(key, endpoint, principal_key)`. So this is the one place in the service that
// opens a transaction without an identity, and it is safe precisely because the
// isolation is carried by the key rather than by a session: "One tenant's key must
// never replay another tenant's response, which would hand a caller data they are
// not entitled to" — and `principal_key` is `user_id:account_id`, so it cannot.
//
// # THE FLOW, AND WHY EACH STEP IS WHERE IT IS
//
//  1. RESERVE. An insert of `(key, endpoint, principal)`. This is the lock — exactly
//     one concurrent request can hold it.
//  2. BODY CHECK. A hit with a different body hash is 409 `idempotency_key_reused`.
//  3. REPLAY. A hit with `status_code != 0` is the recorded response.
//  4. RUN. We hold the reservation, so we do the work.
//  5. RECORD OR RELEASE. Success stores the response. Failure DELETES the
//     reservation so the client's retry is a fresh attempt — "a 500 that poisoned the
//     key would make every subsequent retry fail forever."
//
// # THE IN-FLIGHT CASE IS 409 AND NOT AN EMPTY SUCCESS
//
// "The concurrent case, which is the one that bites": two identical POSTs with one
// key arrive at once, one reserves and the other finds the row. If the first has not
// finished, the second is told 409 `conflict` — "not a fabricated replay of a response
// that does not exist yet. Returning 'success with an empty body' would be worse than
// an error: the client would believe the upload was created and then never have an id
// for it."
package service

import (
	"context"
	"log/slog"

	"github.com/anywaye/darkroom/internal/checksum"
	"github.com/anywaye/darkroom/internal/problem"
)

// MaxIdempotencyKeyLength is the longest key accepted.
//
// core: a key is a client-chosen uuid. Anything longer is not a uuid and is almost
// certainly a mistake or an attempt to fill a table — and the table has no per-key
// size limit beyond this, so the limit is the only thing standing between a client
// and a row of a megabyte.
const MaxIdempotencyKeyLength = 255

// ValidateIdempotencyKey checks a client-supplied key.
//
// A malformed key is a 422 and NOT silently treated as absent: "a client that meant
// to send a key and sent a broken one would otherwise get a non-idempotent request
// and not know why." "No key at all" is a separate, supported state and reaches the
// service as an empty string, which packet 05 treats as "process normally".
func ValidateIdempotencyKey(raw string) *problem.Error {
	switch {
	case raw == "":
		return problem.InvalidFields(
			"Idempotency-Key must not be empty",
			problem.NewField("idempotency-key", "required"),
		)
	case len(raw) > MaxIdempotencyKeyLength:
		return problem.InvalidFields(
			"Idempotency-Key must be at most 255 characters",
			problem.NewField("idempotency-key", "too_long"),
		)
	}
	for _, r := range raw {
		// Printable ASCII and nothing else: the key goes into a `text` column, into
		// a log line, and into a URL a client may quote. A control character in any
		// of those is a log-forging primitive.
		if r < 0x21 || r > 0x7e {
			return problem.InvalidFields(
				"Idempotency-Key must be printable ASCII with no spaces",
				problem.NewField("idempotency-key", "invalid_format"),
			)
		}
	}
	return nil
}

// PrincipalScope is the `principal_key`: `user_id:account_id`.
//
// Both halves and not just the account, because one user in one account is one
// principal and two users in one account are two: core scopes the ledger by
// principal, and a shared key between two users of the same account would replay one
// user's response to the other.
func PrincipalScope(userID, accountID string) string {
	return userID + ":" + accountID
}

// RequestHash is the sha256 of the canonical request body, which is what makes
// "same key, different body" detectable.
//
// Computed over BYTES and not over the parsed value, so two spellings of the same
// JSON — key order, a space, a number written `1` against `1.0` — are different
// bodies and get the 409. That is the strict reading and it is the right one: core's
// contract is about the request, and a client retrying a failed upload resends the
// same bytes it sent the first time.
func RequestHash(body []byte) string { return checksum.SHA256Hex(body) }

// Replay is a stored response returned in place of running the handler again.
type Replay struct {
	// StatusCode is the ORIGINAL response's status, byte for byte. A replay that
	// changed the status would be a different response to the same request.
	StatusCode int
	// Body is the recorded body as stored. Returned verbatim rather than re-encoded,
	// because the bytes a client received the first time are the bytes it must
	// receive on a retry.
	Body []byte
}

// ReserveIdempotent tries to take the reservation for `(key, endpoint, principal)`.
//
// A nil *Replay with a nil error means THE CALLER HOLDS THE RESERVATION and should
// do the work, then call [Service.CompleteIdempotent] or
// [Service.ReleaseIdempotent]. A non-nil *Replay is the original response and the
// handler must not run.
//
// The insert and the read are ONE transaction and therefore one connection, which is
// the whole reason this does not use a pool: "a reserve on one connection followed by
// a read on another is two unrelated queries, and the read would not see the row the
// insert is still writing."
func (s *Service) ReserveIdempotent(ctx context.Context, key, endpoint, principal, requestHash string) (*Replay, *problem.Error) {
	if pErr := ValidateIdempotencyKey(key); pErr != nil {
		return nil, pErr
	}

	var (
		storedHash    string
		statusCode    int
		responseBody  []byte
		reservationOK bool
	)
	err := s.db.InTx(ctx, func(tx pgxTx) error {
		tag, err := tx.Exec(ctx, `insert into idempotency_keys (key, endpoint, principal_key, request_hash)
			values ($1, $2, $3, $4) on conflict do nothing`, key, endpoint, principal, requestHash)
		if err != nil {
			return err
		}
		reservationOK = tag.RowsAffected() == 1
		if reservationOK {
			return nil
		}
		// Someone holds it. Read what they left.
		return tx.QueryRow(ctx, `select request_hash, status_code, response_body
			from idempotency_keys where key = $1 and endpoint = $2 and principal_key = $3`,
			key, endpoint, principal).Scan(&storedHash, &statusCode, &responseBody)
	})
	if err != nil {
		return nil, problem.FromDB(err)
	}
	if reservationOK {
		return nil, nil
	}

	switch {
	case storedHash != requestHash:
		// A distinct CODE and not `conflict`, because a client can act on it: it
		// means "pick a new key", not "the world changed".
		return nil, problem.IdempotencyKeyReused(
			"this Idempotency-Key was already used with a different request body",
		)
	case statusCode == 0:
		// Reserved, handler still running. 409, and never a fabricated success.
		return nil, problem.Conflict("a request with this Idempotency-Key is still in flight")
	default:
		slog.Info("idempotent replay", "key", key, "endpoint", endpoint)
		return &Replay{StatusCode: statusCode, Body: responseBody}, nil
	}
}

// CompleteIdempotent stores the response for a reservation the caller holds.
//
// ITS OWN STATEMENT and not the handler's transaction, deliberately: "the ledger
// write must commit even if a later step rolls back". By the time this is called the
// domain work has already committed in its own transaction, so folding them together
// would mean a replay could never find a response for work that succeeded.
//
// THE ERROR IS LOGGED AND SWALLOWED. The work succeeded and only the ledger write
// failed; a retry with the same key re-runs the handler, which is safe for these
// endpoints because the domain work is compare-and-set or unique-constrained — so a
// 500 here would tell the client the upload failed when it did not. That reasoning is
// the Rust tree's, and it depends on the state machine above being idempotent by
// construction, which it is: `markReady` is a compare-and-set and `create_upload`
// resolves the duplicate.
func (s *Service) CompleteIdempotent(ctx context.Context, key, endpoint, principal string, statusCode int, body []byte) {
	err := s.db.InTx(ctx, func(tx pgxTx) error {
		_, err := tx.Exec(ctx, `update idempotency_keys
			set status_code = $4, response_body = $5::jsonb, updated_at = now()
			where key = $1 and endpoint = $2 and principal_key = $3`,
			key, endpoint, principal, statusCode, body)
		return err
	})
	if err != nil {
		slog.Error("could not record the idempotency outcome", "key", key, "endpoint", endpoint, "error", err)
	}
}

// ReleaseIdempotent deletes a reservation whose work FAILED, so the client's retry
// is a fresh attempt.
//
// Logged and swallowed for the same reason as [Service.CompleteIdempotent]: the
// client is already getting the original error, and replacing it with a 500 would
// tell it less about what it did wrong. A leaked reservation is not fatal — the row
// is `status_code = 0`, so the next retry gets the in-flight 409 until the 24-hour
// sweep in `migrations/0003_idempotency_keys.sql` removes it.
func (s *Service) ReleaseIdempotent(ctx context.Context, key, endpoint, principal string) {
	if err := s.db.InTx(ctx, func(tx pgxTx) error {
		_, err := tx.Exec(ctx, `delete from idempotency_keys
			where key = $1 and endpoint = $2 and principal_key = $3`, key, endpoint, principal)
		return err
	}); err != nil {
		slog.Error("could not release the idempotency reservation", "key", key, "endpoint", endpoint, "error", err)
	}
}
