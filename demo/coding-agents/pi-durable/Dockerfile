# docker build -t coding-agents/pi-durable https://github.com/taeold/coding-agents-on-substrate.git#main:pi-durable/agent
FROM coding-agents/pi-durable
# A sandbox cannot bind a port below 1024, so the server listens on $PORT.
RUN sed -i 's/server.listen(80,/server.listen(Number(process.env.PORT ?? 80),/' /app/server.ts && grep -q process.env.PORT /app/server.ts
COPY --from=us-central1-docker.pkg.dev/daniel-junk/ate-ish/ate-ish:latest /usr/local/bin/ate-ish /usr/local/bin/ate-ish
ENTRYPOINT ["/usr/local/bin/ate-ish", "serve"]
