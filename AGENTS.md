# holtdroid-sync — instructions for coding agents

Single source of rules for AI coding agents and human contributors.
`CLAUDE.md` only imports it. If an `AGENTS.local.md` exists next to this
file, read it too (machine-specific, not committed).

## What the project is

Sync jobs that copy APKs from Slack, S3 buckets and GitHub releases into a
[Holtdroid](https://github.com/kriakiku/holtdroid) folder or S3 source. See
`README.md` and `docs/{slack,s3,github}.md`.

Interfaces with Holtdroid, all defined in Holtdroid's `design/contracts.md`
("Artifact sources"):

- the source layout: `x.apk`, `x.apk.meta.json`, `x.apk.icon.png`,
  sidecars written before the APK (`internal/layout`);
- the include rule syntax (`internal/rules`);
- launch links `/?source=…&launch=…[&quick=1]` (`layout.LaunchURL`);
- the credentials endpoint format (`internal/broker`).

Change them together with Holtdroid. Never depend on Holtdroid's Go module.

## Layout

```
cmd/holtdroid-{slack,s3,github}-sync/   one binary per job: config, wiring, loop
internal/layout/       source layout: Meta, staging an APK (parse + icon), publishing with sidecars, removal, launch links
internal/announce/     new build announcements through shoutrrr
internal/run/          shared main: cobra flags, logging, signals, polling loop
internal/slacksync/    Slack: history, threads, forwards, reactions, Block Kit replies
internal/s3sync/       S3: list, filter, copy, prune
internal/awssso/       IAM Identity Center device login, refresh, token cache
internal/signin/       login links through shoutrrr
internal/broker/       /healthz, /status, /credentials (ECS format)
internal/githubsync/   GitHub releases: list, filter, copy, prune
internal/apk/ rules/ storage/ secrets/ config/   copies of Holtdroid's packages
deploy/helm/holtdroid-sync/   one chart, `job` picks the binary
docs/                  per-job documentation
assets/ slack-app-manifest.yaml   Slack app; keep scopes in sync with docs/slack.md and Holtdroid's docs/src/assets/
```

`apk`, `rules`, `storage`, `secrets` and `config` are copies of Holtdroid's
packages. Keep them close to upstream and port fixes both ways. Shared logic
goes into `layout`, `announce` or `run`, not into one job.

## Rules

- No state except the S3 job's SSO token cache file (mode 0600, never
  logged). Listings are the state; in-memory caches are optimisations.
- Prune only after a complete listing; a failed, rate-limited or
  unauthenticated listing must never delete copies.
- Secrets are references (`env:`, `file:`, `openbao:`), never inlined,
  logged or put into errors. The Slack token is only sent to Slack hosts.
- Everything from an origin is untrusted: icons go through
  `apk.NormalizeIcon`, sidecars and release notes through size limits.
- Never modify APKs. Almost no comments. No speculative abstractions.
- Before the first `v*` tag there is no backward compatibility.
- Image: distroless, non-root, pinned by digest, `linux/amd64` +
  `linux/arm64`.

## Testing

Table-driven tests next to the code. Slack, GitHub, Identity Center and
webhooks are `httptest` fakes, S3 is `gofakes3`. No test hits the network.

## Commands

Toolchain pinned in `mise.toml`; prefix with `mise exec --` if mise isn't
active.

```sh
make check   # golangci-lint, go test -race, build, helm lint per job
make fmt
make image
```

## Git

Conventional Commits, one line only, no body, no trailers. PR description
empty. Agents never commit or push unless asked. Every push to `main`
publishes `ghcr.io/kriakiku/holtdroid-sync:<sha>`.
