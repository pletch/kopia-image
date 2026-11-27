---
title: "Synchronization"
linkTitle: "Synchronization"
weight: 55
---

## Synchronization

Maintaining multiple copies of a repository is important for disaster recovery scenarios. While cloud-based repositories often have better durability than local ones, a local repository copy may help speed up data recovery.

Kopia v0.6.0 adds support for automatic repository replication, which enables incremental copies of the currently connected repository to a separate storage location.

Any repository location can be used as target. For example:

```
$ kopia repository sync-to filesystem --path /dest/repository
$ kopia repository sync-to gcs --bucket my-bucket
```

The calling user must have read access to current repository and read/write access to the destination.

By default, synchronization does not perform any deletions in the destination location, even if the source file has been deleted.  Without deletions, the resulting repository will still be correct, but will not benefit from compaction and will run more slowly. To allow deletions, pass `--delete` option:

```
$ kopia repository sync-to filesystem --path /dest/repository --delete
```

Deletions only happen after every copy has succeeded. `--delete` is refused when the destination does not already contain a copy of this repository, so it can't wipe unrelated data in a bucket or directory; run the first synchronization without it.

Blobs are copied in dependency order: data packs first, then indexes, then repository configuration, with the format blob (`kopia.repository`) last. An interrupted synchronization never leaves the destination with an index that points at missing data. It is safe to synchronize while snapshots or maintenance run on the source. If blobs disappear from the source during the copy, the command fails without deleting anything and asks you to run it again.

If the source repository uses object locking (retention), copies to destinations that support it are locked with the same retention. Destinations that don't support it get a warning and unlocked copies.

Copies run 8 at a time by default; use `--parallel` to change that. To limit the bandwidth used to write to the destination, pass `--max-upload-speed` (bytes per second). Reads from the source follow the throttling configured for the connected repository (`kopia repository throttle set`).

When synchronizing to a filesystem location, it is important to check that the filesystem is mounted correctly. To ensure that Kopia will not unnecessarily download potentially large repositories when destination filesystem is accidentally unmounted, pass `--must-exist`:

```
$ kopia repository sync-to filesystem --path /dest/repository --must-exist
```

### Automation

For automated synchronization tasks, progress output can be suppressed using the `--no-progress` flag to provide clean output suitable for cron jobs and scripts.
