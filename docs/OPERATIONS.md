# Kaigara Operations

How Kaigara is built, versioned, and deployed in the CoinPort stack.

## Version history

| Version | Line | Date | Notes |
| --- | --- | --- | --- |
| `0.2.1` | **CoinPort** | current | Dependency reduction, security fixes, green test suite. What all three consumers pin |
| `0.2.0` | **CoinPort** | 2026-08 | Parity with upstream `0.1.34`, plus the log-stream, signal-handling and restart fixes. No Openware dependency. Published from this repo |
| `0.1.34` | upstream | 2022-03 | What Peatio and Barong used to download from Openware. Last upstream release on the `0.1.x` line |
| `0.1.27` | upstream | 2021-11 | What OpenDAX's `kaisave.rake` used to pin |
| `v1.0.36` | upstream | 2023-07 | Head of an unrelated series — see below |

`0.2.x` is CoinPort's own line. Upstream owns `0.1.x`, so starting a new minor
keeps the namespaces distinct and makes it unambiguous which artifact a pin
refers to.

### The `v1.0.x` line

Upstream's `master` carries a `v1.0.x` series (head `v1.0.36`, July 2023) that
is a rewrite of the storage layer, not an upgrade path. **Do not swap a `1.0.x`
binary in without changing configuration**, because two defaults are inverted
relative to what this deployment relies on:

| Setting | `0.1.34` / `0.2.0` default | `v1.0.36` default |
| --- | --- | --- |
| `KAIGARA_STORAGE_DRIVER` | `vault` | **`sql`** |
| `KAIGARA_ENCRYPTOR` | `transit` | **`plaintext`** |

The encryptor default is the dangerous one. With `KAIGARA_STORAGE_DRIVER=vault`
but no `KAIGARA_ENCRYPTOR`, a `1.0.x` binary reads the `secret` scope and hands
the daemon the literal string `vault:v1:AbCd…` as its password — no error, just
wrong credentials everywhere.

Other differences: the `kaisave`/`kaidump`/`kaidel` binaries are replaced by a
single `kai` with subcommands, `pkg/logstream` is removed in favour of
`openware/pkg`, and the restart path became in-process. The commented-out
`#KAIGARA_ENCRYPTOR=plaintext` line in
`opendax/templates/config/kaigara.env.erb` is a half-finished migration to this
line, and is set to the value that would break decryption.

Adopting `1.0.x` is a deliberate migration, not a version bump.

## Where each consumer is pinned

| Consumer | File | Pin |
| --- | --- | --- |
| Peatio | `Dockerfile` | `KAIGARA_VERSION=0.2.1` from `CoinPort/kaigara` releases |
| Barong | `Dockerfile` | `ARG KAIGARA_VERSION=0.2.1` from `CoinPort/kaigara` releases |
| OpenDAX | `lib/tasks/kaisave.rake` | `KAISAVE_VERSION = '0.2.1'` |

All three previously downloaded from `openware/kaigara` at image-build time —
an unauthenticated dependency on an unmaintained third party, with no fallback
if the repo were archived.

### The `curl` trap

The old Dockerfile line was:

```dockerfile
RUN curl -Lo /usr/bin/kaigara https://github.com/openware/kaigara/releases/download/${KAIGARA_VERSION}/kaigara
```

Without `-f`, curl exits **0** on a 404 and writes the error body to the target.
`chmod +x` then marks a 9-byte text file executable and the image builds green,
failing only when a container starts. Measured against the missing asset:

```
curl -Lo  → exit 0, 9-byte "Not Found" file written
curl -fLo → exit 22, no file, RUN step fails
```

This was not hypothetical: upstream's `v1.0.36` renamed the asset from `kaigara`
to `kaigara_linux_amd64`, so anyone bumping to it would have built a broken
image silently. Both Dockerfiles now use `-f`. Neither verifies a checksum or
smoke-tests the binary yet — Barong tracks that as an open item.

## Deployment topology

Kaigara is baked into the service images and used as the entrypoint wrapper:

```yaml
peatio:
  env_file:
    - ../config/kaigara.env      # Kaigara's own config
    - ../config/peatio.env       # service config
  command: bash -c "bin/link_config && kaigara bundle exec puma --config config/puma.rb"

barong:
  command: bash -c "kaigara bundle exec puma --config config/puma.rb"

sonic:
  entrypoint: /bin/sh -c "kaigara ./bin/sonic serve"
```

`config/kaigara.env` is rendered from `templates/config/kaigara.env.erb` and
supplies `KAIGARA_VAULT_TOKEN`, `KAIGARA_VAULT_ADDR`, `KAIGARA_DEPLOYMENT_ID`,
`KAIGARA_REDIS_URL` and `VAULT_CACERT`. `KAIGARA_APP_NAME` is set per service.

All services share a `KAIGARA_DEPLOYMENT_ID` (lower-cased `app.name` from
`config/app.yml`) and each gets a per-component Vault token.

Kaigara restarts a service by stopping the child and relying on the container's
restart policy, so every wrapped service must run with `restart: always`. They do.

## Secrets workflow

```
config/secrets.yaml  ──kaisave──▶  Vault  ──kaigara──▶  daemon env + files
        ▲                            │
        └──────────kaidump───────────┘
```

Driven by `rake kaisave:fetch` (downloads the binary) and `rake kaisave:save`
(pushes `config/secrets.yaml` using the `sonic_token`).

After `kaisave` writes a new version, each running Kaigara notices within 20–30
seconds and restarts its child, so **saving secrets rolls every affected
service**. Since `0.2.0` the poll interval is jittered, so services restart
staggered across that window rather than all on the same tick.

## Vault setup

Once per Vault instance:

```sh
vault secrets enable -version=2 -path=secret kv
vault secrets enable transit
```

Per deployment, load `etc/kaigara.hcl` with `deployment_id` replaced, then issue
a token per component. The policy grants:

* `read`/`list` on `secret/data/<deployment_id>/*` and `secret/metadata/…`
* `create`/`update`/`read`/`list` on `transit/keys/<deployment_id>_kaigara_*`
* `create`/`read`/`update` on `transit/encrypt/*` and `transit/decrypt/*`
* `update` on `auth/token/renew` and `auth/token/lookup`

Those grants are **deployment-wide**, so a component token can read and decrypt
any other component's secrets. The per-app transit key gives the appearance of
isolation without delivering it — [S3](IMPROVEMENTS.md#s3).

Issue renewable, periodic tokens (`-period=240h`). Kaigara renews automatically
only if the token is renewable; otherwise it stops being able to reload
configuration when the token expires.

## Building and releasing

```sh
make build      # host platform
make release    # all binaries x linux/darwin/windows x amd64/arm64 + SHA256SUMS
make check      # fmt, vet, build, unit tests
```

`make release` produces `<binary>_<os>_<arch>` assets plus unsuffixed `kaigara`
and `kaisave` aliases for consumers that fetch a bare name.

CI is `.github/workflows/ci.yml`:

| Job | Does |
| --- | --- |
| `check` | gofmt, go vet, build, golangci-lint |
| `test` | full suite against Vault, MySQL and PostgreSQL service containers |
| `vulncheck` | govulncheck, reported for visibility |
| `release` | on a tag, cross-compiles and publishes to GitHub Releases |

Release builds use Go 1.26, matching the `go` directive in `go.mod`. The
toolchain, not the directive, determines which standard-library CVEs land in the
shipped binary; bump `GO_VERSION` in the workflow to pick up a newer one.

**To cut a release:** tag `0.2.x`, push the tag, and CI publishes. Consumers
pinning that version only work once the tag is pushed and the release job has
finished.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `Metadata not found. Make sure you have enabled KV v2` | The `secret/` mount is kv v1. Re-mount with `-version=2` |
| Startup failure mentioning `transit` | The `transit` engine is not enabled, or the token lacks `transit/keys/*` capabilities |
| Daemon receives `vault:v1:…` as a password | `KAIGARA_ENCRYPTOR` does not match how the data was written. Should be `transit` |
| `interface{} is bool` / `is json.Number` from `kaisave` | An unquoted boolean or number in `secrets.yaml`. Quote it |
| `SetEntry: <key> is not a string` | A non-string value in the `secret` scope |
| Secret exists but the daemon does not see it | It is a map or list — those are skipped. Flatten it, or use `KFILE_`. Check with `kaienv` |
| Service restarts every 20–30s | A version mismatch that never resolves. Try `KAIGARA_IGNORE_GLOBAL=true` |
| Service exits immediately, no logs | Kaigara fails before the child starts if Vault or Redis is unreachable. Check reachability from inside the container |
| Image builds fine but `kaigara` won't exec | Pre-`0.2.0` Dockerfile without `curl -f`. The release asset 404'd and the error page was saved |
| SQL driver: `too many clients already` | Fixed in `0.2.0` — `StorageService` leaked a connection pool per instantiation |
