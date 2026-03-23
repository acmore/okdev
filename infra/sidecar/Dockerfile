FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /okdev-sshd ./cmd/okdev-sshd/

FROM alpine:3.21

ARG SYNCTHING_VERSION=v1.29.7

# Install syncthing dependencies and helpers used by the sidecar runtime.
RUN apk add --no-cache ca-certificates curl tar util-linux

# Install syncthing
RUN arch="$(uname -m)" \
    && case "$arch" in \
         x86_64) syncthing_arch="amd64" ;; \
         aarch64) syncthing_arch="arm64" ;; \
         *) echo "unsupported architecture: $arch" >&2; exit 1 ;; \
       esac \
    && os="linux" \
    && archive="syncthing-${os}-${syncthing_arch}-${SYNCTHING_VERSION}.tar.gz" \
    && base_url="https://github.com/syncthing/syncthing/releases/download/${SYNCTHING_VERSION}" \
    && curl -fsSL "${base_url}/sha256sum.txt.asc" -o /tmp/sha256sum.txt \
    && curl -fsSL "${base_url}/${archive}" -o "/tmp/${archive}" \
    && (cd /tmp && grep " ${archive}$" sha256sum.txt | sha256sum -c -) \
    && tar -xzf "/tmp/${archive}" -C /tmp \
    && install -m 0755 "/tmp/syncthing-${os}-${syncthing_arch}-${SYNCTHING_VERSION}/syncthing" /usr/local/bin/syncthing \
    && rm -rf /tmp/syncthing* /tmp/sha256sum.txt

COPY --from=builder /okdev-sshd /usr/local/bin/okdev-sshd

COPY infra/sidecar/dev-tmux.conf /etc/okdev-dev.tmux.conf
COPY infra/sidecar/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 8384 22000

ENTRYPOINT ["/entrypoint.sh"]
