# holtdroid-slack-sync

Copies APKs posted to Slack channels into a
[Holtdroid](https://github.com/kriakiku/holtdroid) artifact source: a
directory or an S3 bucket/prefix that the Holtdroid api lists. It is an
independent job (Holtdroid decision #27): it only writes files, Holtdroid
picks them up on its next poll.

## What it writes

For every `*.apk` attached to a message in the configured channels — posted
directly, in a thread reply, or inside a forwarded message:

```
{channel ID}/{file ID}/{file name}.apk
{channel ID}/{file ID}/{file name}.apk.meta.json
{channel ID}/{file ID}/{file name}.apk.icon.png   # when the launcher icon can be rendered
```

The icon is the APK's launcher icon. The job takes the densest bitmap, or
composes the adaptive icon from its bitmap layers. It treats the image as
untrusted input:

- size limits, and dimensions checked before decoding;
- decoded with Go's memory-safe PNG/JPEG/WebP decoders;
- re-rendered as a 256 × 256 PNG.

The icon is optional. If the APK only has a vector icon, none is written.

The sidecar follows Holtdroid's `Meta` (`design/contracts.md`, "Artifact
sources"), is written before the APK, and carries:

| Field | From |
|-------|------|
| `changelog` | message text; for a forward, the forwarder's comment followed by the original text; Slack markup turned into plain text |
| `note`, `link` | permalink of the message in the channel |
| `uploadedBy` | the poster's display name (bots: the bot name; forwards: the original author) |
| `uploadedAt` | message time (forwards: the original message time) |
| `title` | the Slack file title, when it differs from the file name |
| `origin` | `slack` |

Nothing that matters is kept in memory or on local disk: on each poll the
job reads the channel history for the last `lookback`, lists what is already
in the target, and downloads only missing files. Edited messages refresh the
sidecar without downloading the APK again. Restarts lose nothing.

### Threads

Thread replies are read for every thread whose parent is within `lookback`.
A thread is fetched again only when its `latest_reply` changes (an
in-memory cache; after a restart each active thread is read once), so quiet
threads cost nothing. Limitations of polling:

- replies to threads whose parent is older than `lookback` are not seen;
- editing a reply that was already synced doesn't refresh its sidecar until
  the thread gets a new reply.

### Reactions and replies

Both are off by default.

- `slack.react` adds a reaction to the message once its APKs are in the
  target. It doubles as a marker: a message the bot already reacted to gets
  no new reaction or reply, even if the target was emptied and the build is
  copied again. Bots can use any custom emoji of the workspace but can't
  create one: upload [`assets/holtdroid-emoji.png`](../assets/holtdroid-emoji.png)
  as `:holtdroid:` (Customize workspace → Emoji), or pick a stock one such as
  `white_check_mark`.
- `slack.reply` answers in the message's thread with a Block Kit card. The
  card shows the app label, package name, version name and code, file name,
  size, min/target SDK and native ABIs. When `portalURL` is set it also has
  two link buttons:
  - **Quick launch**, `{portalURL}/?source={source}&launch={artifact ID}&quick=1`,
    starts a device right away with the user's last provider and location;
  - **Launch…** (the same link without `quick`) opens the launch dialog.

  The artifact ID is the base64url of the key, as in Holtdroid's UI API, and
  `source` is the name of the Holtdroid source that points at this target.
  The buttons are plain links, so the app needs no interactivity endpoint.

## Slack app

Create it from the manifest: <https://api.slack.com/apps> → **Create New
App** → **From a manifest** → pick the workspace → paste
[`slack-app-manifest.yaml`](../slack-app-manifest.yaml) → **Create** →
**Install to Workspace**. Copy the **Bot User OAuth Token** (`xoxb-…`) from
**OAuth & Permissions**. Optionally set the app icon in **Basic Information**
from [`assets/holtdroid-emoji.png`](../assets/holtdroid-emoji.png); manifests
can't carry icons.

The manifest requests these bot scopes:

- `channels:history` (public channels) and `groups:history` (private ones);
- `files:read`;
- `users:read`: names of uploaders and mentions; without it they show as user IDs;
- `reactions:write`, for `slack.react`;
- `chat:write`, for `slack.reply`.

Drop the last three from the manifest if you don't use those features.

Invite the bot into every channel (`/invite @Holtdroid`). Channels are
configured by ID, not by name: open the channel details, and the ID is at
the bottom of the "About" tab.

Apps that are not internal to your workspace are subject to Slack's strict
`conversations.history` limits for non-Marketplace apps; the job waits out
`429`s, but keep `interval` reasonable.

## Configuration

A YAML file (`--config`), overridden by `HOLTDROID_SLACK_SYNC_*`
environment variables (`__` separates levels, lists are comma-separated:
`HOLTDROID_SLACK_SYNC_SLACK__CHANNELS=C1,C2`).

```yaml
slack:
  token: env:SLACK_BOT_TOKEN   # secret reference: env:, file:, openbao:
  channels: [C0123ABCD]        # channel IDs
  threads: true                # also look into thread replies
  lookback: 168h               # how far back each poll reads history
  retention: 0s                # > lookback: delete synced builds older than this; 0 keeps them
  react:
    enabled: false
    emoji: holtdroid           # custom or stock emoji name, colons optional
  reply:
    enabled: false
    portalURL: ""              # e.g. https://holtdroid.example.com; empty: no launch link
    source: slack              # Holtdroid source name for the launch link
target:                        # where to write; same shape as Holtdroid's storage
  type: filesystem             # filesystem | s3
  filesystem:
    dir: /data
  s3:
    endpoint: https://s3.example.com
    region: ""
    bucket: apks
    prefix: slack
    accessKey: env:S3_ACCESS_KEY   # empty: IAM / instance credentials
    secretKey: env:S3_SECRET_KEY
    pathStyle: false
interval: 1m                   # poll period
secrets:
  openbao:                     # only needed for openbao: references
    address: https://bao.example.com
    token: ""                  # a reference, or use Kubernetes auth:
    kubernetes:
      role: holtdroid-slack-sync
```

Point a Holtdroid source at the same place, read-only:

```yaml
sources:
  - name: slack
    type: s3
    readOnly: true
    s3: { endpoint: https://s3.example.com, bucket: apks, prefix: slack, accessKey: env:S3_ACCESS_KEY, secretKey: env:S3_SECRET_KEY }
```

`retention` ages builds by the time they were written to the target.

## Running

```sh
holtdroid-slack-sync --config config.yaml          # poll every interval
holtdroid-slack-sync --config config.yaml --once   # one pass, non-zero exit on errors (CronJob)
```

Image: `ghcr.io/kriakiku/holtdroid-sync` (all jobs; run `holtdroid-slack-sync`).
Helm chart: `deploy/helm/holtdroid-sync` with `job: slack`, see the
[README](../README.md#deploying).
