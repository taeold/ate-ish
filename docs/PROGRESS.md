# ate-node progress

## Run 2 (2026-10-28): from proof of concept to public repo

Brief: single-template model, review fixes a-f, public images, BYO Gemini
key, gate both shapes, README, publish as taeold/ate-node.

- [x] 1 single-template model (commit; gated with step 2)
- [x] 2 review fixes a-f (commit; live checks of b and d pending the deploy)
  a real: Host header must name the CONNECT host (local check: without the
    fix a Host: www.google.com request reached Cloudflare; with it the proxy
    answers 403).
  b real: record write first, then the tag copy; a failed copy deletes the
    new record.
  c real: a failed stop, tar or upload keeps the sandbox and the running
    record and returns the error.
  d real: worker.release re-reads on 412, waits out 429, 5 tries. Ingress:
    a running record whose owner's live lease does not list the actor goes
    to placement, owner asked first. The proxied 409 itself cannot be
    retried once the body went to the worker, so the ingress reads the
    owner's lease. ate suspend on such a record marks it suspended.
  e real: a resume for a hosted copy waits for it, then refuses 503 if
    stopping, 409 if the record no longer names it (and stops that copy).
  f real: gate.sh asserts both fetch results from the bash tool output.
- [x] 3 images. Renamed mid-run to ate-ish (coordinator): public repo
  us-central1-docker.pkg.dev/daniel-junk/ate-ish (allUsers reader), images
  ate-ish@sha256:bb25d41b6733... and pi-durable@sha256:92dd34fed7d5...;
  anonymous manifest GETs answer 200. The first repo, ate-node, is deleted.
- [x] 4 model key: Gemini primary (demo/template.yaml), Gateway variant
  (demo/template-gateway.yaml) in the same pi-durable image; gates on Gateway
- [x] 5 gate: play (ate-node, us-west1) and scale + park, scale deleted.
  Final runs on pi-durable@sha256:92dd34fed7d5: runs/play.txt, runs/scale.txt,
  runs/park.txt, all exit 0. Scale: turn 1 on w-42db59ba, the fork restored
  on w-43e1e8bb. Park: p1..p8 200 in 1.6 to 4.4 s (p2 and p7 reached the
  ingress 2.7 s after the other six per the Cloud Run request log, then
  placed in 1.5 s); p9 parked 3.9 s, 200 at 5.2 s; p10 503 at 5.5 s.
  Boot: first sandbox on a fresh container 2.6 s (n=3, 2635/2641 ms, and
  2.97 s resume on play); later boots alone 0.74 to 0.95 s (n=6); four at
  once on one worker 2.33 to 2.36 s (n=4); suspend 0.46 to 0.55 s (n=8);
  snapshots 122 to 164 KB. Worker pool, ingress service and
  gs://ate-node-scale-daniel-junk deleted. Instance ate-node left running.
  play: green (runs/play.txt). Found: the instance had been stopped
  (StopInstance by taeold@gmail.com, 2026-10-27). `instances update --image`
  on a stopped instance changes the spec but does not import the image, and
  `start` then runs the last imported one (old code answered the probe
  template with no command). An update while running imported it (92 s).
  The first gate run on the old code is discarded; old atespaces moved to
  gs://ate-node-daniel-junk/archive/2026-10-28/.
  Live: b (create --tag on an existing actor: 409, its snapshot generation
  and md5 unchanged); d (record set to running on the live worker that did
  not host it: the request placed it on that worker, 200 in 3.5 s,
  generation 2 to 3).
  Boot (sandbox run to wakeup probe) 741 to 747 ms, n=3; first-request
  resume at the ingress 1018 to 1128 ms, n=3; suspend 463 to 482 ms.
- [x] 6 README; PROGRESS.md moved to docs/. demo/template*.yaml rebuilt
  from pi-durable-substrate's template.yaml (changes: image, command
  /actor/..., PORT and probe port 8080). The parsed fields match the gated
  file; one actor from the final gateway template booted on the instance
  (/readyz 200 in 1.06 s), atespace moved to archive/.
- [x] 7 commit as Daniel Lee, gh repo create taeold/ate-ish --public

Resources after run 2:
- Running: Cloud Run instance ate-node (us-west1, role all) on
  ate-ish/pi-durable@sha256:92dd34fed7d5, bucket gs://ate-node-daniel-junk
  (ate-demo-pi from runs/play.txt; earlier runs under archive/).
- Public: Artifact Registry repository ate-ish (us-central1).
- Deleted: Artifact Registry repository ate-node, worker pool
  ate-node-workers, service ate-node-ingress, gs://ate-node-scale-daniel-junk.

Notes:
- 2026-10-28: opencode-gemini-key still answers 401 "API key not valid"
  (one curl to v1beta/models). Gates run on the Gateway variant.

## Run 1 (2026-10-26)


Region us-west1, project daniel-junk. The project was called ate-node then.

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
