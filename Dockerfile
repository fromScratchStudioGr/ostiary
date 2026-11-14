# Builds and runs one of this module's server binaries in isolation. Which
# one is picked via the SERVICE build arg (cmd/${SERVICE}) — "gateway" (the
# LLM proxy, default) or "admin" (the admin panel backend). They're built
# from the same image so both containers share one Dockerfile, but each
# runs as its own process/container with its own resource limits. Postgres
# and Redis run as separate services (see docker-compose.yml).
#
# The builder runs on the build machine's own platform and cross-compiles for
# the target one, so a multi-platform build never compiles under emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.0 AS builder
ARG SERVICE=gateway
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# VERSION is the release being built, logged at startup and reported by
# /health. The image workflow sets it from the tag.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-X main.version=${VERSION}" -o /out/service ./cmd/${SERVICE}

FROM alpine:3.24
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 app \
    && adduser -S -u 10001 -G app -H -s /sbin/nologin app
COPY --from=builder /out/service /service

USER 10001:10001

EXPOSE 8080 8081
ENTRYPOINT ["/service"]
