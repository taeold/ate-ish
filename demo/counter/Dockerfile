# The substrate counter demo (counter.go, ported from
# agent-substrate/substrate's demos/counter; one delta: flag parsing uses
# the standard library instead of pflag) as a standalone actor image:
# proves MEMORY fidelity, an in-process counter that survives
# suspend/resume alongside a file counter on a durable volume.
FROM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod counter.go ./
RUN CGO_ENABLED=0 go build -o /counter .

FROM gcr.io/distroless/static-debian12
COPY --from=build /counter /counter
ENTRYPOINT ["/counter"]
