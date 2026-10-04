package service

import (
	"strings"
	"testing"
	"time"

	"github.com/cafaye/darkroom/internal/problem"
)

// THE LIMIT POLICY, asserted rather than trusted.
func TestTheLimitIsClampedAndNotRejected(t *testing.T) {
	// core: "`limit` defaults to 25 and is capped at 100. […] A larger request is
	// clamped, not rejected — the cap is a promise to the server about how much work
	// it will do, not a validation rule for the client to satisfy."
	cases := map[int]int{
		0:       DefaultPageLimit, // "I did not say"
		-1:      DefaultPageLimit, // a negative limit is the same absence
		1:       1,
		25:      25,
		99:      99,
		100:     MaxPageLimit,
		1000:    MaxPageLimit,
		1 << 30: MaxPageLimit,
	}
	for in, want := range cases {
		if got := clampPageLimit(in); got != want {
			t.Errorf("clampPageLimit(%d) = %d, want %d", in, got, want)
		}
	}
	// AND THE TWO NUMBERS ARE NOT THE SAME NUMBER, which is the drift a single
	// constant would allow: a cap that equalled the default would mean no page could
	// ever be short.
	if DefaultPageLimit >= MaxPageLimit {
		t.Errorf("the default (%d) is not below the cap (%d): every page would be full and `has_more` would be unreachable", DefaultPageLimit, MaxPageLimit)
	}
}

func TestCursorsRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 4, 12, 30, 15, 123456789, time.UTC)
	id := NewUUID()

	token := EncodeCursor(at, id)
	back, pErr := DecodeCursor(token)
	if pErr != nil {
		t.Fatalf("the cursor this test just encoded does not decode: %v", pErr)
	}
	if !back.CreatedAt.Equal(at) {
		t.Errorf("time round-tripped to %v, want %v", back.CreatedAt, at)
	}
	if back.ID != id {
		t.Errorf("id round-tripped to %q, want %q", back.ID, id)
	}
}

func TestACursorIsBase64url(t *testing.T) {
	// `+` and `/` in a query string are a percent-encoding bug in every client that
	// has not heard of them, and core specifies base64url. Encoded with the PADDED
	// alphabet they appear often enough that 200 cursors is not a contrived sample.
	for range 200 {
		token := EncodeCursor(time.Now(), NewUUID())
		for _, r := range token {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			default:
				t.Fatalf("cursor %q contains %q, which is not in the base64url alphabet", token, r)
			}
		}
	}
	// And a NONCE, not a fresh URL-safe encoding of the same value: padding is the
	// other half of "base64url" and it is what makes a cursor ugly in a query string.
	at := time.Unix(0, 1_700_000_000_000_000_000).UTC()
	if strings.ContainsRune(EncodeCursor(at, NewUUID()), '=') {
		t.Error("the cursor is padded; base64url here is RawURLEncoding")
	}
}

func TestEveryMalformedCursorIsTheSame422NamingTheField(t *testing.T) {
	// ONE error for all of them, deliberately: a client cannot tell a truncated token
	// from a forged one, and an oracle is not built from parse errors. 422 rather than
	// 400 is the Rust tree's answer and a change to it would be a client-visible
	// change.
	bad := []string{
		"", // empty
		"!!!not base64!!!",
		"YWJj",       // base64 for "abc": no separator
		"MTIzfGV4dA", // "123|ext": not a uuid
		"not-base64|also-text",
		EncodeCursor(time.Unix(0, 0), ""), // a timestamp with no id
		EncodeCursor(time.Unix(0, 0), "has space"), // an id that is not an id
	}
	for _, raw := range bad {
		_, pErr := DecodeCursor(raw)
		if pErr == nil {
			t.Errorf("DecodeCursor(%q) succeeded", raw)
			continue
		}
		if pErr.Status() != 422 || pErr.Code() != problem.CodeValidationFailed {
			t.Errorf("DecodeCursor(%q) is %d/%s, want 422/validation_failed", raw, pErr.Status(), pErr.Code())
		}
		found := false
		for _, f := range pErr.Fields() {
			if f.Field == "cursor" {
				found = true
			}
		}
		if !found {
			t.Errorf("DecodeCursor(%q) does not name the cursor field: %v", raw, pErr.Fields())
		}
	}
}

func TestFilenamesAreValidatedBeforeAnythingIsWritten(t *testing.T) {
	if got, pErr := validateFilename("photo.png"); pErr != nil || got != "photo.png" {
		t.Errorf("validateFilename(photo.png) = %q, %v", got, pErr)
	}
	// TRIMMED, not rejected: a stray space from a copy-paste is not a security event.
	if got, pErr := validateFilename("  photo.png "); pErr != nil || got != "photo.png" {
		t.Errorf("validateFilename(spaced) = %q, %v", got, pErr)
	}
	// And the trimmed value is what gets STORED, so two names that differ only in
	// surrounding whitespace are one name in the registry.
	for _, bad := range []string{
		"",
		"   ",
		"with/slash.png",
		`with\backslash.png`,
		"with\x00null.png",
		"with\nnewline.png",
		strings.Repeat("x", maxFilenameRunes+1),
	} {
		if _, pErr := validateFilename(bad); pErr == nil {
			t.Errorf("validateFilename(%q) was accepted", bad)
		} else if pErr.Status() != 422 {
			t.Errorf("validateFilename(%q) is %d, want 422", bad, pErr.Status())
		}
	}
	// The limit is in RUNES, so a filename of multi-byte characters is measured the
	// way a human measures it — a byte cap would reject 128 four-byte filenames for a
	// reason that has nothing to do with headers or filesystems.
	if _, pErr := validateFilename(strings.Repeat("世", maxFilenameRunes)); pErr != nil {
		t.Errorf("%d runes was rejected, want accepted: the cap is runes, not bytes", maxFilenameRunes)
	}
}
