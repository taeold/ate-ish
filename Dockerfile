# The ate-ish node image: one binary (ate-ish itself; the in-sandbox
# init and forwarders are hidden subcommands of this same binary) and a
# staged runsc release, on Debian. Build it with a plain `docker build .`
# from the repo root. Actor images are standalone (their own Dockerfile,
# built and pulled separately, per their template's image field) — this
# image carries no docker CLI or socket of its own.
FROM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /ate-ish ./cmd/ate-ish
# `fetch-runsc` is the hidden subcommand EnsureRunsc also runs on a bare
# node start (internal/atelet/runsc.go): staging it at build time here
# means a container start never pays that fetch, but needs no second,
# shell-script implementation to stay in sync with it.
ENV ATE_RUNSC_DIR=/runsc
RUN /ate-ish fetch-runsc

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates util-linux systemd uidmap \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /ate-ish /usr/local/bin/ate-ish
COPY --from=build /runsc /ate-ish/bin/runsc
# ATE_RUNSC_DIR tells the node where this already-staged runsc lives;
# nothing else needs an override. The entrypoint runs ate-ish's own
# argv[1] dispatch (cmd/ate-ish/main.go): no args runs a node; any other
# argv runs the kubectl-ate CLI against --endpoint/$ATE_URL. The node
# itself needs no root (docs/ROOTLESS.md): `docker run --privileged`
# here is only for the unprivileged user+mount+network namespace this
# process creates for itself, which some container runtimes still gate
# behind the host-level --privileged flag rather than a plain seccomp/
# capability grant.
ENV ATE_RUNSC_DIR=/ate-ish/bin/runsc
ENTRYPOINT ["/usr/local/bin/ate-ish"]
