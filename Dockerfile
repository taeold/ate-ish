# The ate-ish node image: the ate-ish binary, the vendored ateworker-gvisor,
# and a staged runsc release, on Debian. Build it with a plain
# `docker build .` from the repo root. Actor images are standalone (their
# own Dockerfile, built and pulled separately, per their template's image
# field) — this image carries no docker CLI or socket of its own.
FROM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY third_party ./third_party
RUN CGO_ENABLED=0 go build -o /ate-ish ./cmd/ate-ish
RUN CGO_ENABLED=0 go build -o /ateworker-gvisor ./third_party/substrate/cmd/ateworker-gvisor
# `fetch-runsc` is the hidden subcommand EnsureRunsc also runs on a bare
# node start (internal/atelet/runsc.go): staging it at build time here
# means a container start never pays that fetch, but needs no second,
# shell-script implementation to stay in sync with it.
ENV ATE_RUNSC_DIR=/runsc
RUN /ate-ish fetch-runsc

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates nftables iproute2 util-linux kmod python3 \
    && rm -rf /var/lib/apt/lists/*
# ateworker-gvisor lives next to ate-ish itself: internal/atelet's
# workerBinaryPath resolves it there (symlinks resolved) with no
# ATE_WORKER_BIN and no checkout of this repo on disk, the same way a
# `go install` of both binaries into one GOBIN does on a bare host.
COPY --from=build /ate-ish /usr/local/bin/ate-ish
COPY --from=build /ateworker-gvisor /usr/local/bin/ateworker-gvisor
COPY --from=build /runsc /ate-ish/bin/runsc
# ATE_RUNSC_DIR tells internal/atelet where this already-staged runsc
# lives; nothing else needs an override. The entrypoint runs ate-ish's own
# argv[1] dispatch (cmd/ate-ish/main.go): no args runs a node; any other
# argv runs the kubectl-ate CLI against --endpoint/$ATE_URL.
ENV ATE_RUNSC_DIR=/ate-ish/bin/runsc
ENTRYPOINT ["/usr/local/bin/ate-ish"]
