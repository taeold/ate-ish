# G5: a root-dropping actor image, `docker build -t ate-ish-g5-gosu:latest .`
# from this directory. Stays root (no USER) so its own entrypoint can
# gosu down to nobody itself, the pattern real root-starting entrypoints
# use; see server.js and docs/ROOTLESS.md's G5.
FROM node:24-slim
RUN apt-get update && apt-get install -y --no-install-recommends gosu && rm -rf /var/lib/apt/lists/*
WORKDIR /actor
COPY server.js .
EXPOSE 8080
ENTRYPOINT ["node", "/actor/server.js"]
