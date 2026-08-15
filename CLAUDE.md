# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Kaigara is a Go process wrapper: it reads configuration from HashiCorp Vault, injects it into a
child process's environment, streams the child's output to Redis, and restarts the child when its
Vault secrets change. It is the container entrypoint wrapper for every service in the CoinPort
OpenDAX stack (`kaigara bundle exec puma ...`).

This is CoinPort's internal clone of the unmaintained
[openware/kaigara](https://github.com/openware/kaigara).

## Read these before changing anything

| Document | Contents |
| --- | --- |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Startup sequence, Vault data model, scopes, encryption, log streaming, reload |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | **Version map**, deployment topology, secrets workflow, troubleshooting |
| [docs/IMPROVEMENTS.md](docs/IMPROVEMENTS.md) | Known bugs, security findings, dependency analysis |

## Three things that will mislead you

1. **The branch name lies.** `1-0-stable` is the **0.1.x** line, not the 1.0 line.
   `git describe` returns `0.1.31-1-gcf0bdfe`. The actual 1.0 line is `origin/master`, tagged to
   `v1.0.36`, and it is a substantially different codebase (unified `kai` CLI, `pkg/encryptor`,
   `pkg/sql`, `pkg/k8s`).

2. **`pkg/vault/` is not compiled into the binaries.** It is a separate Go module and `go.mod`
   pins it as a *remote* dependency with no `replace` directive, so the build uses the copy
   published to GitHub in April 2021. Editing `pkg/vault/vault.go` changes nothing until that is
   fixed. See [C1](docs/IMPROVEMENTS.md#c1).

3. **This repo is not what production runs.** Peatio and Barong download `0.1.34` from openware's
   GitHub releases at image build time. This branch is `0.1.31` + 1 commit and is missing seven
   commits that are in the deployed binary.

## Related repositories on this machine

| Path | Relationship |
| --- | --- |
| `/home/app/opendax` | Stack-as-code. Renders `config/kaigara.env` and the Vault policies from `templates/`; `lib/tasks/kaisave.rake` pushes `config/secrets.yaml` to Vault |
| `/home/app/peatio` | Wrapped service. `Dockerfile:13` pins `KAIGARA_VERSION=0.1.34` |
| `/home/app/barong` | Wrapped service. `Dockerfile:26` pins `KAIGARA_VERSION=0.1.34` |

## Big picture

The whole product is one control flow, spread across four files. Reading any one of them alone is
misleading.

```
main()                                    cmd/kaigara/kaigara.go:171
 ├─ initLogStream()      KAIGARA_REDIS_URL → RedisLogStream (nil client if unset)
 ├─ initConfig()         ika.ReadConfig("") → KaigaraConfig from KAIGARA_* env vars
 ├─ getVaultService()    vault.NewService → Vault client + token-renewal goroutine
 └─ kaigaraRun()
     ├─ config.BuildCmdEnv()               pkg/config/config.go:43
     │    for app in ["global"] + KAIGARA_APP_NAME:
     │      for scope in KAIGARA_SCOPES:
     │        LoadSecrets → GetSecrets → env vars + KFILE_ files
     ├─ write KFILE_ files to disk (0640)
     ├─ exec.Command(...).Start()
     ├─ goroutine: stdin  → child
     ├─ goroutine: stdout → LogStream.Publish → log.<apps>.stdout
     ├─ goroutine: stderr → LogStream.Publish → log.<apps>.stderr
     ├─ goroutine: HeartBeat → redis key service.<apps>, 20s TTL
     └─ goroutine: exitWhenSecretsOutdated  cmd/kaigara/kaigara.go:140
          every 20s: GetCurrentVersion (cached at startup)
                  vs GetLatestVersion  (live from Vault metadata)
          if they differ → c.Process.Kill()
```

Three cross-cutting invariants that are not visible from any single file:

1. **`global` is always loaded first**, then the apps in `KAIGARA_APP_NAME` in order. Env vars are
   appended without deduplication, so a later app shadows an earlier one at exec time.
2. **The reload mechanism is "die and let the supervisor restart you."** Kaigara never re-reads
   config in place; it `SIGKILL`s the child and depends on `restart: always`. Anything you change in
   `SaveSecrets` or the version comparison restarts the whole stack.
3. **`KAIGARA_*` is stripped from the child environment** in `BuildCmdEnv`, so the wrapped daemon
   can never see Kaigara's own config.

Vault data model — see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for detail:

```
secret/data/<deploymentID>/<appName>/<scope>       kv v2 (v2 is REQUIRED)
secret/metadata/<deploymentID>/<appName>/<scope>   version numbers drive reload
transit/keys/<deploymentID>_kaigara_<appName>      encrypts the `secret` scope only
```

## Layout

```
cmd/kaigara/   the process wrapper (main binary)
cmd/kaisave/   bulk YAML → Vault loader
cmd/kaidump/   Vault → YAML dumper
cmd/kaidel/    delete a key across scopes
cmd/kaitail/   tail the Redis log stream
pkg/config/    env parsing + BuildCmdEnv (secrets → env vars and files)
pkg/logstream/ LogStream interface + Redis implementation
pkg/vault/     SEPARATE Go module — the Vault SecretStore (see caveat above)
types/         the SecretStore interface
etc/           local docker-compose and the Vault policy template
```

## Build and test

```sh
make build                 # all binaries into bin/; kaisave is cross-compiled for 4 platforms
make clean
go build ./...
go vet ./...
gofmt -l .                 # currently flags config_test.go and logstream/redis.go
```

There is no lint target and no linter config. `golangci-lint run` is what CI uses on the `0.1.34`
and `1.0.x` lines.

### Tests

```sh
go test ./cmd/...                                  # the only tests with no external dependencies
go test ./...                                      # requires a live Vault at KAIGARA_VAULT_ADDR
go test -run TestAppNamesToLoggingName ./cmd/kaigara/          # a single test
go test -v -run TestBuildCmdEnvFileUpperCase ./pkg/config/     # a single Vault-dependent test
cd pkg/vault && go test ./...                      # SEPARATE module — not covered by ./... above
```

`pkg/config` and `pkg/vault` construct a real Vault client at **package-variable init time**
(`pkg/config/config_test.go:17`), so those packages fail to load at all without a reachable Vault —
even for a `-run` that would not touch it. `cmd/kaigara` is the only package that tests offline.

For a local Vault:

```sh
docker run --rm -d -p 8200:8200 -e VAULT_DEV_ROOT_TOKEN_ID=root-token hashicorp/vault:1.15
export KAIGARA_VAULT_ADDR=http://127.0.0.1:8200 KAIGARA_VAULT_TOKEN=root-token
vault secrets enable transit                       # kv v2 is already mounted in dev mode
```

Tests write to fixed paths (`test1`–`test6` under deployment `kaigara_test`; `peatio` under
`opendax_uat` in `pkg/vault/vault_test.go`) — never point them at a shared or production Vault.

## Conventions

* Go 1.14 is declared in `go.mod` and CI builds on `golang:1.14`. Do not use newer language or
  stdlib features without bumping both. (Bumping them is [S4](docs/IMPROVEMENTS.md#s4).)
* Exported identifiers carry `//` doc comments in the existing style — keep that up.
* Vault paths are always `secret/{data,metadata}/<deploymentID>/<appName>/<scope>`; transit keys are
  always `<deploymentID>_kaigara_<appName>`. Use the `secretPath`/`keyPath`/`metadataPath` helpers
  rather than formatting paths inline.
* `KAIGARA_`-prefixed variables are stripped from the child environment. Anything you add with that
  prefix is invisible to the wrapped daemon by design.
* The existing code panics on error almost everywhere. New code should return errors instead — see
  [B5](docs/IMPROVEMENTS.md#b5) — but do not mass-refactor existing panics as a side effect of an
  unrelated change.

## Watch out for

* `secret` scope values are transit-encrypted and **must be strings**. `public` and `private` are
  stored as-is.
* Map and slice values are silently skipped when building the child environment — they cannot be
  represented in an env var. This surprises people regularly.
* Writing secrets triggers a restart of every service that reads them, within 20 seconds. Any
  change to `SaveSecrets` or the version-watching logic has stack-wide blast radius.
* Do not commit `kaigara.env`, `secrets.yaml`, or anything under `etc/` with real values — the
  checked-in ones are dev placeholders (`changeme`).
