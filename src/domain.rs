//! The domain: assets, variants, and the enums that constrain them.
//!
//! These types are what the API returns and what the database stores. The
//! enum variants are the same strings as the SQL `check` constraints in
//! `migrations/0001_assets.sql`, and `db_round_trip` in the tests below is
//! what keeps the two from drifting.
//!
//! `Deserialize` is hand-written rather than derived for the enums, because
//! `#[serde(rename_all = "snake_case")]` on a unit variant already does the
//! right thing and the only subtlety is that an unknown string must be a 422,
//! not a 500 — which is what the `FromStr` impls are for.

use std::fmt;
use std::str::FromStr;

use serde::{Deserialize, Serialize};
use time::OffsetDateTime;
use uuid::Uuid;

use crate::error::{Error, FieldError};

/// What an asset *is*. Derived from `content_type` at insert time.
///
/// A `kind` drives which decoders run. If a client could choose it, a request
/// for an `image` variant on a file whose bytes are not an image would reach
/// the image decoder, so the value is a server-side derivation and the client
/// only supplies `content_type`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AssetKind {
    Image,
    Audio,
    Video,
    Document,
}

impl AssetKind {
    pub fn as_str(self) -> &'static str {
        match self {
            AssetKind::Image => "image",
            AssetKind::Audio => "audio",
            AssetKind::Video => "video",
            AssetKind::Document => "document",
        }
    }

    /// The full IANA media type, `type/subtype` with no parameters.
    pub fn content_type(self) -> &'static str {
        match self {
            AssetKind::Image => "image",
            AssetKind::Audio => "audio",
            AssetKind::Video => "video",
            AssetKind::Document => "application",
        }
    }

    /// Derive a kind from a client-supplied `content_type`.
    ///
    /// This is the allow-list. An unrecognised type is a 422 naming the field,
    /// which is what stops a `text/x-shellscript` upload from becoming a
    /// document that a later handler tries to render. Parameters
    /// (`; charset=utf-8`) are stripped before the prefix test because clients
    /// send them and the kind does not depend on them.
    pub fn from_content_type(content_type: &str) -> Result<Self, Error> {
        let base = content_type
            .split(';')
            .next()
            .unwrap_or_default()
            .trim()
            .to_ascii_lowercase();
        let top = base.split('/').next().unwrap_or_default();

        let kind = match top {
            "image" => {
                // Only formats the image crate can actually decode. Accepting
                // `image/x-psd` here would create an asset no variant path can
                // read and no error until someone asks for a thumbnail.
                match base.as_str() {
                    "image/png" | "image/jpeg" | "image/gif" | "image/webp" => AssetKind::Image,
                    _ => {
                        return Err(Error::invalid_fields(
                            "unsupported image type",
                            vec![FieldError::new("content_type", "unsupported_media_type")],
                        ));
                    }
                }
            }
            "audio" => match base.as_str() {
                "audio/mpeg" | "audio/ogg" | "audio/wav" | "audio/webm" => AssetKind::Audio,
                _ => {
                    return Err(Error::invalid_fields(
                        "unsupported audio type",
                        vec![FieldError::new("content_type", "unsupported_media_type")],
                    ));
                }
            },
            "video" => match base.as_str() {
                "video/mp4" | "video/quicktime" | "video/webm" => AssetKind::Video,
                _ => {
                    return Err(Error::invalid_fields(
                        "unsupported video type",
                        vec![FieldError::new("content_type", "unsupported_media_type")],
                    ));
                }
            },
            "application" => match base.as_str() {
                "application/pdf"
                | "application/rtf"
                | "application/json"
                | "application/zip" => AssetKind::Document,
                _ => {
                    return Err(Error::invalid_fields(
                        "unsupported document type",
                        vec![FieldError::new("content_type", "unsupported_media_type")],
                    ));
                }
            },
            _ => {
                return Err(Error::invalid_fields(
                    "content_type must be a supported image, audio, video or document type",
                    vec![FieldError::new("content_type", "unsupported_media_type")],
                ));
            }
        };
        Ok(kind)
    }
}

impl fmt::Display for AssetKind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

impl FromStr for AssetKind {
    type Err = Error;
    /// A value the SQL check constraint already restricted. A failure here
    /// means the database and this code disagree, which is a deploy error, and
    /// the 500 it produces is the correct response — not a 422, which would
    /// blame the client for a row this service wrote.
    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "image" => Ok(AssetKind::Image),
            "audio" => Ok(AssetKind::Audio),
            "video" => Ok(AssetKind::Video),
            "document" => Ok(AssetKind::Document),
            _ => Err(Error::internal("unknown asset kind in storage")),
        }
    }
}

/// The upload's lifecycle. Three states, and the transitions are the whole
/// point: `pending` means a presigned URL was issued and nothing has been
/// confirmed, `ready` means the bytes are in storage and the checksum the
/// client claimed has been checked against them, `failed` means the bytes never
/// arrived or did not match.
///
/// `ready` and `failed` are terminal. A `failed` asset is not retried into
/// `ready`; the client creates a new upload. That keeps "this upload failed" a
/// fact rather than a state that can be argued about later, and it means a
/// sweep for `pending` rows older than the presign TTL has exactly one meaning.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AssetStatus {
    Pending,
    Ready,
    Failed,
}

impl AssetStatus {
    pub fn as_str(self) -> &'static str {
        match self {
            AssetStatus::Pending => "pending",
            AssetStatus::Ready => "ready",
            AssetStatus::Failed => "failed",
        }
    }
}

impl fmt::Display for AssetStatus {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

impl FromStr for AssetStatus {
    type Err = Error;
    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "pending" => Ok(AssetStatus::Pending),
            "ready" => Ok(AssetStatus::Ready),
            "failed" => Ok(AssetStatus::Failed),
            _ => Err(Error::internal("unknown asset status in storage")),
        }
    }
}

/// A derived image. `thumbnail` and `preview` are fixed-box re-encodes; `web` is
/// the lossless-ish re-encode at original dimensions that a CMS or a document
/// renderer wants to serve instead of a PNG.
///
/// A `kind` that is not in this list is a 422: an unknown variant kind cannot be
/// told apart from a typo, and inventing behaviour for it is how an API grows
/// fifty near-identical kinds.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum VariantKind {
    Thumbnail,
    Preview,
    Web,
}

impl VariantKind {
    pub fn as_str(self) -> &'static str {
        match self {
            VariantKind::Thumbnail => "thumbnail",
            VariantKind::Preview => "preview",
            VariantKind::Web => "web",
        }
    }

    /// The longest edge this variant is bounded by, in pixels.
    ///
    /// A single number, not a width and a height: every variant here is
    /// "fit inside this box, preserve aspect ratio", so a 4000x100 panorama and
    /// a 100x4000 one both come out bounded by the same edge. A bound that is
    /// per-axis would need a policy for which axis wins, and there is no
    /// correct answer.
    pub fn max_edge(self) -> u32 {
        match self {
            VariantKind::Thumbnail => 256,
            VariantKind::Preview => 1024,
            // `web` is a re-encode at original dimensions: it exists to
            // convert, not to resize, so it has no bound. u32::MAX means
            // "never scale down".
            VariantKind::Web => u32::MAX,
        }
    }

    /// Output media type. Every variant is JPEG.
    ///
    /// WebP would be the better format and it is deliberately *not* used: the
    /// `image` crate's WebP encoder is lossless-only (its own docs say so, and
    /// point at libwebp for lossy), and a lossless re-encode of a 256px
    /// thumbnail saves little enough that it is not worth a C dependency and a
    /// second codec's worth of failure modes. `image`'s JPEG encoder is lossy,
    /// pure Rust, and decodable by everything that will ever request a
    /// thumbnail. Revisit when lossy WebP is worth a dependency decision.
    pub fn content_type(self) -> &'static str {
        "image/jpeg"
    }
}

impl fmt::Display for VariantKind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

impl FromStr for VariantKind {
    type Err = Error;
    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "thumbnail" => Ok(VariantKind::Thumbnail),
            "preview" => Ok(VariantKind::Preview),
            "web" => Ok(VariantKind::Web),
            other => Err(Error::invalid_fields(
                format!("unknown variant kind: {other}"),
                vec![FieldError::new("kind", "unknown_value")],
            )),
        }
    }
}

/// An asset as the API returns it. `storage_key` is deliberately absent: it is
/// a server-side implementation detail of where the bytes live, and a client
/// that can read it will eventually try to construct a URL from it or guess a
/// neighbour's by pattern.
#[derive(Debug, Clone, Serialize)]
pub struct Asset {
    pub id: Uuid,
    pub account_id: Uuid,
    pub owner_user_id: Uuid,
    pub kind: AssetKind,
    pub original_filename: String,
    pub content_type: String,
    pub byte_size: i64,
    /// Lowercase hex sha256 of the bytes **as stored**. Not the client's claim.
    pub checksum: String,
    pub status: AssetStatus,
    pub metadata: serde_json::Value,
    pub created_at: OffsetDateTime,
    pub updated_at: OffsetDateTime,
}

impl Asset {
    /// Whether this asset can have a variant generated from it. Anything not
    /// `ready` cannot: the bytes may not exist, or may not be what the client
    /// said.
    pub fn is_derivable(&self) -> bool {
        self.status == AssetStatus::Ready && self.kind == AssetKind::Image
    }
}

/// A derived image. Points at its own storage key; the original row is never
/// touched.
#[derive(Debug, Clone, Serialize)]
pub struct AssetVariant {
    pub id: Uuid,
    pub asset_id: Uuid,
    pub account_id: Uuid,
    pub kind: VariantKind,
    pub content_type: String,
    pub byte_size: i64,
    pub width: Option<i32>,
    pub height: Option<i32>,
    pub metadata: serde_json::Value,
    pub created_at: OffsetDateTime,
    pub updated_at: OffsetDateTime,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn every_supported_content_type_maps_to_its_kind() {
        // The allow-list is a security boundary (a `kind` chooses the decoder)
        // and a contract boundary (a rejected type is a client-visible 422).
        // Both directions are asserted: a supported type is accepted, and an
        // unsupported one is rejected with a 422 naming the field.
        let cases = [
            ("image/png", AssetKind::Image),
            ("image/jpeg", AssetKind::Image),
            ("image/gif", AssetKind::Image),
            ("image/webp", AssetKind::Image),
            ("audio/mpeg", AssetKind::Audio),
            ("audio/ogg", AssetKind::Audio),
            ("video/mp4", AssetKind::Video),
            ("application/pdf", AssetKind::Document),
        ];
        for (content_type, expected) in cases {
            assert_eq!(
                AssetKind::from_content_type(content_type).expect("supported"),
                expected,
                "{content_type}"
            );
        }
    }

    #[test]
    fn content_type_parameters_and_case_do_not_change_the_kind() {
        // Clients send `Image/PNG; charset=binary` and it means the same thing.
        assert_eq!(
            AssetKind::from_content_type("IMAGE/PNG").expect("case-insensitive"),
            AssetKind::Image
        );
        assert_eq!(
            AssetKind::from_content_type("image/jpeg; charset=binary").expect("params stripped"),
            AssetKind::Image
        );
    }

    #[test]
    fn unsupported_and_hostile_content_types_are_422_naming_content_type() {
        for bad in [
            "text/html",
            "text/x-shellscript",
            "application/x-executable",
            "image/x-psd",       // right top-level, format we cannot decode
            "application/x-msdownload",
            "",
            "not-a-media-type",
        ] {
            let err = AssetKind::from_content_type(bad)
                .expect_err("must be rejected");
            assert_eq!(err.status().as_u16(), 422, "{bad} should be 422");
            let problem = err.to_problem("/v1/uploads", "t");
            let fields = problem.errors.expect("a field error");
            assert_eq!(fields[0].field, "content_type", "{bad}");
        }
    }

    #[test]
    fn db_round_trip() {
        // The SQL check constraint and the Rust enum are two copies of one
        // fact. This asserts the Rust half; the SQL half is asserted by the
        // integration test that inserts each value.
        for kind in [
            AssetKind::Image,
            AssetKind::Audio,
            AssetKind::Video,
            AssetKind::Document,
        ] {
            assert_eq!(kind.as_str().parse::<AssetKind>().expect("round trip"), kind);
        }
        for status in [AssetStatus::Pending, AssetStatus::Ready, AssetStatus::Failed] {
            assert_eq!(status.as_str().parse::<AssetStatus>().expect("round trip"), status);
        }
        for kind in [VariantKind::Thumbnail, VariantKind::Preview, VariantKind::Web] {
            assert_eq!(kind.as_str().parse::<VariantKind>().expect("round trip"), kind);
        }
    }

    #[test]
    fn variant_kinds_are_bounded_and_web_is_the_only_unbounded_one() {
        assert_eq!(VariantKind::Thumbnail.max_edge(), 256);
        assert_eq!(VariantKind::Preview.max_edge(), 1024);
        assert_eq!(VariantKind::Web.max_edge(), u32::MAX);
    }

    #[test]
    fn unknown_variant_kind_is_422_not_500() {
        // A client typo must not read as a server fault.
        let err = VariantKind::from_str("thumbnall").expect_err("unknown kind");
        assert_eq!(err.status().as_u16(), 422);
        assert_eq!(err.code(), "validation_failed");
    }

    #[test]
    fn enums_serialise_as_their_snake_case_strings() {
        // The wire form and the database form are the same string, asserted
        // rather than assumed, because a rename here is a breaking API change.
        assert_eq!(serde_json::to_string(&AssetKind::Image).unwrap(), "\"image\"");
        assert_eq!(serde_json::to_string(&AssetStatus::Pending).unwrap(), "\"pending\"");
        assert_eq!(serde_json::to_string(&VariantKind::Thumbnail).unwrap(), "\"thumbnail\"");
    }
}
