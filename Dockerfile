# The GitNest container image.
#
# Three stages, as docs/architecture/packaging.md section 7 specifies: a Node stage for
# the built assets, a Go stage for the binary, and a slim runtime. **Node is a
# build-time dependency only** — it is absent from the final image, which is the
# property the release smoke test checks ("the built `index.html` and a hashed asset,
# with no Node.js present in the image").
#
#   docker build -t gitnest .
#   docker run --rm -p 8080:8080 \
#     -e GITNEST_PUBLIC_URL=http://localhost:8080 \
#     -e GITNEST_DATABASE_URL=postgres://gitnest:gitnest@host.docker.internal:5432/gitnest?sslmode=disable \
#     -v gitnest-data:/var/lib/gitnest \
#     gitnest migrate
#   docker run ... gitnest serve

# ---- stage 1: the built assets -------------------------------------------------
#
# The stage is pinned by digest because a moving tag would make the image
# unreproducible: the same commit would produce a different binary next month.
FROM node:24-bookworm-slim AS web

WORKDIR /src/web

# The manifest and lockfile come first so a dependency change is the only thing that
# invalidates the install layer.
COPY web/package.json web/package-lock.json ./
# `npm ci` rather than `npm install`: it installs exactly the lockfile and fails when
# the two disagree, which is what makes the build reproducible.
RUN npm ci --no-audit --no-fund

COPY web/ ./
# The assets are built here and copied into the Go stage, so the Go build embeds a
# real build rather than the committed placeholder. A release that skipped this would
# ship an image whose interactive surfaces report "the frontend has not been built".
RUN npm run build

# ---- stage 2: the binary --------------------------------------------------------
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Module files first for the same layer-caching reason.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# The built assets replace the placeholder, so `//go:embed` picks up the real build.
RUN rm -rf web/dist && mkdir -p web/dist
COPY --from=web /src/web/dist/ ./web/dist/

# A static binary is not possible: the PostgreSQL driver and the OS user lookups need
# cgo-free networking, which is available, but the image already ships a glibc runtime
# for Git. Building without cgo keeps the binary portable across the Debian family.
ENV CGO_ENABLED=0
# `-trimpath` removes the build directory from the binary, which is what makes two
# builds of the same commit produce the same bytes (section 6).
RUN go build -trimpath -ldflags "-s -w" -o /out/gitnest ./cmd/gitnest

# ---- stage 3: the runtime -------------------------------------------------------
#
# A slim Debian-family image, for one reason: it carries a **Git executable** and CA
# certificates, both of which the server invokes or reads.
FROM debian:bookworm-slim

# Pinned by digest in a release build; the tag is used here so the file is readable,
# and packaging.md section 7 requires the digest for a published image.
RUN apt-get update \
 && apt-get install --no-install-recommends --yes \
      git \
      ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# A fixed non-root account. The packaging document names UID/GID 10001, so a volume
# written by an earlier image keeps working and an operator can predict the owner.
RUN groupadd --gid 10001 gitnest \
 && useradd --uid 10001 --gid 10001 --no-create-home --home-dir /var/lib/gitnest --shell /usr/sbin/nologin gitnest \
 && mkdir -p /var/lib/gitnest \
 && chown 10001:10001 /var/lib/gitnest

COPY --from=build /out/gitnest /usr/local/bin/gitnest

# The data directory, declared so an operator who forgets to mount a volume sees the
# data in the container rather than discovering later that it was written somewhere
# unexpected — and so `docker run` without a mount still works.
VOLUME ["/var/lib/gitnest"]

# The image's defaults. The public URL has no sensible default for a real deployment,
# so it is required from the environment rather than guessed; the data directory and
# the listener do have one.
ENV GITNEST_DATA_DIR=/var/lib/gitnest \
    GITNEST_HOST=0.0.0.0 \
    GITNEST_PORT=8080

USER 10001:10001
WORKDIR /var/lib/gitnest

EXPOSE 8080
# The SSH port is not exposed, because this release serves Git over HTTPS only. The
# phase that adds the SSH transport adds it here.

# The probe is the binary itself rather than `curl` or `wget`, which the image
# deliberately does not carry (packaging.md section 7 excludes anything beyond the
# binary, Git, its runtime dependencies, and CA certificates).
#
# It probes `/api/health`, which the packaging document names for the container check:
# "the process is alive". Switch it to `--ready` in an orchestrator whose scheduler
# should withhold traffic until the **database** is reachable, which is the stronger
# and usually correct gate — readiness is what `/api/ready` exists for.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/gitnest", "health"]

# The binary directly, in exec form, so `SIGTERM` reaches it and the bounded shutdown
# runs. A shell wrapper would swallow the signal and the container would be killed
# after its grace period instead of draining.
ENTRYPOINT ["/usr/local/bin/gitnest"]
CMD ["serve"]
