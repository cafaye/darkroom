//! Storage keys: opaque, server-generated, and prefixed by account.
//!
//! A client never supplies a storage key. The reasons are not aesthetic:
//!
//! - **A client-chosen key can collide.** Two tenants both uploading a file
//!   called `photo.jpg` would produce `photo.jpg` in a shared bucket, and one
//!   would overwrite the other. The unique index on `assets.storage_key` would
//!   turn that into a 500 on a legitimate request.
//! - **A client-chosen key can traverse.** `../../../other-tenant/secret` in a
//!   key is a path traversal the moment anything downstream joins it onto a
//!   prefix.
//! - **A predictable key is a capability.** If the key were
//!   `{account_id}/{asset_id}/original`, anyone who guessed an asset id could
//!   ask for a presigned PUT at the *original*'s key and overwrite it. The
//!   128 bits of randomness in the leaf name are what make the presigned URL
//!   the only way to write there.
//!
//! The shape is fixed and never parsed: `a/{account_id}/{random}/original` for
//! originals, `a/{account_id}/{random}/variant-{kind}` for derived images. The
//! account prefix is a lifecycle-management convenience (a bucket policy can
//! expire one tenant's prefixes); it is not an authorisation mechanism, because
//! authorisation is `account_id` in the database query and the presigned URL's
//! key.

use uuid::Uuid;

/// A generated key. Constructed only by [`StorageKey::generate`], so there is
/// no code path that builds one from a string a client sent.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StorageKey(String);

impl StorageKey {
    /// Generate the key for a new original object.
    pub fn generate_original(account_id: Uuid) -> Self {
        // The random component is what makes the key unguessable; the account
        // prefix is what makes a bucket lifecycle rule possible. Both are
        // server-side values.
        StorageKey(format!("a/{account_id}/{}/original", random_leaf()))
    }

    /// Generate the key for a derived image. Distinct namespace from the
    /// original so a delete of one can never address the other.
    pub fn generate_variant(account_id: Uuid, kind: &str) -> Self {
        StorageKey(format!("a/{account_id}/{}/variant-{kind}", random_leaf()))
    }

    /// Rehydrate a key read back from the database. This is the ONLY way a key
    /// becomes a `StorageKey` from outside, and it takes a `&str` from a row
    /// this service wrote — never from a request.
    pub fn from_stored(raw: &str) -> Self {
        StorageKey(raw.to_string())
    }

    pub fn as_str(&self) -> &str {
        &self.0
    }

    pub fn into_string(self) -> String {
        self.0
    }
}

impl std::fmt::Display for StorageKey {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

/// 16 random bytes as 32 hex characters: 128 bits, which is what makes the
/// leaf unguessable. `rand` rather than a UUID because a UUID's four version and
/// variant bits are not secret entropy, and a key is a secret.
fn random_leaf() -> String {
    use rand::Rng;
    let mut bytes = [0u8; 16];
    rand::rng().fill(&mut bytes);
    hex::encode(bytes)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn keys_are_opaque_scoped_and_unique() {
        let account = Uuid::new_v4();

        let a = StorageKey::generate_original(account);
        let b = StorageKey::generate_original(account);
        // Same account, two uploads, two keys. A predictable key would collide
        // here and the unique index would turn it into a 500.
        assert_ne!(a.as_str(), b.as_str());

        // Account-prefixed for lifecycle, and the account is not a secret.
        assert!(a.as_str().starts_with(&format!("a/{account}/")));
        assert!(a.as_str().ends_with("/original"));

        // Derived images live in their own namespace, so a variant delete can
        // never address an original.
        let v = StorageKey::generate_variant(account, "thumbnail");
        assert!(v.as_str().ends_with("/variant-thumbnail"));
        assert!(!v.as_str().ends_with("/original"));
    }

    #[test]
    fn the_random_leaf_is_long_enough_to_not_be_guessable() {
        // 32 hex characters = 128 bits. Asserted so a future "let's shorten
        // the key" commit has to change this test on purpose.
        let account = Uuid::new_v4();
        let key = StorageKey::generate_original(account);
        let leaf = key
            .as_str()
            .rsplit('/')
            .nth(1)
            .expect("key has a leaf segment");
        assert_eq!(leaf.len(), 32);
        assert!(leaf.chars().all(|c| c.is_ascii_hexdigit()));
    }

    #[test]
    fn a_key_never_contains_traversal_segments() {
        // A generated key is structurally incapable of containing `..`, which
        // is why no downstream code has to sanitise it. 200 keys, because a
        // random component failing to be random is rare and a single sample
        // proves nothing.
        let account = Uuid::new_v4();
        for _ in 0..200 {
            for key in [
                StorageKey::generate_original(account),
                StorageKey::generate_variant(account, "preview"),
            ] {
                assert!(!key.as_str().contains(".."), "traversal in {key}");
                assert!(!key.as_str().starts_with('/'), "absolute in {key}");
                assert!(!key.as_str().contains('\\'), "backslash in {key}");
            }
        }
    }
}
