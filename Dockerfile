# Builds an image whose only contents are the Kaigara binaries.
#
# Its job is to be a source for `COPY --from=` in the Dockerfiles of the
# services Kaigara wraps (Peatio, Barong). Those images used to `curl` a
# release asset from GitHub, which stopped working when this repository became
# private: the asset URLs 404 for an unauthenticated fetch, and cloning during
# their build would mean putting a GitHub token into it. Publishing the
# binaries as a local image keeps credentials out of every build:
#
#   docker build -t coinport/kaigara:0.2.1 .
#
# Rebuild it when this repository changes -- consumers reference it by tag, so
# they pick up whatever that tag currently points at.
#
# Build flags mirror scripts/build.sh and the Makefile, so these are the same
# binaries as a release: static, no cgo, netgo resolver.
ARG GO_VERSION=1.26

FROM golang:${GO_VERSION} AS build

# Stamped into main.version. Left as a default rather than derived from git
# because .git is not part of the build context.
ARG VERSION=0.2.1

WORKDIR /src

# Dependencies first, so editing sources does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN set -eux; \
    for app in kaigara kaisave kaidump kaidel kaitail kaienv; do \
        CGO_ENABLED=0 go build -tags netgo -trimpath \
            -ldflags "-w -s -X main.version=${VERSION}" \
            -o "/out/$app" "./cmd/$app"; \
    done; \
    /out/kaigara 2>&1 | grep -q 'Usage: kaigara'

# scratch is enough: the binaries are static, and nothing here is meant to be
# a runtime for anything other than kaigara itself.
FROM scratch

COPY --from=build /out/ /usr/bin/

ENTRYPOINT ["/usr/bin/kaigara"]
