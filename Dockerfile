# The ate-node base image: the ate binary on Debian. Actor images build FROM it.
FROM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /ate .

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /ate /usr/local/bin/ate
ENTRYPOINT ["/usr/local/bin/ate", "serve"]
