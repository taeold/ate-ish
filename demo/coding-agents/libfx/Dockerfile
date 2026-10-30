# The coding-agents-on-substrate libfx agent, unmodified (it already reads
# AI_GATEWAY_API_KEY itself). Port 80 unchanged: v2's worker runs it
# directly, no PORT env, no sed. Final stage is $BASE, same reason as
# demo/coding-agents/pi-durable/Dockerfile.
ARG BASE=us-central1-docker.pkg.dev/daniel-junk/ate-ish/ate-ish:latest
FROM node:24-slim AS agent
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev
COPY tools.ts server.ts ./
RUN mkdir -p /workspace

FROM $BASE
COPY --from=agent /usr/local/bin/node /usr/local/bin/node
COPY --from=agent /app /app
