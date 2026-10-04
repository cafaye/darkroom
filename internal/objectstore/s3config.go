package objectstore

import (
	"fmt"
	"strings"
)

// r2HostSuffix is the host every R2 account endpoint ends in.
//
// Cloudflare has no global R2 endpoint: it is always
// `https://<ACCOUNT_ID>.r2.cloudflarestorage.com`, so the endpoint is
// configuration and never a constant in this file.
const r2HostSuffix = ".r2.cloudflarestorage.com"

// r2Region is R2's region, and the two values R2 documents as aliases for it:
// `auto` and the empty string.
//
// This is the whole of the R2 region problem, and it is worth four rules because
// each one is a way of getting a 403 from a bucket that is perfectly healthy:
//
//   - R2's region is `auto`. Not "any region works" — R2 REJECTS a SigV4
//     signature made for a real region, even though `us-east-1` is accepted as
//     an ALIAS. Aliases are accepted; the signature still has to say `auto`.
//   - A real region with an R2 endpoint is REFUSED at startup, naming the two
//     values that would work. Failing on the first upload would mean a signed
//     upload is issued, the client PUTs, and the failure surfaces as an opaque
//     403 with a signature in it.
//   - `auto` without an endpoint is REFUSED, because AWS has no `auto` region —
//     that configuration would sign against an AWS default endpoint and fail
//     every request with a signature error. This is the "I configured for R2 and
//     forgot the account id" mistake.
//   - Path style is forced for R2 regardless of the flag, because R2's account
//     endpoints are path-addressed.
const r2Region = "auto"

// r2RegionAlias is the one real region name R2 also accepts, and it is accepted
// only as an ALIAS: the signature is still built for `auto`. An operator who
// wrote `us-east-1` because the console told them to gets a working deployment
// rather than a startup error, and the table row for it asserts the signed
// region is `auto` — which is the only way to tell normalisation from a
// coincidence.
const r2RegionAlias = "us-east-1"

// normalise validates and completes an [S3Config], and it is the only place any
// of the R2 rules live.
//
// IT NORMALISES RATHER THAN BRANCHING, which is the point. The difference between
// "an R2 code path" and "an R2 configuration" is the difference between one
// implementation and two, and `AGENTS.md` is explicit that the second is never the
// answer: "One `ObjectStore` implementation, several S3-compatible services."
func (c S3Config) normalise() (S3Config, error) {
	if strings.TrimSpace(c.Bucket) == "" {
		return S3Config{}, fmt.Errorf("objectstore: a bucket is required")
	}
	out := c
	isR2 := isR2Endpoint(c.Endpoint)

	switch {
	case isR2:
		switch {
		case strings.EqualFold(c.Region, r2Region), strings.EqualFold(c.Region, r2RegionAlias), c.Region == "":
			out.Region = r2Region
		default:
			return S3Config{}, fmt.Errorf("objectstore: Region %q with a Cloudflare R2 endpoint: R2's region is the literal string %q, because R2 rejects a SigV4 signature made for a real region. Set Region=%q, or leave it empty for the default.", c.Region, r2Region, r2Region)
		}
		// R2 is path-style regardless of the flag.
		out.PathStyle = true
	case strings.EqualFold(c.Region, r2Region):
		return S3Config{}, fmt.Errorf("objectstore: Region=%q is only valid with a Cloudflare R2 endpoint: AWS has no %q region, so this would sign against an AWS default endpoint and fail every request. Set Endpoint to https://<ACCOUNT_ID>%s, or set a real region like us-east-1.", r2Region, r2Region, r2HostSuffix)
	case strings.TrimSpace(c.Region) == "":
		return S3Config{}, fmt.Errorf("objectstore: a Region is required when the Endpoint is not Cloudflare R2, whose region is %q and is defaulted", r2Region)
	}

	return out, nil
}

// isR2Endpoint reports whether endpoint is a Cloudflare R2 account endpoint.
//
// A HOST MATCH and not a substring: `https://r2.cloudflarestorage.com.evil
// .example` ends with the right characters and is not R2, and configuration that
// decides to sign that as `auto` produces an authentication failure with no
// obvious cause. It reads case-insensitively because a hostname is
// case-insensitive, and it tolerates a scheme with a trailing slash and a port —
// the shape a local R2-compatible server (MinIO) has, which is how a developer
// tests against it.
func isR2Endpoint(endpoint string) bool {
	if endpoint == "" {
		return false
	}
	rest, ok := strings.CutPrefix(endpoint, "https://")
	if !ok {
		if rest, ok = strings.CutPrefix(endpoint, "http://"); !ok {
			return false
		}
	}
	if rest == "" {
		return false
	}
	// The path is not part of the host, and a base URL may legitimately carry
	// one, so cut it before looking at the suffix.
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		rest = rest[:slash]
	}
	// Strip the port. `net.SplitHostPort` is not used because it errors on a
	// missing port and this function is a boolean.
	if colon := strings.LastIndexByte(rest, ':'); colon >= 0 {
		rest = rest[:colon]
	}
	return strings.ToLower(rest) == strings.ToLower(r2HostSuffix[1:]) || strings.HasSuffix(strings.ToLower(rest), r2HostSuffix)
}
