# holtdroid-github-sync

Brings APKs attached to GitHub releases into
[Holtdroid](https://github.com/kriakiku/holtdroid). The job polls the
releases of one or more repositories and copies the `.apk` assets that match
your rules into a folder or S3 bucket, which Holtdroid lists as a read-only
source.

## What it copies

For every configured repository, the job walks the releases newest first:

- drafts are skipped;
- pre-releases are skipped unless `prereleases: true`;
- only the newest `releases` releases count (`0` = all);
- the `include` rules pick assets by `{tag}/{asset name}`;
- only `*.apk` assets are copied.

Each asset lands under the target as:

```
{owner}/{repo}/{tag}/{asset}.apk
{…}.apk.meta.json   # origin "github", the release name, notes, uploader, link
{…}.apk.icon.png    # the launcher icon re-rendered at 256 px, when there is one
```

The sidecar holds:

| Field        | Value                                                     |
| ------------ | --------------------------------------------------------- |
| `note`       | the release name, or the tag, plus "(pre-release)"        |
| `changelog`  | the release notes (Markdown, up to 64 KiB)                |
| `uploadedBy` | the GitHub login that uploaded the asset                  |
| `uploadedAt` | when the asset was last updated                           |
| `link`       | the release page                                          |
| `tags`       | `repo`, `release`, `target` (branch), `prerelease`        |

An asset is copied again when its size changes or it was re-uploaded after
the copy. With `prune` (the default), copies of assets that are gone from
GitHub, fall out of the newest `releases`, or no longer match the rules are
deleted. Pruning only happens after every repository was listed, so a
GitHub outage or rate limit never deletes copies. Give the job its own
target prefix, because it owns everything there.

## Rules

`include` is a gitignore-style list, evaluated against `{tag}/{asset}`:

```yaml
include: ["!*-debug.apk"]              # everything except debug builds
include: ["/v2.*", "!/v2.0-*"]         # tags starting with v2., without v2.0-…
```

- `*` matches within one path segment, and `**` spans directories.
- A pattern that matches a directory (a tag) matches everything below it.
- `!` excludes.
- The last matching pattern wins.
- An empty list takes every `*.apk`. A list that starts with an exclusion
  starts from "everything".

## Rate limits

Without a token, GitHub allows 60 API requests an hour per IP. A pass costs
one request per repository (more for repositories with over 100 releases)
plus one per downloaded asset, so the default 15-minute interval fits a
handful of public repositories. For more repositories, a shorter interval or
private repositories, set `github.token` to a token with read access to the
repositories' contents (5,000 requests an hour).

## New build announcements

`notify` sends one message per pass that copied anything, through
[shoutrrr](https://github.com/nicholas-fedor/shoutrrr) (Slack, Telegram,
Discord, Matrix, ntfy, email, webhooks, …). For each build, up to ten of
them, it lists:

- name, version and package;
- key and size;
- **Quick launch** and **Launch** links into the portal;
- the release page.

Assets last updated more than `maxAge` ago are left out, so the first copy
stays quiet.

## Configuration

A YAML file (`--config`), overridden by `HOLTDROID_GITHUB_SYNC_*` environment
variables. `__` separates levels, and lists are comma-separated (except
`repos`, which only the file can set).

```yaml
github:
  token: env:GITHUB_TOKEN       # optional; a secret reference
repos:
  - repo: WebView-CG/CanIAndroidWebView
    releases: 5                 # newest releases to keep; 0 = all (default)
    prereleases: false
    include: ["!*-debug.apk"]
interval: 15m
prune: true
target:                         # same shape as Holtdroid's storage
  type: s3                      # filesystem | s3
  s3: { endpoint: https://s3.example.com, bucket: holtdroid, prefix: github, accessKey: env:S3_ACCESS_KEY, secretKey: env:S3_SECRET_KEY }
notify:
  urls: [env:NOTIFY_URL]        # shoutrrr URLs, e.g. slack://xoxb:<bot token>@C0123ABCD; empty = off
  portalURL: https://holtdroid.example.com   # adds Quick launch / Launch links
  source: github                # the Holtdroid source that lists the target
  maxAge: 24h                   # older builds are not announced; 0 = all
secrets:
  openbao: { address: https://bao.example.com, kubernetes: { role: holtdroid-github-sync } }
```

In Holtdroid, list the target as a read-only source:

```yaml
sources:
  - name: github
    type: s3
    readOnly: true
    s3: { endpoint: https://s3.example.com, bucket: holtdroid, prefix: github, accessKey: env:S3_ACCESS_KEY, secretKey: env:S3_SECRET_KEY }
```

## Running

```sh
holtdroid-github-sync --config config.yaml          # poll forever
holtdroid-github-sync --config config.yaml --once   # one pass
```

Image: `ghcr.io/kriakiku/holtdroid-sync` (all jobs; run `holtdroid-github-sync`).
Helm chart: `deploy/helm/holtdroid-sync` with `job: github`, see the
[README](../README.md#deploying).
