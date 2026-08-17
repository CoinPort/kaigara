# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Kaigara is a Go process wrapper: it reads configuration from a secret store
(Vault or SQL), injects it into a child process's environment, streams the
child's output to Redis, and gracefully restarts the child when its secrets
change. It is the container entrypoint wrapper for every service in the
CoinPort OpenDAX stack (`kaigara bundle exec puma ...`).

CoinPort's fork of the unmaintained
[openware/kaigara](https://github.com/openware/kaigara). Current release
`0.2.1` — parity with upstream `0.1.34` plus supervision fixes, dependency
reduction and security fixes, and no remaining Openware dependency.

## Read these before changing anything

| Document | Contents |
| --- | --- |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Startup sequence, storage model, encryptors, supervision loop, reload |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | Version history, deployment, release process, troubleshooting |
| [docs/IMPROVEMENTS.md](docs/IMPROVEMENTS.md) | Findings — what is fixed and what remains |

## Things that will trip you up

1. **Never point the tests at a shared or deployed Vault.** They hardcode
   `deploymentID = "opendax_uat"` with app `peatio`, and they *delete* the keys
   they touch. `make test-env-up` gives you a throwaway stack on `127.0.0.1`.
   This VM also runs an OpenDAX stack with Vault on `8200` and Redis on `6379` —
   don't collide with it.

2. **The SQL tests assume empty storage.** They assert absolute version numbers.
   Run them against a fresh `make test-env-up`, or `make test-env-down &&
   make test-env-up` between runs.

3. **`KAIGARA_ENCRYPTOR` must match how the data was written.** Reading
   transit-encrypted data with the `plaintext` encryptor does not fail — it
   hands the daemon the literal `vault:v1:…` ciphertext as its value. This is
   the trap in upstream's `v1.0.x` line, which flips the default to `plaintext`.

4. **The upstream `v1.0.x` series is not an upgrade path.** It also defaults
   `KAIGARA_STORAGE_DRIVER` to `sql` rather than `vault`. See
   [docs/OPERATIONS.md](docs/OPERATIONS.md#the-v10x-line).

## Related repositories on this machine

| Path | Relationship |
| --- | --- |
| `/home/app/opendax` | Stack-as-code. Renders `config/kaigara.env` and the Vault policies from `templates/`; `lib/tasks/kaisave.rake` pushes `config/secrets.yaml` |
| `/home/app/peatio` | Wrapped service. `Dockerfile` pins `KAIGARA_VERSION` |
| `/home/app/barong` | Wrapped service. `Dockerfile` pins `KAIGARA_VERSION` |

Changing the release process here means updating all three pins.

## Layout

One Go module. Earlier versions split `pkg/encryptor` and `pkg/storage/*` into
separate modules resolved from GitHub, so local edits never reached the
binaries — that is gone, along with every `github.com/openware` dependency.

```
cmd/kaigara/   the process wrapper (main binary)
cmd/kaienv/    print the env Kaigara would build, without running anything
cmd/kaisave/   bulk YAML -> store loader
cmd/kaidump/   store -> YAML dumper
cmd/kaidel/    delete a key across scopes
cmd/kaitail/   tail the Redis log stream
pkg/config/    KaigaraConfig, BuildCmdEnv, GetStorageService (the factory)
pkg/storage/   vault/ and sql/ backends
pkg/encryptor/ transit/, aes/, plaintext/
pkg/logstream/ LogStream interface + Redis implementation
pkg/ika/       vendored config loader (was openware/pkg/ika)
pkg/database/  vendored gorm helper (was openware/pkg/database)
types/         the Storage interface
```

## Build and test

```sh
make build         # all six binaries for the host
make release       # cross-compile everything + SHA256SUMS
make check         # fmt-check, vet, build, unit tests
make help          # list targets

make test-env-up   # Vault, Redis, MySQL, PostgreSQL on 127.0.0.1
make test          # full suite
make test-env-down

make test-unit     # only packages needing no external services
go test -run TestExitCodeSignalledChild ./cmd/kaigara/   # a single test
go test -race ./pkg/storage/vault/                       # a single package
```

`pkg/config` and `pkg/storage/vault` build a real Vault client at
package-variable init time, so those packages will not load at all without a
reachable Vault — even for a `-run` that would not touch it. `cmd/kaigara`'s
`supervise_test.go` is fully offline.

## Conventions

* `go.mod` declares Go 1.26 and CI builds with the 1.26 toolchain. The
  toolchain, not the directive, is what determines the standard-library CVEs in
  the shipped binary; the directive is what sets the language and GODEBUG
  defaults. Two consumers do compile this module from source, so the directive
  constrains their build hosts: `peatio`'s `Dockerfile` has a `kaigara-build`
  stage and `barong`'s a `kaigara-builder` stage over its vendored copy. Both
  base images must be `golang:1.26` or newer, or the build falls back to
  downloading a 1.26 toolchain (`GOTOOLCHAIN=auto`) — and fails outright where
  that is pinned to `local`. Raising the directive here means raising those.
* Storage paths are always `secret/{data,metadata}/<deploymentID>/<appName>/<scope>`
  and transit keys `<deploymentID>_kaigara_<appName>`. Use the
  `secretPath`/`keyPath`/`metadataPath` helpers rather than formatting inline.
* `KAIGARA_`-prefixed variables are stripped from the child environment by
  design; anything you add with that prefix is invisible to the daemon.
* New code should return errors. The existing code panics in many places
  ([B5](docs/IMPROVEMENTS.md#b5)), but don't mass-refactor that as a side effect
  of an unrelated change.
* `cmd/kaigara/superviseChild` is the **only** place that signals the child.
  Keep it that way — the watcher reports on a channel rather than killing
  anything itself.

## Watch out for

* `secret` scope values are encrypted and **must be strings**. `public` and
  `private` are stored as-is.
* Map and slice values are silently skipped when building the child
  environment — they cannot go in an env var. Use `kaienv` to see what a
  service would actually receive.
* Writing secrets restarts every service that reads them within 20–30 seconds.
  Any change to `Write` or the version-watching logic has stack-wide blast
  radius.
* `NewStorageService` (SQL) opens a connection pool per call. Close what you
  create or you will exhaust the server's connection limit.
* Don't commit real values in `kaigara.env`, `secrets.yaml` or `etc/` — the
  checked-in ones are dev placeholders (`changeme`).
