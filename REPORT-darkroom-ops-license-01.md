# REPORT — darkroom: the grant is BSL-1.1, and it was MIT

Packet: `lic-services-b-darkroom`, wave 0 of OPS-P1. One repository, one branch,
two commits, nothing merged and nothing pushed. Manager merges.

- Branch: `ops/w0-lic-services-b-darkroom`
- Commits: `93aa259` (the grant), and this report
- Working tree after both: `git status --porcelain` is empty apart from the
  manager's own `brief.txt` and `brief-lic-services-b.txt`, which were
  untracked before I started and which I did not touch, add, or remove.

## What the packet was

OPS-D2 draws the licence line at the job rather than at the language: the
framework — `core`, `caf`, `kit`, `pantry` — is permissive because its job is
adoption, and the services sit under a Business Source Licence because their
job is to be run: free to run, free to self-host, never to offer a competing
service. darkroom is a service. It shipped MIT from `CHANGELOG.md:222` onward,
so this is a move DOWN a permission ladder, and OPS-D3's rule is that such a
move is made out loud rather than by drift.

## The grant, and where it is

`LICENSE` at the repository root, BSL 1.1, with the parameters a reader can
diff two repositories against (`LICENSE:3-9`):

| parameter | value |
|---|---|
| Licensor | `Cafaye` |
| Licensed Work | `darkroom` |
| Additional Use Grant | `None` |
| Change Date | `2030-10-10` |
| Change Licence | `Apache-2.0` |

The Change Date is four years from the day this landed (2026-10-10) and is also
what BSL's own text would have arrived at unaided — "the fourth anniversary of
the first publicly available distribution of a specific version of the Licensed
Work under this License" (`LICENSE:18-21`). Nothing here invents a date.
`Additional Use Grant: None` is literal: covenant 2 of the licence permits the
Licensor to insert the text "None" rather than name a grant (`LICENSE:70-72`),
and none was specified in the brief.

## The five files, and why each one is in scope

| file | change | lines |
|---|---|---|
| `LICENSE` | MIT text → BSL 1.1 with the parameters above | 76 insertions, 21 deletions |
| `cafaye.yml` | a comment stating the grant and why it cannot be a field | 19 insertions |
| `openapi/v1.yaml` | `info.license` → `BUSL-1.1`, identifier and SPDX page | 8 insertions, 2 deletions |
| `README.md` | `## License` rewritten to state the transition | 40 insertions, 6 deletions |
| `CHANGELOG.md` | a `### Changed` entry under `[Unreleased]`, plus a superseded note on the old entry | 47 insertions |

No source file, no test, no CI file, no migration, no go.mod, no generated file.

## The transition, in prose where a reader will find it

`README.md:1016-1058` is the section a reader lands on, and it says three
things plainly: the grant is now BSL with those parameters; it was MIT, with the
MIT reasoning recorded at `CHANGELOG.md:222-229` kept intact rather than
retracted; and **anyone who already holds a copy keeps the grant it was
published under**. The commit message carries the same three sentences for
anyone reading history rather than the tree.

The old `CHANGELOG.md:222` entry is now marked superseded in place
(`CHANGELOG.md:234-236`), because a reader who reaches that entry should not
learn the current grant from a file that says "darkroom is MIT" in the present
tense. The entry itself is unchanged — Keep a Changelog keeps history, and
editing the record of what MIT was would be the same drift one level down.

## What this packet could NOT do, and it is measured

**`cafaye.yml` carries no licence field, because it cannot.** core owns the
manifest schema and that schema sets `additionalProperties: false` with no
licence property, so a `licence:` or a `license:` key in this manifest is
rejected by `caf contract lint` — the tool `.github/workflows/ci.yml:303-307`
runs against this exact file. I proved it rather than reasoning about it: both
spellings were appended to a copy of the manifest, linted, and both failed with
`declares unknown key`. The copy was removed.

The brief named `Cargo.toml` as darkroom's manifest with `license = "MIT"`.
**There is no `Cargo.toml` in this tree** — it went with the Rust source in
darkroom-go-06, and `AGENTS.md:13` states the position ("ONE LANGUAGE. The Go
rewrite is the whole tree"). The only machine-readable licence surface this
repository owns is `info.license` in `openapi/v1.yaml:84-92`, which
`cafaye.yml`'s `exposes.api` points at, and that now says `BUSL-1.1`. The
manifest states the grant and the reason in a comment (`cafaye.yml:25-43`)
instead, because a key that fails the schema is not a manifest that agrees with
the grant — it is a red build.

**This is a core packet, not a darkroom one.** Adding a `license` field to the
manifest requires two edits that do not live here: core's
`schemas/cafaye.manifest.schema.json`, and caf's vendored pin of it
(`caf/internal/contract/schemas/manifest-0.2.json`, pinned to core `3340e0f` by
`manifestSchemaPin` in `caf/internal/contract/schema.go:22-28`). Until both move,
a licence field in any service manifest fails CI. Recorded here so the next
packet does not rediscover it.

## Verification, with the exit codes I observed

```
$ cd cafaye/caf
$ go run ./cmd/caf contract lint <repo>/cafaye.yml
OK <repo>/cafaye.yml
OK <repo>/openapi/v1.yaml        # linting the manifest lints its exposed document
exit 0
```

That is the check CI runs on both files I edited that CI reads. Then the one
this packet could plausibly have broken: the router is generated from
`openapi/v1.yaml`, so a document edit can drift `internal/api/api.gen.go`.

```
$ go generate ./internal/api/        # exit 0
stripbaseurl: rewrote 9 route registration(s) in api.gen.go
$ git diff --stat -- internal/api/   # empty
```

The generated file is byte-identical: oapi-codegen emits no `info.license` into
`api.gen.go`, so a licence edit to the document cannot drift the router. This
matters more than it sounds — `bin/prime`'s regenerate-and-diff step exists
because a stale generated router "answers the old routes under the new
document's promises".

**No test suite is in this diff's reach and I say so plainly rather than run one
for show.** The change is five prose/manifest files and no Go source. I grepped
the test tree for anything that reads a licence, the word MIT, or the README:
nothing does — the only hits are `LIMIT`, `OMITS` and `EMITTING` inside Go
comments. So `./bin/prime` and `./bin/prime --db` would exercise the same tests
against the same source they were last run against, and I did not run them.
The two commands above are the ones that bear on this diff, and both were run.

## Where I disagree with the brief, flagged rather than implemented

**`Change Licence: Apache-2.0` is in tension with BSL's own covenant.** Covenant
1 (`LICENSE:63-68`) requires the Change Licence to be "the GPL Version 2.0 or
any later version, or a license that is compatible with GPL Version 2.0 or a
later version". The FSF position is that Apache-2.0 is compatible with GPLv3 and
**not** with GPLv2, so Apache-2.0 does not satisfy the covenant as literally
written. Two things keep this from being a blocker: the covenants bind the
Licensor to MariaDB rather than conditioning the grant to the recipient, so the
licence is not invalidated; and the licence text explicitly allows "additional
Change Licenses without limitation". I implemented the brief's value — a
reversal of a decision the brief took is not the worker's to make — and I am
flagging it because the value in the file is the one a reader will audit.
GPL-3.0-or-later or Apache-2.0-with-an-additional-change-licence both satisfy
it. Manager's call.

**`name: BUSL-1.1` in `openapi/v1.yaml:90` uses the SPDX identifier, not the
licence's own name.** muse's equivalent block carries the short identifier in
all three fields (`name`, `identifier`, `url`), so this matches the one existing
fleet shape; the long name is in the comment on the line above. If the other
services workers land a different spelling, the field is one line and the
uniformity is the manager's to normalise.

**I did not add a contact for alternative licensing arrangements.** The BUSL
parameters template has a line for it and the brief did not name a mailbox.
Leaving it out keeps the grant comparable across the fleet — a reader diffing
two `LICENSE` files sees the Licensed Work differ and nothing else, which is
what the brief asked for. `darkroom@cafaye.com` (`cafaye.yml:88`, `owner.contact`)
is the obvious value if the fleet decides it wants the line.

## Not defects — found, not fixed, out of scope

- **`AGENTS.md:419` and `AGENTS.md:484` still describe `Cargo.toml` and
  `rust-toolchain.toml` as part of this repository's pinning.** The tree is Go;
  `AGENTS.md:13` says so in its own first paragraph. Pre-existing document
  staleness, unrelated to a licence grant, and not mine to edit in a licence
  packet.
- **The brief's fleet wording is uniform by construction, not by coordination.**
  I wrote the canonical BSL 1.1 text with the parameters above. If the sibling
  workers (`lic-services-b-billing`, `lic-services-b-muse`, `lic-services-a-*`)
  chose a different parameter order or a different Additional Use Grant
  spelling, the diff between two `LICENSE` files will show more than the
  Licensed Work. Nothing in this repository can enforce that.

## Definition of done, checked

- BSL `LICENSE` with Licensed Work `darkroom` — `LICENSE:3-9`. **Yes.**
- Manifest agreeing with it — `openapi/v1.yaml:84-92` carries the identifier;
  `cafaye.yml:25-43` carries the grant and the reason a field is impossible.
  **Yes, with the schema gap recorded above.**
- No MIT or AGPL string left in a manifest — `grep -rn "MIT\|AGPL"` over the
  tree returns prose only: the history in `README.md:1026,1040`, the two
  `CHANGELOG.md` entries and one `openapi/v1.yaml:85` comment, every one of
  which is a statement about the past. `cafaye.yml` contains neither word.
  **Yes.**
- The transition stated in prose — `README.md:1016-1058`, the commit message,
  and `CHANGELOG.md:16-55`. **Yes.**
- Nothing else touched — `git show --numstat HEAD` lists five files.
  **Yes.**
