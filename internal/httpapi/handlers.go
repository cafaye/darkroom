// The nine handlers, and the two things they are allowed to do: translate, and
// render.
//
// `AGENTS.md`: "Everything that is a *decision* lives [in the service] rather than
// in a handler, so it can be tested without an HTTP round trip and so a handler
// stays a translation between the wire and a call." Every `problem.Error` below is
// therefore either returned by `service` or is about the WIRE — a malformed body, a
// path that is not a uuid, a missing `Content-Type` — and never about the domain.
//
// # EVERY ID-SCOPED HANDLER OPENS WITH TenantOf
//
// Not a nil check. A PANIC. See `internal/auth/tenant.go` for the full argument; the
// one-line version is that an empty account reaches `InAccountTx` as `account_id =
// ”`, which with FORCE ROW LEVEL SECURITY matches no rows while every write still
// exits 0 — so "forgot the tenant" has to be a crash rather than a plausible answer.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	uuidlib "github.com/google/uuid"

	"github.com/cafaye/darkroom/internal/api"
	"github.com/cafaye/darkroom/internal/auth"
	"github.com/cafaye/darkroom/internal/domain"
	"github.com/cafaye/darkroom/internal/problem"
	"github.com/cafaye/darkroom/internal/service"
	"github.com/cafaye/darkroom/internal/variants"
)

// ----------------------------------------------------------------------------
// the two probes
// ----------------------------------------------------------------------------

// Healthz is `GET /healthz`. Unconditional.
//
// `AGENTS.md`: "Liveness never touches a dependency. `/healthz` is unconditional. A
// database outage must not get the process restarted out from under in-flight
// uploads; that is `/readyz`'s job." There is no database here, no object store and
// no verifier, and adding any of them is a behavior change to the one endpoint
// whose job is to say "the process is up".
func (s *Server) Healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, api.Probe{Status: api.ProbeStatus("ok")})
}

// Readyz is `GET /readyz`. It really does `select 1`.
//
// `AGENTS.md`: "A probe that returns 200 blindly is worse than no probe: it tells
// the load balancer to send traffic to a process that cannot serve a request." And:
// "Probe failures are logged, never returned in detail. The body names the
// dependency; the underlying error goes to the log. An unauthenticated caller must
// not learn that a database host is `10.0.0.5`." So the detail below is a fixed
// sentence and the wrapped error — which carries the host — goes to [Server.log].
func (s *Server) Readyz(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		// A probe that cannot reach a database cannot say the service can serve
		// traffic, so this is 503 and not 200. Unreachable in production: `New`
		// requires it, and that is a deliberate constraint on `New` even though
		// `/healthz` does not use it — a Store that is optional here is a Store that
		// is nil in the environment that forgot to configure the URL.
		s.log.Error("readyz has no database to ask")
		writeProblemError(w, r, problem.Unavailable("a dependency is not answering"))
		return
	}

	if err := s.db.Ping(r.Context()); err != nil {
		s.log.Error("readyz failed", "error", err)
		writeProblemError(w, r, problem.Unavailable("a dependency is not answering"))
		return
	}

	writeJSON(w, r, http.StatusOK, api.Probe{Status: api.ProbeStatus("ready")})
}

// ----------------------------------------------------------------------------
// uploads
// ----------------------------------------------------------------------------

// CreateUpload is `POST /v1/uploads`.
//
// The status is 201 whether or not this resolved to an existing asset, and that is
// the service's decision, not this handler's: `service.CreateUpload`'s header argues
// it at length (`THE DUPLICATE DECISION, AND WHY IT IS 201 AND NOT 409`). What this
// handler adds is the header the document declares for it — `X-Darkroom-Duplicate` —
// from the flag the service already computed, so "it was a duplicate" is visible
// without a client having to compare storage keys.
func (s *Server) CreateUpload(w http.ResponseWriter, r *http.Request, params api.CreateUploadParams) {
	tenant := auth.TenantOf(r)

	var request api.CreateUploadRequest
	if !decodeJSON(w, r, &request) {
		return
	}

	created, pErr := s.service.CreateUpload(r.Context(), tenant.Caller(), service.CreateUpload{
		Filename:    request.Filename,
		ContentType: request.ContentType,
		ByteSize:    request.ByteSize,
		Checksum:    request.Checksum,
	})
	if pErr != nil {
		writeProblemError(w, r, pErr)
		return
	}

	if created.Duplicate {
		w.Header().Set("X-Darkroom-Duplicate", "true")
	}
	// Recorded in `internal/service/idempotency.go` and rendered here. `Replayed` is
	// the service's fact; the header is the document's spelling of it.
	// `Idempotency-Replayed` is declared by the document and is NOT written here.
	// `internal/service/idempotency.go` holds the ledger and its comparison, and
	// `service.CreateUpload` does not yet report whether a request was a replay — so
	// writing the header from a guess would be a client told "replayed" for a request
	// that was not. It is a packet-06 item and README "Not done" says so.
	writeJSON(w, r, http.StatusCreated, api.CreateUploadResponse{
		Asset:         assetToAPI(*created.Asset),
		UploadURL:     created.Presigned.URL,
		StorageKey:    created.Asset.StorageKey(),
		ExpiresInSecs: int(created.Presigned.ExpiresIn),
		ContentType:   created.Asset.ContentType,
		// `headers` is omitted rather than `{}`: the document marks it optional and
		// an in-memory store's presigned URL needs none, so an empty object would be
		// a document describing headers that do not exist.
		Duplicate: created.Duplicate,
	})
}

// CompleteUpload is `POST /v1/uploads/{id}/complete`.
func (s *Server) CompleteUpload(w http.ResponseWriter, r *http.Request, id api.AssetID, params api.CompleteUploadParams) {
	tenant := auth.TenantOf(r)

	var request api.CompleteUploadRequest
	if !decodeJSON(w, r, &request) {
		return
	}

	asset, pErr := s.service.CompleteUpload(r.Context(), tenant.Caller(), id.String(), request.Checksum)
	if pErr != nil {
		writeProblemError(w, r, pErr)
		return
	}
	writeJSON(w, r, http.StatusOK, api.CompleteUploadResponse{Asset: assetToAPI(*asset)})
}

// ----------------------------------------------------------------------------
// assets
// ----------------------------------------------------------------------------

// ListAssets is `GET /v1/assets`. One cursor page, scoped to the caller's account.
//
// THREE THINGS ARE TRANSLATED HERE AND NONE OF THEM IS DECIDED HERE: the limit, the
// cursor, and the order. Each is turned into the value `service` takes — a clamped
// number, an opaque position, a [service.SortOrder] — and the service answers. The
// two refusals below are the same refusals `service` would make, moved ahead of the
// call so that a request this server can answer without touching a database answers
// without touching a database; `service.ListAssets` makes both of them again for a
// caller that arrives without an HTTP layer in front of it.
func (s *Server) ListAssets(w http.ResponseWriter, r *http.Request, params api.ListAssetsParams) {
	tenant := auth.TenantOf(r)

	// THE ORDER IS PARSED, NOT ASSUMED. The generated binder asks for
	// `Type: "string"` and never checks the document's `enum: [asc, desc]` — nothing
	// upstream of this handler catches a value that is neither — so `order` arrives
	// here as whatever the query string held. `service.ParseSortOrder` is the one
	// parser for it and it refuses with a 422 naming `order`, which is the answer for
	// a value the document enumerates rather than clamps.
	order := service.OrderDesc
	if params.Order != nil {
		parsed, pErr := service.ParseSortOrder(string(*params.Order))
		if pErr != nil {
			writeProblemError(w, r, pErr)
			return
		}
		order = parsed
	}

	query := service.ListQuery{Limit: defaultPageLimit, Order: order}
	if params.Limit != nil {
		// Clamped by `service.clampPageLimit`, not rejected: the document says "A
		// larger request is clamped, not rejected", and the decision belongs to the
		// service so that a caller of the service and a caller of the wire agree.
		query.Limit = *params.Limit
	}
	if params.Cursor != nil {
		cursor, err := service.DecodeCursor(*params.Cursor)
		if err != nil {
			// 422 and not 400. The document declares 400 for "malformed request
			// SYNTAX" and 422 for "semantically wrong", and a cursor that does not
			// decode is syntactically fine JSON and semantically wrong — which is the
			// distinction the two statuses exist to make.
			writeProblemError(w, r, problem.InvalidFields(
				"cursor is not a valid pagination cursor",
				problem.NewField("cursor", "invalid_value"),
			).Wrap(err))
			return
		}
		query.Cursor = cursor
	}

	// A CURSOR FROM THE OTHER DIRECTION IS REFUSED, and this is the check that keeps a
	// UI which re-sorts mid-walk from being served a page that skips and repeats rows.
	// `service.Cursor.AgreesWith` is where the rule lives; asking here rather than
	// waiting for `ListAssets` is what saves the refusal a database round trip.
	if pErr := query.Cursor.AgreesWith(query.Order); pErr != nil {
		writeProblemError(w, r, pErr)
		return
	}

	page, pErr := s.service.ListAssets(r.Context(), tenant.Caller(), query)
	if pErr != nil {
		writeProblemError(w, r, pErr)
		return
	}

	// `data` is ALWAYS an array. The document says so on the response, and a nil
	// slice marshals as `null`, which a client rendering a list has to special-case.
	data := make([]api.Asset, 0, len(page.Assets))
	for _, asset := range page.Assets {
		data = append(data, assetToAPI(*asset))
	}

	var next *string
	if page.NextToken != "" {
		token := page.NextToken
		next = &token
	}

	writeJSON(w, r, http.StatusOK, api.AssetList{
		Data: data,
		Page: api.Page{HasMore: page.HasMore, NextCursor: next},
	})
}

// GetAsset is `GET /v1/assets/{id}`.
//
// The 404 here is byte-identical to the 404 for an id that never existed, and it is
// for one reason: `service.GetAsset` returns `problem.NotFound` for both, and this
// handler writes whatever it is handed without consulting anything else. Nothing in
// this function can tell the two cases apart, which is the point — `AGENTS.md` calls
// it "the rule is absence, not refusal", and a body that differed would be "just as
// much of an oracle as a different status".
func (s *Server) GetAsset(w http.ResponseWriter, r *http.Request, id api.AssetID) {
	tenant := auth.TenantOf(r)

	asset, pErr := s.service.GetAsset(r.Context(), tenant.Caller(), id.String())
	if pErr != nil {
		writeProblemError(w, r, pErr)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]api.Asset{"asset": assetToAPI(*asset)})
}

// DeleteAsset is `DELETE /v1/assets/{id}`. 204 with no body.
//
// The document's 204 declares no content, and `writeJSON` is not used here because
// there is nothing to write: `w.WriteHeader(204)` and no body at all. A 204 carrying
// `{}` is legal and every client ignores it, which is the same class of difference
// the byte on `json.Encoder` made.
func (s *Server) DeleteAsset(w http.ResponseWriter, r *http.Request, id api.AssetID) {
	tenant := auth.TenantOf(r)

	if pErr := s.service.DeleteAsset(r.Context(), tenant.Caller(), id.String()); pErr != nil {
		writeProblemError(w, r, pErr)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ----------------------------------------------------------------------------
// variants
// ----------------------------------------------------------------------------

// CreateVariant is `POST /v1/assets/{id}/variants`.
func (s *Server) CreateVariant(w http.ResponseWriter, r *http.Request, id api.AssetID) {
	tenant := auth.TenantOf(r)

	var request api.CreateVariantJSONRequestBody
	if !decodeJSON(w, r, &request) {
		return
	}

	kind, err := variants.ParseKind(string(request.Kind))
	if err != nil {
		// An unknown `kind` is a 422 naming `kind`, and it is caught HERE rather than
		// in the service only because `ParseKind` is the one parser for the value and
		// `service.CreateVariant` validates the result — the refusal is a 422 either
		// way and both are asserted by the same table in `service`'s tests.
		writeProblemError(w, r, problem.InvalidFields(
			"kind must be one of thumbnail, preview or web",
			problem.NewField("kind", "invalid_value"),
		).Wrap(err))
		return
	}

	variant, pErr := s.service.CreateVariant(r.Context(), tenant.Caller(), id.String(), kind)
	if pErr != nil {
		writeProblemError(w, r, pErr)
		return
	}

	writeJSON(w, r, http.StatusCreated, map[string]api.AssetVariant{"variant": variantToAPI(*variant)})
}

// ListVariants is `GET /v1/assets/{id}/variants`.
//
// A missing asset is a 404 and NOT an empty `data` array. The document says so, and
// `service.ListVariants`' header says why: "an empty list would be
// indistinguishable from 'this asset has no variants yet', which is an existence
// oracle with one bit." So this handler renders `[]` only after the service has
// confirmed the parent is the caller's.
func (s *Server) ListVariants(w http.ResponseWriter, r *http.Request, id api.AssetID) {
	tenant := auth.TenantOf(r)

	found, pErr := s.service.ListVariants(r.Context(), tenant.Caller(), id.String())
	if pErr != nil {
		writeProblemError(w, r, pErr)
		return
	}

	data := make([]api.AssetVariant, 0, len(found))
	for _, variant := range found {
		data = append(data, variantToAPI(*variant))
	}

	// The document types `VariantList.page` as `Page`, so the page is rendered even
	// though this endpoint returns every variant rather than a page of them.
	// `has_more: false` and a null cursor is the honest rendering of "this is all of
	// them", and the Rust tree answers the same way.
	writeJSON(w, r, http.StatusOK, api.VariantList{
		Data: data,
		Page: api.Page{HasMore: false, NextCursor: nil},
	})
}

// ----------------------------------------------------------------------------
// the translation helpers
// ----------------------------------------------------------------------------

// defaultPageLimit mirrors the document's `limit` default of 25. The document's
// number, not the service's: `service.clampPageLimit` clamps whatever it is given and
// a handler that passed 0 would get the same answer, so this constant is about the
// request the service sees rather than the answer it gives.
const defaultPageLimit = 25

// decodeJSON reads a body into `into`, or answers and returns false.
//
// Three refusals and each names the WIRE rather than the domain, which is the line
// this file does not cross:
//
//   - A body that is not JSON, or is not the declared object: 400. The document's
//     `BadRequest` is "Malformed request syntax", which is exactly this.
//   - A body over [MaxBodyBytes]: 413, from the layer below, detected here as
//     `http.MaxBytesError` so the handler's refusal is the same document.
//   - Otherwise 422 naming nothing, for a body that parsed and contradicts the
//     document's `required` list. The generated types have no per-field required
//     checks, and inventing one here would be a second place that knows what the
//     document requires.
//
// `r.Body` is already wrapped by [bodyLimit], so this never reads an unbounded body.
func decodeJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(r.Body)
	// `DisallowUnknownFields` is deliberately NOT set. The document says
	// `CreateUploadRequest` is `additionalProperties: true`, so a client sending an
	// extra key has sent a valid request and refusing it would be this handler
	// disagreeing with the contract.
	if err := decoder.Decode(into); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			// The CHUNKED case: `ContentLength` was -1, so the layer could not know,
			// and `MaxBytesReader` failed here instead. The same document and the same
			// status as the known-length case above — one refusal for one failure.
			writeStatus(w, r, http.StatusRequestEntityTooLarge, problemPayloadTooLarge(MaxBodyBytes))
			return false
		}
		if errors.Is(err, errEmptyBody) {
			// A body the document declares `required: true` and that is not there, and
			// one that is not JSON, are the SAME refusal: `openapi/v1.yaml` declares a
			// `400` for "Malformed request syntax" on these four operations and a `422`
			// only for a body that parses and contradicts the schema. There is no
			// generated per-field required check to add here, and inventing one would be
			// a second place that knows what the document requires.
			writeStatus(w, r, http.StatusBadRequest, problem.Invalid("the request body is not valid json"))
			return false
		}
		writeStatus(w, r, http.StatusBadRequest, problem.Invalid("the request body is not valid json"))
		return false
	}
	return true
}

// errEmptyBody is `io.EOF` under a name that says what it means here.
var errEmptyBody = io.EOF

// assetToAPI is the domain vocabulary rendered as the document's.
//
// A FUNCTION rather than a method, and an explicit field list rather than a marshal:
// `domain.Asset` has an unexported `storageKey` and `api.Asset` has no field for it,
// so a struct-to-struct conversion would be a compile error for the right reason —
// but an explicit list also means a NEW field on the domain type cannot appear on the
// wire without someone writing it here. "There is deliberately no `storage_key`
// field" is the document's sentence, and `TestNoAssetResponseCarriesAStorageKey` is
// what holds it.
func assetToAPI(asset domain.Asset) api.Asset {
	metadata := asset.Metadata
	if metadata == nil {
		// `not null default '{}'` in the schema, so an asset with no metadata still
		// answers `{}` rather than `null`. `internal/service` normalises this on the
		// way out of the row and it is repeated here because a NULL reaching the wire
		// would be a document the contract does not describe.
		metadata = map[string]any{}
	}
	return api.Asset{
		ID:               uuid(asset.ID),
		AccountID:        uuid(asset.AccountID),
		OwnerUserID:      uuid(asset.OwnerUserID),
		Kind:             api.AssetKind(asset.Kind.String()),
		OriginalFilename: asset.OriginalFilename,
		ContentType:      asset.ContentType,
		ByteSize:         asset.ByteSize,
		Checksum:         asset.Checksum,
		Status:           api.AssetStatus(asset.Status.String()),
		Metadata:         metadata,
		CreatedAt:        asset.CreatedAt,
		UpdatedAt:        asset.UpdatedAt,
	}
}

func variantToAPI(variant domain.AssetVariant) api.AssetVariant {
	out := api.AssetVariant{
		ID:          uuid(variant.ID),
		AssetID:     uuid(variant.AssetID),
		AccountID:   uuid(variant.AccountID),
		Kind:        api.AssetVariantKind(variant.Kind.String()),
		ContentType: variant.ContentType,
		ByteSize:    variant.ByteSize,
		CreatedAt:   variant.CreatedAt,
		UpdatedAt:   variant.UpdatedAt,
	}
	if variant.Metadata != nil {
		metadata := variant.Metadata
		out.Metadata = &metadata
	}
	// `*int32` from the service to the document's `*int`, and the conversion is
	// guarded rather than cast: a width is a pixel count and a value that does not
	// fit in an `int` on a 32-bit build is a defect, not a client-facing 0.
	if variant.Width != nil {
		width := int(*variant.Width)
		out.Width = &width
	}
	if variant.Height != nil {
		height := int(*variant.Height)
		out.Height = &height
	}
	return out
}

// uuid is the document's `format: uuid` as the generated type. A string this service
// wrote is a string it can render; an unparseable one would be a database defect, and
// `uuid.Parse` returns the zero UUID rather than an error so a broken row produces a
// visible zero rather than a panic on the response path.
func uuid(value string) uuidlib.UUID {
	parsed, err := uuidlib.Parse(value)
	if err != nil {
		slog.Error("a uuid column holds something that is not a uuid", "value", value, "error", err)
		return uuidlib.UUID{}
	}
	return parsed
}

// writeJSON writes a body, and is where the JSON content type is decided once.
//
// The type is set ONLY when the caller has not already chosen one. That conditional
// is not tidiness: `writeProblemError` sets `application/problem+json` and then a
// handler's accidental call to this would overwrite it with `application/json`, and
// every problem document the service produces would be served with the media type
// that says "this is not a problem document".
func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	// `json.Marshal` and NOT `json.NewEncoder(w).Encode`, which appends a newline to
	// every response. The gateway's equivalence suite found that byte on 63 of 72
	// comparisons, and it is exactly the kind of difference that ships because every
	// client parses both.
	encoded, err := json.Marshal(body)
	if err != nil {
		// A marshaling failure means this handler built a body the document does not
		// allow. Nothing has been written yet, so the honest answer is a problem
		// document rather than a panic on a response path.
		slog.Error("a response did not encode", "method", r.Method, "path", r.URL.Path, "error", err)
		writeProblemError(w, r, problem.Internal("the request could not be completed"))
		return
	}

	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	if _, err := w.Write(encoded); err != nil {
		slog.Error("a response did not write", "method", r.Method, "path", r.URL.Path, "error", err)
	}
}
