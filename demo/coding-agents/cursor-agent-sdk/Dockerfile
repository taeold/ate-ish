# docker build -t coding-agents/cursor-agent-sdk https://github.com/taeold/coding-agents-on-substrate.git#main:cursor-agent-sdk/agent
FROM coding-agents/cursor-agent-sdk
COPY --from=us-central1-docker.pkg.dev/daniel-junk/ate-ish/ate-ish:latest /usr/local/bin/ate-ish /usr/local/bin/ate-ish
ENTRYPOINT ["/usr/local/bin/ate-ish", "serve"]
