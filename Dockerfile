# The Go builder runs on the build host and cross-compiles for the target platform
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Install necessary dependencies
RUN apk add --no-cache ca-certificates && update-ca-certificates

# Copy go mod and project files
COPY go.* ./
RUN go mod download
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -ldflags "-s -w" -o ./hastebin ./cmd/main.go

# Image for Pterodactyl (Wings): docker build --target pterodactyl .
FROM alpine:3.22 AS pterodactyl

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
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /etc/ssl/certs /etc/ssl/certs

# Copy the binary from the builder stage
COPY --from=builder /src/hastebin /bin/hastebin

# Run as the distroless nonroot user
USER 65532:65532

CMD ["/bin/hastebin", "--config", "/app/config.yaml"]
