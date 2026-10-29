# The ate-ish base image: the ate-ish binary on Debian. Actor images build FROM it.
FROM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /ate-ish .

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /ate-ish /usr/local/bin/ate-ish
ENTRYPOINT ["/usr/local/bin/ate-ish", "serve"]
