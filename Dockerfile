# One image for every role. Build bin/ate first (deploy.sh image).
FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY bin/ate /usr/local/bin/ate
ENTRYPOINT ["/usr/local/bin/ate", "serve"]
