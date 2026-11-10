# holtdroid-chromium-builds-sync

Copies the releases of
[holtdroid/chromium-builds](https://github.com/holtdroid/chromium-builds)
into [Holtdroid](https://github.com/holtdroid/holtdroid): the Chromium
browsers (`org.chromium.chrome`) into a read-only source and the WebViews
(`com.android.webview`) into the WebView catalog, each for x86_64 and arm64.

## What it copies

The job lists the repository's releases and takes one only when:

- it is not a draft;
- its tag is a Chromium version (`158.0.8127.22`);
- its notes have the line `Released to Android Stable: <RFC 3339>` or
  `Released to Android Beta: <RFC 3339>`;
- all four APKs are fully uploaded: `chromium-<tag>-x86_64.apk`,
  `chromium-<tag>-arm64.apk`, `webview-<tag>-x86_64.apk` and
  `webview-<tag>-arm64.apk`.

A release is built one architecture at a time, so until the last APK is up
the job leaves it alone. Builds are copied newest first and land under the
target as:

```
browser/{tag}/chromium-{tag}-{arch}.apk
webview/{tag}/webview-{tag}-{arch}.apk
{…}.apk.meta.json   # see below
{…}.apk.icon.png    # the launcher icon re-rendered at 256 px, when there is one
```

The sidecar holds:

| Field        | Value                                                       |
| ------------ | ----------------------------------------------------------- |
| `title`      | `Chromium {milestone}`                                      |
| `note`       | the version and architecture                                |
| `uploadedAt` | the date from the release notes, not the GitHub publication |
| `link`       | the release page                                            |
| `origin`     | `chromium-builds`                                           |
| `tags`       | see below                                                   |

The `tags` are `release`, `version` and `milestone`; `minSdk`, `targetSdk`
and `abis` from the APK; `channel`, set to `stable` on the newest Stable
release and to `beta` on the newest Beta; and `requiresSdk` from the
`requirements` rules.

An APK is copied again when its size changes or it was re-uploaded after the
copy. Sidecars of the others are rewritten when they no longer match, as when
a newer release takes over the channel. With `prune` (the default), copies of
releases that are gone from GitHub are deleted. A release that loses an APK
keeps its copies, and a failed listing never deletes anything. Give the job
its own target prefix, because it owns everything there.

## Rate limits

A pass costs one GitHub API request per 100 releases. APKs come from the
public download URLs, which don't count against the API limit, so the job
needs no token for a public repository.

## Configuration

A YAML file (`--config`), overridden by `HOLTDROID_CHROMIUM_BUILDS_SYNC_*`
environment variables. `__` separates levels.

```yaml
github:
  token: env:GITHUB_TOKEN       # optional; a secret reference
repo: holtdroid/chromium-builds # the default
requirements:                   # the defaults
  browser:
    - { from: 149, minSdk: 34 }
    - { from: 157, minSdk: 35 }
  webview:
    - { from: 149, minSdk: 34 }
interval: 15m
prune: true
target:                         # same shape as Holtdroid's storage
  type: s3                      # filesystem | s3
  s3: { endpoint: https://s3.example.com, bucket: holtdroid, prefix: chromium-builds, accessKey: env:S3_ACCESS_KEY, secretKey: env:S3_SECRET_KEY }
secrets:
  openbao: { address: https://bao.example.com, kubernetes: { role: holtdroid-chromium-builds-sync } }
```

In Holtdroid, list the `browser` folder as a read-only source and the
`webview` folder as the WebView catalog:

```yaml
sources:
  - name: chromium
    type: s3
    readOnly: true
    s3: { endpoint: https://s3.example.com, bucket: holtdroid, prefix: chromium-builds/browser, accessKey: env:S3_ACCESS_KEY, secretKey: env:S3_SECRET_KEY }
webview:
  storage:
    type: s3
    s3: { endpoint: https://s3.example.com, bucket: holtdroid, prefix: chromium-builds/webview, accessKey: env:S3_ACCESS_KEY, secretKey: env:S3_SECRET_KEY }
```

All releases take about 55 GB. An APK is unpacked into `/tmp` before upload;
the chart's default `tmpSize` is enough.

## Running

```sh
holtdroid-chromium-builds-sync --config config.yaml          # poll forever
holtdroid-chromium-builds-sync --config config.yaml --once   # one pass
```

Image: `ghcr.io/holtdroid/holtdroid-sync` (all jobs). Helm chart:
`deploy/helm/holtdroid-sync` with `job: chromium-builds`, see the
[README](../README.md#deploying).
