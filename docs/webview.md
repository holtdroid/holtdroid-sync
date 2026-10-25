# holtdroid-webview-sync

Fills [Holtdroid](https://github.com/holtdroid/holtdroid)'s WebView catalog
with Chromium builds of Android System WebView, so a session can start with
a chosen WebView version instead of the one built into Android. Holtdroid
installs the chosen build over the built-in WebView before the app starts
(Holtdroid's `design/decisions.md` #88).

## Where the builds come from

Chromium publishes a snapshot build of every few commits on its main branch
in the public `chromium-browser-snapshots` bucket. The
`AndroidDesktop_x64` builds are x86_64, the ABI Holtdroid's redroid
sessions run, and their `chrome-android-desktop.zip` holds
`apks/SystemWebView.apk`: WebView as `com.android.webview`, signed with
Chromium's public debug key. holtdroid-redroid re-signs the built-in
WebView with the same key, so any snapshot installs over it as an update.
They go back to Chromium 129 (mid-2024). Older versions have no x86_64
snapshots, so sessions use the built-in WebView for those (125 to 128,
depending on the Android version).

The job never downloads the whole zip (about 500 MB). It reads the zip's
directory with HTTP range requests and downloads only the WebView APK
(about 190 MB compressed, 420 MB unpacked).

## What it copies

Milestones come from [Chromium Dash](https://chromiumdash.appspot.com):

- the milestones currently on Android's Stable, Beta, Dev and Canary
  channels, always;
- the newest `milestones.latest` milestones;
- every milestone divisible by `milestones.every` (130, 135, 140, …);
- past the newest `milestones.older.after` available milestones, only those
  divisible by `milestones.older.every` instead (off by default);
- `milestones.extra`.

All of them are limited to milestones that have snapshots. For each one,
the job takes the latest Canary release of that milestone and the newest
snapshot at or before its position on the main branch. So a milestone's
build is its last Canary, give or take a few commits. The current Canary
milestone moves with every new Canary release.

Each build lands under the target as:

```
chromium-{milestone}.apk
chromium-{milestone}.apk.meta.json
```

The sidecar holds:

| Field        | Value                                                                  |
| ------------ | ---------------------------------------------------------------------- |
| `title`      | `Chromium {milestone}`                                                 |
| `note`       | the WebView version and the snapshot revision                          |
| `uploadedAt` | when that Canary release came out                                      |
| `link`       | the snapshot's folder in the bucket                                    |
| `origin`     | `chromium-snapshots`                                                   |
| `tags`       | see below                                                              |

The `tags` are:

- `milestone`;
- `revision` (the snapshot);
- `version`, `minSdk` and `targetSdk` (from the APK);
- `requiresSdk`: the Android API level the build actually needs, when a
  `requirements` rule raises it above `minSdk`;
- `frontend`: the Canary release's commit, whose DevTools frontend Holtdroid
  serves, because snapshot builds report no revision of their own;
- `channel`: a comma-separated subset of `stable`, `beta`, `dev` and `canary`.

A build is downloaded again only when its snapshot revision changes. When
only the channels or `requiresSdk` change, only the sidecar is rewritten.

## Requirements

Every snapshot declares `minSdk` 29 (Android 10), but newer ones don't work
on older Android. On Holtdroid's redroid images, Chromium 149 and later
abort when a WebView is created on Android 11, 12 and 13, with
`libc++ Hardening assertion this->has_value() failed`. Chromium 148 and
older work, and Android 14 and 15 run every build. `requirements` records
such breakages, and Holtdroid offers a build only on Android versions at or
above both `minSdk` and `requiresSdk`:

```yaml
requirements:          # the default
  - { from: 149, minSdk: 34 }
```

Each rule applies to its milestone and every later one, and the highest
matching `minSdk` wins. Setting `requirements` replaces the default list. With `prune` (the
default), builds of milestones that are no longer picked are deleted, but
only after a pass in which Chromium Dash and the bucket answered. The job
owns everything under its target prefix.

With the defaults (`latest: 6`, `every: 5`), the catalog holds about 11
builds, around 4.5 GB.

## Configuration

```yaml
milestones:
  latest: 6       # newest milestones
  every: 5        # plus every milestone divisible by this (0: none)
  older:          # past the newest `after`, every `older.every`th instead of `every`th
    after: 0      # 0: off
    every: 0
  extra: [133]    # plus these
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
    prefix: webview
    accessKey: env:S3_ACCESS_KEY
    secretKey: env:S3_SECRET_KEY
    pathStyle: true
```

Environment overrides use `HOLTDROID_WEBVIEW_SYNC_`, for example
`HOLTDROID_WEBVIEW_SYNC_MILESTONES__LATEST=8`.

Holtdroid reads the same place through its `webview.storage` setting. It is
a separate catalog, not an artifact source, so the builds don't show up in
the build list.

## Deploying

```yaml
job: webview
existingSecret: holtdroid-webview-sync
tmpSize: 2Gi # one unpacked APK at a time
config:
  target: { type: s3, s3: { … , prefix: webview } }
```

The job needs only outbound HTTPS to `chromiumdash.appspot.com` and
`storage.googleapis.com`, plus access to the target.
