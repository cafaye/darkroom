package variants

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// The images are BUILT here rather than checked in as fixtures, so the tests
// exercise the decoder rather than a file someone forgot to update — the same
// reason the Rust tests build their PNGs. WebP is the exception and cannot be:
// there is no first-party Go WebP encoder, which is the same fact the package
// comment gives as the reason every variant is JPEG. Those three files are the
// only fixtures and how they were made is recorded beside them in
// `testdata/README.md`.

// gradientPNG builds a real PNG of the requested size with a deterministic
// gradient, so a resize test is resizing something with detail in it rather than
// a flat colour — a flat colour survives almost any filter unchanged and would
// pass a broken resampler.
func gradientPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x % 256),
				G: uint8(y % 256),
				B: 128,
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("building a %dx%d PNG: %v", width, height, err)
	}
	return buf.Bytes()
}

// solidPNG is a PNG that is one flat RGBA colour, 8x8.
//
// 8x8 rather than 1x1 because the matte assertions read the CENTRE pixel, and
// 1x1 would make "the centre" a single pixel whose value depends on the JPEG's
// DC coefficient alone — which is a weaker test than it looks, since a filter
// that shifted every pixel by one would still pass.
func solidPNG(t *testing.T, r, g, b, a uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: a})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("building a solid PNG: %v", err)
	}
	return buf.Bytes()
}

// fixture reads a checked-in image from testdata.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading the %s fixture: %v", name, err)
	}
	if len(raw) == 0 {
		t.Fatalf("the %s fixture is empty, which would make the test that uses it vacuous", name)
	}
	return raw
}

// dimensions decodes `raw` and returns its size, from the BYTES rather than from
// the struct Encode returned.
//
// Every dimension assertion in this file goes through here. A test that asserted
// `encoded.Width == 256` would pass if `Encode` wrote the right number into its
// own struct and the wrong pixels; decoding is the only reading of "the variant
// is 256 wide" a client can perform.
func dimensions(t *testing.T, raw []byte) (int, int) {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the output does not decode: %v", err)
	}
	if format != "jpeg" {
		t.Fatalf("the output is %q, not a JPEG", format)
	}
	b := img.Bounds()
	return b.Dx(), b.Dy()
}

// middlePixel decodes `raw` and returns the centre pixel as RGB.
func middlePixel(t *testing.T, raw []byte) [3]uint8 {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the output does not decode: %v", err)
	}
	b := img.Bounds()
	r, g, bl, _ := img.At(b.Dx()/2, b.Dy()/2).RGBA()
	return [3]uint8{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8)}
}

// encodeOK calls Encode and fails the test if it did not succeed.
func encodeOK(t *testing.T, raw []byte, kind VariantKind) *Encoded {
	t.Helper()
	encoded, err := Encode(raw, kind, "image/png")
	if err != nil {
		t.Fatalf("Encode(%s): %v", kind, err)
	}
	return encoded
}

// TestAThumbnailIsBoundedByItsBoxAndKeepsTheAspectRatio is the widest case, and
// the one a per-axis bound would get wrong.
func TestAThumbnailIsBoundedByItsBoxAndKeepsTheAspectRatio(t *testing.T) {
	// A 4000x1000 panorama: the longest edge is 4000, so the width becomes 256
	// and the height scales with it. A per-axis bound would have made this
	// 256x256, which is a different image.
	source := gradientPNG(t, 4000, 1000)
	encoded := encodeOK(t, source, KindThumbnail)

	if w, h := dimensions(t, encoded.Bytes); w != 256 || h != 64 {
		t.Fatalf("a 4000x1000 thumbnail is 256x64, got %dx%d", w, h)
	}
	if encoded.Width != 256 || encoded.Height != 64 {
		t.Errorf("the reported size is %dx%d, want 256x64", encoded.Width, encoded.Height)
	}
	if encoded.SourceWidth != 4000 || encoded.SourceHeight != 1000 {
		t.Errorf(
			"the source size is %dx%d, want 4000x1000 — a consumer cannot tell "+
				"'the thumbnail is 256 wide' from 'the source was 100 wide and we "+
				"did not upscale' without these",
			encoded.SourceWidth, encoded.SourceHeight,
		)
	}
}

func TestATallImageIsBoundedOnItsLongEdgeToo(t *testing.T) {
	source := gradientPNG(t, 500, 2000)
	encoded := encodeOK(t, source, KindThumbnail)

	// The mirror of the panorama: bounded on the HEIGHT, and the width follows.
	if w, h := dimensions(t, encoded.Bytes); h != 256 || w != 64 {
		t.Fatalf("a 500x2000 thumbnail is 64x256, got %dx%d", w, h)
	}
}

func TestASmallImageIsNeverUpscaled(t *testing.T) {
	// 100x80 asked for as a 256px thumbnail stays 100x80. Upscaling costs bytes
	// and adds no information, and a thumbnail bigger than its source is a bug
	// the client will not catch.
	source := gradientPNG(t, 100, 80)
	encoded := encodeOK(t, source, KindThumbnail)

	if w, h := dimensions(t, encoded.Bytes); w != 100 || h != 80 {
		t.Fatalf("a 100x80 image must come back 100x80, got %dx%d", w, h)
	}
	if encoded.SourceWidth != 100 || encoded.SourceHeight != 80 {
		t.Errorf("the source size is %dx%d, want 100x80", encoded.SourceWidth, encoded.SourceHeight)
	}
}

func TestASmallImageIsNeverUpscaledForAPreviewEither(t *testing.T) {
	// The no-upscale rule is `longest <= maxEdge`, so it has to hold for every
	// bounded kind. Checking only `thumbnail` would leave a `preview` that
	// upscales to 1024 unchecked, and 1024 is the one a client would actually
	// notice.
	source := gradientPNG(t, 300, 200)
	if w, h := dimensions(t, encodeOK(t, source, KindPreview).Bytes); w != 300 || h != 200 {
		t.Fatalf("a 300x200 preview must come back 300x200, got %dx%d", w, h)
	}
}

func TestPreviewIsBoundedAt1024AndWebIsNotResizedAtAll(t *testing.T) {
	source := gradientPNG(t, 3000, 3000)
	if w, h := dimensions(t, encodeOK(t, source, KindPreview).Bytes); w != 1024 || h != 1024 {
		t.Fatalf("a 3000x3000 preview is 1024x1024, got %dx%d", w, h)
	}
	// `web` re-encodes without resizing, which is its whole point.
	if w, h := dimensions(t, encodeOK(t, source, KindWeb).Bytes); w != 3000 || h != 3000 {
		t.Fatalf("web does not resize, so a 3000x3000 source must come back 3000x3000, got %dx%d", w, h)
	}
}

// TestEveryVariantIsJpegAndTheDeclaredTypeAgreesWithTheBytes is the pair that
// keeps `content_type` from lying.
func TestEveryVariantIsJpegAndTheDeclaredTypeAgreesWithTheBytes(t *testing.T) {
	source := gradientPNG(t, 1200, 800)
	for _, kind := range AllVariantKinds() {
		encoded := encodeOK(t, source, kind)

		// The bytes really are JPEG, as sniffed from the container magic by the
		// decoder rather than by a `bytes.HasPrefix` here.
		_, format, err := image.Decode(bytes.NewReader(encoded.Bytes))
		if err != nil {
			t.Fatalf("%s: the output does not decode: %v", kind, err)
		}
		if format != "jpeg" {
			t.Errorf("%s must be JPEG, the decoder says %q", kind, format)
		}

		// And the declared content type agrees with the container, so a client
		// that trusts the `content_type` field gets the bytes it was promised. A
		// drifted pair here is a `text/html` served as an image.
		if got := kind.ContentType(); got != "image/jpeg" {
			t.Errorf("%s declares %q, the bytes are JPEG", kind, got)
		}
	}
}

// TestEveryAcceptedInputFormatDecodes is the Go replacement for the Rust
// `SUPPORTED_INPUT_FORMATS` list, and the package comment explains why it has to
// be this shape: `golang.org/x/image/webp` registers itself through a BLANK
// import, so dropping it compiles clean, links clean, passes every other test in
// this file, and fails on the first WebP a customer uploads. Only decoding real
// bytes of each format notices.
//
// The three WebP rows are the half that matters and they are the half Go cannot
// synthesise: the pure-Go decoder is decode-only, so these are checked-in files.
func TestEveryAcceptedInputFormatDecodes(t *testing.T) {
	// A real JPEG and a real GIF are built here rather than checked in, because
	// the standard library can encode both and a fixture nobody regenerates is a
	// fixture nobody trusts.
	builtJPEG := encodeAsJPEG(t, gradientPNG(t, 400, 300))
	builtGIF := encodeAsGIF(t, gradientPNG(t, 400, 300))

	cases := []struct {
		name          string
		raw           []byte
		wantFormat    string
		wantWidth     int
		wantHeight    int
		wantThumbWide int
		wantThumbTall int
		declaredType  string
	}{
		{
			name:       "png",
			raw:        gradientPNG(t, 400, 300),
			wantFormat: "png",
			wantWidth:  400, wantHeight: 300,
			wantThumbWide: 256, wantThumbTall: 192,
			declaredType: "image/png",
		},
		{
			name:       "jpeg",
			raw:        builtJPEG,
			wantFormat: "jpeg",
			wantWidth:  400, wantHeight: 300,
			wantThumbWide: 256, wantThumbTall: 192,
			declaredType: "image/jpeg",
		},
		{
			name:       "gif",
			raw:        builtGIF,
			wantFormat: "gif",
			wantWidth:  400, wantHeight: 300,
			wantThumbWide: 256, wantThumbTall: 192,
			declaredType: "image/gif",
		},
		{
			// Lossless WebP, which is VP8L. 8x8, so `thumbnail` does not resize it
			// and this row is about the DECODER rather than the resampler.
			name:       "webp (lossless, VP8L)",
			raw:        fixture(t, "solid-red-lossless.webp"),
			wantFormat: "webp",
			wantWidth:  8, wantHeight: 8,
			wantThumbWide: 8, wantThumbTall: 8,
			declaredType: "image/webp",
		},
		{
			// Lossy WebP, which is VP8 — the other bitstream, and the one a
			// photograph arrives as. `golang.org/x/image/webp` covers both and this
			// row is what says so.
			name:       "webp (lossy, VP8)",
			raw:        fixture(t, "solid-red-lossy.webp"),
			wantFormat: "webp",
			wantWidth:  8, wantHeight: 8,
			wantThumbWide: 8, wantThumbTall: 8,
			declaredType: "image/webp",
		},
		{
			// A wide lossless WebP, which is the row that proves a WebP source goes
			// through the SAME resizer as a PNG rather than being passed through.
			// 300x120 bounded at 256 is 256x102.
			name:       "webp (wide, lossless)",
			raw:        fixture(t, "wide-lossless.webp"),
			wantFormat: "webp",
			wantWidth:  300, wantHeight: 120,
			wantThumbWide: 256, wantThumbTall: 102,
			declaredType: "image/webp",
		},
	}

	// Four FORMATS, not four rows: the table carries six rows because WebP has
	// two bitstreams and one of them needs a source wide enough to be resized.
	// Counting distinct format names rather than rows is what makes that
	// legitimate — and a guard against the row count would have pushed a future
	// packet to delete a useful case rather than add one.
	//
	// The count is the assertion that matters, because a decoder registered twice
	// is not an error in Go: a row whose name were misspelled would leave the
	// table one format shorter, every remaining row still green, and the pipeline
	// quietly not accepting a format the packet says it accepts.
	distinct := map[string]int{}
	for _, tc := range cases {
		distinct[tc.wantFormat]++
	}
	if len(distinct) != 4 {
		t.Fatalf("the accepted input formats are PNG, JPEG, GIF and WebP — 4 of them, got %d: %v", len(distinct), distinct)
	}
	for _, format := range []string{"png", "jpeg", "gif", "webp"} {
		if distinct[format] == 0 {
			t.Errorf("no row decodes %q, so the blank import for it is untested", format)
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// First: the decoder for this format is REGISTERED. `image.Decode`
			// returning `ErrFormat` is what a dropped blank import looks like.
			src, format, err := image.Decode(bytes.NewReader(tc.raw))
			if err != nil {
				t.Fatalf("the source does not decode: %v — is the blank import for this format still in variants.go?", err)
			}
			if format != tc.wantFormat {
				t.Fatalf("the source is reported as %q, want %q", format, tc.wantFormat)
			}
			if b := src.Bounds(); b.Dx() != tc.wantWidth || b.Dy() != tc.wantHeight {
				t.Fatalf("the source is %dx%d, want %dx%d", b.Dx(), b.Dy(), tc.wantWidth, tc.wantHeight)
			}

			// Then: the pipeline accepts it, at the dimensions the box implies.
			encoded, err := Encode(tc.raw, KindThumbnail, tc.declaredType)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if w, h := dimensions(t, encoded.Bytes); w != tc.wantThumbWide || h != tc.wantThumbTall {
				t.Fatalf("the thumbnail is %dx%d, want %dx%d", w, h, tc.wantThumbWide, tc.wantThumbTall)
			}
			if encoded.SourceWidth != tc.wantWidth || encoded.SourceHeight != tc.wantHeight {
				t.Errorf(
					"the reported source size is %dx%d, want %dx%d",
					encoded.SourceWidth, encoded.SourceHeight, tc.wantWidth, tc.wantHeight,
				)
			}
		})
	}
}

// TestAlphaIsCompositedOverWhiteRatherThanDropped is the subtle one, and it is a
// real bug the Rust tests caught: JPEG has no alpha channel, so the easy
// conversion from RGBA DROPS the alpha, leaving a transparent red pixel red and a
// transparent black pixel black — both visible in the served image.
func TestAlphaIsCompositedOverWhiteRatherThanDropped(t *testing.T) {
	transparentRed := encodeOK(t, solidPNG(t, 255, 0, 0, 0), KindWeb)
	if pixel := middlePixel(t, transparentRed.Bytes); !allAbove(pixel, 200) {
		t.Errorf("a fully transparent pixel must matte to white, got %v", pixel)
	}

	transparentBlack := encodeOK(t, solidPNG(t, 0, 0, 0, 0), KindWeb)
	if pixel := middlePixel(t, transparentBlack.Bytes); !allAbove(pixel, 200) {
		t.Errorf("a transparent black pixel must matte to white too, got %v", pixel)
	}

	// Half-transparent: `out = src*a + 255*(1-a)` on a red pixel at a=128 gives
	// (255, 127, 127) — the red survives at full strength because it is the
	// source colour, and green and blue land halfway to the white matte. A matte
	// that ignored the source colour, or that used a different alpha, would pass
	// the two assertions above and fail this.
	half := middlePixel(t, encodeOK(t, solidPNG(t, 255, 0, 0, 128), KindWeb).Bytes)
	if diff(half[0], 255) >= 12 {
		t.Errorf("red must survive the matte, got r=%d", half[0])
	}
	if diff(half[1], 127) >= 12 || diff(half[2], 127) >= 12 {
		t.Errorf("green and blue must land halfway to white (127), got %d,%d", half[1], half[2])
	}
}

// TestAlphaIsCompositedOverWhiteThroughAResize covers the path the one above
// does not: a transparent source that also gets RESIZED.
//
// The two together are the property. Compositing correctly and then resizing
// premultiplied RGBA — or resizing and then compositing — both produce plausible
// images, and a test that only ever resizes an opaque image, or only ever
// composites an unresized one, passes with either bug in it.
func TestAlphaIsCompositedOverWhiteThroughAResize(t *testing.T) {
	// 400x400 of fully transparent, so the thumbnail is a 256x256 resize of an
	// image with no colour in it at all and the answer can only come from the
	// matte.
	img := image.NewNRGBA(image.Rect(0, 0, 400, 400))
	for y := range 400 {
		for x := range 400 {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, G: 0, B: 0, A: 0})
		}
	}
	var source bytes.Buffer
	if err := png.Encode(&source, img); err != nil {
		t.Fatalf("encoding the source PNG: %v", err)
	}

	encoded := encodeOK(t, source.Bytes(), KindThumbnail)
	if w, h := dimensions(t, encoded.Bytes); w != 256 || h != 256 {
		t.Fatalf("the thumbnail is %dx%d, want 256x256", w, h)
	}
	// Sampled at four points rather than one: a resize that composited in
	// premultiplied space darkens toward the edges, where the kernel reaches
	// further, so the centre alone would hide it.
	for _, at := range [][2]int{{4, 4}, {251, 4}, {4, 251}, {251, 251}, {128, 128}} {
		pixel := pixelAt(t, encoded.Bytes, at[0], at[1])
		if !allAbove(pixel, 200) {
			t.Errorf("at (%d,%d) a fully transparent pixel must matte to white, got %v", at[0], at[1], pixel)
		}
	}
}

// TestAnOpaqueSaturatedPixelDoesNotOverflow holds the multiply the matte does.
func TestAnOpaqueSaturatedPixelDoesNotOverflow(t *testing.T) {
	// The blend runs in a wide intermediate on purpose: a `uint8` multiply of a
	// saturated channel by a full alpha wraps, and a fully opaque red pixel comes
	// out black. This is the test that would catch that regression, and it is
	// why the assertion is on an OPAQUE pixel rather than only on transparent
	// ones.
	for _, want := range [][3]uint8{
		{255, 0, 0},
		{0, 255, 0},
		{0, 0, 255},
		{255, 255, 255},
		{0, 0, 0},
	} {
		encoded := encodeOK(t, solidPNG(t, want[0], want[1], want[2], 255), KindWeb)
		got := middlePixel(t, encoded.Bytes)
		// JPEG is lossy, so this is a tolerance rather than an equality.
		if diff(got[0], want[0]) >= 12 || diff(got[1], want[1]) >= 12 || diff(got[2], want[2]) >= 12 {
			t.Errorf("opaque %v came out as %v", want, got)
		}
	}
}

// noisyPNG builds a PNG whose pixels do not compress — deterministic noise, not
// a random source, so the test is the same on every run.
//
// It exists for the size comparison below, and the reason it is needed is worth
// recording: a smooth synthetic gradient is the WORST possible input for that
// claim. Deflate encodes a ramp in a few kilobytes while a DCT codec spends a
// quantisation table's worth of bytes on the same ramp's discontinuities, so a
// gradient inverts the comparison and the test would go red for a reason that has
// nothing to do with the pipeline. The claim is about PHOTOGRAPHS — which is what
// an asset is, per `domain.AssetKind` — so the fixture has to be photo-shaped
// enough for the claim to be a claim about JPEG rather than about the fixture.
func noisyPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	// A linear congruential generator, fixed seed. `math/rand` is not used because
	// its output is a compatibility promise between Go releases and this is a
	// byte-for-byte size comparison.
	state := uint32(0x2545f491)
	next := func() uint8 {
		state = state*1664525 + 1013904223
		return uint8(state >> 24)
	}
	for y := range height {
		for x := range width {
			img.SetNRGBA(x, y, color.NRGBA{R: next(), G: next(), B: next(), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("building a %dx%d noisy PNG: %v", width, height, err)
	}
	return buf.Bytes()
}

// TestAReEncodeIsSmallerThanThePngItCameFrom holds the claim that makes JPEG the
// output format worth having.
//
// At `preview`, with the source at the SAME dimensions as the box, so the
// comparison measures the codec rather than the shrink: a 1024x768 JPEG against
// a 1200x800 PNG is smaller for the boring reason that it has fewer pixels,
// which is a true statement about nothing. Equal dimensions is the only version
// of this claim that is a claim about JPEG.
//
// The ratio is asserted as well as the direction, so a pipeline that started
// emitting 16x16 thumbnails would still pass the direction check.
func TestAReEncodeIsSmallerThanThePngItCameFrom(t *testing.T) {
	source := noisyPNG(t, 1024, 768)
	preview := encodeOK(t, source, KindPreview)
	if w, h := dimensions(t, preview.Bytes); w != 1024 || h != 768 {
		t.Fatalf("the preview is %dx%d, want the source's 1024x768 — the comparison below is only about the codec", w, h)
	}
	if len(preview.Bytes) >= len(source) {
		t.Fatalf(
			"a 1024x768 JPEG must be smaller than the 1024x768 PNG it came from (%d vs %d bytes)",
			len(preview.Bytes), len(source),
		)
	}
	if ratio := float64(len(preview.Bytes)) / float64(len(source)); ratio > 0.5 {
		t.Fatalf(
			"JPEG at quality %d should be well under half the PNG's bytes, got a ratio of %.2f (%d vs %d)",
			JPEGQuality, ratio, len(preview.Bytes), len(source),
		)
	}
}

func TestAOneByOneImageDoesNotScaleToZero(t *testing.T) {
	// The rounding in the scale calculation can produce 0 for a very extreme
	// aspect ratio. The `max(…, 1)` in fitInside is what stops a variant with
	// zero width, which the encoder would reject with an opaque error.
	source := gradientPNG(t, 1, 1)
	encoded := encodeOK(t, source, KindPreview)
	w, h := dimensions(t, encoded.Bytes)
	if w < 1 || h < 1 {
		t.Fatalf("a 1x1 image must stay at least 1x1, got %dx%d", w, h)
	}
}

// TestFitInsideHoldsTheRustArithmetic pins the sizing function on its own.
//
// The rounding is `float32` and rounded half away from zero, because the Rust
// version's is `f32` and `.round()` and the two implementations are supposed to
// agree on the dimensions for the same input. A `float64` here would agree on
// every row of this table except possibly the last, which is the point of having
// the last row.
func TestFitInsideHoldsTheRustArithmetic(t *testing.T) {
	cases := []struct {
		name                  string
		width, height, maxEge int
		wantWidth, wantHeight int
	}{
		{"a panorama fits by width", 4000, 1000, 256, 256, 64},
		{"a tall image fits by height", 500, 2000, 256, 64, 256},
		{"preview at 1024", 3000, 3000, 1024, 1024, 1024},
		{"an exact fit is not resized by this function", 256, 256, 256, 256, 256},
		// 3x1000 bounded at 256: height becomes 256 and the width rounds to 1.
		// Without the floor this is 0x256, and `image.NewNRGBA` with a zero
		// dimension is a one-pixel image that `jpeg.Encode` then rejects.
		{"an extreme aspect ratio floors at 1", 3, 1000, 256, 1, 256},
		// The rounding tie. 0.5 rounds AWAY from zero in both languages, so a
		// `math.Floor`-based port would give 128 here and disagree with the Rust
		// binary on every such image.
		{"a half rounds away from zero", 513, 100, 256, 256, 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, h := fitInside(tc.width, tc.height, uint32(tc.maxEge))
			if w != tc.wantWidth || h != tc.wantHeight {
				t.Fatalf("fitInside(%d, %d, %d) = %dx%d, want %dx%d",
					tc.width, tc.height, tc.maxEge, w, h, tc.wantWidth, tc.wantHeight)
			}
		})
	}
}

// TestMatteOverWhiteHoldsTheFormula holds the blend on its own, including the
// rounding that decides every half-alpha pixel.
func TestMatteOverWhiteHoldsTheFormula(t *testing.T) {
	// out = src*a + 255*(1-a), with +127 so it rounds to nearest rather than
	// always truncating down. Written out here rather than derived from the
	// function under test, because a table built from `matteOverWhite` would
	// agree with any matte at all.
	cases := []struct {
		channel, alpha uint8
		want           uint8
	}{
		{255, 0, 255}, // any colour, fully transparent: the white matte, exactly
		{0, 0, 255},   // and the same for black, which is the pixel `to_rgb8` kept
		{255, 255, 255},
		{0, 255, 0},
		{255, 128, 255}, // a saturated channel at half alpha stays saturated
		{0, 128, 127},   // and an empty one lands halfway to white
		{255, 1, 255},
		{128, 128, 191},
		// The last two are the rows that make the `+127` load-bearing. Truncating
		// instead of rounding gives 127 for the first and 252 for the second, so
		// dropping the rounding term shifts every half-alpha pixel by one and this
		// table is what notices.
		{1, 128, 128},
		{200, 10, 253},
	}
	for _, tc := range cases {
		if got := matteOverWhite(tc.channel, tc.alpha); got != tc.want {
			t.Errorf("matteOverWhite(%d, %d) = %d, want %d", tc.channel, tc.alpha, got, tc.want)
		}
	}
}

// TestUndecodableBytesAre422NamingTheThingTheClientControls is the status the
// packet is about: a decode failure is the CALLER's 422, never our 500.
func TestUndecodableBytesAre422NamingTheThingTheClientControls(t *testing.T) {
	// The bytes passed a checksum check, so they are what the client sent.
	// Declaring image/png and sending a PDF is a client error worth a 422 about
	// content_type, not a 500.
	notAnImage := []byte("%PDF-1.7\nnot an image at all\n%%EOF")

	err := encodeErr(t, notAnImage, "image/png")
	assertClientField(t, err, StatusUnprocessable, "content_type")

	// When the client did not claim an image, the field is the asset.
	err = encodeErr(t, notAnImage, "application/pdf")
	assertClientField(t, err, StatusUnprocessable, "asset")
}

// TestATruncatedImageIsAlsoAClientError covers the second decode failure, which
// is not the same one: the container magic is right and the pixels are not all
// there. That is still the client's declared content type disagreeing with the
// bytes, so it is still a 422 rather than a 500.
func TestATruncatedImageIsAlsoAClientError(t *testing.T) {
	complete := gradientPNG(t, 64, 64)
	truncated := complete[:len(complete)/2]

	err := encodeErr(t, truncated, "image/png")
	assertClientField(t, err, StatusUnprocessable, "content_type")
}

func TestEmptyBytesAre422NotAPanic(t *testing.T) {
	err := encodeErr(t, nil, "image/png")
	assertClientField(t, err, StatusUnprocessable, "content_type")

	err = encodeErr(t, []byte{}, "image/png")
	assertClientField(t, err, StatusUnprocessable, "content_type")
}

// TestAnUnknownVariantKindIs422NamingTheKind is the vocabulary's other client
// error, and it is here rather than in a routing test because `ParseKind` is what
// a route calls.
func TestAnUnknownVariantKindIs422NamingTheKind(t *testing.T) {
	kind, err := ParseKind("thumbnall")
	if kind.Valid() {
		t.Errorf("ParseKind accepted a typo and returned %q", kind)
	}
	assertClientField(t, err, StatusUnprocessable, "kind")

	// And a kind that IS one of the three round trips.
	for _, want := range AllVariantKinds() {
		got, err := ParseKind(want.String())
		if err != nil {
			t.Errorf("ParseKind(%q): %v", want, err)
			continue
		}
		if got != want {
			t.Errorf("ParseKind(%q) = %q", want, got)
		}
	}
}

// TestTheContentTypesOfTheThreeKindsAreOneString pins the pair that must not
// drift. Three kinds, one output format: that is the whole reason
// `ContentType` takes a receiver.
func TestTheContentTypesOfTheThreeKindsAreOneString(t *testing.T) {
	kinds := AllVariantKinds()
	if len(kinds) != 3 {
		t.Fatalf("there are three variant kinds, got %d", len(kinds))
	}
	for _, kind := range kinds {
		if kind.ContentType() != "image/jpeg" {
			t.Errorf("%s declares %q", kind, kind.ContentType())
		}
	}
	// `web` has no bound, and that is what keeps it out of the resizer. Asserted
	// here because a `MaxEdge` that started returning 1024 would silently turn
	// `web` into a second `preview`.
	if got := KindWeb.MaxEdge(); uint64(got) != uint64(^uint32(0)) {
		t.Errorf("web's max edge is %d, want no bound at all", got)
	}
	if got := KindThumbnail.MaxEdge(); got != 256 {
		t.Errorf("a thumbnail is bounded at 256, got %d", got)
	}
	if got := KindPreview.MaxEdge(); got != 1024 {
		t.Errorf("a preview is bounded at 1024, got %d", got)
	}
}

// TestTheQualityIsTheNamedConstant asserts [JPEGQuality] against REAL OUTPUT
// rather than against itself.
//
// The table's 82 is a LITERAL. That is the whole design: a table built out of
// `JPEGQuality` would agree with any value the constant took, so moving the
// constant to 85 and re-running would stay green and the quality would have
// changed without anybody deciding it. Written as literals, moving the constant
// makes the first row disagree.
//
// 79 and 90 are there so the assertion is not vacuous in the other direction:
// they prove the pipeline's bytes are sensitive to the quality at all, which a
// comparison against a single reference could not.
func TestTheQualityIsTheNamedConstant(t *testing.T) {
	// One source, encoded three ways at the reference quality and its neighbours.
	// `web` so the comparison is against the same pixels the pipeline produced —
	// a JPEG round trip of a resize is not byte-comparable to a resize of a PNG.
	source := solidPNG(t, 200, 120, 60, 255)
	encoded := encodeOK(t, source, KindWeb)

	pipeline := normalise(decodeImage(t, encoded.Bytes))
	reference := func(quality int) []byte {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, opaqueOf(pipeline), &jpeg.Options{Quality: quality}); err != nil {
			t.Fatalf("reference encode at quality %d: %v", quality, err)
		}
		return buf.Bytes()
	}

	at82 := reference(82)
	if !bytes.Equal(encoded.Bytes, at82) {
		t.Errorf(
			"the pipeline's bytes are not what quality 82 produces (%d vs %d bytes) — JPEGQuality is %d",
			len(encoded.Bytes), len(at82), JPEGQuality,
		)
	}
	if bytes.Equal(encoded.Bytes, reference(79)) {
		t.Errorf("the output is byte-identical to quality 79, so this assertion is not measuring quality at all")
	}
	if bytes.Equal(encoded.Bytes, reference(90)) {
		t.Errorf("the output is byte-identical to quality 90, so this assertion is not measuring quality at all")
	}
}

// --- helpers -----------------------------------------------------------------

// encodeAsJPEG round-trips `raw` through the standard library's JPEG encoder, so
// the JPEG row of the format table is fed a genuinely JPEG-encoded file rather
// than a PNG wearing a `.jpg` name.
func encodeAsJPEG(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, decodeImage(t, raw), &jpeg.Options{Quality: JPEGQuality}); err != nil {
		t.Fatalf("building a JPEG fixture: %v", err)
	}
	return buf.Bytes()
}

// encodeAsGIF round-trips `raw` through the standard library's GIF encoder.
func encodeAsGIF(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := gif.Encode(&buf, decodeImage(t, raw), nil); err != nil {
		t.Fatalf("building a GIF fixture: %v", err)
	}
	return buf.Bytes()
}

func decodeImage(t *testing.T, raw []byte) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return img
}

// opaqueOf returns `src` as an opaque RGBA image, which is what `jpeg.Encode`
// is handed in production: the matte has already run and there is no alpha left.
func opaqueOf(src *image.NRGBA) *image.RGBA {
	bounds := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			pixel := src.NRGBAAt(x, y)
			dst.SetRGBA(x, y, color.RGBA{R: matteOverWhite(pixel.R, pixel.A), G: matteOverWhite(pixel.G, pixel.A), B: matteOverWhite(pixel.B, pixel.A), A: 255})
		}
	}
	return dst
}

// pixelAt decodes `raw` and returns the pixel at (x, y) as RGB.
func pixelAt(t *testing.T, raw []byte, x, y int) [3]uint8 {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decoding the output: %v", err)
	}
	r, g, b, _ := img.At(x, y).RGBA()
	return [3]uint8{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8)}
}

// encodeErr calls Encode with bytes that must not decode, and fails the test if
// they did.
func encodeErr(t *testing.T, raw []byte, declaredContentType string) error {
	t.Helper()
	encoded, err := Encode(raw, KindThumbnail, declaredContentType)
	if err == nil {
		t.Fatalf(
			"these bytes decoded (%dx%d), so the test that depends on them failing proves nothing",
			encoded.Width, encoded.Height,
		)
	}
	if errors.Is(err, err) && err.Error() == "" {
		t.Fatalf("the error carries no message, so a caller cannot log it")
	}
	return err
}

// assertClientField asserts that `err` is a client error carrying `status` and
// naming `field`.
//
// `status` is a parameter rather than a hardcoded 422 so that the two
// constructors' numbers are asserted against each other: a change to
// `undecodable` that started returning `internal` fails these rows, and so does
// one that started returning the right status with the wrong field.
func assertClientField(t *testing.T, err error, status int, field string) {
	t.Helper()
	fault, ok := asError(err)
	if !ok {
		t.Fatalf("the error is %T, not a deliberate *variants.Error: %v", err, err)
	}
	if fault.Status != status {
		t.Errorf("the status is %d, want %d — %s", fault.Status, status, fault.Detail)
	}
	if fault.Status == StatusInternal {
		t.Errorf("a decode failure must never be a %d: the bytes were checksum-verified, so it is the caller's mistake", StatusInternal)
	}
	if len(fault.Fields) != 1 {
		t.Fatalf("the problem names %d fields, want exactly 1: %v", len(fault.Fields), fault.Fields)
	}
	if fault.Fields[0].Field != field {
		t.Errorf("the problem names %q, want %q", fault.Fields[0].Field, field)
	}
	if fault.Fields[0].Code == "" {
		t.Errorf("the field error carries no code: %v", fault.Fields[0])
	}
}

func allAbove(pixel [3]uint8, floor uint8) bool {
	for _, channel := range pixel {
		if channel <= floor {
			return false
		}
	}
	return true
}

func diff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}
