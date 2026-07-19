# Security Policy

## Reporting a vulnerability

Please report vulnerabilities privately via [GitHub Security Advisories](https://github.com/raiyanyahya/zapq/security/advisories/new) rather than public issues. You should get an initial response within a few days.

## Deployment guidance

ZapQ is designed to run **next to** your services, not on the public internet.

- Bind to a private interface or run it inside your service mesh / cluster network.
- Configure tokens in any multi-tenant environment. Use the scoped ones: give producers `ZAPQ_PRODUCER_TOKEN`, consumers `ZAPQ_CONSUMER_TOKEN`, and keep `ZAPQ_ADMIN_TOKEN` (which can clear queues and write files in the data dir) with your deploy tooling only. Without any token, anyone who can reach the port can read, write and clear every queue.
- Token comparison is constant-time; pass tokens via environment variables rather than flags so they don't appear in `ps`.
- Use `--tls-cert`/`--tls-key` (or a mesh sidecar) when traffic crosses hosts. This covers both listeners: the binary TCP protocol (`--tcp-addr`) sends tokens in cleartext just like HTTP does, so the same rule applies.
- Message payloads are stored in RAM and, if snapshots are enabled, on disk (file mode 0600, CRC-checked but **not encrypted**). Don't queue secrets you wouldn't write to disk, or point `--data-dir` at an encrypted volume.
- `/persist` and `/load` are restricted to the data directory and reject path traversal, but they still let an admin-token holder write files there — treat that token like a deploy credential.

## Supply chain

- Release binaries ship with a `SHA256SUMS` file signed with cosign (keyless, verifiable against the GitHub Actions OIDC identity), plus an SPDX SBOM.
- Container images on GHCR are cosign-signed and built `FROM scratch` — no OS packages, no shell.
- CI runs govulncheck, CodeQL and OpenSSF Scorecard; Dependabot watches the (Actions-only) dependency surface.

Verify a release:

```bash
cosign verify-blob SHA256SUMS \
  --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp 'github.com/raiyanyahya/zapq' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum -c SHA256SUMS
```

## Supported versions

Only the latest release receives security fixes.
