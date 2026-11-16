---
title: "Disk Images"
linkTitle: "Disk Images"
weight: 57
---

`kopia image` takes block-level images of Windows disks for bare-metal restore.
Images are stored in the same repository as your file snapshots, deduplicated
and encrypted like everything else.

Every image is a full, independent copy of the disk at one moment. Kopia reads
the allocated parts of each volume from a Volume Shadow Copy, but uploads only
the 4 MiB chunks the repository does not already have, so a nightly image
usually uploads a few gigabytes or less. Any image can be restored on its own;
there are no incremental chains to break.

## Requirements and limits

* Windows, run as administrator or SYSTEM (volume shadow copies need it).
* GPT disks. MBR disks, dynamic disks, Storage Spaces and ReFS volumes are refused.
* NTFS data volumes are imaged from a shadow copy. The EFI system, recovery and
  other partitions without a shadow copy are read twice and compared, so a
  change during the read is detected and retried. The Microsoft reserved
  partition is recreated on restore rather than imaged.
* Restore to the same or similar hardware. Restoring to different hardware may
  need driver and boot repairs.
* BitLocker volumes are imaged as their decrypted contents and come back
  decrypted; the repository's encryption protects them in between. Re-enable
  BitLocker after restoring. See [What an image contains](#what-an-image-contains).

## Creating images

Preview what will be imaged, without saving an image:

```shell
kopia image create C: --dry-run
```

A dry run still takes shadow copies to measure the volumes and removes them
again, so it needs an administrator and prepares `C:\ProgramData\kopia` like a
real run does; see [Scheduling](#scheduling).

Create an image of the disk holding `C:`:

```shell
kopia image create C:
```

The image belongs to the source `user@host:image:disk:<GUID>`, named after the
disk's GPT GUID so that it survives drive letter and disk number changes. Use
that full form with the standard commands, for example
`kopia snapshot list tim@desktop:image:disk:<GUID>`. `kopia image list` shows the
images with bytes read and uploaded per run.

Image chunks are compressed with `zstd-fastest` unless the image source's own
policy sets a compressor, because partly used chunks are mostly zeroed free
space. Free space, `pagefile.sys`, `hiberfil.sys`, `swapfile.sys` and the shadow
copy storage files are stored as zeros; Windows recreates them at boot.

A run that fails, for example when the network drops or the computer sleeps,
saves no image. Its uploads are recorded periodically (`--flush-interval`,
default 10 minutes) and once more when it fails, so the next run does not upload
them again. Shadow copies left behind by a crash are removed by the next run.

Every 45 minutes, and when it fails, a run also saves an incomplete
"checkpoint" snapshot of what it has uploaded so far. The checkpoint keeps
maintenance from removing those uploads as unused before a run completes, which
matters for a first upload that takes days of retries. Checkpoints are not
images: `kopia image list` hides them, `kopia snapshot list --incomplete` shows
them, and retention removes them once a newer image exists.

### What an image contains

An image is every allocated block of the disk, so it holds what the running
system keeps from other users and from someone who takes the disk:

* the contents of BitLocker volumes, decrypted;
* the Windows account database (the SAM and SECURITY registry hives) and the
  keys that protect saved credentials (DPAPI master keys);
* every user's files, whatever their permissions.

Whoever can read the repository can read all of it. The repository password is
the only protection, so guard it as you would the computer itself, and limit
who can reach the repository storage. On a computer whose disk encryption is
required by policy, check that policy before imaging it. `kopia image create`
prints a reminder of this on every run.

## Scheduling

Set up nightly images and a weekly check, from an administrator prompt:

```shell
kopia image schedule install C: --at 02:00
```

This creates two Task Scheduler tasks that run as SYSTEM:

| Task | Default | Options |
|---|---|---|
| `\Kopia\Image create` | every day at 02:00 | `--at`, `--days Mon,Thu` |
| `\Kopia\Image check` | Sundays at 12:00, 1% sample | `--check-day`, `--check-at`, `--check-sample`, `--check-day none` |

Both tasks wake the computer (`--wake=false` to disable), run a missed run as
soon as possible, need a network connection, never overlap, and stop after
`--time-limit` (default 4h). A failed image is retried `--retries` times
`--retry-interval` apart (default 3 times, 15 minutes). By default runs do not
start on battery power (`--allow-on-battery`).

SYSTEM cannot read the password saved in your Windows Credential Manager, so
`install` copies your current repository connection to
`C:\ProgramData\kopia\image`, with the password in a file, its own cache and its
logs. Images keep your `user@host`, so scheduled and interactive images share
one source.

The tasks run as SYSTEM, so nothing they use may be changeable by an ordinary
user:

* `install` also copies `kopia.exe` into `C:\ProgramData\kopia\image\bin`, and
  the tasks run that copy. The kopia you ran may be in a folder your account
  can write, such as a per-user installation, and whoever replaced it there
  would run as SYSTEM. **Run `kopia image schedule install` again after
  upgrading kopia**, or the tasks keep running the old version.
* `C:\ProgramData\kopia\image` is owned by Administrators and only SYSTEM and
  Administrators can open it.
* `C:\ProgramData\kopia`, which holds the run status and the list of shadow
  copies to clean up, is owned by Administrators; other users can only read it.
* The password file is encrypted with a key of this computer (Windows DPAPI),
  so a copy of the file is useless elsewhere. Any administrator of this
  computer can still decrypt it.

If either folder already holds a file or link that an administrator did not
create, or was itself created by another user and is not empty, `install` and
`kopia image create` stop and name it. Remove it and run the command again.

```shell
kopia image schedule run      # start an image now
kopia image schedule show     # next and last runs, and the last run's result
kopia image schedule remove   # remove the tasks and the saved password
```

Run the first image by hand (`kopia image schedule run`, or
`kopia image create C:`) and watch it finish: the first upload is the full disk
and can take longer than the time limit on a slow connection. If it is stopped
or interrupted, the next run reads the disk again but uploads only what is still
missing, so a slow first upload completes over several runs.

### Monitoring

Every run writes `C:\ProgramData\kopia\image-last-run.json` with its start and
end time, success or error, and the images it saved. A run that only finds
another run in progress leaves the file to that run. The tasks also exit with a
non-zero code on failure, which Task Scheduler records as the last result.
Point your monitoring at either.

## Retention

Set retention on the image source like any other source:

```shell
kopia policy set tim@desktop:image:disk:<GUID> \
  --keep-latest 3 --keep-daily 7 --keep-weekly 4 --keep-monthly 3
```

Keep images long enough to reach back past an infection. Ransomware often waits
days or weeks before encrypting, and the image to restore is one from before it
arrived. Keep weekly and monthly images, not only recent ones.

Each retained image keeps a list of its chunks, about 262,000 entries per TB of
disk, which full maintenance reads, so do not keep more images than you need.

Do not give the image source a snapshot schedule in the Kopia UI or policy:
image sources are never snapshotted as directories, and the UI shows them with
the status `IMAGE`.

## Checking images

```shell
kopia image check                  # latest image of each source of this host
kopia image check --all            # every retained image
kopia image check <source|id> --full
```

The check confirms that every chunk is in the repository index, that every
chunk's pack file exists in storage and is long enough, and fetches and
authenticates a sample of chunks (`--sample`, default 1%; `--full` for all).
Chunks this computer has read before may come from its local cache.

## Browsing images and restoring files

NTFS volume images can be browsed file by file without restoring them. Only
the parts of the image a listing or file read needs are downloaded.

```shell
kopia image ls <snapshot-id>                       # the volumes in the image
kopia image ls <snapshot-id>/p3-C/Users/tim -l     # files, with sizes and times
kopia image restore-files <snapshot-id>/p3-C/Users/tim/Documents C:\Recovered
```

The path starts with a snapshot ID from `kopia image list` or an image source
such as `tim@desktop:image:disk:<GUID>` (its latest image), then the volume
(`p3-C`, `p3-C.img` or the partition number `3`), then the path in it.
`restore-files` restores the file or folder into the given folder and does not
replace existing files unless you pass `--overwrite-files`. It restores file
contents and modification times, not NTFS permissions, and skips alternate data
streams; restore the whole disk image to keep those.

Mounting an image snapshot, with `kopia mount <snapshot-root-id> Z:` or the
**Mount** button in the Kopia UI, shows a folder such as `p3-C` next to each NTFS
volume image, so you can browse and copy files in Explorer.

NTFS metadata files such as `$MFT` are hidden. Alternate data streams appear as
files named `name:stream`. Files encrypted with EFS are listed but their
contents are not readable.

## Restoring

Restore from a Linux live USB with the `kopia` binary and a connection to the
repository; read-only access is enough. Restore the latest image of a source,
or a snapshot ID from `kopia image list`:

```shell
kopia image restore tim@desktop:image:disk:<GUID> --target /dev/nvme0n1 --dry-run
kopia image restore tim@desktop:image:disk:<GUID> --target /dev/nvme0n1
```

`--dry-run` opens the target read-only and changes nothing on it.

`image restore` recreates the partition table with the original disk and
partition GUIDs, so the Windows boot configuration stays valid, then writes
every volume, fetching `--parallel` chunks at once (default 16) and zeroing free
space without downloading it. Each volume is checked against its partition
before anything is written, and the unused end of a partition is zeroed, so
nothing of the disk's previous contents remains inside a partition. It refuses a
target that:

* has a different logical sector size or is too small (larger is fine),
* is mounted or in use,
* already has a partition table (pass `--overwrite`),
* shares its disk GUID with another attached disk, such as the original disk
  (pass `--force`; Windows takes one of two disks with the same GUID offline).

`--only p3-C` restores a single volume into a disk that already has the image's
partition table. `--target disk.img` writes a raw disk image file instead, which
virtual machines can boot. A missing file is created, except under `/dev`, where
a missing name is a mistyped device.

Each image also contains `restore.sh`, which restores using only stock `kopia`,
`sgdisk` and `dd`, if the `image restore` command is not available.

## Protecting images from ransomware

A client with full repository access can delete its own snapshots, and so can
ransomware running on it. Two layers keep images out of its reach:

* **Connect clients through a [repository server](../../repository-server/)
  that does not let them delete.** By default every user has full access to
  their own snapshots and policies. Replace those rules so clients can add
  snapshots but not delete them, and can read but not change their policies,
  so retention, which the server applies, cannot be turned against them:

  ```shell
  kopia server acl enable
  kopia server acl add --user '*@*' --access APPEND --overwrite \
    --target 'type:snapshot,username:OWN_USER,hostname:OWN_HOST'
  kopia server acl add --user '*@*' --access READ --overwrite \
    --target 'type:policy,username:OWN_USER,hostname:OWN_HOST'
  ```

  Set image retention on the server with full access. Run maintenance on the
  server, not on clients.

* **Keep read-only copies of the repository**, with storage that supports it
  ([object locks](../ransomware-protection/)) or with read-only file system
  snapshots, such as btrfs or ZFS, on the repository server.

To recover an infected computer, restore an image from before the infection
with a bare-metal restore. You do not need to roll back the repository for that,
and other computers' backups are unaffected. Then change the repository
password, since the restored computer holds the old one.

Roll the repository itself back to a file system snapshot only if it was
damaged. Before you do, stop every client, and clear each client's cache
(`kopia cache clear`) before reconnecting: for 15 minutes after writing, a client
trusts the index blobs it wrote even if storage does not list them, and after a
rollback those blobs are gone.
