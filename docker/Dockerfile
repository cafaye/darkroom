# kit template — Rust service image.
#
# Copied from cafaye/kit/docker/Dockerfile.rust. The two things that are NOT
# optional in that template and are not negotiable here:
#
#   - Two stages. The builder has rustc + cargo; the runtime does not.
#   - `--locked` is not optional: without it cargo silently updates Cargo.lock
#     in the build, so two builds of the same commit are not the same binary.
#
# The RUST_VERSION default below must match mise.toml. kit says so in the
# template it ships: "keep the toolchain triple in sync with
# docker/Dockerfile.rust: a worktree on a newer rustc than the image builds on
# produces a binary CI never produced."
ARG RUST_VERSION=1.95

FROM rust:${RUST_VERSION}-slim-bookworm AS build
RUN rustup target add x86_64-unknown-linux-musl
WORKDIR /src

# Cargo resolves against the manifests first so the dependency layer caches. A
# change to src/ does not re-download the dependency tree.
COPY Cargo.toml Cargo.lock ./
RUN mkdir -p src \
    && echo 'fn main() {}' > src/main.rs \
    && echo '' > src/lib.rs \
    && cargo build --release --locked \
    && rm -rf src

COPY src ./src
COPY migrations ./migrations
# `--features s3` is the deployment build. The default build cannot construct a
# real object-storage client at all, which is what makes "tests never touch the
# network" a property of the dependency graph rather than a promise in a
# document. The image is the thing that turns it on.
RUN cargo build --release --locked --features s3 --bin darkroom \
    && cp "target/x86_64-unknown-linux-musl/release/darkroom" /out/darkroom \
    && strip "target/x86_64-unknown-linux-musl/release/darkroom"

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
WORKDIR /app
# A service that calls an OIDC provider's JWKS URL fails TLS without these, and
# distroless ships none.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/darkroom /app/darkroom
USER nonroot:nonroot
EXPOSE 8080
ENV DARKROOM_BIND_ADDR=0.0.0.0 \
    DARKROOM_PORT=8080 \
    DARKROOM_OBJECT_STORE=s3 \
    DARKROOM_LOG_FORMAT=json
# No HEALTHCHECK: distroless has no shell and no curl, so a HEALTHCHECK here
# would either be a lie or need a binary copied in to check a binary. Liveness
# and readiness are /healthz and /readyz, and the orchestrator polls them.
ENTRYPOINT ["/app/darkroom"]
