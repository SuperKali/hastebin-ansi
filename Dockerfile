# Base images are pinned by digest, with the tag in the comment above each one.
# Dependabot (.github/dependabot.yml) proposes new digests every week.

# The Go builder runs on the build host and cross-compiles for the target platform
# golang:1.26-alpine
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308 AS builder

ARG TARGETOS
ARG TARGETARCH

# The version reported in the log and /api/info: the release tag or the commit
ARG VERSION=dev

WORKDIR /src

# Install necessary dependencies
RUN apk add --no-cache ca-certificates && update-ca-certificates

# Copy go mod and project files
COPY go.* ./
RUN go mod download
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -ldflags "-s -w -X main.version=${VERSION}" -o ./hastebin ./cmd/main.go

# Image for Pterodactyl (Wings): docker build --target pterodactyl .
# alpine:3.22
FROM alpine:3.22@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f AS pterodactyl

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 988 -S container \
    && adduser -u 988 -G container -h /home/container -s /bin/sh -D container

COPY --from=builder /src/hastebin /usr/local/bin/hastebin
COPY about.md /opt/hastebin/about.md
COPY --chmod=0755 pterodactyl/entrypoint.sh /entrypoint.sh

USER container
ENV USER=container HOME=/home/container
WORKDIR /home/container

ENTRYPOINT ["/entrypoint.sh"]

# Default image, keep it the last stage so a plain docker build produces it
# gcr.io/distroless/static:nonroot
FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
COPY --from=builder /etc/ssl/certs /etc/ssl/certs

# Copy the binary from the builder stage
COPY --from=builder /src/hastebin /bin/hastebin

# Run as the distroless nonroot user
USER 65532:65532

CMD ["/bin/hastebin", "--config", "/app/config.yaml"]
