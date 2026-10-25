# holtdroid-chromium-sync

Copies Chromium browser builds into a
[Holtdroid](https://github.com/holtdroid/holtdroid) source, one per
milestone, so a page can be opened in the exact browser version a bug was
reported on.

## Where the builds come from

The same snapshots as [holtdroid-webview-sync](webview.md): the
`AndroidDesktop_x64` builds in the public `chromium-browser-snapshots`
bucket, whose `chrome-android-desktop.zip` also holds
`apks/ChromePublic.apk`. That is Chromium as `org.chromium.chrome`, x86_64,
one APK with no splits, signed with Chromium's public debug key. It is
Chromium, not Google Chrome: no Google sign-in, sync or Play services
features. The snapshots go back to Chromium 129 (mid-2024).

The job reads the zip's directory with HTTP range requests and downloads
only the browser APK (about 420 MB unpacked).

## What it copies

Milestones come from [Chromium Dash](https://chromiumdash.appspot.com),
limited to the ones that have snapshots. Counting the available milestones
newest first, the job keeps:

- the milestones currently on Android's Stable, Beta, Dev and Canary
  channels, always;
- the newest `milestones.latest`;
- every milestone divisible by `milestones.every`;
- past the newest `milestones.older.after`, only milestones divisible by
  `milestones.older.every` instead;
- `milestones.extra`.

With the defaults (`latest: 10`, `every: 2`, `older: {after: 24, every: 5}`)
that is every milestone of the last ten months or so, every other one for
the two years before, and every fifth beyond. In October 2026 (Canary 158)
it picks 149–158, the even milestones 136–148, and 130: 18 builds, about
7.5 GB. As new milestones come out, odd ones leave the newest ten and even
ones pass the newest 24, and with `prune` (the default) their builds are
deleted. Pruning happens only after a pass in which Chromium Dash and the
bucket answered, and the job owns everything under its target prefix.

For each milestone the job takes the latest Canary release of that
milestone and the newest snapshot at or before its position on the main
branch, as the WebView job does. Each build lands under the target as:

```
chromium-{milestone}.apk
chromium-{milestone}.apk.meta.json
```

The sidecar holds:

| Field        | Value                                             |
| ------------ | ------------------------------------------------- |
| `title`      | `Chromium {milestone}`                            |
| `note`       | the Chromium version and the snapshot revision    |
| `uploadedAt` | when that Canary release came out                 |
| `link`       | the snapshot's folder in the bucket               |
| `origin`     | `chromium-snapshots`                              |
| `tags`       | the same as the [WebView job's](webview.md)       |

`requirements` works as in the WebView job and has the same default
(`{from: 149, minSdk: 34}`): it sets the `requiresSdk` tag. Holtdroid shows
tags on builds but doesn't stop a launch on an older Android version.

A build is downloaded again only when its snapshot revision changes. When
only the channels or `requiresSdk` change, only the sidecar is rewritten.

## Configuration

```yaml
milestones:
  latest: 10
  every: 2
  older:
    after: 24     # 0: no thinning
    every: 5      # 0: nothing past the newest 24 except channels and extra
  extra: [133]
requirements:
  - { from: 149, minSdk: 34 }
interval: 6h
prune: true
target:
  type: s3
  s3:
    endpoint: https://s3.example.com
    region: us-east-1
    bucket: holtdroid
    prefix: chromium
    accessKey: env:S3_ACCESS_KEY
    secretKey: env:S3_SECRET_KEY
    pathStyle: true
```

Environment overrides use `HOLTDROID_CHROMIUM_SYNC_`, for example
`HOLTDROID_CHROMIUM_SYNC_MILESTONES__OLDER__EVERY=10`.

Holtdroid lists the prefix as an ordinary read-only source:

```yaml
sources:
  - name: chromium
    type: s3
    readOnly: true
    s3: { endpoint: https://s3.example.com, bucket: holtdroid, prefix: chromium, accessKey: env:S3_ACCESS_KEY, secretKey: env:S3_SECRET_KEY }
```

## Deploying

```yaml
job: chromium
existingSecret: holtdroid-chromium-sync
tmpSize: 2Gi # one unpacked APK at a time
config:
  target: { type: s3, s3: { … , prefix: chromium } }
```

The job needs only outbound HTTPS to `chromiumdash.appspot.com` and
`storage.googleapis.com`, plus access to the target.
