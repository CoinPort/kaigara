# Kaigara — findings and suggested improvements

Reviewed against `1-0-stable` @ `cf0bdfe`. Every item cites the code it refers to. Items are
grouped by kind and ordered by priority within each group.

**Verified state of the tree:** `go build ./...` succeeds, `go vet ./...` is clean,
`go test ./cmd/kaigara/...` passes. The remaining tests need a live Vault. `gofmt -l` flags
`pkg/config/config_test.go` and `pkg/logstream/redis.go`.

---

## Critical

### <a id="c1"></a>C1 — `pkg/vault` in this tree is not what the binaries compile against

`pkg/vault` is a separate Go module and `go.mod:13` pins it as a remote dependency:

```
github.com/openware/kaigara/pkg/vault v0.0.0-20210426162849-04557d766383
```

There is no `replace` directive, so the build downloads the April 2021 published copy. Diffing the
module cache against the working tree confirms the local `PushPolicies` (`pkg/vault/vault.go:340`,
added in `10bf020`) **is not in the compiled binaries**, and nothing calls it anyway.

Consequences: edits to `pkg/vault/vault.go` silently do nothing; `go test ./...` from the root does
not exercise the local `pkg/vault` code; and the build depends on `github.com/openware` remaining
reachable.

**Fix.** Add to `go.mod`:

```
replace github.com/openware/kaigara/pkg/vault => ./pkg/vault
```

Better still, collapse `pkg/vault` into the root module — it has one consumer and the submodule
split buys nothing here. Note that `origin/master` has the same latent problem, with the `replace`
lines present but **commented out** (`go.mod:5-11`).

### <a id="c2"></a>C2 — Redis log stream publishes stale buffer bytes

`pkg/logstream/redis.go:36-58`:

```go
n, err := stream.Read(buf)
os.Stdout.Write(buf[:n])          // correct
if r.client != nil {
    e := r.client.Publish(channel, buf).Err()   // BUG: publishes all 64 bytes
}
```

Stdout gets `buf[:n]`; Redis gets the whole 64-byte array. Every read shorter than 64 bytes
publishes leftover bytes from the previous iteration. Anything consuming `kaitail` or the Redis
stream sees corrupted, duplicated log fragments — and since the tail of a log line can be a
credential or token fragment, this leaks previously-read bytes into later messages.

**Fix.** `r.client.Publish(channel, buf[:n])`.

While there: the loop reads 64 bytes at a time and publishes each chunk, so messages are not line
aligned. Use a `bufio.Scanner` over lines, and size the buffer to something sane (4–64 KiB).

### <a id="c3"></a>C3 — Publish loop spins forever on a persistent read error

Same loop. It only breaks on `io.EOF` or `os.ErrClosed`. Any other persistent error (a broken pipe
from a crashed child, for example) leaves `n == 0` and loops without a delay, logging two lines per
iteration — a hot loop that will saturate a core and flood the log destination.

**Fix.** Break on any non-nil error after handling `n > 0`.

### <a id="c4"></a>C4 — No signal handling; child exit code is discarded

`cmd/kaigara/kaigara.go`. There is no `signal.Notify` anywhere in the repo. `SIGTERM` to Kaigara
terminates Kaigara and orphans the child, which is then killed by the container runtime after the
stop timeout. Every Kaigara-wrapped service takes the full Docker/Kubernetes grace period to stop
and never shuts down cleanly — no connection draining, no in-flight request completion.

Separately, `kaigara.go:126`:

```go
if err := c.Wait(); err != nil {
    log.Fatal(err)      // always exits 1, whatever the child returned
}
```

The child's exit code never reaches the supervisor, so `docker inspect` and Kubernetes restart
policies cannot distinguish a clean exit from a crash.

**Fix.** Install a signal handler that forwards `SIGTERM`/`SIGINT`/`SIGHUP` to the child; on
`c.Wait()`, extract the status via `*exec.ExitError` and `os.Exit` with the child's code.

### <a id="c5"></a>C5 — Configuration reload `SIGKILL`s the daemon

`cmd/kaigara/kaigara.go:162` calls `c.Process.Kill()` — `SIGKILL`, unblockable. A Peatio or Barong
process handling live trading requests is terminated mid-request whenever anyone runs `kaisave`.
There is also no jitter: every service polling on the same 20-second tick sees the same version
change and dies simultaneously, so the whole stack restarts at once.

**Fix.** Send `SIGTERM` and wait with a timeout before escalating to `SIGKILL`. Upstream fixed this
on the `1.0.x` line in `5b2b1a6` ("restart the subprocess instead of crashing the main one") and
`eb9dd54`. Add a small random offset to the poll interval.

---

## Security

### <a id="s0"></a>S0 — Credentials are exposed in this environment *(act on this first)*

These are not code defects but they are live and they are the highest-severity thing found:

* **A GitHub personal access token is embedded in this repo's git remote URL** (`.git/config`:
  `https://petermcooney:ghp_…@github.com/CoinPort/kaigara`). Anyone with read access to the
  filesystem, any process that runs `git remote -v`, and every tool that logs command output has
  it.
* **Live Vault tokens are committed to the OpenDAX repo.** `opendax/compose/app.yaml` contains
  `VAULT_TOKEN=hvs.CAES…` for Peatio, Barong, and Sonic, and `compose/` is **tracked by git** — it
  is not a generated-and-ignored directory.

**Action.** Revoke and reissue the GitHub PAT and all three Vault tokens. Move the remote to SSH or
a credential helper. Add `compose/` to `.gitignore` and render it from `templates/` only, then
purge the tokens from history.

### <a id="s1"></a>S1 — `KFILE_` allows arbitrary file write as the daemon user

`cmd/kaigara/kaigara.go:56-62` takes the file path verbatim from Vault:

```go
os.MkdirAll(path.Dir(file.Path), 0750)
ioutil.WriteFile(file.Path, []byte(file.Content), 0640)
```

Anyone who can write to a component's `private` or `public` scope can therefore write any file the
daemon user can write — `~/.ssh/authorized_keys`, `/etc/cron.d/*`, an entrypoint script — turning
Vault write access into remote code execution in the container. There is also no error check on
`MkdirAll`.

**Fix.** Reject absolute paths and any path containing `..`; resolve everything under a configured
root directory (e.g. `KAIGARA_FILE_ROOT`, defaulting to the working directory) and verify the
cleaned result is still inside it. Check the `MkdirAll` error.

### <a id="s2"></a>S2 — `kaidump` writes decrypted secrets world-readable and prints them to stdout

`cmd/kaidump/kaidump.go:86-92`:

```go
fmt.Print(b.String())                              // every secret, decrypted, to stdout
ioutil.WriteFile(*filepath, b.Bytes(), 0644)       // and to a world-readable file
```

`GetSecrets` decrypts the `secret` scope, so this is the full plaintext credential set for the
deployment. On a shared host, mode `0644` means every local user can read it. Printing to stdout
puts it into shell history-adjacent logs, CI logs, and terminal scrollback.

**Fix.** Write with `0600`. Drop the unconditional `fmt.Print` or put it behind an explicit
`-stdout` flag. Consider a `-scopes` filter so operators can dump `public`/`private` without ever
materialising `secret`.

### <a id="s3"></a>S3 — Vault policy is deployment-wide, not per-component

`etc/kaigara.hcl` grants each component `read`/`list` across `secret/data/<deployment_id>/*` and
`create`/`read`/`update` across `transit/decrypt/<deployment_id>_kaigara_*`. Because both are
wildcarded at the deployment level, the Peatio token can read **and decrypt** Barong's `secret`
scope. The per-app transit key gives the appearance of isolation without delivering it.

**Fix.** Parameterise the policy per component:

```hcl
path "secret/data/<deployment_id>/<component>/*"     { capabilities = ["read", "list"] }
path "secret/data/<deployment_id>/global/*"          { capabilities = ["read", "list"] }
path "transit/decrypt/<deployment_id>_kaigara_<component>" { capabilities = ["update"] }
path "transit/decrypt/<deployment_id>_kaigara_global"      { capabilities = ["update"] }
```

The OpenDAX ERB template already renders per-component files, so this is a template change plus
token reissue.

### <a id="s4"></a>S4 — 50 reachable vulnerabilities in dependencies

`govulncheck v1.0.1` against the tree (Go 1.19.8 toolchain):

> Your code is affected by **50 vulnerabilities from 4 modules and the Go standard library**.

Affected modules with live call paths:

| Module | Pinned | Fixed in |
| --- | --- | --- |
| `golang.org/x/text` | `v0.3.5` | `v0.39.0` |
| `golang.org/x/net` | `v0.0.0-20210220033124` | `v0.4.0`+ |
| `gopkg.in/yaml.v3` | `v3.0.0-20210107192922` | `v3.0.0`+ |
| `github.com/hashicorp/go-retryablehttp` | `v0.6.8` | `v0.7.7` |

Plus 14 more in imported-but-not-called code, including `gopkg.in/square/go-jose.v2 v2.5.1`
(decompression bomb, **no fix available** — it must be replaced with `github.com/go-jose/go-jose`).

The `go.mod` still declares `go 1.14` and CI builds on `golang:1.14`, so **published binaries are
built with a toolchain that is years past end-of-life** and carries every unpatched stdlib
vulnerability listed above (HTTP/2 rapid reset, `net/textproto` CPU exhaustion, `crypto/tls`
issues).

**Fix.** Bump the `go` directive and the CI image to a supported Go, then `go get -u` the four
modules above. `github.com/hashicorp/vault/api` should move off the 2020 pre-release pin
(`v1.0.5-0.20201001211907-38d91b749c77`) to a current `v1.x`.

### <a id="s5"></a>S5 — Development credentials committed at the repo root

`kaigara.env` (`KAIGARA_VAULT_TOKEN=changeme`) and `etc/backend.yml`
(`VAULT_DEV_ROOT_TOKEN_ID=changeme`) are fine as local dev defaults but sit at the top level with
no marking. `secrets.yaml` likewise looks like a real secrets file. Move them to `examples/` — the
`1.0.x` line already did exactly this — and name them `*.example`.

---

## Correctness

### <a id="b1"></a>B1 — `KFILE_` panics on a non-string value

`pkg/config/config.go:112-115` type-asserts the raw value even though a normalised `val` is already
in hand:

```go
case "PATH":    f.Path = v.(string)
case "CONTENT": f.Content = v.(string)
```

A `KFILE_X_PATH` written unquoted in `secrets.yaml` arrives as a `json.Number` and panics the
process at startup. **Fix:** use `val`, which is already the stringified form.

### <a id="b2"></a>B2 — `DeleteSecret` deletes the whole scope, then rewrites it

`pkg/vault/vault.go:406-417`:

```go
metadata, err := vs.vault.Logical().Delete(vs.keyPath(appName, scope))  // deletes the ENTIRE scope
...
delete(vs.data[appName][scope].(map[string]interface{}), name)
err = vs.SaveSecrets(appName, scope)                                    // writes the rest back
```

Between the delete and the save, the scope is empty in Vault. If `SaveSecrets` fails — expired
token, network blip, Vault restart — **every secret in that scope is left deleted**. It also burns
two kv versions per key deletion and, because Kaigara's watcher sees a version change, restarts
every service using that scope twice.

**Fix.** Drop the key from the in-memory map and call `SaveSecrets` alone. The kv v2 write is a
full replace, so the delete round-trip is unnecessary.

### <a id="b3"></a>B3 — Errors silently discarded

* `pkg/vault/vault.go:183` — `vs.initTransitKey(appName)` return value ignored in `LoadSecrets`. A
  missing `transit` mount or a policy gap surfaces later as a confusing encrypt failure instead of
  a clear startup error.
* `pkg/vault/vault.go:239-244` — `SetSecrets` ignores the error from every `SetSecret` and
  unconditionally returns `nil`. A batch write can partially fail and report success.
* `pkg/vault/vault.go:349` — `PushPolicies` ignores the error from `Sys().PutPolicy`.
* `cmd/kaigara/kaigara.go:57` — `os.MkdirAll` error ignored (see [S1](#s1)).
* `cmd/kaidump/kaidump.go:84` — `yamlEncoder.Encode` error ignored, and the encoder is never
  `Close()`d, so the final document may be truncated.

### <a id="b4"></a>B4 — `kaitail` parses messages with a regex instead of struct fields

`cmd/kaitail/kaitail.go:24` reconstructs the channel and payload by regex-matching the *formatted
string* of a `*redis.Message`:

```go
re := regexp.MustCompile(`^Message<(log\.[A-z.]+?): (.+?)>$`)
rs := re.FindStringSubmatch(msg.String())
```

`msg.Channel` and `msg.Payload` are right there. The regex is also wrong: `[A-z.]` is a character
*range* spanning ASCII 65–122, which includes `` [ \ ] ^ _ ` `` — and `.` does not match newlines,
so any multi-line payload is dropped with "Could not parse message".

**Fix.** `fmt.Printf("%s: %s\n", msg.Channel, msg.Payload)`.

### <a id="b5"></a>B5 — Panics used as the error-handling strategy

`panic` appears 34 times across `cmd/` and `pkg/` outside tests, including in `NewService` for missing
configuration, in `BuildCmdEnv` for any Vault error, and in `NewRedisClient` for an unreachable
Redis. For a process supervisor this means a transient Vault or Redis blip at container start takes
the daemon down with a Go stack trace instead of a diagnosable message — the exact failure mode
commit `1bcbcdc` ("Do not exit when vault is not accessible") tried to address but did not finish.

**Fix.** Return errors; `log.Fatalf` with a readable message at the top level only. Add bounded
retry with backoff around the initial Vault and Redis connections.

### <a id="b6"></a>B6 — Getters panic if `LoadSecrets` was not called first

`GetSecret`, `GetSecrets`, `ListSecrets`, and `GetCurrentVersion` all do
`vs.data[appName][scope].(map[string]interface{})` on a map that is only populated by
`LoadSecrets`. A missing entry yields a nil interface and the assertion panics. The contract is
undocumented and unenforced.

**Fix.** Use the comma-ok form and return a typed error. Document the ordering requirement on the
`SecretStore` interface.

### <a id="b7"></a>B7 — `KAIGARA_SECRET_STORE` is parsed but never used

`pkg/config/config.go:15` declares it with an `env-default:"vault"`, and nothing reads
`cnf.SecretStore`. Every call site constructs `vault.NewService` directly. Either wire it up to a
factory over `types.SecretStore` or delete the field — as written it advertises configurability
that does not exist.

### <a id="b8"></a>B8 — Fragile `KAIGARA_IGNORE_GLOBAL` slicing

`cmd/kaigara/kaigara.go:143-145` drops the last element of the slice to remove `global`:

```go
appNames := append(parseAppNames(), "global")
if ignore, ok := os.LookupEnv("KAIGARA_IGNORE_GLOBAL"); ok && ignore == "true" {
    appNames = appNames[:len(appNames)-1]
}
```

Correct only because `global` happens to be appended last. Filter by name instead. Note also that
`BuildCmdEnv` puts `global` **first** while this puts it **last** — two different orderings of the
same conceptual list, which is how this kind of bug starts.

### <a id="b9"></a>B9 — Hard-coded 2-second Vault timeout

`pkg/vault/vault.go:34-37` sets `Timeout: time.Second * 2`, overriding the SDK's 60-second default
and making `VAULT_CLIENT_TIMEOUT` inert. The timeout applies to every request. Against a remote
Vault over TLS — which is the production configuration, given `VAULT_CACERT` in
`kaigara.env.erb` — 2 seconds is tight enough to cause spurious startup panics under load.

**Fix.** Make it configurable (`KAIGARA_VAULT_TIMEOUT`) with a more forgiving default.

### <a id="b10"></a>B10 — Minor

* `getVaultService(appName string)` in `kaisave`, `kaidump`, and `kaidel` takes an `appName`
  parameter that is never used; all three are called with `"global"`.
* `cmd/kaidump/kaidump.go:48` and `cmd/kaidel/kaidel.go:54` check `len(scopesList) <= 0`, which is
  unreachable — `strings.Split` never returns an empty slice. The real guard is against `[""]`.
* `pkg/config/config.go:89` uses `val == ""` as a proxy for "not yet converted", so a legitimately
  empty string takes an extra branch. Works, but restructure as a `switch v := v.(type)`.
* `initConfig`, `getVaultService`, and `initLogStream` are copy-pasted across four `main` packages.
  Move them into `pkg/config`.
* `gofmt -l` flags `pkg/config/config_test.go` and `pkg/logstream/redis.go`.

---

## External dependencies

You asked specifically about external dependencies and whether to merge them in. The dependency
surface is small, and the risky part of it is all Openware-hosted.

### Openware-hosted (unmaintained upstream — merge candidates)

| Module | Used by | Size | Recommendation |
| --- | --- | --- | --- |
| `github.com/openware/kaigara/pkg/vault` | root module | 1 file, ~420 LOC | **Merge into the root module.** It is already in this repo — see [C1](#c1). The submodule split provides no benefit and actively breaks the build's relationship to the source |
| `github.com/openware/pkg/ika` | all 4 `main` packages | 1 file, 559 LOC, MIT, one dep (`yaml.v2`) | **Vendor or replace.** It is a struct-tag env/YAML config loader. Either copy it in under `pkg/ika` preserving the MIT notice, or drop it — the config struct has six fields and `os.Getenv` would do. Note the code passes `ika.ReadConfig("")`, so only the env-var half is used |

On `origin/master` the surface is larger — four in-repo submodules
(`pkg/{vault,encryptor,k8s,sql}`) plus `openware/pkg/{ika,kli,kube}`, all resolved from GitHub with
the `replace` directives commented out. If you adopt the `1.0.x` line, do the merge there too.

Both `github.com/openware/kaigara` and `github.com/openware/pkg` are still reachable today, so
nothing is broken *yet*. That is the argument for merging now rather than after an archive or
deletion breaks every image build.

### Third-party

| Module | Pinned | Status |
| --- | --- | --- |
| `github.com/hashicorp/vault/api` | `v1.0.5-0.20201001211907` | 2020 pre-release pin. Update to current `v1.x` |
| `github.com/go-redis/redis/v7` | `v7.2.0` | v7 is end-of-life; current is `redis/v9` |
| `gopkg.in/yaml.v3` | `20210107192922` | Vulnerable — see [S4](#s4) |
| `github.com/iancoleman/strcase` | `v0.1.3` | Used in one place, by dead code (`PushPolicies`). Removable |
| `github.com/stretchr/testify` | `v1.3.0` | Test-only, very old |
| `golang.org/x/{net,text,crypto,time}` | 2021 | Vulnerable — see [S4](#s4) |

### The real supply-chain exposure

The dependency that actually matters is not in `go.mod` at all — it is in the **Dockerfiles**:

```dockerfile
RUN curl -Lo /usr/bin/kaigara https://github.com/openware/kaigara/releases/download/${KAIGARA_VERSION}/kaigara
```

Peatio and Barong fetch the production binary from an unmaintained third party's GitHub releases at
image build time, unauthenticated, unpinned by checksum, and — because `curl` lacks `-f` — without
failing the build on a 404 (see
[OPERATIONS.md](OPERATIONS.md#release-asset-naming-changed-at-v1036)). If Openware archives or
deletes that repo, every Peatio and Barong image build breaks, and there is no internal artifact to
fall back on. This fork exists precisely to remove that exposure but is not yet wired up.

---

## Infrastructure and process

### <a id="i1"></a>I1 — CI does not work for this fork

`.drone.yml` targets the `master` branch (this repo's default is `1-0-stable`), uses openware's
Drone secrets, builds on `golang:1.14`, and publishes to `${DRONE_REPO_NAMESPACE}/${DRONE_REPO_NAME}`
on GitHub. It also runs `go test ./...`, which — per [C1](#c1) — does not test `pkg/vault`.

**Fix.** Replace with GitHub Actions (or the internal CI) that builds all binaries for
linux/amd64 + arm64, runs tests against a Vault service container, runs `golangci-lint` and
`govulncheck`, and publishes to an internal artifact store or GHCR.

### <a id="i2"></a>I2 — The fork is not consumed by anything

Until CI publishes internally and the Peatio/Barong Dockerfiles point at that output, this repo is
documentation of what production runs, not the source of it. That is the highest-value change on
this page. Sequence:

1. Decide the target line (see [OPERATIONS.md](OPERATIONS.md#version-map)).
2. Fix [C1](#c1) so the build reflects the source.
3. Stand up CI, publish `kaigara` + `kaisave` (or `kai`) for linux/amd64 and arm64.
4. Repoint Peatio, Barong, and `opendax/lib/tasks/kaisave.rake` at the internal artifacts, adding
   `curl -f` and a checksum verification.
5. Only then start changing behaviour.

### <a id="i3"></a>I3 — Makefile has no test, lint, or format targets

`make build` and `make clean`, no `.PHONY`, and `kaigara` is built for the host platform only while
`kaisave` is cross-compiled for four. Add `test`, `fmt`, `vet`, `lint`, and cross-compilation for
all binaries.

### <a id="i4"></a>I4 — Tests require a live Vault and share fixed paths

`pkg/config/config_test.go:17` constructs a real `vault.NewService` at package-variable
initialisation time, so the whole package fails to load without a reachable Vault. Tests write to
fixed app names (`test1`–`test6`, and `peatio` under `opendax_uat` in `pkg/vault/vault_test.go`) —
running the suite against a shared Vault mutates real-looking paths. `pkg/vault/vault_test.go:12`
also references `http://vault-prod.core:8200`, a hostname from someone else's infrastructure.

**Fix.** Introduce a `types.SecretStore` in-memory fake for `pkg/config` tests, keep Vault
integration tests behind a build tag, and randomise the deployment ID per run.

### <a id="i5"></a>I5 — Stale local dev environment

`etc/backend.yml` pins `vault:0.11.4` (2018) and `redis:5-alpine`, and the `vault` Docker Hub image
is deprecated in favour of `hashicorp/vault`. `.drone.yml` uses `vault:1.5.3`. Update both to
`hashicorp/vault` and a current Redis, and make the local compose file enable the `kv` v2 and
`transit` engines automatically so `docker compose up` gives a working environment in one step.

---

## Suggested order of work

| # | Item | Why first |
| --- | --- | --- |
| 1 | [S0](#s0) revoke leaked credentials | Live exposure, unrelated to any code change |
| 2 | [I2](#i2) + [C1](#c1) own the build | Nothing else you do here reaches production until this is done |
| 3 | Decide the version line | Determines whether later fixes go on `1-0-stable` or `master` |
| 4 | [C2](#c2) [C3](#c3) [C4](#c4) [C5](#c5) | Small, contained, and each fixes a daily operational annoyance |
| 5 | [S1](#s1) [S2](#s2) [S3](#s3) | Privilege-escalation and credential-exposure paths |
| 6 | [S4](#s4) toolchain + dependency bump | Large but mechanical; do it once CI can verify it |
| 7 | [B1](#b1)–[B10](#b10), [I3](#i3)–[I5](#i5) | Cleanup, best done alongside the above |
