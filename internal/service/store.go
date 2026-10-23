// The SQL. Every function here takes a `pgx.Tx` that `darkroomdb.InAccountTx`
// opened with an identity already set on it, and NOT a pool — so there is no
// signature in this file that can be called without a tenant.
//
// # WHY THERE IS NO `and account_id = $N` IN ANY OF IT
//
// `AGENTS.md`, on the Rust side: "`Tenant` is the only way to scope a query. […]
// A repository method that does not take a `Tenant` will not typecheck, which is
// the point." The Go version keeps the shape of that guarantee and changes its
// mechanism: `Tenant` becomes `pgx.Tx` from `InAccountTx`, and the predicate
// becomes a POLICY. `migrations/0004_account_isolation.sql` puts `assets` and
// `asset_variants` under `FORCE ROW LEVEL SECURITY`, so:
//
//   - A cross-tenant `select … where id = $1` returns ZERO ROWS, because the policy
//     filters the row out of the result. `AGENTS.md`: "The rule is absence, not
//     refusal."
//   - A cross-tenant `update`/`delete` affects ZERO ROWS and says so, which is
//     exactly what the compare-and-set in `markReady` needs in order to detect a
//     lost race.
//
// Writing the predicate by hand as well would be belt-and-braces at the cost of
// being WRONG in a way RLS is not: it is a second source of truth for the tenant,
// and `AGENTS.md` calls the Go rule "a function that does not take an account, or
// that takes a transaction from somewhere other than `InAccountTx`, is doing the
// thing the Rust `Tenant` typecheck prevented." This file takes no transaction
// from anywhere else, so the thing it prevents cannot happen here.
//
// # WHY EVERY READ IS A TRANSACTION, AND WHAT THAT COSTS
//
// `darkroom.Store` has no method that runs a query outside a transaction and its
// pool field is unexported, because the identity is set TRANSACTION-LOCAL — "a
// connection that set it at session scope would carry one request's account into
// the next request to be handed that connection from the pool, and that is a
// cross-tenant read with no query wrong anywhere." So a `select` here is a
// `begin; select; commit`.
//
// That is a real cost and it is stated rather than hidden: one round trip to BEGIN
// and one to COMMIT per read. The trade buys a boundary no query can forget. A
// packet that wants cheaper reads adds a method to `darkroomdb` that sets the
// identity on a single acquired connection — it does not add an un-scoped read
// path to this file, which is the only place that would make one possible.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/anywaye/darkroom/internal/domain"
	"github.com/anywaye/darkroom/internal/objectstore"
	"github.com/anywaye/darkroom/internal/variants"
)

// assetColumns is the one list of columns every asset read returns.
//
// It is a constant rather than written out per query because the Rust tree had the
// same duplication and `tests/tenant_scoping.rs` had to guard against a query
// forgetting `account_id`; here the guard for "a query forgot a column" is that
// there is nowhere to write one.
const assetColumns = `id, account_id, owner_user_id, kind, original_filename,
	content_type, byte_size, checksum, status, metadata, created_at, updated_at, storage_key`

const variantColumns = `id, asset_id, account_id, kind, content_type, byte_size,
	storage_key, width, height, metadata, created_at, updated_at`

// findAssetByChecksum is the duplicate-upload PRE-CHECK, and it exists to make the
// common duplicate a cheap indexed read instead of a failed transaction.
//
// A select before the insert rather than catching the constraint violation, because
// the violation aborts the transaction and every duplicate would pay for a rollback.
// The insert remains the authority: two concurrent duplicates both miss here and one
// of them loses the constraint and is resolved by the same code path.
func findAssetByChecksum(ctx context.Context, tx pgx.Tx, checksum string) (*domain.Asset, error) {
	rows, err := tx.Query(ctx, `select `+assetColumns+`
		from assets where checksum = $1 order by created_at desc limit 1`, checksum)
	if err != nil {
		return nil, fmt.Errorf("findAssetByChecksum: %w", err)
	}
	assets, err := collectAssets(rows)
	if err != nil {
		return nil, fmt.Errorf("findAssetByChecksum: %w", err)
	}
	if len(assets) == 0 {
		return nil, nil
	}
	return assets[0], nil
}

// insertAsset writes the `pending` row.
//
// `account_id` IS NOT A PARAMETER. It is read from the transaction's own identity:
// `select anywaye.current_account_id()`. That is stronger than passing it, because a
// row's tenant is then the identity the policies governed the insert with, and
// there is no argument for the two to disagree — the `with check` on the insert
// policy would refuse a mismatch anyway, which would turn a plumbing bug into a 42501
// at the one moment a client is uploading.
func insertAsset(ctx context.Context, tx pgx.Tx, a domain.Asset) (*domain.Asset, error) {
	metadata, err := json.Marshal(orEmpty(a.Metadata))
	if err != nil {
		return nil, fmt.Errorf("insertAsset: marshaling metadata: %w", err)
	}
	row := tx.QueryRow(ctx, `insert into assets (
			id, account_id, owner_user_id, kind, original_filename,
			content_type, byte_size, checksum, storage_key, status, metadata,
			created_at, updated_at)
		values ($1, (select anywaye.current_account_id()), $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12)
		returning `+assetColumns,
		a.ID, a.OwnerUserID, string(a.Kind), a.OriginalFilename, a.ContentType,
		a.ByteSize, a.Checksum, a.StorageKey(), string(a.Status), metadata,
		a.CreatedAt, a.UpdatedAt)

	asset, err := scanAsset(row)
	if err != nil {
		return nil, fmt.Errorf("insertAsset: %w", err)
	}
	return asset, nil
}

// findAsset reads one asset by id, or `pgx.ErrNoRows`.
//
// `pgx.ErrNoRows` IS THE 404, and it covers both "no such asset" and "not yours"
// identically — which is the whole information-disclosure rule: "Cross-tenant is
// 404, not 403. And the body must be byte-identical to the one for an id that never
// existed." Here it is one line of SQL rather than a decision, because the policy
// has already made them the same answer.
func findAsset(ctx context.Context, tx pgx.Tx, id string) (*domain.Asset, error) {
	row := tx.QueryRow(ctx, `select `+assetColumns+` from assets where id = $1`, id)
	asset, err := scanAsset(row)
	if err != nil {
		return nil, fmt.Errorf("findAsset: %w", err)
	}
	return asset, nil
}

// markReady is the compare-and-set that completes an upload: `pending` → `ready`,
// carrying the checksum COMPUTED from the stored bytes.
//
// `and status = 'pending'` is what makes a second concurrent complete lose, and a
// loss is ZERO ROWS rather than an error — which is how the caller tells "another
// request won" from "something went wrong" without a lock. The verified checksum is
// written HERE and nowhere else: "`AGENTS.md`: The checksum is computed, never
// trusted. […] the row ends up carrying the computed value."
//
// No `account_id` predicate, and none needed: the update policy's `using` clause
// hides another account's row, so a cross-tenant id matches nothing here either.
func markReady(ctx context.Context, tx pgx.Tx, id, verifiedChecksum string) (*domain.Asset, error) {
	row := tx.QueryRow(ctx, `update assets
		set status = 'ready', checksum = $2, updated_at = now()
		where id = $1 and status = 'pending'
		returning `+assetColumns, id, verifiedChecksum)

	asset, err := scanAsset(row)
	if errorsIsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("markReady: %w", err)
	}
	return asset, nil
}

// markFailed moves `pending` → `failed` and records why, in the row's own metadata.
//
// The ROW IS KEPT rather than deleted: "the upload is a permanent record of 'this
// was attempted and did not succeed' and so the UNIQUE (account_id, checksum)
// constraint stops a second bad attempt." That is the constraint's job and the
// reason `failed` is terminal rather than a retryable state.
//
// Only from `pending`, for the same reason [markReady] only writes `ready`: a
// terminal asset does not move.
func markFailed(ctx context.Context, tx pgx.Tx, id, reason string) (*domain.Asset, error) {
	row := tx.QueryRow(ctx, `update assets
		set status = 'failed',
		    metadata = metadata || jsonb_build_object('failure_reason', $2::text),
		    updated_at = now()
		where id = $1 and status = 'pending'
		returning `+assetColumns, id, reason)

	asset, err := scanAsset(row)
	if errorsIsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("markFailed: %w", err)
	}
	return asset, nil
}

// listAssets is one cursor page, in the direction `order` names.
//
// `order by created_at <dir>, id <dir>` is the same order as
// `assets_account_created_idx` — `(account_id, created_at desc, id desc)` — read in
// one direction or the other, so a page is an index range scan rather than a sort,
// and the cursor's row-value comparison `(created_at, id) < ($1, $2)` (or `>`) is
// LEXICOGRAPHIC ON THE SAME PAIR — which is why the cursor carries BOTH columns.
// Ordering on `created_at` alone would drop or repeat every row sharing a timestamp,
// and `now()` is a timestamp.
//
// `order` IS A PARAMETER AND NOT READ FROM THE CURSOR, and the two are the same value
// in every call because [Service.ListAssets] refuses a cursor whose direction differs
// (see [Cursor.AgreesWith]). Passing it separately is what makes a first page — the
// one call with NO cursor to read it from — come back in the direction the request
// asked for, and the predicate and the sort are derived from this one argument, so
// they cannot disagree with each other inside this function even if a future caller
// gets the pairing wrong.
func listAssets(ctx context.Context, tx pgx.Tx, order SortOrder, limit int, cursor *Cursor) ([]*domain.Asset, error) {
	query := `select ` + assetColumns + ` from assets`
	var args []any
	if cursor != nil {
		query += ` where (created_at, id) ` + order.comparison() + ` ($1, $2::uuid)`
		args = append(args, cursor.CreatedAt, cursor.ID)
	}
	// ONE EXTRA ROW tells the caller whether there is a next page, without a second
	// `count(*)`. `limit + 1` in the service, `limit` here, so this function does
	// not have to know why the number is one larger than the page.
	args = append(args, limit)
	query += " order by created_at " + order.direction() + ", id " + order.direction() +
		fmt.Sprintf(" limit $%d", len(args))

	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listAssets: %w", err)
	}
	assets, err := collectAssets(rows)
	if err != nil {
		return nil, fmt.Errorf("listAssets: %w", err)
	}
	return assets, nil
}

// listStorageKeys returns every object belonging to an asset: the original plus
// each variant.
//
// Read BEFORE the row is deleted, because a delete that has already committed has
// no keys left to delete and the objects become orphans nothing will ever find. And
// "Every variant's object goes too: deleting an asset whose thumbnail remains is not
// a delete."
func listStorageKeys(ctx context.Context, tx pgx.Tx, assetID string) ([]string, error) {
	rows, err := tx.Query(ctx, `select storage_key from assets where id = $1
		union all
		select storage_key from asset_variants where asset_id = $1`, assetID)
	if err != nil {
		return nil, fmt.Errorf("listStorageKeys: %w", err)
	}
	keys, err := collectStrings(rows)
	if err != nil {
		return nil, fmt.Errorf("listStorageKeys: %w", err)
	}
	return keys, nil
}

// deleteAsset removes the row and reports whether it was there.
//
// `asset_variants` goes with it through the migration's `references assets (id) on
// delete cascade` — the one real foreign key in this schema, because both tables are
// darkroom's.
func deleteAsset(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	tag, err := tx.Exec(ctx, `delete from assets where id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("deleteAsset: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// upsertVariant inserts or replaces one variant of one kind.
//
// `on conflict (asset_id, kind) do update` is what makes "re-request a thumbnail"
// idempotent rather than accumulating rows nobody will ever read, and it is why the
// storage key is replaced too — the old derived object is left to the delete path
// or the operator's sweeper, and README "Not done" says so.
func upsertVariant(ctx context.Context, tx pgx.Tx, v domain.AssetVariant) (*domain.AssetVariant, error) {
	metadata, err := json.Marshal(orEmpty(v.Metadata))
	if err != nil {
		return nil, fmt.Errorf("upsertVariant: marshaling metadata: %w", err)
	}
	row := tx.QueryRow(ctx, `insert into asset_variants (
			id, asset_id, account_id, kind, content_type, byte_size,
			storage_key, width, height, metadata, created_at, updated_at)
		values ($1, $2, (select anywaye.current_account_id()), $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11)
		on conflict (asset_id, kind) do update
		   set content_type = excluded.content_type,
		       byte_size    = excluded.byte_size,
		       storage_key  = excluded.storage_key,
		       width        = excluded.width,
		       height       = excluded.height,
		       metadata     = excluded.metadata,
		       updated_at   = now()
		returning `+variantColumns,
		v.ID, v.AssetID, v.Kind.String(), v.ContentType, v.ByteSize, v.StorageKey(),
		v.Width, v.Height, metadata, v.CreatedAt, v.UpdatedAt)

	variant, err := scanVariant(row)
	if err != nil {
		return nil, fmt.Errorf("upsertVariant: %w", err)
	}
	return variant, nil
}

// listVariants returns one asset's variants, newest first.
//
// No predicate for the caller to forget and no `join` to get wrong: the policy
// already scopes the table, and `asset_variants_asset_id_kind_uniq` makes at most
// three rows.
func listVariants(ctx context.Context, tx pgx.Tx, assetID string) ([]*domain.AssetVariant, error) {
	rows, err := tx.Query(ctx, `select `+variantColumns+`
		from asset_variants where asset_id = $1
		order by created_at desc, id desc`, assetID)
	if err != nil {
		return nil, fmt.Errorf("listVariants: %w", err)
	}
	defer rows.Close()

	out := []*domain.AssetVariant{}
	for rows.Next() {
		variant, err := scanVariant(rows)
		if err != nil {
			return nil, fmt.Errorf("listVariants: %w", err)
		}
		out = append(out, variant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listVariants: %w", err)
	}
	return out, nil
}

// ----------------------------------------------------------------------------
// scanning
// ----------------------------------------------------------------------------

// rowScanner is what pgx returns from `QueryRow` and what a `*pgx.Rows` satisfies.
// Taking it as an interface is what lets one scan function serve both the
// single-row and the multi-row queries instead of existing twice.
type rowScanner interface {
	Scan(dest ...any) error
}

// assetRow is the scan target. It is a struct of `any` rather than typed fields
// because the two things that need care — `metadata` and the enums — are decoded
// after the scan, in one place, where a failure can name which one it was.
type assetRow struct {
	id               string
	accountID        string
	ownerUserID      string
	kind             string
	originalFilename string
	contentType      string
	byteSize         int64
	checksum         string
	status           string
	metadata         []byte
	createdAt        time.Time
	updatedAt        time.Time
	storageKey       string
}

func (r *assetRow) dest() []any {
	return []any{
		&r.id, &r.accountID, &r.ownerUserID, &r.kind, &r.originalFilename,
		&r.contentType, &r.byteSize, &r.checksum, &r.status, &r.metadata,
		&r.createdAt, &r.updatedAt, &r.storageKey,
	}
}

// scanAsset turns one row into a domain asset, and is where a value the SCHEMA
// allowed and the CODE does not recognize is caught.
//
// `pgx.ErrNoRows` is passed through UNWRAPPED so the caller's `errors.Is` reaches
// it; every other error names the column it was decoding. The kind check is the
// half that matters: `domain.AssetKind` and `assets_kind_check` are two copies of
// one fact, and a row carrying a fourth kind means the database and this binary
// disagree — a deploy error, and therefore a 500 rather than a 422 that would blame
// the client for a row darkroom wrote.
func scanAsset(row rowScanner) (*domain.Asset, error) {
	var r assetRow
	if err := row.Scan(r.dest()...); err != nil {
		if errorsIsNoRows(err) {
			return nil, pgx.ErrNoRows
		}
		return nil, fmt.Errorf("scanning an asset row: %w", err)
	}

	kind := domain.AssetKind(r.kind)
	if !kind.Valid() {
		return nil, fmt.Errorf("scanning an asset row: %q is not a known asset kind, so this binary and assets_kind_check disagree", r.kind)
	}
	metadata, err := decodeMetadata(r.metadata)
	if err != nil {
		return nil, fmt.Errorf("scanning an asset row: metadata: %w", err)
	}
	status := domain.AssetStatus(r.status)
	if !isKnownStatus(status) {
		return nil, fmt.Errorf("scanning an asset row: %q is not a known asset status", r.status)
	}
	asset := domain.Asset{
		ID:               r.id,
		AccountID:        r.accountID,
		OwnerUserID:      r.ownerUserID,
		Kind:             kind,
		OriginalFilename: r.originalFilename,
		ContentType:      r.contentType,
		ByteSize:         r.byteSize,
		Checksum:         r.checksum,
		Status:           status,
		Metadata:         metadata,
		CreatedAt:        r.createdAt,
		UpdatedAt:        r.updatedAt,
	}
	return ptr(asset.WithStorageKey(r.storageKey)), nil
}

type variantRow struct {
	id          string
	assetID     string
	accountID   string
	kind        string
	contentType string
	byteSize    int64
	storageKey  string
	width       *int32
	height      *int32
	metadata    []byte
	createdAt   time.Time
	updatedAt   time.Time
}

func (r *variantRow) dest() []any {
	return []any{
		&r.id, &r.assetID, &r.accountID, &r.kind, &r.contentType, &r.byteSize,
		&r.storageKey, &r.width, &r.height, &r.metadata, &r.createdAt, &r.updatedAt,
	}
}

func scanVariant(row rowScanner) (*domain.AssetVariant, error) {
	var r variantRow
	if err := row.Scan(r.dest()...); err != nil {
		if errorsIsNoRows(err) {
			return nil, pgx.ErrNoRows
		}
		return nil, fmt.Errorf("scanning a variant row: %w", err)
	}
	kind, err := variants.ParseKind(r.kind)
	if err != nil {
		return nil, fmt.Errorf("scanning a variant row: %q is not a known variant kind, so this binary and asset_variants_kind_check disagree: %w", r.kind, err)
	}
	metadata, err := decodeMetadata(r.metadata)
	if err != nil {
		return nil, fmt.Errorf("scanning a variant row: metadata: %w", err)
	}
	variant := domain.AssetVariant{
		ID:          r.id,
		AssetID:     r.assetID,
		AccountID:   r.accountID,
		Kind:        kind,
		ContentType: r.contentType,
		ByteSize:    r.byteSize,
		Width:       r.width,
		Height:      r.height,
		Metadata:    metadata,
		CreatedAt:   r.createdAt,
		UpdatedAt:   r.updatedAt,
	}
	return ptr(variant.WithStorageKey(r.storageKey)), nil
}

func collectAssets(rows pgx.Rows) ([]*domain.Asset, error) {
	defer rows.Close()
	out := []*domain.Asset{}
	for rows.Next() {
		asset, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, asset)
	}
	return out, rows.Err()
}

func collectStrings(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scanning a text column: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// decodeMetadata turns a `jsonb` column into a map, and treats a SQL NULL as an
// empty object.
//
// A NULL rather than a nil map, because `metadata` is `not null default '{}'` and a
// nil map would make every `json.Marshal` of one produce `null`, which is a payload
// shape no reader asked for.
func decodeMetadata(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%q is not a JSON object: %w", string(raw), err)
	}
	return out, nil
}

// orEmpty is the write-side twin of [decodeMetadata]'s NULL case.
func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func isKnownStatus(s domain.AssetStatus) bool {
	switch s {
	case domain.StatusPending, domain.StatusReady, domain.StatusFailed:
		return true
	}
	return false
}

// errorsIsNoRows is `errors.Is(err, pgx.ErrNoRows)`, and the reason it is a named
// helper is that three of the compare-and-set functions above must tell "matched
// nothing" from "failed". A bare `==` on a wrapped error is wrong, and this is the
// one place that gets it right.
func errorsIsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// ptr takes the address of a copy, because `WithStorageKey` returns a VALUE — it is
// the non-mutating spelling of "the same asset with its key set", and a method that
// mutated its receiver would let a caller's asset change underneath it.
func ptr[T any](v T) *T { return &v }

// pgxTx is the transaction type, named once so the signature of every function in
// this package reads as "a transaction somebody already scoped" rather than as an
// import.
type pgxTx = pgx.Tx

// errAlreadyGone is the delete path's private signal: the row was not there when the
// delete ran, even though the key read found it.
//
// A private sentinel rather than a `bool` out-parameter, because the transaction
// callback returns `error` and a bool would have to become one anyway — and the
// sentinel cannot be confused with a database failure the way a bool can be ignored.
var errAlreadyGone = errors.New("service: the asset was already gone")

func isAlreadyGone(err error) bool { return errors.Is(err, errAlreadyGone) }

// isNotFound reports whether an object-store error means "no such key".
//
// `errors.Is` and not `==`, because a real store returns `*objectstore.Error` with
// the sentinel WRAPPED and the operation name attached, and a bare comparison would
// miss every production call while passing against a sentinel returned directly.
func isNotFound(err error) bool { return errors.Is(err, objectstore.ErrNotFound) }

// isNoRows reports whether a database error is the absence of a row.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
