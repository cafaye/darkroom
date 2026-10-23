// Package domain is the vocabulary: assets, variants, and the enums that constrain
// them.
//
// These are what the API returns and what the database stores. The enum values are
// the same strings as the SQL `check` constraints in `migrations/0001_assets.sql`,
// and `TestTheEnumValuesAreTheCheckConstraints` is what keeps the two from drifting
// — asserted by reading the migration rather than by a comment, because a comment
// about a constraint is exactly the kind of thing that stops being true quietly.
//
// # WHY `AssetKind` IS DERIVED AND NEVER SUPPLIED
//
// A `kind` decides which decoders run. If a client could choose it, a request for an
// `image` variant on a file whose bytes are not an image would reach the image
// decoder — so `kind` is a server-side derivation from `content_type` and the client
// only supplies `content_type`. The allow-list is both a security boundary and a
// contract boundary, and both directions are asserted.
package domain

import (
	"errors"
	"strings"
	"time"

	"github.com/anywaye/darkroom/internal/variants"
)

// AssetKind is what an asset IS. The values are the four names in
// `assets_kind_check`.
type AssetKind string

// The four kinds. A plain string type and not an int with a String() method,
// because the value on the wire and in the database is the same text and there is
// nothing to convert between.
const (
	KindImage    AssetKind = "image"
	KindAudio    AssetKind = "audio"
	KindVideo    AssetKind = "video"
	KindDocument AssetKind = "document"
)

// String returns the value as it is stored and as it goes on the wire.
func (k AssetKind) String() string { return string(k) }

// Valid reports whether k is one of the four. Used when reading a row BACK: a row
// this service wrote cannot carry anything else, so a failure there is a deploy
// error and is reported as one rather than as a 422 that would blame the client for
// a row darkroom wrote.
func (k AssetKind) Valid() bool {
	switch k {
	case KindImage, KindAudio, KindVideo, KindDocument:
		return true
	}
	return false
}

// ParseAssetKind derives a kind from a client-supplied `content_type`.
//
// Parameters (`; charset=utf-8`) are stripped and the comparison is
// case-insensitive before the allow-list is applied, because clients send
// `Image/PNG; charset=binary` and the kind does not depend on any of that.
//
// WHY AN UNRECOGNIZED TYPE IS REJECTED RATHER THAN GUESSED: the accept-list for
// `image/*` holds only the four formats the pipeline can decode. Accepting
// `image/x-psd` would create an asset no variant path can read and no error until
// somebody asked for a thumbnail — a failure that surfaces as a 500 on a request
// that never mentioned the format. Every refusal names `content_type`, so a client
// learns which field to fix.
func ParseAssetKind(contentType string) (AssetKind, error) {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))

	// `top` is the `type` half of `type/subtype`. A bare word with no slash is not a
	// media type, and it falls through to the refusal below rather than matching a
	// prefix.
	top, _, hasSubtype := strings.Cut(base, "/")
	if !hasSubtype {
		return "", unsupportedContentType("content_type must be a supported image, audio, video or document type")
	}

	switch top {
	case "image":
		// Only formats the decoder can actually read.
		switch base {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
			return KindImage, nil
		}
		return "", unsupportedContentType("unsupported image type")
	case "audio":
		switch base {
		case "audio/mpeg", "audio/ogg", "audio/wav", "audio/webm":
			return KindAudio, nil
		}
		return "", unsupportedContentType("unsupported audio type")
	case "video":
		switch base {
		case "video/mp4", "video/quicktime", "video/webm":
			return KindVideo, nil
		}
		return "", unsupportedContentType("unsupported video type")
	case "application":
		switch base {
		case "application/pdf", "application/rtf", "application/json", "application/zip":
			return KindDocument, nil
		}
		return "", unsupportedContentType("unsupported document type")
	}
	return "", unsupportedContentType("content_type must be a supported image, audio, video or document type")
}

// BaseContentType normalises a client-supplied content type to `type/subtype` in
// lowercase, which is what gets stored, signed and presigned.
//
// Lowercased rather than stored as sent because the presigned URL's signature covers
// it: two spellings of the same media type would produce two signatures for one
// asset, and a client that sent `Image/PNG` would have to guess which spelling the
// URL was signed with.
func BaseContentType(contentType string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
}

// AssetStatus is the upload's lifecycle. Three states, and the TRANSITIONS are the
// point:
//
//	pending → ready    (complete: object present, measured size matches, checksum verified)
//	pending → failed   (complete: object absent, size mismatch, or checksum mismatch)
//
// `ready` and `failed` are TERMINAL. A `failed` asset is never retried into `ready`;
// the client creates a new upload. That is what keeps "this upload failed" a fact
// rather than a state that can be argued about later, and it is what keeps the
// `UNIQUE (account_id, checksum)` constraint meaningful — a checksum that failed
// once cannot be silently retried into a second attempt under the same key. It also
// gives `pending` rows older than the presign TTL exactly one meaning, which is the
// set a sweeper would fail.
type AssetStatus string

// The three statuses, matching `assets_status_check`.
const (
	StatusPending AssetStatus = "pending"
	StatusReady   AssetStatus = "ready"
	StatusFailed  AssetStatus = "failed"
)

// String returns the stored and wire value.
func (s AssetStatus) String() string { return string(s) }

// Terminal reports whether no further transition is possible.
//
// A method rather than a `switch` at the three call sites, because the property is
// the reason the state machine exists and a call site that re-derives it is a call
// site that can forget it.
func (s AssetStatus) Terminal() bool {
	return s == StatusReady || s == StatusFailed
}

// Asset is an asset as the API returns it.
//
// `StorageKey` is PRESENT and UNEXPORTED because the service needs it and the API
// does not: `AGENTS.md` says "`Asset` has no `storage_key` field. It is a
// server-side detail and a client that can read it will try to build a URL from
// it." An unexported field is the Go spelling of that rule — a caller in another
// package cannot name it, and packet 05's JSON encoder cannot serialize it.
//
// `Checksum` is the value COMPUTED from the stored bytes, never the client's claim:
// "`assets.checksum` is set at insert from the client's claim, then REPLACED by the
// value computed from the stored object at complete time" (the migration's own
// comment).
type Asset struct {
	ID               string
	AccountID        string
	OwnerUserID      string
	Kind             AssetKind
	OriginalFilename string
	ContentType      string
	ByteSize         int64
	Checksum         string
	Status           AssetStatus
	Metadata         map[string]any
	CreatedAt        time.Time
	UpdatedAt        time.Time

	// storageKey is the object's key. Read by the service, invisible to the wire.
	storageKey string
}

// StorageKey returns the object's key, for the code that has to talk to the object
// store. It is NOT serialized, and no caller in this repository uses it to build a
// URL — the only two callers are the complete path, which hashes what is at that
// key, and the delete path, which removes it. `AGENTS.md` gives the reason a key
// must not reach a client: "`Asset` has no `storage_key` field. It is a server-side
// detail and a client that can read it will try to build a URL from it."
func (a Asset) StorageKey() string { return a.storageKey }

// WithStorageKey returns a copy carrying `key`.
//
// A constructor rather than an exported field so that the key can only ever be set
// from inside this package, where [storagekey.GenerateOriginal] or a value read back
// from a row put it there — `AGENTS.md`: "Storage keys are never client-supplied."
func (a Asset) WithStorageKey(key string) Asset {
	a.storageKey = key
	return a
}

// IsDerivable reports whether a variant can be generated from this asset: it must
// be `ready` AND an image. Anything not `ready` may not have bytes at all, or may
// have bytes that are not what the client said.
func (a Asset) IsDerivable() bool {
	return a.Status == StatusReady && a.Kind == KindImage
}

// AssetVariant is a derived image. It points at its own storage key and the original
// row is never mutated by variant generation.
//
// `Kind` is `variants.VariantKind` and is NOT redefined here. It is a single
// source of truth for "what is a thumbnail", and the alternative — a second enum
// that has to agree with the pipeline's bounds and content types — is precisely the
// kind of copy that drifts. Packet 05 renders it with `.String()`.
type AssetVariant struct {
	ID          string
	AssetID     string
	AccountID   string
	Kind        variants.VariantKind
	ContentType string
	ByteSize    int64
	Width       *int32
	Height      *int32
	Metadata    map[string]any
	CreatedAt   time.Time
	UpdatedAt   time.Time

	storageKey string
}

// StorageKey is the derived object's key, for the object store and for the delete
// sweep. Same rule as [Asset.StorageKey] and the same comment applies.
func (v AssetVariant) StorageKey() string { return v.storageKey }

// WithStorageKey returns a copy carrying `key`.
func (v AssetVariant) WithStorageKey(key string) AssetVariant {
	v.storageKey = key
	return v
}

// unsupportedContentType builds the refusal. Every one of these is a 422 naming
// `content_type`, which is a client-visible fix rather than a 500.
func unsupportedContentType(detail string) error {
	return errUnsupportedContentType{detail: detail}
}

// errUnsupportedContentType is a private error so `ParseAssetKind` does not have to
// depend on `problem` — the DOMAIN knows what a media type is, and importing the
// HTTP envelope to say so would make the vocabulary depend on the wire. The service
// maps it, once, in one place.
type errUnsupportedContentType struct{ detail string }

func (e errUnsupportedContentType) Error() string { return e.detail }

// IsUnsupportedContentType reports whether err came from [ParseAssetKind].
//
// The service's single mapping point: `errors.As` on a private type would need an
// exported constructor to be useful, and this predicate is the exported form of the
// same check.
func IsUnsupportedContentType(err error) bool {
	var target errUnsupportedContentType
	return errors.As(err, &target)
}
