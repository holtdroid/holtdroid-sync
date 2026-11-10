# holtdroid-archive-sync

Copies APKs from public release archives that are served as plain HTTP
directory listings, such as Opera's and Firefox's, into a folder or S3 bucket
that [Holtdroid](https://github.com/holtdroid/holtdroid) lists as a read-only
source. No site-specific code: each archive is a few lines of config.

## What it copies

For each archive, the job reads the listing at `url` and takes:

- with `versionDir`, the newest `versions` subdirectories whose name matches
  it, and in each the files of its newest `fileVersion`;
- without `versionDir`, the newest `versions` file versions directly under
  `url`.

`depth` is how many levels of subdirectories to look through for APKs below
a version directory. `include` picks files by their path below `url`, with
the same gitignore-style rules as the other jobs. `fileVersion` is a regular
expression with one capture group; set it whenever a version has one file per
architecture, so that they count as one version. Without it the whole file
name is the version.

Each APK lands under the target as `{name}/{path below url}` with a sidecar:

| Field        | Value                                                     |
| ------------ | --------------------------------------------------------- |
| `title`      | `title` and the major version of the APK                  |
| `note`       | the file name                                             |
| `uploadedAt` | the file's `Last-Modified`                                |
| `link`       | the directory it came from                                |
| `origin`     | `archive`                                                 |
| `tags`       | `archive` (its name), `version`                           |

An APK is copied again when its size changes or its `Last-Modified` is newer
than the copy. With `prune` (the default), copies that drop out of the
selection are deleted, but only after a pass in which every listing was read.
Give the job its own target prefix, because it owns everything there.

The job identifies itself as `holtdroid-archive-sync/{version}` and makes no
attempt to pass bot checks, so only use it on archives that serve automated
clients.

## Configuration

A YAML file (`--config`), overridden by `HOLTDROID_ARCHIVE_SYNC_*`
environment variables. `archives` can only be set in the file.

```yaml
archives:
  - name: opera
    title: Opera
    url: https://get.opera.com/pub/opera/android/
    versionDir: '^\d+$'
    fileVersion: 'Opera_([0-9_]+)_Generic'
    include: ["*_arm8_*.apk", "*_x86_*.apk"]
    versions: 5                 # the default
  - name: opera-gx
    title: Opera GX
    url: https://get.opera.com/pub/opera/android/opera-gx/
    fileVersion: 'opera-gx-([0-9.]+)\.apk'
  - name: firefox
    title: Firefox
    url: https://archive.mozilla.org/pub/fenix/releases/
    versionDir: '^\d+\.\d+(\.\d+)?$'
    fileVersion: 'fenix-([0-9.]+)\.multi'
    depth: 2
    include: ["*arm64-v8a.apk", "*x86_64.apk"]
interval: 6h
prune: true
target:                         # same shape as Holtdroid's storage
  type: s3                      # filesystem | s3
  s3: { endpoint: https://s3.example.com, bucket: holtdroid, prefix: archive, accessKey: env:S3_ACCESS_KEY, secretKey: env:S3_SECRET_KEY }
```

In Holtdroid, list the target as a read-only source:

```yaml
sources:
  - name: browsers
    type: s3
    readOnly: true
    s3: { endpoint: https://s3.example.com, bucket: holtdroid, prefix: archive, accessKey: env:S3_ACCESS_KEY, secretKey: env:S3_SECRET_KEY }
```

## Running

```sh
holtdroid-archive-sync --config config.yaml          # poll forever
holtdroid-archive-sync --config config.yaml --once   # one pass
```

Image: `ghcr.io/holtdroid/holtdroid-sync` (all jobs). Helm chart:
`deploy/helm/holtdroid-sync` with `job: archive`, see the
[README](../README.md#deploying).
