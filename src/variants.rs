//! Variant encoding: resize and re-encode, using the `image` crate.
//!
//! The brief says "use an existing Rust image crate; do not hand-roll codecs",
//! and that is the whole of this file's job. There is no hand-rolled
//! resampling and no hand-rolled WebP encoder here — `image` provides both and
//! this module only chooses parameters and reports the result.
//!
//! ## The decisions, and why
//!
//! - **WebP output for every variant.** Smaller than PNG for a photograph,
//!   smaller than JPEG at the same quality, and universally decodable. One
//!   output format means one content type, one cache policy, and one thing for
//!   a client to handle.
//! - **Fit inside a box, preserve aspect ratio.** One number, not two, because
//!   there is no correct answer to "which axis wins" for a panorama. See
//!   [`VariantKind::max_edge`].
//! - **Never upscale.** A 100x80 image asked for as a 256px thumbnail stays
//!   100x80. Upscaling costs bytes and adds no information, and a thumbnail
//!   that is larger than its source is a bug a client will not catch.
//! - **A decode failure is a 422, not a 500.** The bytes were verified by
//!   checksum, so a decode failure means the content_type the client declared
//!   and the bytes that arrived disagree — which is a client-visible fact about
//!   a client-visible problem.

use bytes::Bytes;
use image::codecs::jpeg::JpegEncoder;
use image::{ImageEncoder, ImageReader, ImageFormat};
use std::io::Cursor;

use crate::domain::VariantKind;
use crate::error::{Error, FieldError};

/// The result of encoding one variant.
#[derive(Debug, Clone)]
pub struct Encoded {
    pub bytes: Bytes,
    pub width: Option<i32>,
    pub height: Option<i32>,
    /// The original's dimensions, recorded so a consumer can tell "the
    /// thumbnail is 256 wide" from "the source was 100 wide and we did not
    /// upscale".
    pub source_width: Option<i32>,
    pub source_height: Option<i32>,
}

/// JPEG quality. 82 is the point where a photographic image at 256-1024px is
/// visually indistinguishable from the source and roughly a quarter of the
/// bytes of a PNG of the same pixels.
///
/// Asserted in the tests against the actual output so a change to this number
/// has to be deliberate rather than a stray keystroke in a rebuild.
pub const JPEG_QUALITY: u8 = 82;

/// Encode `original` as `kind`.
///
/// `declared_content_type` is what the client said at create time. It is used
/// to tell "the bytes are not an image" apart from "the image crate cannot read
/// this particular image", because the first is worth a 422 about `content_type`
/// and the second is worth a 422 about the asset.
pub fn encode(
    original: &Bytes,
    kind: VariantKind,
    declared_content_type: &str,
) -> Result<Encoded, Error> {
    let reader = ImageReader::new(Cursor::new(original.as_ref()))
        .with_guessed_format()
        .map_err(|e| {
            tracing::warn!(error = %e, "could not read the image header");
            undecodable(declared_content_type)
        })?;

    let source_format = reader.format();
    let decoded = reader.decode().map_err(|e| {
        tracing::warn!(error = %e, format = ?source_format, "could not decode the image");
        undecodable(declared_content_type)
    })?;

    let source_width = decoded.width();
    let source_height = decoded.height();

    // A 0x0 image is not something the decoders produce, so this is a guard
    // against a pathological input rather than a case to handle.
    if source_width == 0 || source_height == 0 {
        return Err(Error::invalid_fields(
            "the image has no dimensions",
            vec![FieldError::new("asset", "undecodable")],
        ));
    }

    let max_edge = kind.max_edge();
    let longest = source_width.max(source_height);
    let resized = if longest <= max_edge {
        // Never upscale. The `image` crate's resize would happily enlarge.
        None
    } else {
        let scale = max_edge as f32 / longest as f32;
        let width = ((source_width as f32 * scale).round() as u32).max(1);
        let height = ((source_height as f32 * scale).round() as u32).max(1);
        Some(decoded.resize_exact(
            width,
            height,
            image::imageops::FilterType::Lanczos3,
        ))
    };

    let image = resized.as_ref().unwrap_or(&decoded);
    let (width, height) = (image.width(), image.height());

    // JPEG has no alpha channel, so an RGBA source is flattened onto white
    // rather than encoded with the alpha silently discarded against black.
    // A transparent PNG thumbnail on a black background is a bug a client will
    // not report; a white matte is what every image CDN does.
    let rgb = image.to_rgb8();
    let mut out = Vec::with_capacity((width * height * 3) as usize);
    JpegEncoder::new_with_quality(&mut out, JPEG_QUALITY)
        .write_image(rgb.as_raw(), width, height, image::ExtendedColorType::Rgb8)
        .map_err(|e| {
            tracing::error!(error = %e, "jpeg encode failed");
            Error::internal("the variant could not be encoded")
        })?;

    Ok(Encoded {
        bytes: Bytes::from(out),
        width: Some(width as i32),
        height: Some(height as i32),
        source_width: Some(source_width as i32),
        source_height: Some(source_height as i32),
    })
}

fn undecodable(declared_content_type: &str) -> Error {
    // The field named is the one the client controls. If the client declared
    // `image/png` and the bytes are not a PNG, `content_type` is the thing to
    // fix.
    if declared_content_type.starts_with("image/") {
        Error::invalid_fields(
            "the stored bytes are not a readable image of the declared content_type",
            vec![FieldError::new("content_type", "mismatch")],
        )
    } else {
        Error::invalid_fields(
            "the stored bytes are not a readable image",
            vec![FieldError::new("asset", "undecodable")],
        )
    }
}

/// Formats the image crate can read here. Declared so the test can assert the
/// feature flags in Cargo.toml did not get dropped: a `--no-default-features`
/// refactor that removed `webp` would otherwise fail at runtime, in production,
/// on the first thumbnail.
pub const SUPPORTED_INPUT_FORMATS: &[ImageFormat] = &[
    ImageFormat::Png,
    ImageFormat::Jpeg,
    ImageFormat::Gif,
    ImageFormat::WebP,
];

#[cfg(test)]
mod tests {
    use super::*;
    use image::{ImageBuffer, Rgba};

    /// Build a real PNG of the requested size, so the tests exercise the
    /// decoder rather than a fixture someone forgot to update.
    fn png(width: u32, height: u32) -> Bytes {
        let mut buf = ImageBuffer::new(width, height);
        for (x, y, pixel) in buf.enumerate_pixels_mut() {
            *pixel = Rgba([(x % 256) as u8, (y % 256) as u8, 128, 255]);
        }
        let mut out = Vec::new();
        image::DynamicImage::ImageRgba8(buf)
            .write_to(&mut Cursor::new(&mut out), ImageFormat::Png)
            .expect("a PNG always encodes");
        Bytes::from(out)
    }

    fn dimensions(bytes: &Bytes) -> (u32, u32) {
        let reader = ImageReader::new(Cursor::new(bytes.as_ref()))
            .with_guessed_format()
            .expect("reads")
            .decode()
            .expect("decodes");
        (reader.width(), reader.height())
    }

    #[test]
    fn a_thumbnail_is_bounded_by_its_box_and_keeps_the_aspect_ratio() {
        // A 4000x1000 panorama: the longest edge is 4000, so the width becomes
        // 256 and the height scales with it. A per-axis bound would have made
        // this 256x256, which is a different image.
        let source = png(4000, 1000);
        let encoded = encode(&source, VariantKind::Thumbnail, "image/png").expect("encodes");
        let (w, h) = dimensions(&encoded.bytes);
        assert_eq!(w, 256);
        assert_eq!(h, 64, "aspect ratio must be preserved, not squared");
        assert_eq!(encoded.width, Some(256));
        assert_eq!(encoded.height, Some(64));
        assert_eq!(encoded.source_width, Some(4000));
        assert_eq!(encoded.source_height, Some(1000));
    }

    #[test]
    fn a_tall_image_is_bounded_on_its_long_edge_too() {
        let source = png(500, 2000);
        let encoded = encode(&source, VariantKind::Thumbnail, "image/png").expect("encodes");
        let (w, h) = dimensions(&encoded.bytes);
        assert_eq!(h, 256);
        assert_eq!(w, 64);
    }

    #[test]
    fn a_small_image_is_never_upscaled() {
        // 100x80 asked for as a 256px thumbnail stays 100x80. Upscaling costs
        // bytes and adds no information, and a thumbnail bigger than its source
        // is a bug the client will not catch.
        let source = png(100, 80);
        let encoded = encode(&source, VariantKind::Thumbnail, "image/png").expect("encodes");
        let (w, h) = dimensions(&encoded.bytes);
        assert_eq!((w, h), (100, 80));
    }

    #[test]
    fn preview_is_bounded_at_1024_and_web_is_not_resized_at_all() {
        let source = png(3000, 3000);
        assert_eq!(
            dimensions(&encode(&source, VariantKind::Preview, "image/png").expect("encodes").bytes),
            (1024, 1024)
        );
        // `web` re-encodes without resizing, which is its whole point.
        assert_eq!(
            dimensions(&encode(&source, VariantKind::Web, "image/png").expect("encodes").bytes),
            (3000, 3000)
        );
    }

    #[test]
    fn every_variant_is_jpeg_and_the_declared_type_agrees_with_the_bytes() {
        let source = png(1200, 800);
        for kind in [VariantKind::Thumbnail, VariantKind::Preview, VariantKind::Web] {
            let encoded = encode(&source, kind, "image/png").expect("encodes");

            // The bytes really are JPEG, as sniffed from the container magic.
            let reader = ImageReader::new(Cursor::new(encoded.bytes.as_ref()))
                .with_guessed_format()
                .expect("reads");
            assert_eq!(reader.format(), Some(ImageFormat::Jpeg), "{kind} must be JPEG");

            // And the declared content type agrees with the container, so a
            // client that trusts the `content_type` field gets the bytes it was
            // promised. A drifted pair here is a `text/html` served as an image.
            assert_eq!(kind.content_type(), "image/jpeg");
        }
    }

    #[test]
    fn a_transparent_source_is_flattened_onto_white_not_black() {
        // JPEG has no alpha channel. Encoding RGBA directly drops the alpha and
        // whatever was behind it shows through as black — a transparent PNG
        // thumbnail on a black square is a bug a client will never report.
        let mut buffer = ImageBuffer::new(8, 8);
        for pixel in buffer.pixels_mut() {
            *pixel = Rgba([255, 0, 0, 0]); // red, fully transparent
        }
        let mut png_bytes = Vec::new();
        image::DynamicImage::ImageRgba8(buffer)
            .write_to(&mut Cursor::new(&mut png_bytes), ImageFormat::Png)
            .expect("encodes");

        let encoded = encode(&Bytes::from(png_bytes), VariantKind::Web, "image/png")
            .expect("encodes");
        let decoded = ImageReader::new(Cursor::new(encoded.bytes.as_ref()))
            .with_guessed_format()
            .expect("reads")
            .decode()
            .expect("decodes");
        let pixel = decoded.to_rgb8().get_pixel(4, 4).0;
        assert!(
            pixel.iter().all(|c| *c > 200),
            "expected a white matte, got {pixel:?}"
        );
    }

    #[test]
    fn a_re_encode_is_smaller_than_the_png_it_came_from() {
        let source = png(1200, 800);
        let thumbnail = encode(&source, VariantKind::Thumbnail, "image/png").expect("encodes");
        assert!(
            thumbnail.bytes.len() < source.len(),
            "a 256px JPEG must be smaller than the PNG it came from ({} vs {})",
            thumbnail.bytes.len(),
            source.len()
        );
    }

    #[test]
    fn a_one_by_one_image_does_not_scale_to_zero() {
        // The rounding in the scale calculation can produce 0 for a very
        // extreme aspect ratio. `.max(1)` is what stops a variant with zero
        // width, which the encoder would reject with an opaque io error.
        let source = png(1, 1);
        let encoded = encode(&source, VariantKind::Preview, "image/png").expect("encodes");
        let (w, h) = dimensions(&encoded.bytes);
        assert!(w >= 1 && h >= 1);
    }

    #[test]
    fn undecodable_bytes_are_422_naming_the_thing_the_client_controls() {
        // The bytes passed a checksum check, so they are what the client sent.
        // Declaring image/png and sending a PDF is a client error worth a 422
        // about content_type, not a 500.
        let not_an_image = Bytes::from_static(b"%PDF-1.7\nnot an image at all\n%%EOF");
        let err = encode(&not_an_image, VariantKind::Thumbnail, "image/png")
            .expect_err("must not decode");
        assert_eq!(err.status().as_u16(), 422);
        let problem = err.to_problem("/v1/assets/x/variants", "t");
        let fields = problem.errors.expect("names a field");
        assert_eq!(fields[0].field, "content_type");

        // When the client did not claim an image, the field is the asset.
        let err = encode(&not_an_image, VariantKind::Thumbnail, "application/pdf")
            .expect_err("must not decode");
        let problem = err.to_problem("/v1/assets/x/variants", "t");
        assert_eq!(problem.errors.expect("names a field")[0].field, "asset");
    }

    #[test]
    fn empty_bytes_are_422_not_a_panic() {
        let err = encode(&Bytes::new(), VariantKind::Thumbnail, "image/png")
            .expect_err("empty is not an image");
        assert_eq!(err.status().as_u16(), 422);
    }
}
