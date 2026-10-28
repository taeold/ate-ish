# The pi-durable agent as an ate-ish image: the node image is the actor.
ARG BASE=us-central1-docker.pkg.dev/daniel-junk/ate-ish/ate-ish:latest

FROM node:24-slim AS agent
WORKDIR /actor
COPY package.json package-lock.json ./
RUN npm ci --omit=dev
COPY server.ts server-gateway.ts ./

FROM $BASE
COPY --from=agent /usr/local/bin/node /usr/local/bin/node
COPY --from=agent /actor /actor
