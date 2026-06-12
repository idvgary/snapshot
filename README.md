# Local RunsOn Snapshot Action

This is a local fork of `runs-on/snapshot` for snapshotting directories on RunsOn self-hosted runners.

The fork keeps the action generic, but adds snapshot stream keys and path-scoped identity so matrix jobs can use independent snapshots safely.

## Usage

```yaml
jobs:
  cargo-build:
    runs-on:
      - runs-on=${{ github.run_id }}
      - family=m8azn
      - image=ubuntu24-full-x64

    steps:
      - name: Install Rust
        uses: dtolnay/rust-toolchain@stable

      - name: Restore Cargo build-state snapshot
        id: cargo-snapshot
        uses: your-org/snapshot@v1
        with:
          path: /mnt/cargo-build-state
          key: cargo-${{ runner.os }}-${{ runner.arch }}-release
          volume_size: 20
          save: auto
          save-if: git-paths-changed
          save-policy-name: cargo-build-inputs
          save-policy-version: v1
          save-marker-file: /mnt/cargo-build-state/.runs-on-snapshot/save-marker
          git-repository: /mnt/cargo-build-state/workspace
          git-head: ${{ github.sha }}
          git-paths: |
            Cargo.lock
            Cargo.toml
            .cargo/**
            build.rs
            crates/**
            src/**

      - name: Checkout source into snapshot workspace
        shell: bash
        env:
          SNAPSHOT_WORKSPACE: /mnt/cargo-build-state/workspace
          GITHUB_TOKEN: ${{ github.token }}
        run: |
          set -euo pipefail

          mkdir -p "$SNAPSHOT_WORKSPACE"
          git config --global --add safe.directory "$SNAPSHOT_WORKSPACE"
          cd "$SNAPSHOT_WORKSPACE"

          if [ ! -d .git ]; then
            git init .
            git remote add origin "https://github.com/${GITHUB_REPOSITORY}.git"
          else
            git remote set-url origin "https://github.com/${GITHUB_REPOSITORY}.git"
          fi

          auth_header="AUTHORIZATION: basic $(printf 'x-access-token:%s' "$GITHUB_TOKEN" | base64 | tr -d '\n')"
          if [ "${GITHUB_REF#refs/heads/}" != "$GITHUB_REF" ]; then
            fetch_ref="+${GITHUB_REF}:refs/remotes/origin/${GITHUB_REF_NAME}"
          else
            fetch_ref="$GITHUB_REF"
          fi

          git -c "http.https://github.com/.extraheader=$auth_header" fetch --force --prune --no-tags origin "$fetch_ref"

          current_head="$(git rev-parse --verify HEAD 2>/dev/null || true)"
          if [ "$current_head" = "$GITHUB_SHA" ] && [ -z "$(git status --porcelain --untracked-files=no)" ]; then
            echo "HEAD already matches $GITHUB_SHA; skipping checkout to preserve file mtimes."
            exit 0
          fi

          git -c advice.detachedHead=false checkout --detach --force "$GITHUB_SHA"

      - name: Build with Cargo state on snapshot volume
        shell: bash
        env:
          SNAPSHOT_ROOT: /mnt/cargo-build-state
          CARGO_HOME: /mnt/cargo-build-state/cargo-home
          CARGO_TARGET_DIR: /mnt/cargo-build-state/workspace/target
        run: |
          set -euo pipefail
          mkdir -p "$CARGO_HOME"
          cd /mnt/cargo-build-state/workspace
          cargo build --release --locked

          mkdir -p "$SNAPSHOT_ROOT/.runs-on-snapshot"
          printf 'save=true\n' > "$SNAPSHOT_ROOT/.runs-on-snapshot/save-marker"
```

## Inputs

| Input | Description | Required | Default |
| --- | --- | --- | --- |
| `path` | Absolute mount path to snapshot. | Yes | - |
| `key` | Snapshot stream identity. Use distinct keys for independent matrix jobs or workloads. | Yes | - |
| `restore-keys` | Optional newline-delimited fallback keys to try after the primary key. | No | - |
| `default-branch-fallback` | Try the repository default branch after current-branch lookups miss. | No | `true` |
| `version` | Snapshot format/manual invalidation version. | No | `v1` |
| `volume_type` | EBS volume type. Supports `standard`, `gp2`, `gp3`, `io1`, `io2`, `st1`, and `sc1`. | No | `gp3` |
| `volume_iops` | EBS volume IOPS. Used only for `gp3`, `io1`, and `io2`. | No | `3000` |
| `volume_throughput` | EBS volume throughput in MiB/s. Used only for `gp3`; must not exceed `0.25 MiB/s` per provisioned IOPS. | No | `750` |
| `volume_size` | EBS volume size in GiB. | No | `40` |
| `volume_initialization_rate` | EBS provisioned volume initialization rate in MiB/s for volumes created from snapshots. Use `100`-`300`; `0` disables this setting. | No | `0` |
| `wait_for_completion` | Wait for snapshot completion before exiting. The first snapshot always waits. | No | `false` |
| `save` | Save mode. Use `true`, `false`, or `auto`. | No | `true` |
| `save-if` | Policy used when `save` is `auto`. Supported values are `always` and `git-paths-changed`. | No | `always` |
| `force-save` | Force snapshot creation even when `save` is `auto` and the policy would skip. | No | `false` |
| `save-on-empty` | When `save` is `auto`, save if restored source metadata is missing. | No | `true` |
| `wait-for-cleanup` | When snapshot creation is skipped or `save=false`, wait for detach and delete the volume. Snapshot saves always wait for detach before creating the snapshot. | No | `true` |
| `save-policy-name` | Optional policy name written into snapshot source metadata. | No | - |
| `save-policy-version` | Optional policy version written into snapshot source metadata. | No | - |
| `save-marker-file` | Optional file inside the snapshot. If missing, post cleanup runs but snapshot creation is skipped. If it contains `save=false`, snapshot creation is skipped. | No | - |
| `git-repository` | Git repository path for smart-save path comparisons. | No | `<path>/workspace` |
| `git-head` | Current source SHA to store and compare. Defaults to `GITHUB_SHA`. | No | - |
| `git-paths` | Newline-delimited Git pathspecs used by `save-if: git-paths-changed`. | No | - |
| `retention-days` | Tag snapshots with a delete-after epoch this many days in the future. `0` disables. | No | `0` |
| `keep-last-snapshots` | Best-effort prune older snapshots for the same key after save, keeping this many newest. `0` disables. | No | `0` |

## Outputs

| Output | Description |
| --- | --- |
| `restored` | `true` when an existing snapshot was restored, `false` when a blank volume was used. |
| `restored-from` | Restore source: `branch`, `restore-key`, `default-branch`, `default-branch-restore-key`, or `empty`. |
| `restored-branch` | Branch that supplied the restored snapshot. |
| `restored-snapshot-id` | EBS snapshot ID used for restore. |
| `volume-id` | EBS volume ID mounted by the action. |
| `restored-source-sha` | Source SHA recorded in the restored snapshot metadata. |
| `restored-source-ref` | Source ref recorded in the restored snapshot metadata. |

## Snapshot Identity

Snapshot lookup includes:

```text
repository
branch
key hash
path hash
version
arch
platform
RunsOn stack tags
```

The path hash prevents two different mount paths with the same key/version from restoring each other's snapshots.

## Restore Order

The action tries snapshots in this order:

```text
current branch + key
current branch + restore-keys
default branch + key
default branch + restore-keys
blank volume
```

## Smart Save

When `save: auto` and `save-if: git-paths-changed` are set, the post step reads source metadata from the restored snapshot and compares it with `git-head`.

The action saves by default when metadata is missing. This seeds new snapshot streams safely.

If `save-marker-file` is configured, the workflow should write the marker only after the build output has been uploaded successfully. This lets the post step always unmount/detach while avoiding snapshots of failed or partial builds.

The action skips saving when:

```text
restored source SHA already equals current source SHA
no configured git-paths changed between restored SHA and current SHA
```

Before creating a snapshot, the action writes:

```text
<snapshot-root>/.runs-on-snapshot/source.json
```

That metadata becomes the base for the next restore.

## Workspace Snapshot Pattern

For source/build-state snapshots, do not mount directly over `${{ github.workspace }}`. Mount under `/mnt/...`, then checkout and build inside a child directory such as:

```text
/mnt/build-state/workspace
```

This avoids busy workspace unmount failures and keeps the GitHub workspace available for local actions and artifact uploads.
