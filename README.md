# Kaigara

Kaigara is a process wrapper. It fetches configuration from a secret store,
injects it into a child process's environment (and onto disk as files), streams
the child's STDOUT/STDERR to Redis, and restarts the child when its
configuration changes.

It is the entrypoint wrapper used by every service in the CoinPort OpenDAX stack:

```
command: bash -c "kaigara bundle exec puma --config config/puma.rb"
```

---

## Fork status

This is CoinPort's fork of [openware/kaigara](https://github.com/openware/kaigara),
which is unmaintained upstream. **Current release: `0.2.0`.**

`0.2.0` is behaviour parity with upstream `0.1.34` — the version Peatio and
Barong used to download from Openware's GitHub releases — plus fixes to the log
stream, signal handling and restart path. It is a drop-in replacement:
`KAIGARA_STORAGE_DRIVER` still defaults to `vault` and `KAIGARA_ENCRYPTOR` to
`transit`.

Two things worth knowing:

* **`0.2.x` is our line.** Upstream owns `0.1.x` and an unrelated `v1.0.x`
  series whose defaults differ dangerously — see
  [docs/OPERATIONS.md](docs/OPERATIONS.md#the-v10x-line).
* **No Openware Go dependency remains.** `pkg/encryptor` and `pkg/storage/*`
  are ordinary packages of this module, and `openware/pkg`'s `ika` and
  `database` are vendored in-tree.

## Documentation

| Document | Contents |
| --- | --- |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Startup sequence, storage model, scopes, encryption, log streaming, reload |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | Version history, deployment topology, release process, secrets workflow, troubleshooting |
| [docs/IMPROVEMENTS.md](docs/IMPROVEMENTS.md) | Findings — what is fixed and what remains |
| [CLAUDE.md](CLAUDE.md) | Orientation for AI coding agents |

## Features

* Fetch configuration from Vault (or a SQL database) and inject it into the
  daemon's environment
* Materialise configuration **files** from the store (the `KFILE_` convention)
* Encrypt the `secret` scope at rest — Vault transit, AES, or plaintext
* Publish the daemon's STDOUT and STDERR to Redis pub/sub
* Publish a Redis heartbeat key while the daemon is alive
* Restart the daemon gracefully when its secrets are updated

## Binaries

| Binary | Purpose |
| --- | --- |
| `kaigara` | The process wrapper. `kaigara <command> [args...]` |
| `kaisave` | Bulk-load a YAML file of secrets into the store |
| `kaidump` | Dump all secrets for the deployment into YAML |
| `kaidel` | Delete a single key from one or more scopes |
| `kaienv` | Print the environment Kaigara would build, without running anything |
| `kaitail` | Tail the Redis log stream published by `kaigara` |

```sh
make build      # host platform, into bin/
make release    # every binary for linux/darwin/windows on amd64+arm64, plus SHA256SUMS
make help       # list all targets
```

## Quick start

```sh
export KAIGARA_VAULT_ADDR=http://127.0.0.1:8200
export KAIGARA_VAULT_TOKEN=changeme
export KAIGARA_DEPLOYMENT_ID=opendax_uat
export KAIGARA_APP_NAME=peatio

# Optional
export KAIGARA_REDIS_URL=redis://localhost:6379/0
export KAIGARA_SCOPES=public,private,secret
export KAIGARA_IGNORE_GLOBAL=true

kaigara service_command arguments...
```

Local dependencies (Vault, Redis, MySQL, PostgreSQL — all bound to `127.0.0.1`):

```sh
make test-env-up
make test
make test-env-down
```

Vault needs both the `kv` v2 and `transit` engines. `make test-env-up` enables
transit for you; on a fresh Vault elsewhere:

```sh
vault secrets enable -version=2 -path=secret kv
vault secrets enable transit
```

> **Warning:** Kaigara requires **kv version 2**. With kv v1 there is no
> `metadata` in the read response and Kaigara fails at startup telling you so.

## Configuration reference

All configuration is by environment variable.

| Variable | Default | Description |
| --- | --- | --- |
| `KAIGARA_DEPLOYMENT_ID` | *(required)* | Storage path prefix and transit key prefix. One per environment |
| `KAIGARA_APP_NAME` | — | Comma-separated app names to load, e.g. `peatio,peatio_daemons` |
| `KAIGARA_SCOPES` | `public,private,secret` | Comma-separated scopes to load |
| `KAIGARA_STORAGE_DRIVER` | `vault` | `vault` or `sql` |
| `KAIGARA_ENCRYPTOR` | `transit` | `transit`, `aes`, or `plaintext`. **Must match how the data was written** |
| `KAIGARA_ENCRYPTOR_AES_KEY` | — | 16/24/32-byte key, required when `KAIGARA_ENCRYPTOR=aes` |
| `KAIGARA_VAULT_ADDR` | `http://127.0.0.1:8200` | Vault address |
| `KAIGARA_VAULT_TOKEN` | *(required for vault/transit)* | Vault token. Auto-renewed if renewable |
| `KAIGARA_REDIS_URL` | — | Redis URL for log streaming. Unset disables streaming; output still goes to stdout |
| `KAIGARA_IGNORE_GLOBAL` | — | `true` stops restarts triggered by the `global` app. Global secrets are still *loaded* |
| `KAIGARA_LOG_LEVEL` | `1` | gorm log level for the SQL driver |
| `VAULT_CACERT` | — | Standard Vault variable, honoured via the Vault SDK's default client |

With `KAIGARA_STORAGE_DRIVER=sql`, the database is configured by
`DATABASE_DRIVER`, `DATABASE_HOST`, `DATABASE_PORT`, `DATABASE_USER`,
`DATABASE_PASS` and `DATABASE_POOL`. The database **name is always**
`kaigara_<KAIGARA_DEPLOYMENT_ID>` — `DATABASE_NAME` is overwritten.

`DATABASE_DRIVER` takes `mysql` or `postgres`. There is a third value,
`memory`, which is **for the test suite only**: it is in-memory SQLite, so it
persists nothing, and SQLite needs cgo. Released binaries are built with
`CGO_ENABLED=0` to stay static, so they do not contain the SQLite driver at
all and `memory` returns an error explaining as much.

`KAIGARA_*` variables are **stripped** from the child process's environment.
The daemon never sees Kaigara's own configuration.

## Scopes

| Scope | Encrypted at rest | Intended for |
| --- | --- | --- |
| `public` | No | Values safe to expose, e.g. to a frontend |
| `private` | No | Backend configuration that is not a credential |
| `secret` | **Yes** | Credentials. Values **must be strings** |

The environment is built from the `global` app first, then the apps in
`KAIGARA_APP_NAME` in order. Later values shadow earlier ones.

## Secret to environment variable mapping

* Keys are **upper-cased**: `key1` → `KEY1=value`.
* Maps and slices are **skipped** — they cannot be represented in an
  environment variable. They are still stored and readable by `kaidump`.
* Booleans and numbers are stringified (`true`, `1337`).
* Keys matching `KFILE_<NAME>_PATH` / `KFILE_<NAME>_CONTENT` (case-insensitive)
  are written to disk instead of exported. Directories are created `0750`,
  files `0640`.

```yaml
kfile_name_path: config/config.json
kfile_name_content: '{"app":"example"}'
```

Use `kaienv` to see exactly what a service would receive without starting it.

## Managing secrets

### Bulk write with `kaisave`

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
          key4: value4        # stored, but NOT exported to env
      secret:
        key1: value1
```

Two rules behind almost every `kaisave` failure:

* **Every scope a component uses must be initialised**, even if empty — `public: {}`.
* **Quote numeric and boolean values** (`"4269"`, `"true"`). Errors like
  `interface{} is bool|json.Number|etc` are always an unquoted value. `secret`
  scope values must be strings.

Saving secrets bumps the stored version, so every service reading them restarts
within 20–30 seconds. Plan writes accordingly.

### Dump with `kaidump`

```sh
kaidump -a outputs.yaml
```

> **Warning:** `kaidump` **decrypts** the `secret` scope and prints the whole
> dump to stdout. Treat the output as a plaintext credential file —
> [S2](docs/IMPROVEMENTS.md#s2).

### Delete with `kaidel`

```sh
kaidel -a peatio -k some_key -s public,private,secret
```

### Inspect with the Vault CLI

```sh
vault list secret/metadata/$KAIGARA_DEPLOYMENT_ID
vault list secret/metadata/$KAIGARA_DEPLOYMENT_ID/$KAIGARA_APP_NAME
vault read secret/data/$KAIGARA_DEPLOYMENT_ID/$KAIGARA_APP_NAME/<scope> -format=yaml
```

`secret` scope values read this way are transit ciphertext (`vault:v1:...`).

### Tail logs with `kaitail`

```sh
kaitail -c 'log.peatio.*' -n
```

## Vault policy

[`etc/kaigara.hcl`](etc/kaigara.hcl) is the policy template — replace
`deployment_id` with your `KAIGARA_DEPLOYMENT_ID`. In OpenDAX it is generated
from `templates/config/vault/kaigara.hcl.erb`. Note the shipped policy is
deployment-wide rather than per-component; see
[S3](docs/IMPROVEMENTS.md#s3).

## Testing

```sh
make test-unit    # no external services
make test         # full suite; needs Vault, MySQL and PostgreSQL
go test -run TestExitCodeSignalledChild ./cmd/kaigara/   # a single test
```

`pkg/config` and `pkg/storage/vault` build a real Vault client at package-init
time, so those packages will not load without a reachable Vault. The SQL tests
assume empty storage — run them against a fresh `make test-env-up`, never a
shared server.

## License

MIT — see [LICENSE](LICENSE). Copyright (c) 2020 Openware. Vendored `pkg/ika`
and `pkg/database` retain their original MIT notices.
