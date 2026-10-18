# kit template — Go service image.
#
# The Rust version of this file is in git history at 3d85d0d^, and its header
# recorded two problems that are the whole reason this file is different:
#
#   - it ran `rustup target add x86_64-unknown-linux-musl` and then copied out of
#     `target/x86_64-unknown-linux-musl/release/darkroom` while its `cargo build`
#     had no `--target`, so the file it copied was not there and the image build
#     failed;
#   - and `ring` needed a C toolchain for that target, so a glibc `ubuntu`
#     runner could not link it either.
#
# THE RUST README LISTED BOTH UNDER "Not done" — "CI does not build the deployment
# target" — as a fix belonging to "a decision about the release pipeline rather
# than a fix that belongs in a CI packet". It is fixed here, and it is fixed BY
# THE REWRITE rather than by a flag: Go cross-compiles to a static binary with
# `CGO_ENABLED=0` and no musl target, no C toolchain and no `rustup`. There is no
# equivalent of the missing `--target`, because there is no equivalent of the
# problem.
#
# Two things carried over from kit's template and not negotiable here:
#
#   - Two stages. The builder has the toolchain; the runtime does not.
#   - A pinned, non-floating base. The Go version below is the one `go.mod`
#     declares and the one `mise.toml` pins, so a worktree on a newer toolchain
#     than the image builds with is the same failure the Rust header warned about,
#     one language over.
ARG GO_VERSION=1.26.1

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src

# The module files first, so the dependency layer caches. A change to `cmd/` or
# `internal/` does not re-download the dependency tree — and `go mod download`
# rather than a warm-up build, because a warm-up build compiles the module's own
# packages and then throws the work away.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
COPY openapi ./openapi

# `CGO_ENABLED=0` is what makes the runtime stage possible at all: distroless
# ships no libc and no DNS resolver, so a cgo binary would fail at startup on a
# dynamic loader that is not there. The AWS SDK and pgx are both pure Go, so
# nothing here needs cgo.
#
# `-trimpath` and `-ldflags="-s -w"` are what `cargo`'s `strip` line was doing:
# the runtime stage gets a binary with no absolute build paths in it and no symbol
# table, which is both smaller and one less thing to leak about the build host.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/darkroom ./cmd/darkroom

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
WORKDIR /app
# A service that calls an OIDC provider's JWKS URL fails TLS without these, and
# distroless ships none.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/darkroom /app/darkroom
USER nonroot:nonroot
EXPOSE 8080
#
# THE TWO ENV LINES THAT CHANGED, and both are corrections rather than additions:
#
#   `PORT`      the Rust `Config` read `PORT` and never read `DARKROOM_PORT`, so
#               `ENV DARKROOM_PORT=8080` was a variable nothing read — a variable
#               this build ignores is a lie in a deployment manifest. The Go
#               `cmd/darkroom` reads `PORT`, so that is what is declared.
#   no DATABASE  the Rust binary read `DATABASE_URL`, the OWNER credential. The Go
#               binary reads `DARKROOM_DATABASE_URL` ONLY and refuses to start
#               without it; see `cmd/darkroom/config.go`'s header for why the
#               owner is not an acceptable fallback. A default here would be an
#               owner credential baked into an image.
ENV PORT=8080 \
    DARKROOM_BIND_ADDR=0.0.0.0 \
    DARKROOM_OBJECT_STORE=s3 \
    DARKROOM_LOG_FORMAT=json
# No HEALTHCHECK: distroless has no shell and no curl, so a HEALTHCHECK here would
# either be a lie or need a binary copied in to check a binary. Liveness and
# readiness are /healthz and /readyz, and the orchestrator polls them.
ENTRYPOINT ["/app/darkroom"]