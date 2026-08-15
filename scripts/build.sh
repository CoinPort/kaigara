#!/usr/bin/env bash
#
# Cross-compile every Kaigara binary into bin/.
#
# Upstream only ever cross-compiled kaisave; kaigara itself was built for the
# host platform alone, which is why the published releases were linux/amd64
# only. Everything is built for every platform here so an arm64 host can run
# the same release as an amd64 one.
#
# Asset names are "<binary>_<os>_<arch>", plus an unsuffixed linux/amd64
# "kaigara" for backwards compatibility with the Peatio and Barong
# Dockerfiles, which fetch a bare "kaigara". Dropping that name is what made
# upstream's v1.0.36 release 404 for those images.
set -euo pipefail

BINARIES=(kaigara kaisave kaidump kaidel kaitail kaienv)
PLATFORMS=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64)

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT_DIR="${OUT_DIR:-bin}"

mkdir -p "$OUT_DIR"

echo "Building Kaigara ${VERSION}"

for binary in "${BINARIES[@]}"; do
  for platform in "${PLATFORMS[@]}"; do
    goos="${platform%/*}"
    goarch="${platform#*/}"

    # No point shipping a Windows build of the process wrapper: it forwards
    # POSIX signals and writes POSIX file modes.
    if [ "$goos" = "windows" ] && [ "$binary" = "kaigara" ]; then
      continue
    fi

    output="${OUT_DIR}/${binary}_${goos}_${goarch}"
    if [ "$goos" = "windows" ]; then
      output="${output}.exe"
    fi

    echo "  ${binary} ${goos}/${goarch}"
    GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
      go build -tags netgo -trimpath \
        -ldflags "-w -s -X main.version=${VERSION}" \
        -o "$output" "./cmd/${binary}"
  done
done

# Compatibility aliases for consumers that fetch an unsuffixed asset name.
cp "${OUT_DIR}/kaigara_linux_amd64" "${OUT_DIR}/kaigara"
cp "${OUT_DIR}/kaisave_linux_amd64" "${OUT_DIR}/kaisave"

( cd "$OUT_DIR" && sha256sum ./* > SHA256SUMS )

echo
echo "Artifacts in ${OUT_DIR}/:"
ls -1 "$OUT_DIR"
