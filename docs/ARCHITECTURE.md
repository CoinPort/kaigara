# Kaigara Architecture

> Describes the code on the **`1-0-stable`** branch (the `0.1.x` line). The `origin/master`
> (`v1.0.x`) line is structured differently — see [OPERATIONS.md](OPERATIONS.md#version-map).

## What problem it solves

Services in the OpenDAX stack need their configuration — including credentials — delivered at
process start, from a central store, without baking it into images or `.env` files. Kaigara is a
thin wrapper that sits between the container entrypoint and the actual daemon:

```
docker entrypoint → kaigara → bundle exec puma
                      │
                      ├── reads secrets from Vault  → child process env + files on disk
                      ├── pipes child stdout/stderr  → Redis pub/sub
                      ├── writes a heartbeat key     → Redis
                      └── polls Vault for changes    → kills the child on change
```

## Repository layout

```
cmd/
  kaigara/    the process wrapper (the main binary)
  kaisave/    bulk YAML → Vault loader
  kaidump/    Vault → YAML dumper
  kaidel/     delete one key across scopes
  kaitail/    tail the Redis log stream
pkg/
  config/     KaigaraConfig (env parsing) and BuildCmdEnv (secrets → env/files)
  logstream/  LogStream interface + Redis implementation
  utils/      GetEnv helper
  vault/      SEPARATE Go module: the Vault SecretStore implementation
types/
  secretstore.go   the SecretStore interface
etc/
  backend.yml      docker-compose for local Vault + Redis
  kaigara.hcl      Vault policy template
```

### The `pkg/vault` submodule trap

`pkg/vault` has **its own `go.mod`** (`github.com/openware/kaigara/pkg/vault`). The root module
declares it as an ordinary versioned dependency:

```
github.com/openware/kaigara/pkg/vault v0.0.0-20210426162849-04557d766383
```

There is **no `replace` directive**. The binaries therefore compile against the copy downloaded
from GitHub at that April 2021 commit, **not** against `pkg/vault/vault.go` in this working tree.
Editing that file changes nothing until you add a `replace`. This is tracked as
[C1 in IMPROVEMENTS.md](IMPROVEMENTS.md#c1).

## Startup sequence

`cmd/kaigara/kaigara.go:171` `main()`:

1. `initLogStream()` — reads `KAIGARA_REDIS_URL`. If empty, logs a notice and runs with a nil
   client (streaming silently disabled). If set but unreachable, **panics**.
2. `initConfig()` — `ika.ReadConfig("", cnf)` populates `KaigaraConfig` from `KAIGARA_*` env vars.
3. `getVaultService()` — builds the Vault client, panicking if `KAIGARA_VAULT_TOKEN` or
   `KAIGARA_DEPLOYMENT_ID` is missing, then starts token renewal.
4. `kaigaraRun()` — builds the environment, writes files, starts the child, wires up the log
   pumps, the heartbeat, and the config watcher.

### Vault client and token renewal

`pkg/vault/vault.go:23` `NewService`:

* Builds `&api.Config{Address: addr, Timeout: 2 * time.Second}`. Because the Vault SDK's
  `NewClient` falls back to `DefaultConfig()` for the HTTP client, standard Vault env vars such as
  `VAULT_CACERT` **are** honoured for TLS. The **2-second timeout is hard-coded** and overrides the
  SDK's 60s default; `VAULT_CLIENT_TIMEOUT` has no effect.
* `startRenewToken` looks the token up, and if it is renewable, starts an
  `api.LifetimeWatcher` goroutine that renews it for the life of the process. Non-renewable tokens
  are used as-is and will eventually expire.

### Building the child environment

`pkg/config/config.go:43` `BuildCmdEnv(appNames, secretStore, currentEnv, scopes)`:

1. Copy the current process environment, **dropping every `KAIGARA_`-prefixed variable**. The child
   never sees Kaigara's own configuration.
2. For each app in `["global"] + appNames`, for each scope in `scopes`:
   * `LoadSecrets(app, scope)` — read `secret/data/<deploymentID>/<app>/<scope>` into memory.
   * `GetSecrets(app, scope)` — return the values, decrypting the `secret` scope via transit.
   * For each key/value:
     * `map` and `[]interface{}` values are **skipped** — they cannot go into an env var.
     * `bool` → `"true"`/`"false"`; `json.Number` → its literal digits; `string` → itself.
       Anything else is skipped.
     * If the key matches `(?i)^KFILE_(.*)_(PATH|CONTENT)$`, it contributes to a `File` entry
       instead of an env var.
     * Otherwise the key is **upper-cased** and appended as `KEY=value`.

Because apps are processed in order with `global` first, and env vars are appended rather than
deduplicated, a later app's value shadows an earlier one — the child's runtime sees the last
occurrence.

Any Vault error during this phase **panics**, taking the daemon down before it starts.

### Materialising files

`cmd/kaigara/kaigara.go:56`. For each `File` collected above, Kaigara creates the parent directory
(`0750`) and writes the content (`0640`). The path comes straight from Vault, so **write access to
a scope is equivalent to arbitrary file write as the daemon user** — see
[S1 in IMPROVEMENTS.md](IMPROVEMENTS.md#s1).

### Process supervision

* STDIN: a goroutine reads lines from Kaigara's stdin and forwards them to the child, closing the
  child's stdin on EOF.
* STDOUT/STDERR: two goroutines call `LogStream.Publish`, which writes each chunk to Kaigara's own
  stdout **and** publishes it to Redis.
* On exit, `c.Wait()` errors are handled with `log.Fatal`, so **the child's exit code is not
  propagated** — Kaigara always exits `0` or `1`.
* There is **no signal handling**. `SIGTERM` to Kaigara does not reach the child, so containers
  never shut down gracefully and always hit the Docker/Kubernetes kill timeout.

## Vault data model

```
secret/data/<deploymentID>/<appName>/<scope>       ← kv v2 data
secret/metadata/<deploymentID>/<appName>/<scope>   ← kv v2 metadata (version numbers)
transit/keys/<deploymentID>_kaigara_<appName>      ← per-app encryption key
```

* `<deploymentID>` — `KAIGARA_DEPLOYMENT_ID`, one per environment. In OpenDAX it is the
  lower-cased app name from `config/app.yml`.
* `<appName>` — a component (`peatio`, `barong`, `sonic`, …), plus the reserved `global` app that
  every component inherits, and `tokens` used by `PushPolicies`.
* `<scope>` — `public`, `private`, or `secret`.

Kaigara requires **kv v2**. `LoadSecrets` panics with an explicit message if the read response has
no `metadata` key, which is what a kv v1 mount looks like.

### The `secret` scope and transit encryption

`LoadSecrets` calls `initTransitKey`, which creates `transit/keys/<deploymentID>_kaigara_<appName>`
on first use. `SetSecret` on the `secret` scope base64-encodes the plaintext, calls
`transit/encrypt/...`, and stores the resulting `vault:v1:...` ciphertext. `GetSecret` reverses it.

Consequences:

* Values in the `secret` scope **must be strings**. Anything else returns an error.
* Reading `secret` scope values with the Vault CLI gives you ciphertext, not plaintext.
* The transit key is per-app, so a token scoped to one app cannot decrypt another's secrets — this
  is what makes the per-component policies in `etc/kaigara.hcl` meaningful.
* The `initTransitKey` error is **discarded** at `pkg/vault/vault.go:183`, so a missing `transit`
  mount surfaces later as a confusing encrypt/decrypt failure rather than a clear startup error.

## Log streaming

`pkg/logstream/redis.go`. Channels are derived from the app names joined with `&`:

```
log.<appNames>.stdout
log.<appNames>.stderr
```

For `KAIGARA_APP_NAME=peatio,peatio_daemons` that is `log.peatio&peatio_daemons.stdout`.

`Publish` reads the child's pipe into a **64-byte** buffer and publishes each chunk. Two problems
follow: chunks are not line-aligned, so subscribers receive arbitrary fragments; and the publish
call sends the **whole buffer** rather than the bytes actually read, so stale bytes from the
previous iteration are appended to every short read
([C2 in IMPROVEMENTS.md](IMPROVEMENTS.md#c2)).

### Heartbeat

`HeartBeat` sets Redis key `service.<appNames>` with a 20-second TTL and refreshes the TTL every 10
seconds, deleting the key on shutdown. External health checks can watch for its absence.

## Configuration reload

`cmd/kaigara/kaigara.go:140` `exitWhenSecretsOutdated` runs on a 20-second ticker. For each app and
scope it compares:

* `GetCurrentVersion` — the kv v2 `version` from the metadata **cached in memory at startup**, and
* `GetLatestVersion` — `current_version` read live from `secret/metadata/...`.

If they differ, it logs and calls `c.Process.Kill()`. Kaigara does not restart anything itself — it
relies on the container's `restart: always` policy to bring the service back with fresh
configuration. `KAIGARA_IGNORE_GLOBAL=true` removes `global` from this watch list only; global
secrets are still loaded at startup.

`SIGKILL` means the daemon gets no chance to drain connections or finish in-flight work. The
`v1.0.x` line replaced this with an in-process restart
([`5b2b1a6`, `eb9dd54`](OPERATIONS.md#version-map)).

## The SecretStore interface

`types/secretstore.go` abstracts the backend:

```go
type SecretStore interface {
	LoadSecrets(appName, scope string) error
	SaveSecrets(appName, scope string) error
	SetSecret(appName, name string, value interface{}, scope string) error
	SetSecrets(appName string, data map[string]interface{}, scope string) error
	GetSecret(appName, name, scope string) (interface{}, error)
	GetSecrets(appName, scope string) (map[string]interface{}, error)
	ListSecrets(appName, scope string) ([]string, error)
	DeleteSecret(appName, name, scope string) error
	ListAppNames() ([]string, error)
	GetCurrentVersion(appName, scope string) (int64, error)
	GetLatestVersion(appName, scope string) (int64, error)
}
```

`vault.Service` is the only implementation on this branch. `KAIGARA_SECRET_STORE` is parsed into
`KaigaraConfig` but never read — the Vault store is hard-wired at every call site. The `v1.0.x`
line generalises this into `pkg/storage` with Vault and SQL backends plus a separate
`pkg/encryptor` abstraction.

`LoadSecrets` must be called before any other method for a given `(app, scope)` pair; the getters
type-assert on an in-memory map and will panic on a nil entry otherwise.
