# ate-node progress

Spec: ../ate-node-SPEC.md. Region us-west1, project daniel-junk.

## Decisions

- Language Go: one static binary is the server, the sandbox's pid-1 init and
  the CLI, and the MITM egress proxy needs X.509 minting, which Node core lacks.
- Gemini key `opencode-gemini-key` returns 401 (checked 2026-10-26 ~18:50 UTC).
  Turns use Vercel AI Gateway through pi-ai's `vercelAIGatewayProvider`.
  It sends the key as `x-api-key` (measured with a fetch hook), so the gateway
  policy replaces `x-api-key`. `anthropic/claude-haiku-5.5` is not in pi-ai
  1.0.4's catalog (turn settles `unanswered`, reason `no_model`);
  `anthropic/claude-haiku-4.5` works.

## Probe (throwaway instance ate-node-probe, deleted)

Sandbox network reach, curl from inside, host server on 0.0.0.0:9999:

| sandbox flags | host 169.254.8.1 / 172.20.0.1 | metadata 169.254.169.254 | example.com |
|---|---|---|---|
| `-p` only | refused (rc 7) | timeout | timeout |
| `--allow-egress -p` | 200 | timeout | 200 |
| neither | refused | refused | DNS fails |

A sandbox without `--allow-egress` cannot reach the host, so actors run with
egress allowed and HTTPS_PROXY pointing at the worker's per-actor listener.
Enforcement is by convention (spec's fallback).

## Built

Go program (bucket.go, records.go, ingress.go, worker.go, sandbox.go,
egress.go, cli.go, main.go), Dockerfile, deploy.sh (setup, image, play,
scale, url, down), gate.sh (README sequence), demo/ (templates, policies,
gateway agent). Bucket gs://ate-node-daniel-junk (us-west1).

## Found while building

- MITM proxy: Go's default transport speaks HTTP/2 upstream, and
  Response.Write then writes an `HTTP/2.0` status line to the actor's
  HTTP/1.1 connection (Node: `Connection error.`, curl hung until timeout).
  The upstream transport is HTTP/1.1 only (`TLSNextProto` empty map).
- An actor (egress allowed) reached `http://169.254.8.1:8080/admin/workers`
  on the instance, past Cloud Run IAM. The server now refuses requests from
  172.20.0.0/16; measured source of actor connections: 172.20.0.56.
- `instances update --image` took 94 s and 128 s. The old container got
  SIGTERM and suspended its actor in 475 ms (`suspend on shutdown`).
- gcloud `projects describe` hit a 429 on the shared gcloud client quota;
  deploy.sh looks the service account up only when it needs it.

- Scale shape, first parking run: eight concurrent wakes, three failed. GCS
  answered `429 ... exceeded the rate limit for object mutation operations`
  on `atespaces/park/actors/p5.json`. Cause: the ingress wrote the record
  (running, owner) before asking a worker, every worker it asked by a stale
  lease refused as full, and each refusal cost a write and a revert. Fix:
  the worker reserves a slot first and then claims the record with
  ifGenerationMatch; a full worker refuses with no write. Lease writes are
  coalesced to at most one a second.

## Status

- [x] program builds, local smoke, instance smoke
- [x] play gate on the final image's code path (runs/play.txt), exit 0
- [x] scale gate, ingress + 2 workers (runs/scale.txt), exit 0; turn 1 on one
      worker, the fork restored on the other
- [x] parking, 10 actors on 8 slots (runs/park.txt): 8 at once 200 in 10.5 s
      (fresh workers, image pull); p9 parked 4.3 s then 200; p10 503
      "no worker has room for the actor" after 5.4 s
- [x] scale deployment and its bucket deleted
- [x] README
- [x] review: the independent reviewer did not report before the run ended.
      Self-review of worker.go and ingress.go found one bug: a suspend that
      arrived while a boot was failing would close a nil egress server and
      clear the slot after the boot had freed it. suspend now returns
      errNotHosted when the boot failed. One comment fixed (call also maps
      409). Neither change was redeployed or gated; the running image is
      sha256:bb6d3e88...
- [x] commit

## Resources

- Cloud Run instance `ate-node` (us-west1), left running, role all
- Bucket gs://ate-node-daniel-junk (ate-demo-pi: demo-agent, demo-agent-fork, tag checkpoint-v1)
- Images ate-node:latest and ate-node:agent-gateway in
  us-central1-docker.pkg.dev/daniel-junk/cloud-run-source-deploy
- IAM: re-applied existing bindings (compute SA: secretAccessor on
  opencode-gemini-key and ai-gateway-token; objectAdmin on the buckets)
- Deleted: instance ate-node-probe and its image ate-node:probe, worker pool ate-node-workers, service
  ate-node-ingress, bucket gs://ate-node-scale-daniel-junk
