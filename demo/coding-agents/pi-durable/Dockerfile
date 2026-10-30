# The coding-agents-on-substrate pi-durable agent, with Gemini swapped for
# the Vercel AI Gateway (GEMINI_API_KEY is revoked in this project): same
# package.json/package-lock.json as pi-durable/agent upstream, server.ts
# replaced by server-gateway.ts (demo/pi-durable's own Gateway variant).
# Port 80 unchanged (v2's worker runs it directly; no PORT env, no sed).
# Final stage is $BASE (ate-ish's own image: the binary, its vendored
# ateworker-gvisor, bin/runsc, and the kodata runsc reads by relative
# path), not node:24-slim, because v2 bundles its own gVisor worker and
# needs all of that in the actor image; node itself is copied in since
# the base image doesn't carry it.
ARG BASE=us-central1-docker.pkg.dev/daniel-junk/ate-ish/ate-ish:latest
FROM node:24-slim AS agent
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev
COPY server-gateway.ts ./server.ts
RUN mkdir -p /workspace

FROM $BASE
COPY --from=agent /usr/local/bin/node /usr/local/bin/node
COPY --from=agent /app /app
