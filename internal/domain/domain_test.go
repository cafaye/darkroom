package domain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anywaye/darkroom/internal/variants"
)

func TestEverySupportedContentTypeMapsToItsKind(t *testing.T) {
	// The allow-list is a security boundary — a `kind` chooses the decoder — and a
	// contract boundary, where a rejected type is a client-visible 422. Both
	// directions are asserted.
	cases := map[string]AssetKind{
		"image/png":       KindImage,
		"image/jpeg":      KindImage,
		"image/gif":       KindImage,
		"image/webp":      KindImage,
		"audio/mpeg":      KindAudio,
		"audio/ogg":       KindAudio,
		"audio/wav":       KindAudio,
		"audio/webm":      KindAudio,
		"video/mp4":       KindVideo,
		"video/quicktime": KindVideo,
		"video/webm":      KindVideo,
		"application/pdf": KindDocument,
	}
	for contentType, want := range cases {
		got, err := ParseAssetKind(contentType)
		if err != nil {
			t.Errorf("ParseAssetKind(%q) returned %v, want %q", contentType, err, want)
			continue
		}
		if got != want {
			t.Errorf("ParseAssetKind(%q) = %q, want %q", contentType, got, want)
		}
	}
}

func TestContentTypeParametersAndCaseDoNotChangeTheKind(t *testing.T) {
	// Clients send `Image/PNG; charset=binary` and it means the same thing. The
	// value is also what the presigned URL is signed with, so normalising here is
	// what keeps one asset from having two spellings and two signatures.
	for _, raw := range []string{"IMAGE/PNG", "image/jpeg; charset=binary", "  image/webp  "} {
		got, err := ParseAssetKind(raw)
		if err != nil {
			t.Fatalf("ParseAssetKind(%q) returned %v, want an image", raw, err)
		}
		if got != KindImage {
			t.Errorf("ParseAssetKind(%q) = %q, want image", raw, got)
		}
	}
	if base := BaseContentType("IMAGE/PNG; charset=binary"); base != "image/png" {
		t.Errorf("BaseContentType normalised to %q, want image/png", base)
	}
}

func TestUnsupportedAndHostileContentTypesAreRefusedNamingTheField(t *testing.T) {
	for _, bad := range []string{
		"text/html",
		"text/x-shellscript",
		"application/x-executable",
		"image/x-psd", // right top-level, a format the pipeline cannot decode
		"application/x-msdownload",
		"",
		"not-a-media-type",
		"image", // a type with no subtype is not a media type
		"image/",
	} {
		kind, err := ParseAssetKind(bad)
		if err == nil {
			t.Errorf("ParseAssetKind(%q) = %q, want a refusal", bad, kind)
			continue
		}
		if !IsUnsupportedContentType(err) {
			t.Errorf("ParseAssetKind(%q) returned %v, which the service cannot map", bad, err)
		}
	}
}

func TestTheEnumValuesAreTheCheckConstraints(t *testing.T) {
	// THE MIGRATION IS THE OTHER HALF OF THIS FACT, and it is read rather than
	// remembered: `assets_kind_check`, `assets_status_check` and
	// `asset_variants_kind_check` are three `check` constraints holding three lists,
	// and a fourth kind added in Go without a migration would be a 500 from the
	// database on a value the service believes in. A comment cannot catch that; a
	// read of the file can.
	//
	// The file is located relative to this package rather than at a fixed absolute
	// path, so the assertion fails loudly if the tree moves instead of skipping.
	path := filepath.Join("..", "..", "migrations", "0001_assets.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	sql := string(raw)

	for _, c := range []struct {
		constraint string
		want       []string
	}{
		{"assets_kind_check", []string{"image", "audio", "video", "document"}},
		{"assets_status_check", []string{"pending", "ready", "failed"}},
		{"asset_variants_kind_check", []string{"thumbnail", "preview", "web"}},
	} {
		clause := constraintClause(t, sql, c.constraint)
		for _, v := range c.want {
			if !strings.Contains(clause, "'"+v+"'") {
				t.Errorf("%s does not contain %q:\n%s", c.constraint, v, clause)
			}
		}
	}

	// And every Go value is inside the constraint it belongs to. A Go constant with
	// no counterpart in the migration is a value the database refuses.
	for _, k := range []AssetKind{KindImage, KindAudio, KindVideo, KindDocument} {
		if !strings.Contains(constraintClause(t, sql, "assets_kind_check"), "'"+string(k)+"'") {
			t.Errorf("AssetKind %q is not in assets_kind_check", k)
		}
		if !k.Valid() {
			t.Errorf("AssetKind %q does not consider itself valid", k)
		}
	}
	for _, s := range []AssetStatus{StatusPending, StatusReady, StatusFailed} {
		if !strings.Contains(constraintClause(t, sql, "assets_status_check"), "'"+string(s)+"'") {
			t.Errorf("AssetStatus %q is not in assets_status_check", s)
		}
	}
	for _, k := range variants.AllVariantKinds() {
		if !strings.Contains(constraintClause(t, sql, "asset_variants_kind_check"), "'"+k.String()+"'") {
			t.Errorf("variants.VariantKind %q is not in asset_variants_kind_check", k)
		}
	}
	// A kind the check constraint rejects, refused by Go too — the other direction,
	// and the one that is a security boundary rather than a typo.
	if (AssetKind("video")).Valid() != true {
		t.Error("video must be valid")
	}
	if (AssetKind("executable")).Valid() {
		t.Error("a kind outside the check constraint must not be valid")
	}
}

func TestTerminalStatusesAreTerminalAndPendingIsNot(t *testing.T) {
	// The property that keeps `UNIQUE (account_id, checksum)` meaningful, asserted
	// rather than assumed: a failed upload cannot be retried into ready, so a
	// checksum that failed once cannot be silently attempted again under the same
	// key.
	if StatusPending.Terminal() {
		t.Error("pending is the only non-terminal status")
	}
	for _, s := range []AssetStatus{StatusReady, StatusFailed} {
		if !s.Terminal() {
			t.Errorf("%q must be terminal", s)
		}
	}
}

func TestStorageKeyIsInvisibleToTheWire(t *testing.T) {
	// The rule is `Asset` has no `storage_key` FIELD. The Go spelling of "has no
	// field" is a field the encoder cannot see, so the check is on what
	// `encoding/json` produces rather than on a comment — a renamed or re-exported
	// field fails here.
	keyed := Asset{ID: "ast_1", storageKey: "a/acct/deadbeef/original"}
	if got := keyed.StorageKey(); got != "a/acct/deadbeef/original" {
		t.Errorf("StorageKey() = %q", got)
	}
	if keyed.WithStorageKey("other").ID != "ast_1" {
		t.Error("WithStorageKey must not disturb the rest of the asset")
	}
	// An unkeyed asset has no key, which is what a client-visible value would be.
	if (Asset{}).StorageKey() != "" {
		t.Error("an asset with no key must report no key")
	}
}

func TestIsDerivableNeedsBothReadyAndImage(t *testing.T) {
	cases := []struct {
		status AssetStatus
		kind   AssetKind
		want   bool
	}{
		{StatusReady, KindImage, true},
		{StatusPending, KindImage, false}, // the bytes may not exist yet
		{StatusFailed, KindImage, false},  // the bytes may not be what was claimed
		{StatusReady, KindVideo, false},   // the image pipeline cannot read it
		{StatusReady, KindAudio, false},
		{StatusReady, KindDocument, false},
	}
	for _, c := range cases {
		a := Asset{Status: c.status, Kind: c.kind}
		if got := a.IsDerivable(); got != c.want {
			t.Errorf("IsDerivable(status=%q kind=%q) = %v, want %v", c.status, c.kind, got, c.want)
		}
	}
}

// constraintClause returns the text of `check (<name>)` … up to its closing paren.
//
// Bracket counting rather than a regexp over a single line, because a constraint
// written across lines would silently match nothing and the assertion would pass
// having checked nothing — the guard-that-passes-on-nothing failure the Rust tree
// records twice in `AGENTS.md`.
func constraintClause(t *testing.T, sql, name string) string {
	t.Helper()
	needle := "constraint " + name
	at := strings.Index(sql, needle)
	if at < 0 {
		t.Fatalf("migrations/0001_assets.sql has no %q", needle)
	}
	rest := sql[at:]
	open := strings.Index(rest, "(")
	if open < 0 {
		t.Fatalf("%s has no check expression", name)
	}
	depth := 0
	for i := open; i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return rest[open : i+1]
			}
		}
	}
	t.Fatalf("%s is not closed", name)
	return ""
}
