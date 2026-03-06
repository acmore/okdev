# okdev Syncthing Sidecar

This image is used as the `sync.engine=syncthing` sidecar runtime.

Build locally:

```bash
docker build -f infra/syncthing/Dockerfile -t okdev-syncthing:dev infra/syncthing
```

Run locally:

```bash
docker run --rm -p 8384:8384 -p 22000:22000 okdev-syncthing:dev
```
