FROM alpine:3.20

ARG SYNCTHING_VERSION=v1.29.7

RUN apk add --no-cache ca-certificates curl tar \
    && update-ca-certificates \
    && arch="$(uname -m)" \
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

RUN addgroup -S syncthing && adduser -S syncthing -G syncthing
USER syncthing
WORKDIR /workspace

EXPOSE 8384 22000

CMD ["syncthing", "serve", "--home", "/var/syncthing", "--no-browser", "--gui-address=http://0.0.0.0:8384", "--no-restart", "--skip-port-probing"]
