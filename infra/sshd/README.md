# okdev SSH Sidecar

This image provides an SSH daemon sidecar for `spec.ssh.mode=sidecar`.

Build locally:

```bash
docker build -f infra/sshd/Dockerfile -t okdev-sshd:dev infra/sshd
```

Run locally:

```bash
docker run --rm -p 2222:22 okdev-sshd:dev
```
