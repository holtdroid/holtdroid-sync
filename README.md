# holtdroid-sync

Sync jobs that bring APKs into [Holtdroid](https://github.com/kriakiku/holtdroid).
Each job writes Holtdroid's source layout into a folder or an S3 prefix, and
Holtdroid lists that place as a read-only source:

```
x.apk
x.apk.meta.json   # who, when, changelog, link back to the origin
x.apk.icon.png    # the launcher icon re-rendered at 256 px, when there is one
```

| Job                     | Brings builds from                                                       | Docs                                 |
| ----------------------- | ------------------------------------------------------------------------ | ------------------------------------ |
| `holtdroid-slack-sync`  | Slack channels: messages, threads and forwards; reacts and replies with launch buttons | [docs/slack.md](docs/slack.md)   |
| `holtdroid-s3-sync`     | an S3 bucket, with AWS IAM Identity Center logins; or brokers credentials so Holtdroid reads it directly | [docs/s3.md](docs/s3.md) |
| `holtdroid-github-sync` | assets of GitHub releases                                                | [docs/github.md](docs/github.md)     |

The jobs are independent of Holtdroid's code and of each other. They share
only this repository: the APK parser and icon renderer, the folder/S3
target, secret references, `include` rules and new-build announcements
through [shoutrrr](https://github.com/nicholas-fedor/shoutrrr).

## Deploying

One image, `ghcr.io/kriakiku/holtdroid-sync` (`linux/amd64`, `linux/arm64`,
distroless, non-root), holds all jobs as `/usr/local/bin/holtdroid-{slack,s3,github}-sync`:

```sh
docker run --rm -v $PWD/config.yaml:/config.yaml ghcr.io/kriakiku/holtdroid-sync holtdroid-github-sync --config /config.yaml --once
```

One Helm chart, `deploy/helm/holtdroid-sync` (tagged releases:
`oci://ghcr.io/kriakiku/charts/holtdroid-sync`), runs one job per release:

```yaml
job: github # slack | s3 | github
existingSecret: holtdroid-github-sync # env vars for env: references in config
config: # the job's config.yaml, see its docs
  repos:
    - repo: WebView-CG/CanIAndroidWebView
  target:
    type: s3
    s3: { endpoint: https://s3.example.com, bucket: holtdroid, prefix: github, accessKey: env:S3_ACCESS_KEY, secretKey: env:S3_SECRET_KEY }
persistence:
  enabled: false # true for a filesystem target or the s3 job's SSO token cache (/data)
```

Resource names are the release name. The `s3` job also gets a Service on
port 8080 and `/healthz` probes. `extraObjects` renders extra manifests,
e.g. an `ExternalSecret`.

## Common options

Every job takes a YAML file (`--config`), overridden by
`HOLTDROID_{SLACK,S3,GITHUB}_SYNC_*` environment variables (`__` separates
levels, lists are comma-separated), and `--once` for a single pass.

- **Secrets** are references, never values: `env:NAME`, `file:/path`,
  `openbao:secret/path#key` (with `secrets.openbao`).
- **Targets** have the shape of Holtdroid's storage: `type: filesystem` with
  `filesystem.dir`, or `type: s3` with `endpoint`, `region`, `bucket`,
  `prefix`, `accessKey`, `secretKey`, `pathStyle`.
- **`include` rules** (S3 and GitHub) are gitignore-style: `*` within a path
  segment, `**` across them, a pattern that matches a directory matches
  everything below it, `!` excludes, the last match wins, and a list that
  starts with `!` starts from everything.
- **Launch links** are `{portalURL}/?source={source}&launch={base64url key}`,
  plus `&quick=1` to start a device right away.

Give each job its own target prefix: the S3 and GitHub jobs delete copies
that are gone from their origin.

## Development

```sh
mise install
make check   # golangci-lint, go test -race, build, helm lint for every job
```

## License

Apache-2.0
