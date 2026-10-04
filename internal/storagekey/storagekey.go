// Package storagekey generates darkroom's storage keys: opaque,
// server-generated, and prefixed by account.
//
// A client never supplies a storage key. The reasons are not aesthetic, and
// they are the reasons `AGENTS.md` gives for keeping the generator the only
// constructor:
//
//   - "Storage keys are never client-supplied. A client-chosen key collides
//     across tenants, traverses, and is guessable." Two tenants both uploading a
//     file called `photo.jpg` would produce `photo.jpg` in a shared bucket, and
//     one would overwrite the other; the unique index on `assets.storage_key`
//     would turn that into a 500 on a legitimate request.
//   - `../../../other-tenant/secret` in a key is a path traversal the moment
//     anything downstream joins it onto a prefix.
//   - "A predictable key is a capability." If the key were
//     `{account_id}/{asset_id}/original`, anyone who guessed an asset id could
//     ask for a presigned PUT at the *original*'s key and overwrite it. The 128
//     bits of randomness in the leaf name are what make the presigned URL the
//     only way to write there.
//
// The shape is fixed and NEVER PARSED: `a/{account_id}/{random}/original` for
// originals, `a/{account_id}/{random}/variant-{kind}` for derived images. The
// account prefix is a lifecycle-management convenience (a bucket policy can
// expire one tenant's prefixes); it is not an authorisation mechanism, because
// authorisation is `account_id` in the database query and the presigned URL's
// key. `AGENTS.md`: "`Asset` has no `storage_key` field. It is a server-side
// detail and a client that can read it will try to build a URL from it."
package storagekey

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// Key is a generated storage key. The field is unexported and there is exactly
// one constructor that takes a string, [FromStored], so there is no code path
// that builds a key from a value a client sent — which is the whole point of
// the type and the reason it is a type rather than a bare string.
type Key struct {
	raw string
}

// leafBytes is the size of the random component: 16 bytes, hex-encoded to 32
// characters. 128 bits is what makes the leaf unguessable, and a UUID was
// deliberately not used for it because a UUID's four version and variant bits
// are not secret entropy — and a key is a secret.
const leafBytes = 16

// GenerateOriginal returns the key for a new original object.
//
// accountID is darkroom's opaque account UUID in its canonical text form — the
// same string form packet 01's `darkroomdb.InAccountTx` takes, so there is one
// representation of an account in the Go tree rather than two.
func GenerateOriginal(accountID string) Key {
	return Key{raw: fmt.Sprintf("a/%s/%s/original", accountID, randomLeaf())}
}

// GenerateVariant returns the key for a derived image. A DISTINCT NAMESPACE from
// the original, so a delete of one can never address the other.
func GenerateVariant(accountID, kind string) Key {
	return Key{raw: fmt.Sprintf("a/%s/%s/variant-%s", accountID, randomLeaf(), kind)}
}

// FromStored rehydrates a key read back from the database.
//
// This is the ONLY way a key arrives from outside, and it takes a string from a
// row this service wrote — never from a request. It is on this function that
// the "never parsed back out of a path" rule rests, so it validates what it can
// cheaply and refuses the traversal shapes rather than reproducing them.
func FromStored(raw string) (Key, error) {
	if raw == "" {
		return Key{}, fmt.Errorf("storagekey.FromStored: empty key")
	}
	if strings.HasPrefix(raw, "/") || strings.Contains(raw, `\`) {
		return Key{}, fmt.Errorf("storagekey.FromStored: %q is absolute or contains a backslash", raw)
	}
	for _, segment := range strings.Split(raw, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return Key{}, fmt.Errorf("storagekey.FromStored: %q has a traversal segment", raw)
		}
	}
	return Key{raw: raw}, nil
}

// String returns the key as it goes on the wire to the bucket. It is the only
// accessor, and there is no parser: nothing in this repository reads an account,
// an asset id or a variant kind back out of a key.
func (k Key) String() string { return k.raw }

// randomLeaf is 16 random bytes as 32 hex characters.
//
// crypto/rand rather than math/rand, and not negotiable: math/rand is not a
// CSPRNG and this value is the guessability defence for a presigned PUT's target
// key.
func randomLeaf() string {
	b := make([]byte, leafBytes)
	if _, err := rand.Read(b); err != nil {
		// There is no weaker fallback to reach for. A predictable leaf would make
		// every key guessable, and a key that is guessable turns a presigned URL
		// from the only way to write there into one of many.
		panic("storagekey: no entropy from crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}
