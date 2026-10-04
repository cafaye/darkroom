// Package variants is darkroom's derived-image pipeline: decode an uploaded
// image, resize it to a variant's box, and encode it as JPEG.
//
// The Rust version of this file (`src/variants.rs`, deleted by a later packet)
// says the whole job in one line — "use an existing Rust image crate; do not
// hand-roll codecs" — and that is this package's job too. There is no hand-rolled
// resampling here and there is no hand-rolled WebP decoder here: `image` in the
// standard library plus `golang.org/x/image` provide both, and this module only
// chooses parameters and reports the result.
//
// # THE DECISIONS, AND WHY
//
//   - **JPEG output for every variant.** Smaller than PNG for a photograph and
//     decodable by everything that will ever request a thumbnail. One output
//     format means one content type, one cache policy, and one thing for a
//     client to handle. See [VariantKind.ContentType] for why WebP is
//     deliberately not used.
//
//   - **Fit inside a box, preserve aspect ratio.** One number, not two, because
//     there is no correct answer to "which axis wins" for a panorama. See
//     [VariantKind.MaxEdge].
//
//   - **Never upscale.** A 100x80 image asked for as a 256px thumbnail stays
//     100x80. Upscaling costs bytes and adds no information, and a thumbnail
//     that is larger than its source is a bug a client will not catch.
//
//   - **A decode failure is a 422, not a 500.** The bytes were already verified
//     by checksum, so a decode failure means the content_type the client declared
//     and the bytes that arrived disagree — which is a client-visible fact about
//     a client-visible problem. See [undecodable].
//
// # WHY THE ACCEPTED INPUT FORMATS ARE NOT A VARIABLE HERE
//
// The Rust file declares `SUPPORTED_INPUT_FORMATS` so its test can assert that
// `Cargo.toml`'s `image` features were not dropped: without the `webp` feature
// the crate does not compile, so the risk was a build-time one and a list of
// format names was the right guard for it.
//
// In Go the risk is strictly worse and the same shape of guard does not cover
// it. `golang.org/x/image/webp` reaches `image.Decode` through a BLANK IMPORT —
// `import _ "golang.org/x/image/webp"` — and deleting that line leaves a program
// that compiles, links, vets clean and passes every other test, and then fails on
// the first WebP a customer uploads. There is no way to enumerate what
// `image.Decode` has been registered with; the standard library exposes
// `image.RegisterFormat` and no reader for the registry.
//
// So the guard is behavioural rather than declarative: `TestEveryAcceptedInputFormatDecodes`
// carries a real file of each accepted format and asserts each one decodes. Drop
// an import and that test goes red on the one format it dropped. A variable
// listing four format names, asserted against nothing, would have reported green
// the entire time — which is the failure [AGENTS.md] calls a check that proves
// nothing.
//
// # NO cgo, AND WHY THE WEBP DECODER IS PURE GO
//
// `golang.org/x/image/webp` is a first-party pure-Go decoder. The two widely
// used alternatives — `chai2010/webp` and `gen2brain/webp` — are cgo bindings to
// libwebp, and adding either would cost darkroom its static binary and complicate
// the musl build for no gain: darkroom only ever DECODES WebP, and the decoder is
// free and maintained.
//
// WebP ENCODING does not exist first-party (golang/go#45121 is open and
// unassigned), and darkroom does not need it: every variant is JPEG, and a WebP
// only ever arrives as an uploaded original. If a future packet needs to WRITE
// WebP, that packet owns the dependency decision and this comment is the record
// of why there was nothing to reach for.
package variants

import (
	"bytes"
	"image"
	"image/jpeg"
	"log/slog"
	"math"

	"golang.org/x/image/draw"

	// The decoders this package READS, registered by their own `init` and
	// therefore declared here rather than relied upon.
	//
	// `image/jpeg` is also ENCODED below, so it is a real import and not one of
	// these; the other three are blank for a reason: this is darkroom's only
	// statement of which formats it accepts, and a format that stops being
	// accepted should stop compiling rather than stop decoding.
	//
	// They are blank in the PIPELINE'S file rather than only in the tests' on
	// purpose. A test file that imported `image/png` to build its fixtures would
	// register the PNG decoder all by itself, and then deleting the registration
	// from this file would leave every test green — the guard passing on a broken
	// source, which is the failure `AGENTS.md` is most explicit about.
	//
	// image/gif is here for the same reason as the rest: GIF is an accepted input
	// format, so a GIF upload reaches the same decode path. An animated GIF
	// decodes to its first frame, which is what the Rust decoder also produced —
	// `image::DynamicImage` holds one frame and the `image` crate's GIF decoder
	// gives it the first — so the two agree without either being told to.
	_ "golang.org/x/image/webp"
	_ "image/gif"
	_ "image/png"
)

// JPEGQuality is the quality every variant is encoded at. 82 is the point where
// a photographic image at 256-1024px is visually indistinguishable from the
// source and roughly a quarter of the bytes of a PNG of the same pixels.
//
// Asserted in the tests against the ACTUAL OUTPUT — by re-encoding at this
// quality and at its neighbours and requiring the pipeline's bytes to match one
// and differ from the others — so a change to this number has to be deliberate
// rather than a stray keystroke in a rebuild. The literal 82 appears in that
// table as itself and NOT as `JPEGQuality`, because a table built out of the
// constant would agree with any value the constant took.
const JPEGQuality = 82

// Encoded is the result of encoding one variant.
type Encoded struct {
	// Bytes is the JPEG. Width and Height are the dimensions OF THE BYTES, which
	// is the pair a client can verify by decoding them.
	Bytes []byte
	// Width and Height are the encoded dimensions.
	Width  int
	Height int
	// SourceWidth and SourceHeight are the original's dimensions, recorded so a
	// consumer can tell "the thumbnail is 256 wide" from "the source was 100 wide
	// and we did not upscale".
	//
	// The Rust struct holds these as `Option<i32>` because they are columns in
	// `asset_variants` and a variant row written by an older build could be null.
	// Here they are plain ints and always populated; the packet that writes the
	// row decides whether the columns are NOT NULL, and this function has no path
	// that produces a variant without dimensions.
	SourceWidth  int
	SourceHeight int
}

// Encode encodes `original` as `kind`.
//
// `declaredContentType` is what the client said at create time. It is used to
// tell "the bytes are not an image" apart from "Go cannot read this particular
// image", because the first is worth a 422 about `content_type` and the second
// is worth a 422 about the asset.
func Encode(original []byte, kind VariantKind, declaredContentType string) (*Encoded, error) {
	// Two reads of the header, mirroring the Rust reader's `with_guessed_format`
	// and then `decode`. The first distinguishes "these bytes are not an image at
	// all" from "this is an image and it is truncated", which is worth having in
	// the log and is not worth having in the response body.
	if _, _, err := image.DecodeConfig(bytes.NewReader(original)); err != nil {
		slog.Default().Warn("could not read the image header", "error", err, "declared_content_type", declaredContentType)
		return nil, undecodable(declaredContentType)
	}
	decoded, format, err := image.Decode(bytes.NewReader(original))
	if err != nil {
		slog.Default().Warn("could not decode the image", "error", err, "format", format)
		return nil, undecodable(declaredContentType)
	}

	// `Bounds().Dx()` rather than a `Width` field: Go has no `Image::width()`, and
	// `Dx()` is the same number. It is taken from `Bounds()`, not from the config
	// above, because a decoder's config and its image agreeing is not something
	// this package should assume twice.
	bounds := decoded.Bounds()
	sourceWidth, sourceHeight := bounds.Dx(), bounds.Dy()

	// A 0x0 image is not something the decoders produce, so this is a guard
	// against a pathological input rather than a case to handle.
	if sourceWidth == 0 || sourceHeight == 0 {
		return nil, invalidFields("the image has no dimensions", FieldError{Field: "asset", Code: "undecodable"})
	}

	// NORMALISE TO NRGBA, ORIGIN AT ZERO, BEFORE ANYTHING ELSE TOUCHES A PIXEL.
	//
	// Two reasons, both about `Pix` rather than about `At`, because this file
	// deliberately never reads a pixel through `color.Color.RGBA()`:
	//
	//  1. NRGBA STORES COLOUR AND ALPHA INDEPENDENTLY, which is the only space the
	//     matte below means anything in. `out = c*a + 255*(1-a)` says "how much of
	//     this pixel's own colour survives"; against `image.RGBA`, whose Pix is
	//     premultiplied, `c` is already `c*a` and the formula would apply alpha
	//     twice and matte every partially transparent pixel to twice as much white.
	//     Go's PNG decoder hands back `*image.NRGBA` for a truecolour image WITH
	//     alpha and `*image.RGBA` for one without, JPEG hands back `*image.YCbCr`
	//     and GIF `*image.Paletted`, so three of the four accepted formats need
	//     this and the fourth gets it for free.
	//  2. `Bounds().Min` is not guaranteed to be the origin — an `image.SubImage`
	//     or a decoder that crops moves it — and the pixel walk below is index
	//     arithmetic over `Pix`, which has no idea where the image starts.
	//
	// See [alphaPremultiplicationIsNotWhatTheInterfaceDocSays] for why reading
	// `Pix` directly is the load-bearing part of this and not an accident.
	source := normalise(decoded)

	img := source
	maxEdge := kind.MaxEdge()
	// `noMaxEdge` for `web` makes this comparison always false, so `web` never
	// enters the branch. That is the whole of `web`'s resize behaviour and it is
	// expressed by the constant rather than by a special case on the kind.
	//
	// Compared as `uint32` because `noMaxEdge` does not fit in an `int` on a
	// 32-bit platform, and a comparison that did not compile there would be a
	// reason to give `web` a branch of its own — which is the thing this avoids.
	longest := max(sourceWidth, sourceHeight)
	if uint32(longest) > maxEdge {
		width, height := fitInside(sourceWidth, sourceHeight, maxEdge)
		img = resizeToFit(source, width, height)
	}

	width, height := img.Bounds().Dx(), img.Bounds().Dy()

	// JPEG HAS NO ALPHA CHANNEL, and the easy conversion from RGBA DROPS the
	// alpha rather than compositing it: a fully transparent red pixel stays red,
	// and a transparent pixel that was black stays black. That is a visible
	// difference in the served image, so the matte is done here explicitly. It is
	// a real bug the Rust tests caught, and `TestAlphaIsCompositedOverWhite…` is
	// the same test.
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	for i, j := 0, 0; i+3 < len(img.Pix); i, j = i+4, j+4 {
		alpha := img.Pix[i+3]
		out.Pix[j+0] = matteOverWhite(img.Pix[i+0], alpha)
		out.Pix[j+1] = matteOverWhite(img.Pix[i+1], alpha)
		out.Pix[j+2] = matteOverWhite(img.Pix[i+2], alpha)
		// The alpha byte is written, and NOTHING DEPENDS ON IT — which is worth
		// saying rather than leaving as an apparent load-bearing line.
		//
		// `image/jpeg` encodes through `toYCbCr`, which reads
		// `m.At(x, y).RGBA()`, and in Go 1.26 `color.RGBA.RGBA()` returns its four
		// channels SCALED TO 16 BITS AND NOT MULTIPLIED BY ALPHA. So an
		// `*image.RGBA` with an alpha byte of zero still encodes its RGB channels,
		// and this write can be removed without changing a single output byte.
		//
		// It stays because `image.RGBA`'s documented invariant is that a
		// non-opaque image holds PREMULTIPLIED values, and leaving 0x00 here makes
		// this image a lie in its own storage: the next reader to hand `out` to
		// something that does respect premultiplication — a future `draw.Over`, a
		// different encoder — would get a black image, and nothing in this
		// repository's tests would have said why. A line that costs a store and
		// keeps the representation honest is cheaper than a comment explaining an
		// inconsistent one.
		out.Pix[j+3] = 0xff
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: JPEGQuality}); err != nil {
		// The one failure in this file that is OURS rather than the caller's. The
		// Rust version logs and returns an internal error for the same reason: an
		// encoder that fails on bytes it just built is a bug here, and a 422 would
		// tell the client to fix their upload.
		slog.Default().Error("jpeg encode failed", "error", err, "width", width, "height", height)
		return nil, internal("the variant could not be encoded")
	}

	return &Encoded{
		Bytes:        buf.Bytes(),
		Width:        width,
		Height:       height,
		SourceWidth:  sourceWidth,
		SourceHeight: sourceHeight,
	}, nil
}

// undecodable builds the 422 for bytes that are not a readable image.
//
// The field named is the one the client controls. If the client declared
// `image/png` and the bytes are not a PNG, `content_type` is the thing to fix.
func undecodable(declaredContentType string) *Error {
	if isImageContentType(declaredContentType) {
		return invalidFields(
			"the stored bytes are not a readable image of the declared content_type",
			FieldError{Field: "content_type", Code: "mismatch"},
		)
	}
	return invalidFields(
		"the stored bytes are not a readable image",
		FieldError{Field: "asset", Code: "undecodable"},
	)
}

// isImageContentType reports whether the client claimed an image at all.
//
// A prefix test and not a table lookup, and the same shape as the Rust
// `starts_with("image/")`: the question here is whether `content_type` is the
// field worth naming, and "the client said it was some kind of image" answers
// that without this package needing its own copy of the accepted media types.
// Which types are ACCEPTED is `domain.AssetKind`'s decision, one layer up.
func isImageContentType(contentType string) bool {
	return len(contentType) >= len("image/") && contentType[:len("image/")] == "image/"
}

// fitInside scales `width`x`height` so its LONGEST edge is `maxEdge`, preserving
// the aspect ratio, and returns at least 1 on each axis.
//
// The arithmetic is `float32` on purpose, because the Rust version's is `f32` and
// the two implementations are supposed to produce the same dimensions for the
// same input. `float64` would be the better choice in a package that had no
// predecessor, and `math.Round` rounds half away from zero in both languages, so
// the two agree on the ties.
func fitInside(width, height int, maxEdge uint32) (int, int) {
	longest := max(width, height)
	scale := float32(maxEdge) / float32(longest)
	fitted := func(axis int) int {
		// `.max(1)` in Rust: the rounding in the scale calculation can produce 0
		// for a very extreme aspect ratio, and a zero-width variant is one the
		// encoder rejects with an opaque error.
		return max(int(math.Round(float64(float32(axis)*scale))), 1)
	}
	return fitted(width), fitted(height)
}

// normalise returns `src` as an `*image.NRGBA` whose bounds start at the origin.
//
// See the call site for why both halves of that sentence matter.
func normalise(src image.Image) *image.NRGBA {
	// The fast path is an ALLOCATION saved and nothing else: `draw.Draw` with
	// `draw.Src` into a fresh NRGBA converts a NRGBA source to the same non-
	// premultiplied bytes, so removing this branch is a slowdown rather than a
	// bug. It is here because a 4000x1000 upload that skips a full-image copy on
	// the no-resize path is worth two lines, and said so because a reader finding
	// no test guarding it would otherwise assume one exists.
	if nrgba, ok := src.(*image.NRGBA); ok && nrgba.Bounds().Min == (image.Point{}) {
		return nrgba
	}
	bounds := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), src, bounds.Min, draw.Src)
	return dst
}

// resizeToFit scales `src` to exactly width x height with the pipeline's kernel.
//
// It is a named function rather than three lines inside `Encode` because the
// KERNEL is a decision with nothing else in the file depending on it, and
// `TestTheResampleKernelIsSharperThanBiLinear` needs to reach it without going
// through a JPEG round trip that would add lossy noise to the measurement. The
// first draft of that test called `draw.CatmullRom` and `draw.BiLinear` side by
// side and passed while `Encode` used either of them, because it never touched
// `Encode` — a guard that measures the two candidates and not the thing that
// chose between them.
//
// `CatmullRom` because it is the closest thing `golang.org/x/image/draw` offers
// to the Rust `FilterType::Lanczos3` this replaces, and the idiomatic Go choice
// besides. `BiLinear` is the other kernel in that package and it is visibly
// softer on the 256px thumbnail this service serves most.
//
// The resample happens in PREMULTIPLIED space, which is the right space for it
// and not an accident: `x/image/draw` reads `src.At().RGBA()` and writes through
// `color.NRGBAModel`, so an NRGBA source is premultiplied on the way in and
// un-premultiplied on the way out. Interpolating the non-premultiplied colour
// across a transparent edge instead would drag the transparent pixel's hue into
// its opaque neighbour — a halo, and a visible one at the edge of every cut-out
// logo a customer uploads.
//
// `draw.Src` rather than `draw.Over`. On THIS destination the two are
// indistinguishable, and that was measured rather than assumed: `Over`
// composited against the zeroed destination `image.NewNRGBA` just made is the
// identity on the source, so swapping the two changes no byte of output and no
// test here can tell them apart. `Src` is chosen because it states the intent —
// every pixel is written, not blended with whatever was there — and stays correct
// if a future change reuses a destination buffer. That it is currently
// unobservable is why there is no plant for it in the divergence table rather
// than there being a test that has gone missing.
func resizeToFit(src *image.NRGBA, width, height int) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	return dst
}

// matteOverWhite composites one channel over a white matte:
//
//	out = src*a + 255*(1-a)
//
// with the `+127` rounding to nearest rather than truncation, so a half-alpha
// channel lands on the nearest integer rather than always one below it. The
// intermediate is `uint32` rather than the Rust version's `u16` — the arithmetic
// is identical and `uint32` cannot overflow at all, where a `u8` multiply wraps
// and a fully opaque red pixel comes out black. The Rust comment about the `u16`
// intermediate is the rule; Go does not need the workaround and must not lose the
// formula.
func matteOverWhite(channel, alpha uint8) uint8 {
	c, a := uint32(channel), uint32(alpha)
	return uint8((c*a + 255*(255-a) + 127) / 255)
}

// # ALPHA PREMULTIPLICATION IS NOT WHAT THE INTERFACE DOC SAYS
//
// A note that belongs to no one function and is load-bearing in two of them, so
// it is written once here and linked from both.
//
// The `color.Color` interface documents `RGBA()` as returning "the
// ALPHA-PREMULTIPLIED red, green, blue and alpha values". In Go 1.26 — the
// toolchain `go.mod` pins — the two concrete types darkroom actually handles do
// the OPPOSITE of each other:
//
//	color.RGBA.RGBA()   →  r|g<<8, with NO multiply by A   (NOT premultiplied)
//	color.NRGBA.RGBA()  →  (r|g<<8) * A / 0xff             (premultiplied)
//
// Both were read out of `$GOROOT/src/image/color/color.go` and then MEASURED,
// because the doc and the code disagree and the measurement is the part that
// settles it: an 8x8 `image.RGBA` of pure red with every alpha byte zero,
// encoded by `image/jpeg` and decoded again, comes back as (254, 0, 0) — not
// black. Under a premultiplying implementation `255 * 0 / 255` is 0, so that
// could not have come out any other way.
//
// The consequences, and they are the reason the note exists:
//
//   - **This package reads `Pix`, never `At()`.** Walking `Pix` is the only way
//     to be sure which of the two conventions is in front of it, and it is why
//     `matteOverWhite` takes two `uint8`s rather than a `color.Color`.
//   - **`golang.org/x/image/draw` reads `At().RGBA()`**, so it sees an NRGBA
//     source PREMULTIPLIED. That is the correct space to resample in, and it is
//     wanted — see the resize in `Encode`.
//
// `TestTheJpegEncoderReadsRawChannelsNotPremultipliedOnes` holds the measurement
// as a test, because a standard-library behaviour this package's correctness
// leans on is a thing that can change underneath it, and a comment is not a
// canary.

// The ACCEPTED INPUT FORMATS, written down once, in one place, as this file's
// first test drives over them: PNG, JPEG, GIF and WebP.
//
// There is deliberately no exported variable holding that list, and the package
// comment says why at length: in Rust the equivalent list guarded a BUILD-time
// risk that a missing Cargo feature would not compile, and here the same risk is
// a blank import that compiles perfectly and fails on the first WebP a customer
// uploads. The registry behind `image.Decode` cannot be read back — the standard
// library exposes `image.RegisterFormat` and no reader for the registry — so a
// variable asserted against nothing would report green the entire time it was
// wrong.
//
// `golang.org/x/image/webp` is a pure-Go decoder covering both WebP bitstreams:
// lossless (VP8L) and lossy (VP8). Both are in `testdata/`, so the test that
// holds this import in place covers both halves of it.
