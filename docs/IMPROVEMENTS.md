# Kaigara — findings

Status as of `0.2.0`. Items marked **Fixed** are resolved in this repo; the rest
are open, with the reason they were left.

**Verified state:** `go build ./...`, `go vet ./...` and `gofmt -l .` are clean.
The full suite passes against Vault 1.15, MySQL 8 and PostgreSQL 14. The `go`
directive and CI toolchain are both 1.26; the suite was previously verified on
1.19, 1.21, 1.22 and 1.24, and those are no longer supported build toolchains.

---

## Fixed in 0.2.0

### <a id="c1"></a>C1 — Source did not match the built binary ✅

`pkg/vault`, `pkg/encryptor` and `pkg/storage/sql` were separate Go modules
resolved from GitHub, so local edits never reached the binaries. They are now
ordinary packages of the root module. `openware/pkg`'s `ika` and `database` are
vendored in-tree, so **no `github.com/openware` dependency remains** and the
build no longer depends on that org staying reachable.

Collapsing the modules also meant `pkg/storage/sql`'s tests ran for the first
time — which is how B11 and B12 below were found.

### <a id="c2"></a>C2 — Redis log stream published stale bytes ✅

`Publish` sent the whole 64-byte buffer instead of `buf[:n]`, so every read
shorter than the buffer appended whatever the previous read left behind.
Subscribers saw duplicated fragments, and since a log line can end in a
credential, stale bytes leaked between messages. Now publishes `buf[:n]`, with
the buffer raised to 64 KiB. Verified over a live subscriber.

### <a id="c3"></a>C3 — Publish loop span on read errors ✅

The loop only stopped on `io.EOF` and `os.ErrClosed`, so any other persistent
error left `n == 0` and spun at full speed logging two lines per iteration. It
now returns on any error. A failed publish also no longer panics — a logging
problem should not kill the wrapped daemon.

### <a id="c4"></a>C4 — No signal handling; exit status discarded ✅

There was no `signal.Notify` anywhere. `SIGTERM` stopped Kaigara and orphaned
the daemon, which the runtime then killed after the stop timeout, so nothing
ever shut down cleanly. Termination signals are now forwarded, with `SIGHUP`
passed through as a reload hint rather than a shutdown.

`c.Wait()` errors went to `log.Fatal`, so Kaigara always exited 0 or 1. The
child's status is now propagated, including `128+signal`. Verified end to end:
exit 7/0/42 propagate, and a child trapping `SIGTERM` exits 17 through Kaigara.

### <a id="c5"></a>C5 — Reload `SIGKILL`ed the daemon ✅

A secret change called `Process.Kill()`, terminating daemons mid-request.
Restarts now send `SIGTERM` and allow 8 seconds — under Docker's default 10s
stop timeout — before escalating. The poll interval is also jittered over 20–30s
so one `kaisave` run no longer restarts the whole stack on the same tick.

### <a id="b11"></a>B11 — SQL storage leaked a connection pool ✅

`NewStorageService` calls `database.Connect`, which opens a fresh pool every
time, and nothing ever closed it. The test suite exhausted PostgreSQL's
100-connection limit. Adds `StorageService.Close`, called at every site.

### <a id="b12"></a>B12 — SQL tests were order-dependent ✅

They assert absolute version numbers but only cleared storage on the way out, so
they passed alone and failed in sequence. They now clear on entry as well.

### <a id="b1"></a>B1 — `KFILE_` panicked on a non-string value ✅

`f.Path = v.(string)` re-asserted on the raw value even though a normalised
`val` was in hand, so an unquoted `KFILE_X_PATH` arrived as a `json.Number` and
panicked the process at startup. Now uses `val`.

### <a id="b4"></a>B4 — `kaitail` parsed messages by regex ✅

It reconstructed the channel and payload by regex-matching a message's
*formatted string*, when `msg.Channel` and `msg.Payload` were right there. The
pattern also dropped every multi-line payload (`.` does not match newlines) and
its `[A-z.]` class spanned the punctuation between the ASCII case ranges. Now
uses the struct fields; verified against a multi-line payload that the old code
rejected.

### <a id="b8"></a>B8 — Fragile `KAIGARA_IGNORE_GLOBAL` ✅

It dropped the slice's last element, which only worked because `global` happened
to be appended last. Now filters by name.

### <a id="i1"></a>I1 — CI could not work for this fork ✅

`.drone.yml` triggered on `master`, used Openware's Drone secrets, published to
their namespace, and built on the EOL `golang:1.14`. Replaced with GitHub
Actions running check, test, vulncheck and release jobs.

### <a id="i3"></a>I3 — No test, lint or format tooling ✅

The Makefile had only `build` and `clean`, no `.PHONY`, and cross-compiled
`kaisave` alone — which is why published `kaigara` releases were linux/amd64
only. Now has `test`, `test-unit`, `test-env-up/down`, `fmt`, `fmt-check`,
`vet`, `lint`, `vulncheck`, `check` and `help`, and `scripts/build.sh`
cross-compiles all six binaries for linux/darwin/windows on amd64 and arm64
with checksums.

### <a id="i5"></a>I5 — Stale local dev environment ✅

`etc/backend.yml` pinned the deprecated `vault:0.11.4` image and Redis 5. Now
`hashicorp/vault:1.15`, Redis 7 and Postgres 14, with healthchecks and every
port bound to `127.0.0.1` so it cannot collide with a deployed stack.

### <a id="b7"></a>B7 — `KAIGARA_SECRET_STORE` was parsed but unused ✅

Resolved by the `0.1.34` merge: it is now `KAIGARA_STORAGE_DRIVER` and is read
by `GetStorageService`.

### <a id="s2"></a>S2 — `kaidump` exposed decrypted secrets ✅ (partly)

It wrote the decrypted dump `0644` and printed it unconditionally to stdout.
Now writes `0600`, and stdout output is behind an explicit `-stdout` flag. A
`-scopes` filter, so operators can dump `public`/`private` without ever
materialising `secret`, is still worth adding.

---

## Open

### <a id="s0"></a>S0 — Exposed credentials *(needs action outside this repo)*

* **A GitHub PAT was embedded in this repo's git remote URL.** The remote has
  been rewritten to a plain HTTPS URL, but **the token itself must still be
  revoked and reissued** — it was on disk, in shell history, and in any tool
  output that ran `git remote -v`.
* **Live Vault tokens were committed to the OpenDAX repo.** `config/app.yml`
  (the source of every token) and the generated `compose/*.yaml` were tracked.
  Both are now gitignored and untracked, and `config/app.yml.example` was added
  with 42 values redacted. **The tokens must still be rotated, and the history
  purged** — untracking does not remove them from past commits.

### <a id="s1"></a>S1 — `KFILE_` allows arbitrary file write

The file path comes verbatim from the store, so write access to a scope means
writing any file the daemon user can — `~/.ssh/authorized_keys`, `/etc/cron.d/*`
— turning store write access into code execution in the container.

**Fix.** Reject absolute paths and `..`, resolve under a configured root
(`KAIGARA_FILE_ROOT`, defaulting to the working directory), and verify the
cleaned path stays inside it.

*Left open:* it changes behaviour for any existing secret using an absolute
`KFILE_` path, so it needs an audit of what is deployed first.

### <a id="s3"></a>S3 — Vault policy is deployment-wide

`etc/kaigara.hcl` wildcards both `secret/data/<deployment_id>/*` and
`transit/decrypt/<deployment_id>_kaigara_*`, so the Peatio token can read **and
decrypt** Barong's secrets. The per-app transit key gives the appearance of
isolation without delivering it.

**Fix.** Per-component paths:

```hcl
path "secret/data/<deployment_id>/<component>/*"           { capabilities = ["read", "list"] }
path "secret/data/<deployment_id>/global/*"                { capabilities = ["read", "list"] }
path "transit/decrypt/<deployment_id>_kaigara_<component>" { capabilities = ["update"] }
path "transit/decrypt/<deployment_id>_kaigara_global"      { capabilities = ["update"] }
```

*Left open:* requires reissuing every component token in lockstep with the
policy change — an operational task, not a code one.

### <a id="s4"></a>S4 — Remaining dependency vulnerabilities

Bumped in `0.2.0`: `golang.org/x/net`, `x/text`, `x/sys`, `x/crypto`,
`hashicorp/go-retryablehttp` (CVE-2024-6104), `google.golang.org/grpc`, and
`yaml.v3` to v3.0.1.

Still reachable: `gopkg.in/square/go-jose.v2 v2.5.1` (GO-2024-2631, **no fixed
release**), pulled in by `hashicorp/vault/api v1.3.1`. Clearing it means moving
to `vault/api` v1.9+, which is a behaviour change and belongs in its own commit
once `0.2.0` has soaked.

The `go` directive is 1.26 and **release builds use Go 1.26**, which is what
actually determines the standard-library CVEs in the shipped binary. Note that
CI is not the only thing compiling this module: `peatio` and `barong` both build
`cmd/kaigara` from source in their Dockerfiles, so their `golang:` base images
have to keep up with the directive.

### <a id="b2"></a>B2 — `DeleteEntry` deletes the whole scope, then rewrites it

`vault.Logical().Delete(keyPath)` removes the entire scope before the remaining
keys are written back. If that write fails — expired token, network blip — every
secret in the scope is left deleted. It also burns two kv versions per key
deletion, triggering two restarts of every service using that scope.

**Fix.** Drop the key from the in-memory map and call `Write` alone; the kv v2
write is a full replace, so the delete round-trip is unnecessary.

*Left open:* it changes the version-bump pattern that the restart watcher keys
off, so it wants its own change with test coverage.

### <a id="b3"></a>B3 — Errors silently discarded

* `pkg/storage/vault/vault.go` — `initTransitKey` return value ignored in
  `Read`, so a missing `transit` mount surfaces later as a confusing encrypt
  failure rather than a clear startup error.
* `SetEntries` ignores the error from every `SetEntry` and returns `nil`, so a
  batch write can partially fail and report success.
* `PushPolicies` ignores the error from `Sys().PutPolicy`.
* `cmd/kaidump` — `yamlEncoder.Encode` error ignored and the encoder is never
  `Close()`d, so the final document may be truncated.

### <a id="b5"></a>B5 — Panics used as the error-handling strategy

`panic` appears throughout `cmd/` and `pkg/`, including for missing
configuration and any store error during `BuildCmdEnv`. A transient Vault or
Redis blip at container start takes the daemon down with a stack trace instead
of a diagnosable message. Wants errors returned and `log.Fatalf` at the top
level only, plus bounded retry with backoff on the initial connections.

### <a id="b6"></a>B6 — Getters panic if `Read` was not called first

`GetEntry`, `GetEntries`, `ListEntries` and `GetCurrentVersion` type-assert on a
map only `Read` populates; a missing entry yields a nil interface and panics.
The contract is undocumented and unenforced — use the comma-ok form and return a
typed error.

### <a id="b9"></a>B9 — Hard-coded 2-second Vault timeout

`pkg/storage/vault/vault.go` and `pkg/encryptor/transit/transit.go` both set
`Timeout: 2 * time.Second`, overriding the SDK's 60s default and making
`VAULT_CLIENT_TIMEOUT` inert. Against a remote Vault over TLS — the production
configuration, given `VAULT_CACERT` — that is tight enough to cause spurious
startup failures under load. Should be configurable.

### <a id="b10"></a>B10 — Minor

* `getVaultService(appName string)` in several CLI tools takes an unused
  parameter.
* `len(scopesList) <= 0` checks are unreachable; `strings.Split` never returns
  an empty slice. The real guard is against `[""]`.
* `pkg/config/config.go` uses `val == ""` as a proxy for "not yet converted";
  restructure as a `switch v := v.(type)`.
* Storage bootstrap is duplicated across the `main` packages.
* `pkg/storage/vault/vault_test.go` references `http://vault-prod.core:8200`, a
  hostname from someone else's infrastructure.

### <a id="i2"></a>I2 — Consumers repointed, release not yet published

Peatio, Barong and `opendax/lib/tasks/kaisave.rake` now pin `0.2.0` from
`CoinPort/kaigara`, with `curl -f` and a smoke test. **The `0.2.0` tag exists
locally but has not been pushed**, so the release does not exist yet and those
builds will fail until it is. Push the tag, let CI publish, then rebuild.

### <a id="i4"></a>I4 — Tests need live services and share fixed paths

`pkg/config` and `pkg/storage/vault` construct a real Vault client at
package-variable initialisation, so those packages fail to load without a
reachable Vault even for tests that would not touch it. Tests also write to
fixed paths (`test1`–`test6`, and `peatio` under `opendax_uat`), so running them
against a shared Vault mutates real-looking paths.

**Fix.** An in-memory `types.Storage` fake for `pkg/config`, Vault integration
tests behind a build tag, and a randomised deployment ID per run.

---

## Suggested order

| # | Item | Why |
| --- | --- | --- |
| 1 | [S0](#s0) rotate credentials | Live exposure; untracking does not undo it |
| 2 | [I2](#i2) push the tag | Nothing reaches production until the release exists |
| 3 | [S3](#s3) per-component policies | Cross-component secret access, operational fix |
| 4 | [S1](#s1) `KFILE_` path confinement | Privilege escalation, after auditing deployed paths |
| 5 | [B2](#b2) [B3](#b3) [B6](#b6) | Data-loss and diagnosability, each small |
| 6 | [S4](#s4) `vault/api` bump | Clears the last unfixable CVE; behaviour change |
| 7 | [B5](#b5) [B9](#b9) [I4](#i4) [B10](#b10) | Robustness and test quality |
