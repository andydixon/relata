# Relata production image.
#
# Multi-stage: a full toolchain builds a static binary, and the runtime stage
# contains that binary, CA certificates and nothing else. There is no shell and
# no package manager in the final image, so a compromised process has very
# little to work with.

# --- Build stage -----------------------------------------------------------

FROM golang:1.26.5-alpine AS build

# git is needed only if a dependency resolves via VCS; ca-certificates lets the
# module proxy be reached over TLS.
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# Dependencies are copied and downloaded first so that a source-only change
# does not invalidate the module cache layer.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

ARG VERSION=dev
ARG TARGETARCH

# CGO is disabled so the result is a genuinely static binary that runs on
# scratch. Symbols and DWARF are stripped; the build is reproducible given the
# same inputs (-trimpath removes local filesystem paths).
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/relata \
      ./cmd/relata

# Confirm the binary is static and runnable before it reaches the runtime stage.
RUN /out/relata version

# --- Runtime stage ---------------------------------------------------------

FROM scratch

# Certificates for outbound TLS (a remote Qdrant or an OpenAI-compatible
# provider), and zone data so timestamps format correctly.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo

COPY --from=build /out/relata /usr/local/bin/relata

# A fixed non-root UID/GID. There is no /etc/passwd in a scratch image, so the
# numeric form is used; the upload volume must be writable by this UID.
USER 65532:65532

# The upload directory is the only path Relata writes to. It is declared as a
# volume so that the root filesystem can be mounted read-only.
VOLUME ["/data/uploads"]

ENV RELATA_UPLOAD_DIR=/data/uploads \
    RELATA_HTTP_ADDR=0.0.0.0:8080 \
    RELATA_LOG_JSON=true

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/relata"]
CMD ["serve"]
