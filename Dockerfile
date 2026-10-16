# Build context = the repository root:
#   docker build -t msvcservice:local .
# One image, two shapes: `serve` (default) and `worker`, selected by the container command.
# The database schema is NOT baked or migrated here — db/*.sql is applied by hand (db/README.md).
#
# Base images are pinned by digest (multi-arch index): a tag is a moving pointer, and a rebuild of
# the same commit must not silently pick up a different toolchain or base. Renovate (renovate.json)
# proposes digest updates as reviewable PRs.
FROM golang:1.27-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
ARG VERSION=dev
WORKDIR /src

# Dependencies first, so a code change does not invalidate the module download layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/msvcservice ./cmd/msvcservice

# distroless static: no shell, no package manager, runs as the nonroot user (65532). The binary
# is static (CGO off) and needs only CA roots and tzdata, which the image carries.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS runtime
WORKDIR /app
COPY --from=build /out/msvcservice /app/msvcservice
ENV APP_ENV=production
# 8080: the API listener. 8081: probes and /metrics (the admin listener, never behind the ingress).
EXPOSE 8080 8081
USER nonroot:nonroot
ENTRYPOINT ["/app/msvcservice"]
CMD ["serve"]
