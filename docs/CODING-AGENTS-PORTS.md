# coding-agents-on-substrate ports

Three actors from [github.com/taeold/coding-agents-on-substrate](https://github.com/taeold/coding-agents-on-substrate)
were run on ate-ish to check compatibility. None of their source is copied
here; each ran from its own upstream clone, with the one-line delta below
(swapping the model provider where the project's own API key is revoked,
or nothing at all).

- **pi-durable**: `GEMINI_API_KEY` swapped for `AI_GATEWAY_API_KEY` (the
  Gemini key in the test project is revoked) by running the project's own
  `server-gateway.ts` instead of `server.ts`. No other change. This is
  also ate-ish's own quickstart actor (`demo/pi-durable`), which ships
  both variants side by side.
- **libfx**: unmodified. It already reads `AI_GATEWAY_API_KEY` itself.
- **cursor-agent-sdk**: unmodified. No credential is supplied
  (`CURSOR_API_KEY` stays the template's placeholder), so the run gets as
  far as the agent's first model call and stops there on an invalid key,
  not past it.

Every port ran with its command unchanged (listening on port 80, no
`PORT` env, no path rewriting) against a template whose `containers[0].image`
names a pullable image: ate-ish exports that image's filesystem once as
the actor's rootfs (see `internal/atelet`'s `imageRootfsDir`), the same
image reference substrate itself would use.
