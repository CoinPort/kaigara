# Kaigara

Kaigara is a process wrapper. It fetches configuration from HashiCorp Vault, injects it into a
child process's environment (and onto disk as files), streams the child's STDOUT/STDERR to Redis,
and restarts the child when its configuration changes in Vault.

It is the entrypoint wrapper used by every Ruby/Go service in the OpenDAX stack:

```
command: bash -c "kaigara bundle exec puma --config config/puma.rb"
```

---

## ⚠️ Fork status — read this first

This is CoinPort's internal clone of [openware/kaigara](https://github.com/openware/kaigara),
which is no longer maintained upstream. **The fork is not yet wired into anything.** Three facts
matter before you change code here:

1. **The checked-out branch is not the source of the deployed binary.**
   `1-0-stable` is `0.1.31` plus one commit. Peatio and Barong deploy `0.1.34`, which contains
   **seven commits this branch does not have** — the encryptor interface + AES driver, the storage
   interface, the MySQL storage driver, the `kaienv` CLI, and golangci-lint in CI.

2. **`origin/master` is a different, much newer codebase** — 52 commits ahead, tagged up to
   `v1.0.36`. It is the `v1.0.x` line referenced by the commented-out
   `#ARG KAIGARA_VERSION=v1.0.28` in the Peatio/Barong Dockerfiles. It restructures the CLI into a
   single `kai` binary and adds `pkg/encryptor`, `pkg/sql`, and `pkg/k8s`.

3. **Production still downloads binaries from the upstream GitHub repo at image build time**, not
   from this fork.

See [docs/OPERATIONS.md](docs/OPERATIONS.md) for the full version map, and
[docs/IMPROVEMENTS.md](docs/IMPROVEMENTS.md) for what to do about it.

---

## Documentation

| Document | Contents |
| --- | --- |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | How Kaigara works: startup sequence, Vault layout, scopes, encryption, log streaming, config reload |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | How Kaigara is deployed in the CoinPort stack, version map, secrets workflow, Vault setup, troubleshooting |
| [docs/IMPROVEMENTS.md](docs/IMPROVEMENTS.md) | Prioritised list of bugs, security issues, and suggested improvements |
| [CLAUDE.md](CLAUDE.md) | Orientation for AI coding agents working in this repo |

## Features

* Fetch configuration from Vault and inject it into the daemon's environment
* Materialise configuration **files** from Vault (the `KFILE_` convention)
* Encrypt the `secret` scope at rest using Vault's transit engine
* Publish the daemon's STDOUT and STDERR to Redis pub/sub
* Publish a Redis heartbeat key while the daemon is alive
* Restart the daemon when its Vault secrets are updated

## Binaries

| Binary | Purpose |
| --- | --- |
| `kaigara` | The process wrapper. `kaigara <command> [args...]` |
| `kaisave` | Bulk-load a YAML file of secrets into Vault |
| `kaidump` | Dump all secrets for the deployment out of Vault into YAML |
| `kaidel` | Delete a single key from one or more scopes |
| `kaitail` | Tail the Redis log stream published by `kaigara` |

Build all of them (`kaisave` is cross-compiled for four platforms):

```sh
make build      # outputs to bin/
make clean
```

## Quick start

```sh
export KAIGARA_VAULT_ADDR=http://127.0.0.1:8200
export KAIGARA_VAULT_TOKEN=s.ozytsgX1BcTQaR5Y07SAd2VE
export KAIGARA_DEPLOYMENT_ID=opendax_uat
export KAIGARA_APP_NAME=peatio

# Optional
export KAIGARA_REDIS_URL=redis://localhost:6379/0
export KAIGARA_SCOPES=public,private,secret
export KAIGARA_IGNORE_GLOBAL=true

kaigara service_command arguments...
```

A local Vault + Redis pair for development is provided:

```sh
docker compose -f etc/backend.yml up -d
```

Vault must have both the `kv` v2 and `transit` engines enabled. On a fresh Vault:

```sh
vault secrets enable -version=2 -path=secret kv
vault secrets enable transit
```

> **Warning:** Kaigara requires **kv version 2**. With kv v1 there is no `metadata` in the read
> response and Kaigara will panic on startup with a message telling you to enable v2.

## Configuration reference

All configuration is by environment variable. There is no config file.

| Variable | Default | Read by | Description |
| --- | --- | --- | --- |
| `KAIGARA_VAULT_ADDR` | `http://127.0.0.1:8200` | all | Vault address |
| `KAIGARA_VAULT_TOKEN` | *(required)* | all | Vault token. Kaigara auto-renews it if it is renewable |
| `KAIGARA_DEPLOYMENT_ID` | *(required)* | all | Vault path prefix and transit key prefix. One per environment |
| `KAIGARA_APP_NAME` | — | `kaigara` | Comma-separated app names to load, e.g. `peatio,peatio_daemons` |
| `KAIGARA_SCOPES` | `public,private,secret` | `kaigara`, `kaidump` | Comma-separated scopes to load |
| `KAIGARA_REDIS_URL` | — | `kaigara`, `kaitail` | Redis URL for log streaming. If unset, log streaming is disabled and output only goes to stdout |
| `KAIGARA_IGNORE_GLOBAL` | — | `kaigara` | `true` stops the watcher from restarting the daemon when the `global` app's secrets change. Global secrets are still *loaded* at startup |
| `KAIGARA_SECRET_STORE` | `vault` | — | **Parsed but never used on this branch.** Vault is the only backend |
| `VAULT_CACERT` | — | all | Standard Vault env var. Honoured — the Vault SDK's default HTTP client picks it up |

`KAIGARA_*` variables are **stripped** from the child process's environment. The daemon never sees
Kaigara's own configuration.

## Scopes

Kaigara reads secrets under three conventional scopes. They are just Vault paths; the only one
Kaigara treats specially is `secret`.

| Scope | Encrypted at rest | Intended for |
| --- | --- | --- |
| `public` | No | Values safe to expose, e.g. to a frontend |
| `private` | No | Backend configuration that is not a credential |
| `secret` | **Yes** — Vault transit | Credentials. Values **must be strings** |

Every app's environment is built from the `global` app first, then the apps named in
`KAIGARA_APP_NAME`, in order. Later values overwrite earlier ones.

## Secret to environment variable mapping

* Keys are **upper-cased**: `key1` → `KEY1=value`.
* Maps and slices are **skipped** — they cannot be represented in an environment variable. They are
  still stored in Vault and readable by `kaidump`.
* Booleans and numbers are stringified (`true`, `1337`).
* Keys matching `KFILE_<NAME>_PATH` / `KFILE_<NAME>_CONTENT` (case-insensitive) are written to disk
  as a file instead of being exported. The directory is created with mode `0750` and the file with
  mode `0640`.

Example — writing `config/config.json` for the daemon:

```yaml
kfile_name_path: config/config.json
kfile_name_content: '{"app":"example"}'
```

## Managing secrets

### Bulk write with `kaisave`

Write the secrets into a YAML file (see [`secrets.yaml`](secrets.yaml) for the shape) and run:

```sh
kaisave --filepath secrets.yaml
```

```yaml
secrets:
  global:
    scopes:
      public: {}
      private:
        global_key1: value1
      secret:
        global_key1: just a string
  peatio:
    scopes:
      public:
        key1: value1
      private:
        key3:
          key4: value4        # nested maps are stored but NOT exported to env
      secret:
        key1: value1
```

Two rules that cause almost every `kaisave` failure:

* **Every scope a component uses must be initialised**, even if empty — `public: {}`.
* **Quote numeric and boolean values** (`"4269"`, `"true"`). Errors like
  `interface{} is bool|json.Number|etc` are always an unquoted value. Values in the `secret` scope
  must be strings.

### Dump with `kaidump`

```sh
kaidump -a outputs.yaml
```

Iterates every app under `KAIGARA_DEPLOYMENT_ID` and every scope in `KAIGARA_SCOPES`.

> **Warning:** `kaidump` **decrypts** the `secret` scope, prints the whole dump to stdout, and
> writes it world-readable (`0644`). Treat the output as a plaintext credential file. See
> [docs/IMPROVEMENTS.md](docs/IMPROVEMENTS.md#s2).

### Delete with `kaidel`

```sh
kaidel -a peatio -k some_key -s public,private,secret
```

### Inspect with the Vault CLI

```sh
vault list secret/metadata/$KAIGARA_DEPLOYMENT_ID                              # app names
vault list secret/metadata/$KAIGARA_DEPLOYMENT_ID/$KAIGARA_APP_NAME            # scopes
vault read secret/data/$KAIGARA_DEPLOYMENT_ID/$KAIGARA_APP_NAME/<scope> -format=yaml
vault delete secret/data/$KAIGARA_DEPLOYMENT_ID/$KAIGARA_APP_NAME/<scope>
```

Note that values in the `secret` scope are transit ciphertext (`vault:v1:...`) when read this way.

### Tail logs with `kaitail`

```sh
kaitail -c 'log.peatio.*' -n
```

## Vault policy

[`etc/kaigara.hcl`](etc/kaigara.hcl) is the policy template. Replace `deployment_id` with your
`KAIGARA_DEPLOYMENT_ID`. In the OpenDAX repo this is generated from
`templates/config/vault/kaigara.hcl.erb`.

## Testing

```sh
go test ./cmd/...          # no external dependencies
go test ./...              # requires a live Vault
```

Most tests talk to a real Vault. Start one and point the env at it:

```sh
docker run --rm -d -p 8200:8200 -e VAULT_DEV_ROOT_TOKEN_ID=root-token hashicorp/vault:1.15
export KAIGARA_VAULT_ADDR=http://127.0.0.1:8200 KAIGARA_VAULT_TOKEN=root-token
vault secrets enable transit
go test ./...
```

Note that `pkg/vault` is a **separate Go module** and is not covered by a root-level `go test ./...`.
It is also not what the binaries compile against — see
[docs/IMPROVEMENTS.md](docs/IMPROVEMENTS.md#c1).

## License

MIT — see [LICENSE](LICENSE). Copyright (c) 2020 Openware.
