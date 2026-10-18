package variants

import "strconv"

// VariantKind is a derived image. `thumbnail` and `preview` are fixed-box
// re-encodes; `web` is the re-encode at original dimensions that a CMS or a
// document renderer wants to serve instead of a PNG.
//
// It lives in this package rather than in a `domain` one because no `domain`
// package exists yet in this tree, and creating one holding a single type would
// be a shape the packet that writes `AssetKind` and `Asset` would have to
// reshape. The packet that builds the domain moves this type; nothing above it
// should depend on which package it came from, which is why every use of a kind
// goes through the three methods below and never through the numeric value.
type VariantKind int

const (
	// KindInvalid is the zero value, and it is deliberately not a kind.
	//
	// Go hands every declared-and-unassigned variable its type's zero value, so
	// `var kind VariantKind` is `KindThumbnail` if the kinds start at 0 — and
	// then a caller who ignored [ParseKind]'s error, or built a kind by casting
	// an int, gets a THUMBNAIL rather than a missing value. Reserving 0 makes the
	// zero value fail [VariantKind.Valid] and bounds an image to nothing, which
	// is a loud failure rather than a wrong 256-pixel answer.
	//
	// The Rust enum's first variant is the real `Thumbnail`, and this is the one
	// place the two representations deliberately disagree. The wire names are the
	// contract; the numeric values are not.
	KindInvalid VariantKind = 0

	// KindThumbnail is the 256px longest-edge variant.
	KindThumbnail VariantKind = iota + 1
	// KindPreview is the 1024px longest-edge variant.
	KindPreview
	// KindWeb is the full-size re-encode.
	KindWeb
)

// noMaxEdge is `web`'s bound, and it is `u32::MAX` in the Rust version: never
// scale down.
//
// It is a real number rather than a sentinel the resizer checks for, so that
// `longest <= maxEdge` is the whole of the no-upscale rule and `web` needs no
// branch of its own. `^uint32(0)` and `math.MaxUint32` are the same value; the
// former is written without importing `math` into a file whose only arithmetic
// is this.
const noMaxEdge = ^uint32(0)

// MaxEdge is the longest edge this variant is bounded by, in pixels.
//
// A single number, not a width and a height: every variant here is "fit inside
// this box, preserve aspect ratio", so a 4000x100 panorama and a 100x4000 one
// both come out bounded by the same edge. A bound that is per-axis would need a
// policy for which axis wins, and there is no correct answer.
func (k VariantKind) MaxEdge() uint32 {
	switch k {
	case KindThumbnail:
		return 256
	case KindPreview:
		return 1024
	case KindWeb:
		// `web` is a re-encode at original dimensions: it exists to convert, not
		// to resize, so it has no bound.
		return noMaxEdge
	default:
		return 0
	}
}

// ContentType is the output media type. Every variant is JPEG.
//
// WebP would be the better format and it is deliberately NOT used, for two
// reasons that are the same reason. The Rust version refused it because the
// `image` crate's WebP encoder is lossless-only — its own docs say so, and point
// at libwebp for lossy — and a lossless re-encode of a 256px thumbnail saves
// little enough that it is not worth a C dependency and a second codec's worth of
// failure modes. Go refuses it for a stronger version of the same reason: there
// is no first-party WebP ENCODER at all (golang/go#45121 is open and
// unassigned), and the pure-Go decoder in `golang.org/x/image` is decode-only. So
// "switch the variants to WebP" is not a smaller change here — it is cgo libwebp,
// and with it the static binary the Dockerfile builds.
//
// `image/jpeg` is lossy, pure Go, statically linked and decodable by everything
// that will ever request a thumbnail. Revisit when a first-party lossy WebP
// encoder exists and the bytes are worth it.
func (k VariantKind) ContentType() string {
	return "image/jpeg"
}

// String is the wire name of the kind.
//
// [KindInvalid] prints as `invalid` rather than as an empty string, so a bug that
// manufactures one says something in a log instead of nothing — and a kind
// outside all four prints as its number, so a bug that casts a stray int to a
// kind says `VariantKind(7)` rather than `thumbnail`.
func (k VariantKind) String() string {
	switch k {
	case KindThumbnail:
		return "thumbnail"
	case KindPreview:
		return "preview"
	case KindWeb:
		return "web"
	case KindInvalid:
		return "invalid"
	default:
		return "VariantKind(" + strconv.Itoa(int(k)) + ")"
	}
}

// AllVariantKinds is every kind there is.
//
// It exists so a caller that iterates — a warmer, a CLI — and this package's own
// tests enumerate the set from one place rather than listing three literals at
// each site and forgetting the fourth. It returns a fresh slice each call so a
// caller that sorts or appends to it cannot edit the next caller's answer.
func AllVariantKinds() []VariantKind {
	return []VariantKind{KindThumbnail, KindPreview, KindWeb}
}

// Valid reports whether `k` is one of the three kinds above.
//
// [VariantKind.MaxEdge] returns 0 for a value built by casting an int, and 0 is
// not a usable box: a caller that forgot to check its error would otherwise
// resize every image down to a 1x1.
func (k VariantKind) Valid() bool {
	switch k {
	case KindThumbnail, KindPreview, KindWeb:
		return true
	default:
		// [KindInvalid] and every value that was cast into a kind. See
		// [KindInvalid] for why the zero value is one of these.
		return false
	}
}

// ParseKind reads a wire name.
//
// A `kind` that is not in this list is a 422: an unknown variant kind cannot be
// told apart from a typo, and inventing behavior for it is how an API grows
// fifty near-identical kinds.
func ParseKind(s string) (VariantKind, error) {
	switch s {
	case "thumbnail":
		return KindThumbnail, nil
	case "preview":
		return KindPreview, nil
	case "web":
		return KindWeb, nil
	default:
		// [KindInvalid], not `0` written out: since the kinds start at 1, `0` IS
		// `KindInvalid`, and naming it is what keeps a caller that ignored this
		// error from holding a thumbnail.
		return KindInvalid, invalidFields(
			"unknown variant kind: "+s,
			FieldError{Field: "kind", Code: "unknown_value"},
		)
	}
}
