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

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates nftables iproute2 util-linux kmod python3 curl \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /ate-ish
COPY --from=build /ate-ish /usr/local/bin/ate-ish
COPY --from=build /ateworker-gvisor bin/ateworker-gvisor
COPY bin/runsc bin/runsc
COPY third_party/substrate/cmd/ateworker-gvisor/kodata third_party/substrate/cmd/ateworker-gvisor/kodata
# ATE_ISH_HOME/ATE_RUNSC_DIR tell internal/atelet where ateworker-gvisor,
# kodata and this already-staged runsc live, in case a run's cwd is
# something other than this WORKDIR. The entrypoint runs ate-ish's own
# argv[1] dispatch (cmd/ate-ish/main.go): no args runs a node; any other
# argv runs the kubectl-ate CLI against --endpoint/$ATE_URL.
ENV ATE_ISH_HOME=/ate-ish
ENV ATE_RUNSC_DIR=/ate-ish/bin/runsc
ENTRYPOINT ["/usr/local/bin/ate-ish"]
