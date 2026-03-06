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
    && archive="syncthing-${SYNCTHING_VERSION}-${os}-${syncthing_arch}.tar.gz" \
    && base_url="https://github.com/syncthing/syncthing/releases/download/${SYNCTHING_VERSION}" \
    && curl -fsSL "${base_url}/sha256sum.txt" -o /tmp/sha256sum.txt \
    && curl -fsSL "${base_url}/${archive}" -o /tmp/syncthing.tar.gz \
    && grep " ${archive}$" /tmp/sha256sum.txt | sha256sum -c - \
    && tar -xzf /tmp/syncthing.tar.gz -C /tmp \
    && install -m 0755 "/tmp/syncthing-${SYNCTHING_VERSION}-${os}-${syncthing_arch}/syncthing" /usr/local/bin/syncthing \
    && rm -rf /tmp/syncthing* /tmp/sha256sum.txt

RUN addgroup -S syncthing && adduser -S syncthing -G syncthing
USER syncthing
WORKDIR /workspace

EXPOSE 8384 22000

CMD ["syncthing", "-home", "/var/syncthing", "-no-browser", "-gui-address=0.0.0.0:8384", "-no-restart", "-skip-port-probing", "-listen=tcp://0.0.0.0:22000"]
