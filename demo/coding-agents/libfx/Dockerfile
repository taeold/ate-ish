# docker build -t coding-agents/libfx https://github.com/taeold/coding-agents-on-substrate.git#main:libfx/agent
FROM coding-agents/libfx
# A sandbox cannot bind a port below 1024, so the server listens on $PORT.
RUN sed -i 's/server.listen(80,/server.listen(Number(process.env.PORT ?? 80),/' /app/server.ts && grep -q process.env.PORT /app/server.ts
COPY --from=us-central1-docker.pkg.dev/daniel-junk/ate-ish/ate-ish:latest /usr/local/bin/ate /usr/local/bin/ate
ENTRYPOINT ["/usr/local/bin/ate", "serve"]
