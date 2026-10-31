# The ate-ish base image: ate-ish (atelet+worker-pool) plus the vendored
# ateworker-gvisor and a staged runsc release, on Debian. Actor images build
# FROM it (demo/pi-durable/Dockerfile). v2: the node's own filesystem is the
# actor rootfs (bundle.go's overlay of "/"), so whatever an actor's command
# needs (node, python3, ...) has to be installed in THIS image, not just the
# ate-ish binary's own runtime deps (nft/ip/mount/nsenter/bash, which
# atelet.go, bundle.go and the vendored actornet package exec directly).
FROM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY internal ./internal
COPY third_party ./third_party
RUN CGO_ENABLED=0 go build -o /ate-ish .
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
# Stage 8: ATE_ISH_HOME/ATE_RUNSC_DIR tell worker.go/dev.go where
# ateworker-gvisor, kodata and this already-staged runsc live, since
# `docker run ... -w /work ...` (ate-ish dev's own invocation) overrides
# this WORKDIR as the process's cwd. The entrypoint dispatches on argv[1]
# (main.go): no args keeps today's Cloud Run behaviour (ate-ish serve);
# `docker run <image> dev -f template.yaml -e egress-policy.yaml` runs
# ate-ish dev instead.
ENV ATE_ISH_HOME=/ate-ish
ENV ATE_RUNSC_DIR=/ate-ish/bin/runsc
ENTRYPOINT ["/usr/local/bin/ate-ish"]
CMD ["serve"]
