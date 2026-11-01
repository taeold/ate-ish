# G5: a non-root actor image, `docker build -t ate-ish-g5-nonroot:latest .`
# from this directory. USER node means every request this server answers
# runs as node:24-slim's own built-in uid 1000 user, not root; see
# server.js and docs/ROOTLESS.md's G5.
FROM node:24-slim
USER node
WORKDIR /home/node
COPY server.js .
EXPOSE 8080
ENTRYPOINT ["node", "/home/node/server.js"]
