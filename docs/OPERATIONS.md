# Kaigara Operations

How Kaigara is built, versioned, and deployed in the CoinPort stack.

## Version map

This is the source of the "which version should we use" confusion. There are **two parallel
release lines** in the same repository, with different tag formats, different code, and different
release asset names.

| | `0.1.x` line | `1.0.x` line |
| --- | --- | --- |
| Branch | `1-0-stable` *(the checked-out branch)* | `origin/master` |
| Tag format | `0.1.34` (no `v`) | `v1.0.36` (with `v`) |
| Latest tag | `0.1.34` — 2022-03-25 | `v1.0.36` — 2023-07-26 |
| CLI shape | `kaigara`, `kaisave`, `kaidump`, `kaidel`, `kaitail` | `kaigara` + one unified `kai` binary with `save`/`dump`/`del`/`env` subcommands |
| Backends | Vault only | Vault, SQL (`pkg/sql`), Kubernetes (`pkg/k8s`) |
| Encryption | Vault transit, hard-wired | Pluggable `pkg/encryptor`: `transit`, `aes`, `plaintext` |
| On secret change | `SIGKILL`s the child and relies on `restart: always` | Restarts the child in-process |
| **Deployed today** | **Yes** — Peatio and Barong pin `0.1.34` | No — present as a commented-out `#ARG KAIGARA_VERSION=v1.0.28` |

> **The tag names are misleading.** The branch called `1-0-stable` is the **0.1.x** line, not the
> 1.0 line. `git describe` on this branch returns `0.1.31-1-gcf0bdfe`.

### Where each consumer is pinned

| Consumer | File | Pin | Notes |
| --- | --- | --- | --- |
| Peatio | `Dockerfile:13` | `KAIGARA_VERSION=0.1.34` | Downloaded from openware's GitHub releases at image build |
| Barong | `Dockerfile:26` | `ARG KAIGARA_VERSION=0.1.34` | Same, with `#ARG KAIGARA_VERSION=v1.0.28` commented out |
| OpenDAX | `lib/tasks/kaisave.rake` | `KAISAVE_VERSION = '0.1.27'` | Downloads `kaisave_<platform>` at task run time |
| This fork | `1-0-stable` @ `cf0bdfe` | `0.1.31` + 1 commit | **Not built, not published, not consumed by anything** |

### What `1-0-stable` is missing relative to the deployed `0.1.34`

Seven commits are in the deployed binary but **not** on this branch:

```
7bf218e Fix: Use deploymentID instead of passed database name (#55)
05df6c4 Feature: Add kaienv cli tool (#54)
1636269 Feature: Storage driver unit test
e2763a0 Enhancement: Add golangci-lint to the CI
22547d2 Feature: Add MySQL storage driver
20206de Feature: Add storage interface
ad1b8c3 Feature: Add encryptor interface + AES driver
```

Conversely this branch carries one commit that `0.1.34` does not: `cf0bdfe` (Vault API tests +
go.mod cleanup).

This explains why `templates/config/kaigara.env.erb` in OpenDAX has commented-out
`KAIGARA_ENCRYPTOR`, `KAIGARA_ENCRYPTOR_AES_KEY`, and `KAIGARA_DATABASE_*` settings that are
undocumented and unsupported by the code in this working tree — those options come from the
storage/encryptor work that landed in `0.1.32`–`0.1.34` and matured in `1.0.x`.

### Release asset naming changed at v1.0.36

The Peatio/Barong Dockerfiles fetch a bare `kaigara` asset:

```dockerfile
RUN curl -Lo /usr/bin/kaigara https://github.com/openware/kaigara/releases/download/${KAIGARA_VERSION}/kaigara
```

| Tag | `kaigara` asset | Result of the command above |
| --- | --- | --- |
| `0.1.34` | present | works |
| `v1.0.28` – `v1.0.35` | present | works |
| `v1.0.36` | **absent** — renamed to `kaigara_linux_amd64` | **404** |

Because `curl` is invoked **without `-f`**, a 404 does not fail the build. Curl writes GitHub's
error page to `/usr/bin/kaigara`, `chmod +x` marks it executable, and the image builds green — then
every container fails at startup with an exec format error. Add `-f` (or `--fail-with-body`) to
both Dockerfiles regardless of which version you settle on.

## Deployment topology

Kaigara is baked into the service images and used as the entrypoint wrapper. From
`opendax/compose/app.yaml`:

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

`config/kaigara.env` is rendered from `opendax/templates/config/kaigara.env.erb` and supplies
`KAIGARA_VAULT_TOKEN`, `KAIGARA_VAULT_ADDR`, `KAIGARA_DEPLOYMENT_ID`, `KAIGARA_REDIS_URL`, and
`VAULT_CACERT`. `KAIGARA_APP_NAME` is set per service.

Every service shares the same `KAIGARA_DEPLOYMENT_ID` (the lower-cased `app.name` from
`config/app.yml`), and each gets a per-component Vault token whose policy is rendered from
`opendax/templates/config/vault/kaigara.hcl.erb`.

Because Kaigara restarts a service by killing it, all Kaigara-wrapped services must run with
`restart: always`. They do.

## Secrets workflow

```
config/secrets.yaml  ──kaisave──▶  Vault  ──kaigara──▶  daemon env + files
        ▲                            │
        └──────────kaidump───────────┘
```

In OpenDAX this is driven by `rake kaisave:fetch` (downloads the binary) and `rake kaisave:save`
(pushes `config/secrets.yaml`, using the `sonic_token`). To pull the current state back out, run
`kaidump` with the same deployment ID.

After `kaisave` writes a new version, every running Kaigara notices the version bump within 20
seconds and kills its child, so **saving secrets triggers a rolling restart of every affected
service**. Plan writes accordingly.

## Vault setup

One-time, per Vault instance:

```sh
vault secrets enable -version=2 -path=secret kv
vault secrets enable transit
```

Per deployment, load the policy from `etc/kaigara.hcl` (or the OpenDAX ERB template) with
`deployment_id` replaced by your `KAIGARA_DEPLOYMENT_ID`, then issue a token per component.

The policy grants:

* `read`/`list` on `secret/data/<deployment_id>/*` and `secret/metadata/<deployment_id>/*`
* `create`/`update`/`read`/`list` on `transit/keys/<deployment_id>_kaigara_*`
* `create`/`read`/`update` on `transit/encrypt/*` and `transit/decrypt/*`
* `update` on `auth/token/renew` and `auth/token/lookup`

Note that the read/list grants are **deployment-wide**, not per app. A token issued to Peatio can
read Barong's `secret` scope ciphertext. It cannot *decrypt* it, because the transit key is per
app — but the transit grant is also wildcarded to `<deployment_id>_kaigara_*`, so in practice a
component token can decrypt any other component's secrets. Tightening this to
`transit/decrypt/<deployment_id>_kaigara_<component>` is
[S3 in IMPROVEMENTS.md](IMPROVEMENTS.md#s3).

Kaigara auto-renews its token if the token is renewable. Issue renewable, periodic tokens
(`-period=240h`) or Kaigara will stop being able to reload configuration when the token expires.

## Building and releasing

```sh
make build     # bin/kaigara, bin/kaitail, bin/kaidump, bin/kaidel, bin/kaisave_<os>_<arch>
make clean
```

`kaisave` is cross-compiled for `darwin/arm64`, `darwin/amd64`, `linux/amd64`, and
`windows/amd64` by `build-kaisave.sh`. `kaigara` itself is built **only for the host platform** —
if you start publishing releases from this fork, that asymmetry needs fixing.

CI is `.drone.yml`, which:

* runs `go test ./...` against a `vault:1.5.3` dev service on **`golang:1.14`**,
* bumps and pushes a patch tag on `master`,
* on tag, builds and publishes to GitHub Releases via `ghr`.

**None of this works for the fork.** It targets the `master` branch (this repo's default branch is
`1-0-stable`), authenticates with openware's Drone secrets, and publishes to
`${DRONE_REPO_NAMESPACE}/${DRONE_REPO_NAME}`. See [I1 in IMPROVEMENTS.md](IMPROVEMENTS.md#i1).

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| Panic: `Metadata not found. Make sure you have enabled KV v2` | The `secret/` mount is kv v1. Re-mount with `-version=2` |
| Panic on startup mentioning `transit` | The `transit` engine is not enabled, or the token lacks `transit/keys/*` capabilities |
| `interface{} is bool` / `is json.Number` from `kaisave` | An unquoted boolean or number in `secrets.yaml`. Quote it |
| `secretStore.SetSecret: <key> is not a string` | A non-string value in the `secret` scope. Only strings can be encrypted |
| A secret exists in Vault but the daemon does not see it | It is a map or a list — those are skipped when building the environment. Flatten it, or use the `KFILE_` convention |
| Service restarts every 20s | A version mismatch that never resolves. Usually a scope listed in `KAIGARA_SCOPES` that was written by another writer, or a `global` change loop — try `KAIGARA_IGNORE_GLOBAL=true` |
| Service exits immediately, no logs | Kaigara panics before the child starts if Vault or Redis is unreachable. Check `KAIGARA_VAULT_ADDR` / `KAIGARA_REDIS_URL` reachability from inside the container |
| Container takes 10s to stop | Expected. Kaigara does not forward `SIGTERM`; Docker waits for the timeout then `SIGKILL`s. See [C4](IMPROVEMENTS.md#c4) |
| Truncated or garbled lines in the Redis log stream | Known bug — see [C2](IMPROVEMENTS.md#c2) |
| Image builds fine but `kaigara` won't exec | The release asset 404'd and curl saved the error page. See [Release asset naming](#release-asset-naming-changed-at-v1036) |
