package outbox

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// The two patterns copied verbatim from core's event-envelope.schema.json $defs,
// so that a value this package accepts is one core's schema accepts.
//
// core's own tests assert the envelope and manifest schemas agree on these, so
// they cannot drift apart inside core. Copying them here is the duplication that
// keeps a malformed type from reaching the bus at all.
var (
	eventTypePattern   = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*\.[a-z][a-z0-9]*(_[a-z0-9]+)*\.[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	serviceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	subjectPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]*$`)
)

// Length bounds copied from core's schema: subject 1-200, source 2-40,
// type 5-120.
const (
	maxSubjectLength = 200
	minSourceLength  = 2
	maxSourceLength  = 40
	minTypeLength    = 5
	maxTypeLength    = 120
)

// Validate reports whether the envelope satisfies core's schema.
//
// It is called before every publish. The alternative is letting a bad envelope
// reach the bus and fail at every consumer instead of at the one place that
// wrote it.
func (e Envelope) Validate() error {
	if e.SpecVersion != SpecVersion {
		return fmt.Errorf("outbox: specversion is %q, want %q", e.SpecVersion, SpecVersion)
	}
	if e.ID == "" {
		return fmt.Errorf("outbox: id is required; consumers deduplicate on it")
	}
	if !validEventType(e.Type) {
		return fmt.Errorf("outbox: type %q does not match core's eventType pattern", e.Type)
	}
	if !validServiceName(e.Source) {
		return fmt.Errorf("outbox: source %q does not match core's serviceName pattern", e.Source)
	}
	if !validSubject(e.Subject) {
		return fmt.Errorf("outbox: subject %q does not match core's subject pattern", e.Subject)
	}
	if e.Time == "" {
		return fmt.Errorf("outbox: time is required")
	}
	if !isJSONObject(e.Data) {
		return fmt.Errorf("outbox: data must be a JSON object")
	}
	return e.RejectCapabilities()
}

func validEventType(s string) bool {
	if len(s) < minTypeLength || len(s) > maxTypeLength {
		return false
	}
	return eventTypePattern.MatchString(s)
}

func validServiceName(s string) bool {
	if len(s) < minSourceLength || len(s) > maxSourceLength {
		return false
	}
	return serviceNamePattern.MatchString(s)
}

func validSubject(s string) bool {
	if len(s) < 1 || len(s) > maxSubjectLength {
		return false
	}
	return subjectPattern.MatchString(s)
}

// isJSONObject reports whether raw is a JSON object. core allows any JSON value
// for `data`, and every darkroom payload is an object, so an object is what this
// service produces and an array or a bare string here would mean a payload that
// was not built by darkroom.
func isJSONObject(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var probe map[string]json.RawMessage
	return json.Unmarshal(raw, &probe) == nil && probe != nil
}

// forbiddenDataKeys are the names a payload may not carry.
//
// The rule behind the list, stated once: **an event describes a FACT and never
// carries the CAPABILITY to act on it.** darkroom's whole design is signed
// uploads and tenant-scoped assets — `internal/service` refuses to hand a
// presigned URL to a caller without a verified principal, and
// `migrations/0004_account_isolation.sql` puts FORCE RLS between an account and
// another account's rows. An event that carried a presigned URL or a bucket key
// onto the bus would hand every consumer on the platform the ability to read an
// object the HTTP surface spent a whole packet protecting, and it would do so
// once, to everyone, with no retraction.
//
// A tenant id is refused for the other reason: it is an identifier a consumer
// should not need. An event about `darkroom.asset.ready` says an asset exists
// and what it is; a consumer wanting to know whose it is asks darkroom through a
// request that carries a principal and is scoped by a policy. `outbox_events`
// itself has no `account_id` column — `tenancy.yml` records that as deliberate
// — so an event that needed one would be the first thing in this service's read
// path to reach across the tenant axis.
//
// The check is by key name AND by value shape. Key names catch a payload that
// literally declares itself; the value check below catches a capability hidden
// under an innocent key, which is the failure a name list cannot see.
var forbiddenDataKeys = map[string]bool{
	// tenancy
	"account_id": true,
	"tenant_id":  true,
	"tenant":     true,
	"account":    true,
	// storage and object identity
	"storage_key": true,
	"bucket":      true,
	"bucket_key":  true,
	"object_key":  true,
	"object_id":   true,
	"key":         true,
	// capabilities
	"presigned":     true,
	"presigned_url": true,
	"presigned_put": true,
	"presigned_get": true,
	"upload_url":    true,
	"download_url":  true,
	"url":           true,
	"uri":           true,
	"signature":     true,
	"credential":    true,
	"credentials":   true,
	"access_key":    true,
	"access_key_id": true,
	"secret":        true,
}

// RejectCapabilities reports whether the payload carries anything a consumer
// could act on: a capability URL, a storage key, or a tenant identifier.
//
// It runs on every publish, which makes it the last darkroom-owned gate before
// the bus. That placement is deliberate rather than defensive duplication — the
// payloads are built in `internal/service` and are asserted there too, but an
// assertion on a call site is a statement about one code path while this is a
// statement about the wire. A future event type added by someone who did not
// read that test is stopped here.
//
// A refusal is a FAILED PUBLISH: the row stays unpublished with attempts
// incremented, which is the loud direction. The alternative — publishing anyway
// and logging — puts the capability on the bus and the warning in a log nobody
// reads.
func (e Envelope) RejectCapabilities() error {
	if len(e.Data) == 0 {
		return nil
	}

	var data map[string]json.RawMessage
	if err := json.Unmarshal(e.Data, &data); err != nil {
		// A non-object payload has no keys to reject; core allows any JSON value and
		// darkroom's are all objects.
		return nil
	}

	for key, raw := range data {
		if forbiddenDataKeys[strings.ToLower(key)] {
			return fmt.Errorf("outbox: %s carries the forbidden payload key %q: "+
				"an event describes a fact and never carries the capability to act on it", e.Type, key)
		}

		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			continue // not a string, so it cannot be a URL or a key
		}
		if isCapabilityURL(value) {
			return fmt.Errorf("outbox: %s carries a URL in %q; a presigned or "+
				"absolute address on the bus is a capability handed to every consumer at once", e.Type, key)
		}
	}
	return nil
}

// isCapabilityURL reports whether s is an absolute http(s) address.
//
// The rule is "no event payload carries a URL", and it is absolute because every
// field darkroom emits is an identifier, an enum, a MIME type or a number — none
// of which is a URL, and none of which needs to be. A relative path would also
// be a capability if a consumer could resolve it, so the test is on the scheme.
func isCapabilityURL(s string) bool {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return false
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return false
	}
	return u.IsAbs() && (u.Scheme == "http" || u.Scheme == "https")
}
