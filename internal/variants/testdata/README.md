# testdata — the three files Go cannot build for itself

Every other image in this package's tests is encoded by the test, with the
standard library, from a pattern the test also wrote. These three cannot be, and
the reason is the same fact the package comment gives as the reason every variant
is JPEG:

**Go has no first-party WebP encoder.** `golang.org/x/image/webp` is a pure-Go
*decoder*; the proposal for an encoder (golang/go#45121) is open and unassigned.
So a WebP fixture has to be produced by something outside this repository.

They were made with libwebp's `cwebp`, and the commands are here so a reader can
regenerate them rather than trusting the bytes:

```sh
# solid-red-lossless.webp — 8x8, fully opaque red, lossless (VP8L)
cwebp -lossless -quiet -exact -o solid-red-lossless.webp solid-red.png

# solid-red-lossy.webp — 8x8, fully opaque red, lossy (VP8), quality 82
cwebp -quiet -q 82 -exact -o solid-red-lossy.webp solid-red.png

# wide-lossless.webp — 300x120, opaque blue, lossless (VP8L)
cwebp -lossless -quiet -exact -o wide-lossless.webp wide.png
```

where `solid-red.png` is 8x8 of `#ff0000` and `wide.png` is 300x120 of `#0080ff`,
both as 8-bit RGBA.

`cwebp` is a **build-time** tool for regenerating fixtures. It is not a
dependency: nothing in this repository links libwebp, `bin/prime` does not run
`cwebp`, and the tests read these files as opaque bytes. That distinction is the
whole reason for using `golang.org/x/image/webp` rather than
`chai2010/webp` or `gen2brain/webp` — a cgo decoder would cost darkroom its
static binary for no gain, since darkroom only ever *decodes* WebP.

**Both bitstreams are here on purpose.** `solid-red-lossless.webp` is VP8L and
`solid-red-lossy.webp` is VP8; the decoder handles both and a fixture holding only
one would leave the other half of the blank import untested.
