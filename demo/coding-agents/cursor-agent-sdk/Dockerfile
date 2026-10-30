# The coding-agents-on-substrate cursor-agent-sdk agent, unmodified. Port 80
# unchanged: v2's worker runs it directly, no PORT env, no sed. No key is
# supplied (CURSOR_API_KEY stays the placeholder from template.yaml); this
# demo runs up to the agent's first model call, which fails on an invalid
# key (recorded in the README table), not past it. Final stage is $BASE,
# same reason as demo/coding-agents/pi-durable/Dockerfile.
ARG BASE=us-central1-docker.pkg.dev/daniel-junk/ate-ish/ate-ish:latest
FROM node:24-slim AS agent
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev
COPY bot ./bot
RUN mkdir -p /workspace

FROM $BASE
COPY --from=agent /usr/local/bin/node /usr/local/bin/node
COPY --from=agent /app /app
