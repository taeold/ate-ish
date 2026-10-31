# The pi-durable agent as a standalone actor image: a plain
# `docker build .` from this directory. The node pulls this image itself
# (its own Docker daemon if reachable, else the registry named in
# template.yaml's containers[0].image) and exports its filesystem once as
# the actor's rootfs.
FROM node:24-slim
WORKDIR /actor
COPY package.json package-lock.json ./
RUN npm ci --omit=dev
COPY server.ts server-gateway.ts ./
