package storagekey_test

import (
	"strings"
	"testing"

	"github.com/cafaye/darkroom/internal/storagekey"
)

const account = "3f1b2c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"

func TestKeysAreOpaqueScopedAndUnique(t *testing.T) {
	a := storagekey.GenerateOriginal(account)
	b := storagekey.GenerateOriginal(account)

	// Same account, two uploads, two keys. A predictable key would collide here
	// and the unique index on `assets.storage_key` would turn it into a 500.
	if a.String() == b.String() {
		t.Fatalf("two originals for one account share a key: %s", a)
	}

	// Account-prefixed for lifecycle, and the account is not a secret.
	if got := a.String(); !strings.HasPrefix(got, "a/"+account+"/") {
		t.Errorf("key is not account-prefixed: %s", got)
	}
	if got := a.String(); !strings.HasSuffix(got, "/original") {
		t.Errorf("key does not end in /original: %s", got)
	}

	// Derived images live in their own namespace, so a variant delete can never
	// address an original.
	v := storagekey.GenerateVariant(account, "thumbnail")
	if !strings.HasSuffix(v.String(), "/variant-thumbnail") {
		t.Errorf("variant key has the wrong suffix: %s", v)
	}
	if strings.HasSuffix(v.String(), "/original") {
		t.Errorf("a variant key ended in /original: %s", v)
	}
}

// Asserted so a future "let's shorten the key" commit has to change this test on
// purpose. 32 hex characters is 128 bits, and it is the guessability defense
// for the presigned PUT's target: with a guessable leaf, anyone who could guess
// it could ask for a presigned URL at the ORIGINAL's key and overwrite it.
func TestTheRandomLeafIsLongEnoughToNotBeGuessable(t *testing.T) {
	segments := strings.Split(storagekey.GenerateOriginal(account).String(), "/")
	if len(segments) != 4 {
		t.Fatalf("key is not four segments: %s", strings.Join(segments, "/"))
	}
	leaf := segments[2]
	if len(leaf) != 32 {
		t.Errorf("leaf is %d characters, want 32 (128 bits): %s", len(leaf), leaf)
	}
	for _, c := range leaf {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("leaf is not lowercase hex: %s", leaf)
		}
	}
}

// A generated key is structurally incapable of containing traversal, which is
// why no downstream code has to sanitize it. 200 keys, because a random
// component failing to be random is rare and a single sample proves nothing.
func TestAGeneratedKeyNeverContainsTraversalSegments(t *testing.T) {
	for i := 0; i < 200; i++ {
		for _, k := range []storagekey.Key{
			storagekey.GenerateOriginal(account),
			storagekey.GenerateVariant(account, "preview"),
		} {
			s := k.String()
			switch {
			case strings.Contains(s, ".."):
				t.Fatalf("traversal in %s", s)
			case strings.HasPrefix(s, "/"):
				t.Fatalf("absolute key %s", s)
			case strings.Contains(s, `\`):
				t.Fatalf("backslash in %s", s)
			}
		}
	}
}

// FromStored is the one door a string comes in through, so it is also where a
// traversal would have to be stopped. The Rust original did not validate here —
// it trusted "a row this service wrote" — and the Go version checks what it can
// cheaply, because the cost of refusing a malformed row is one clear error and
// the cost of accepting one is a bucket write outside the tenant prefix.
func TestFromStoredRefusesWhatAGeneratedKeyCannotProduce(t *testing.T) {
	for _, bad := range []string{
		"",
		"/a/abc/original",
		"a/../../etc/passwd",
		"a//original",
		`a\account\original`,
		"a/./original",
	} {
		if k, err := storagekey.FromStored(bad); err == nil {
			t.Errorf("FromStored(%q) accepted a key the generator cannot produce: %q", bad, k)
		}
	}

	// And a real generated key round-trips, because refusing valid rows would be
	// just as broken as accepting invalid ones.
	want := storagekey.GenerateOriginal(account).String()
	got, err := storagekey.FromStored(want)
	if err != nil {
		t.Fatalf("FromStored(%q): %v", want, err)
	}
	if got.String() != want {
		t.Fatalf("round trip changed the key: %q became %q", want, got)
	}
}
