# Kaigara Architecture

Describes `0.2.0`.

## What problem it solves

Services in the OpenDAX stack need their configuration — including credentials
— delivered at process start, from a central store, without baking it into
images or `.env` files. Kaigara sits between the container entrypoint and the
daemon:

```
docker entrypoint → kaigara → bundle exec puma
                      │
                      ├── reads config from the store → child env + files on disk
                      ├── pipes child stdout/stderr    → Redis pub/sub
                      ├── writes a heartbeat key       → Redis
                      ├── forwards signals             → child
                      └── polls for config changes     → graceful child restart
```

## Repository layout

```
cmd/
  kaigara/    the process wrapper (the main binary)
  kaisave/    bulk YAML -> store loader
  kaidump/    store -> YAML dumper
  kaidel/     delete one key across scopes
  kaienv/     print the env Kaigara would build, without running anything
  kaitail/    tail the Redis log stream
  env/        shared storage bootstrap used by the CLI tools
pkg/
  config/     KaigaraConfig, BuildCmdEnv, GetStorageService
  storage/
    vault/    Vault kv v2 backend
    sql/      MySQL/PostgreSQL backend (gorm)
  encryptor/
    transit/  Vault transit engine
    aes/      in-process AES
    plaintext/ no-op
    types/    the Encryptor interface
  logstream/  LogStream interface + Redis implementation
  ika/        vendored config loader (was github.com/openware/pkg/ika)
  database/   vendored gorm connection helper (was openware/pkg/database)
  utils/
types/
  storage.go  the Storage interface
etc/
  backend.yml local Vault, Redis, MySQL and PostgreSQL
  kaigara.hcl Vault policy template
```

This is **one Go module**. Earlier versions split `pkg/encryptor` and
`pkg/storage/*` into their own modules resolved from GitHub, which meant local
edits to them did not affect the built binaries. That is gone.

## Startup sequence

`cmd/kaigara/kaigara.go` `main()`:

1. `seedJitter()` — per-process seed, because `math/rand` is not auto-seeded
   before Go 1.20 and every service would otherwise pick the same poll offset.
2. `initLogStream()` — reads `KAIGARA_REDIS_URL`. Empty means streaming is
   disabled and output only goes to stdout.
3. `ika.ReadConfig("", cnf)` — populates `KaigaraConfig` from `KAIGARA_*` and
   `DATABASE_*` environment variables.
4. `config.GetStorageService(cnf)` — builds the encryptor, then the storage
   backend, from `KAIGARA_ENCRYPTOR` and `KAIGARA_STORAGE_DRIVER`.
5. `kaigaraRun()` — builds the environment, writes files, starts the child, and
   supervises it. Its return value becomes the process exit status.

### Storage and encryptor selection

`pkg/config/config.go` `GetStorageService` is the single factory:

| `KAIGARA_ENCRYPTOR` | Behaviour |
| --- | --- |
| `transit` *(default)* | Vault transit engine, key `<deploymentID>_kaigara_<appName>` |
| `aes` | In-process AES with `KAIGARA_ENCRYPTOR_AES_KEY` |
| `plaintext` | No encryption |

| `KAIGARA_STORAGE_DRIVER` | Behaviour |
| --- | --- |
| `vault` *(default)* | Vault kv v2 |
| `sql` | gorm against MySQL or PostgreSQL |

The encryptor is orthogonal to the backend: the `secret` scope is encrypted
before it reaches storage, so a SQL backend gets ciphertext too. **The encryptor
must match how the data was written** — reading transit-encrypted data with the
plaintext encryptor returns the ciphertext string rather than failing.

### Building the child environment

`pkg/config/config.go` `BuildCmdEnv(appNames, store, currentEnv, scopes)`:

1. Copy the current environment, **dropping every `KAIGARA_`-prefixed
   variable**, so the child never sees Kaigara's own configuration.
2. For each app in `["global"] + appNames`, for each scope in `scopes`:
   * `Read(app, scope)` loads the scope into memory.
   * `GetEntries(app, scope)` returns the values, decrypting `secret`.
   * Per key/value:
     * `map` and `[]interface{}` values are **skipped** — they cannot be
       represented in an environment variable.
     * `bool` → `"true"`/`"false"`; `json.Number` → its digits; `string` → itself.
     * Keys matching `(?i)^KFILE_(.*)_(PATH|CONTENT)$` become a file instead.
     * Otherwise the key is **upper-cased** and appended as `KEY=value`.

Apps are processed with `global` first, and vars are appended rather than
deduplicated, so a later app's value shadows an earlier one at exec time.

`kaienv` runs exactly this and prints the result, which is the quickest way to
answer "why doesn't the daemon see this secret".

### Materialising files

Files are written with the parent directory at `0750` and the file at `0640`.
The path comes straight from the store, so write access to a scope is
equivalent to arbitrary file write as the daemon user —
[S1](IMPROVEMENTS.md#s1).

## Process supervision

Since `0.2.0` there is one supervisor loop, `superviseChild`, and it is the only
place that signals the child:

```
     ┌──────────────┐  child exits   ┌──────────────────┐
     │  c.Wait()    ├───────────────▶│                  │
     └──────────────┘                │                  │
     ┌──────────────┐  SIGTERM/INT   │  superviseChild  │──▶ exit status
     │ signal.Notify├───────────────▶│                  │
     └──────────────┘                │                  │
     ┌──────────────┐  version bump  │                  │
     │ watchSecrets ├───────────────▶│                  │
     └──────────────┘                └──────────────────┘
                                              │
                                    SIGTERM, then SIGKILL
                                    after shutdownGrace (8s)
```

* **Signals.** `SIGTERM`, `SIGINT` and `SIGHUP` are forwarded to the child.
  `SIGHUP` is passed through as a reload hint and does not start the shutdown
  clock. Before `0.2.0` there was no signal handling at all, so stopping the
  container orphaned the daemon and the runtime killed it after the stop
  timeout.
* **Exit status.** The child's status is propagated, using `128+signal` for
  signalled children. Previously `c.Wait()` errors went to `log.Fatal`, so
  Kaigara always exited 0 or 1.
* **Grace period.** `shutdownGrace` is 8 seconds, under Docker's default 10s
  stop timeout, so the daemon rather than the runtime decides how it stops.

### STDIN and log pumps

A goroutine forwards Kaigara's stdin to the child line by line, closing the
child's stdin on EOF. Two more pump stdout and stderr through
`LogStream.Publish`, which writes to Kaigara's own stdout **and** publishes to
Redis.

## Storage data model

Vault:

```
secret/data/<deploymentID>/<appName>/<scope>       kv v2 data
secret/metadata/<deploymentID>/<appName>/<scope>   version numbers
transit/keys/<deploymentID>_kaigara_<appName>      per-app encryption key
```

SQL: a single `data` table in database `kaigara_<deploymentID>`, one row per
`(app_name, scope)` holding a JSON blob and a version. The database name is
derived from the deployment ID and **overrides `DATABASE_NAME`**.

* `<deploymentID>` — one per environment.
* `<appName>` — a component, plus the reserved `global` that every component
  inherits.
* `<scope>` — `public`, `private`, or `secret`.

Vault requires **kv v2**; a v1 mount has no `metadata` in the read response and
Kaigara fails at startup saying so.

## Log streaming

Channels derive from the app names joined with `&`:

```
log.<appNames>.stdout
log.<appNames>.stderr
```

`Publish` reads into a 64 KiB buffer and publishes `buf[:n]`. Before `0.2.0` it
published the whole 64-byte buffer, so every short read appended stale bytes
from the previous iteration to the message. Messages are chunk-aligned, not
line-aligned — a subscriber may receive a partial line.

A publish failure is logged, not fatal: a logging problem should not take the
wrapped daemon down.

### Heartbeat

`HeartBeat` sets Redis key `service.<appNames>` with a 20-second TTL, refreshes
it every 10 seconds, and deletes it on shutdown.

## Configuration reload

`watchSecrets` polls every 20–30 seconds (jittered) comparing:

* `GetCurrentVersion` — the version cached in memory at startup, and
* `GetLatestVersion` — read live from the store.

On a difference it reports the reason on a channel and returns; the supervisor
does the stopping. Kaigara does not restart anything itself — it relies on the
container's `restart: always` to bring the service back with fresh config.

`KAIGARA_IGNORE_GLOBAL=true` removes `global` from the watch list only; global
secrets are still loaded at startup.

The jitter matters at stack scale: without it every service polls on the same
tick and one `kaisave` run restarts everything simultaneously.

## The Storage interface

`types/storage.go`:

```go
type Storage interface {
	Read(appName, scope string) error
	Write(appName, scope string) error

	SetEntry(appName, scope, name string, value interface{}) error
	SetEntries(appName, scope string, data map[string]interface{}) error
	GetEntry(appName, scope, name string) (interface{}, error)
	GetEntries(appName, scope string) (map[string]interface{}, error)
	ListEntries(appName, scope string) ([]string, error)
	DeleteEntry(appName, scope, name string) error
	ListAppNames() ([]string, error)

	GetCurrentVersion(appName, scope string) (int64, error)
	GetLatestVersion(appName, scope string) (int64, error)
}
```

`Read` must be called before any getter for a given `(app, scope)`; the getters
index an in-memory map and will panic on a missing entry otherwise —
[B6](IMPROVEMENTS.md#b6).

`StorageService.Close` releases the SQL connection pool. `NewStorageService`
opens a fresh pool per call, so anything constructing more than one over a
process lifetime must close the ones it finishes with.
