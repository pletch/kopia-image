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
the 1 MiB chunks the repository does not already have. A chunk is new when any
of its bytes changed, so how much a nightly image uploads depends on how
scattered the day's writes are: from a few gigabytes or less on a quiet
computer to tens of gigabytes, before compression, on a busy one.
`kopia image compare <old> <new>` shows where an image's new data comes from.
Any image can be restored on its own; there are no incremental chains to break.

Images made before kopia `0.23.1-wails.40` have 4 MiB chunks, which store an
estimated twice as much per night. They still restore, browse and check, but the
first image of each disk with 1 MiB chunks shares nothing with them and uploads
in full. `kopia image create --chunk-size` takes sizes from 128K to 8M.

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

A run whose connection to the repository breaks or cannot be made, for example
when a laptop switches from Wi-Fi to Ethernet, images again over a new
connection after 30 seconds, 2 minutes and 5 minutes. What it saved is not
uploaded again, disks it already imaged are not imaged again, and the run's
status records only the final outcome.

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
  would run as SYSTEM. **After upgrading kopia, update that copy**, or the
  tasks keep running the old version: the Kopia-Image installer does it after
  a UAC prompt, or run `kopia image schedule update` as administrator. It
  keeps the tasks' settings and saved password.
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

Every user can read the tasks' status, so `schedule show` and the Kopia UI show
the next and last runs without administrator rights; only administrators can
start, change or remove the tasks. Tasks scheduled with kopia before
`0.23.1-wails.45` are hidden from other users until `install` or `update` runs
again.

If an image run is still going when you remove the schedule, the files it has
open, such as its copy of `kopia.exe` and its log, are removed when Windows
restarts. The saved password is always removed at once.

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

Each retained image keeps a list of its chunks, about a million entries per TB
of disk with 1 MiB chunks, which full maintenance reads, so do not keep more
images than you need.

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

Mounting an image snapshot, with `kopia mount <snapshot-root-id> Z:` or
**Browse** on the **Disk Images** page of the Kopia UI, shows a folder such as
`p3-C` next to each NTFS volume image, so you can browse and copy files in
Explorer. With [WinFsp](../../mounting/#windows) installed, the drive can be read
only by the user who mounted it.

NTFS metadata files such as `$MFT` are hidden. Alternate data streams appear as
files named `name:stream`. Files encrypted with EFS are listed but their
contents are not readable.

An image shows its files without the NTFS permissions they had, so the
credential stores that only administrators can read on the original disk are
hidden unless kopia runs as an administrator: `Windows\System32\config` (the
registry hives with password hashes and LSA secrets),
`Windows\System32\Microsoft\Protect` (the machine's DPAPI keys),
`Windows\ServiceProfiles`, `Windows\repair`, `Windows\NTDS`, `pagefile.sys`,
`hiberfil.sys` and `swapfile.sys`. To see them, run `kopia image ls`,
`kopia image restore-files` or `kopia mount` in an administrator command prompt.
Anyone with the repository password can still restore the whole disk, so keep
the password as safe as the computer itself.

## Recovery media

A computer whose disk failed cannot run kopia to restore itself. Build recovery
media while it still works: a USB stick that boots into the computer's own
Windows Recovery Environment (WinRE), with kopia, the repository connection and
the drivers it needs, and walks you through the restore. Nothing needs
installing; Windows already has everything the media is built from. Run it as
administrator:

```shell
kopia image recovery-media create --usb 3                    # erase USB disk 3 and make it the media
kopia image recovery-media create --iso D:\kopia-recovery.iso # keep an ISO file to write later
```

Without a target, `create` lists the USB disks to choose from, and offers to
save an ISO file instead. Other targets are `--output-dir`, a folder to copy
onto any FAT32 USB stick, and `--vhdx`, a virtual disk to try the media in
Hyper-V. The **Create Recovery Media** button on the **Disk Images** page of
the Kopia UI builds a USB stick or an ISO file after a UAC prompt.

An ISO file is for keeping until it is needed and then writing to a USB stick
with a tool such as [Rufus](https://rufus.ie/), which copies its files to a
FAT32 stick that boots like media made with `--usb`. The ISO does not boot as a
DVD or in a virtual machine: Windows does not keep its DVD boot files up to
date with its recovery environment. To test the media in a virtual machine, use
`--vhdx`.

The media boots with Secure Boot on. It holds:

* **The repository connection,** by default the current one, or the file given
  with `--connect-from`, such as a connection with read-only access. Certificate
  authorities the recovery system must trust to reach a repository server are
  added with `--ca-cert`.
* **No password,** unless you pass `--embed-password`. Anyone holding media with
  the password can read every image in the repository, so keep it as safe as
  the password itself. With `--iso` it asks first, since an ISO file is easily
  copied to other places.
* **Drivers:** this computer's storage, network and USB drivers, so that the
  recovery system sees its disks and network (`--drivers none` adds none).
  Every image stores the drivers of the computer it was taken of, so one stick
  can restore several computers: `--drivers-from <host>` adds those of another
  computer's newest image, `--drivers-from all` those of every computer, and
  `--add-driver` adds a driver `.inf` file or folder. `kopia image drivers` lists
  what the images store.

The recovery system reaches the repository over a wired network connection;
restoring over Wi-Fi is untested.

### Restoring with recovery media

Boot the computer from the stick. The media starts `kopia image recover`, which:

1. connects to the repository, asking for the password unless the media holds
   it. Through a repository server it asks which user to log in as, the media's
   by default, so that an account that reads every computer's images can
   restore any of them; another user always enters their own password;
2. lists the images to choose from, newest first;
3. lists the disks to restore to, shows the plan, including any NTFS volumes
   it must shrink to fit a smaller disk, and erases the disk only after you
   type `erase`;
4. restores the image, deletes the files the image left out once Windows
   mounts the volumes, and says when the restore is complete before asking to
   restart.

A system disk restored inside a USB enclosure does not boot Windows from the
enclosure: Windows does not start from USB disks. Install the disk in the
computer, or restore with the disk already installed.

### Keeping media current

The media records what it was built with, and `kopia image recovery-media
status`, the Kopia UI and every image run say when it no longer matches the
computer:

* a newer kopia is installed;
* Windows updated its recovery environment, after which old media may stop
  booting with Secure Boot on;
* the repository connection changed, or the server's certificate is no longer
  trusted the way the media trusts it;
* the partitions of an imaged disk changed;
* the computer has drivers that the media lacks;
* the media is more than 180 days old.

Images that left files out need media built with kopia `0.23.1-wails.29` or
newer, which deletes those files after the restore; older media refuses such
images rather than leave the files filled with zeros.

Media built on one computer can serve others: run `kopia image recovery-media
register <media>` as administrator on each of them, so that their status checks
it too. `kopia image recovery-media kit` prints the recovery kit, which is what
someone needs to restore the computer without you: the repository, the disks
with their newest images, and a line to write down where the password is kept.
It never holds the password. Keep it with the media, and a printed copy
somewhere else.

## Restoring

The [recovery media](#recovery-media) restores a Windows computer without
anything else. You can also restore from a Linux live USB with the `kopia`
binary and a connection to the repository; read-only access is enough. Restore
the latest image of a source, or a snapshot ID from `kopia image list`:

```shell
kopia image restore tim@desktop:image:disk:<GUID> --target /dev/nvme0n1 --dry-run
kopia image restore tim@desktop:image:disk:<GUID> --target /dev/nvme0n1
```

`--dry-run` opens the target read-only and changes nothing on it.

`image restore` recreates the partition table with the original disk and
partition GUIDs, so the Windows boot configuration stays valid, then writes
every volume, fetching about 64 MiB of chunks at once (`--parallel` sets the
number of chunks: by default 64 of 1 MiB, or 16 of 4 MiB). Chunks of zeros are
not downloaded. In the free clusters of NTFS volumes they are not written
either, since NTFS ignores what free clusters hold, so a mostly empty volume
restores quickly; zeros inside files are written. Each volume is checked against
its partition before anything is written, and the unused end of a partition is
zeroed. Writes to a disk that briefly stops responding, as some USB enclosures
do, are retried for about a minute.

A target smaller than the imaged disk is fine if the volumes fit: NTFS volumes
are shrunk, the largest first and no more than needed, and the partitions after
them move down, keeping their GUIDs. Files are not moved, so a volume shrinks
only as far as its last used cluster. The plan is shown before anything is
written. Windows checks a shrunk volume when it next mounts it; if the recovery
partition moved, run `reagentc /disable` and `reagentc /enable` in Windows
afterwards.

`image restore` refuses a target that:

* has a different logical sector size, or is too small even with its NTFS
  volumes shrunk,
* is mounted or in use,
* is not blank, with a partition table or any other data in its first or last
  MiB (pass `--overwrite`),
* shares its disk GUID with another attached disk, such as the original disk
  (pass `--force`; Windows takes one of two disks with the same GUID offline).

`--only p3-C` restores a single volume into a disk that already has the image's
partition table, keeping the disk's other partitions as they are. On Windows
the volume may be mounted: it is locked and dismounted while it is written, and
refused if it holds Windows or the repository. On Linux no partition of the
disk may be mounted. The volume's boot sectors are written last, so an
interrupted restore leaves a volume that does not mount rather than one that
holds partly old data. `--target disk.img` writes a raw disk image file instead, which
virtual machines can boot. A missing file is created, except under `/dev`, where
a missing name is a mistyped device.

Each image also contains `restore.sh`, which restores using only stock `kopia`,
`sgdisk`, `lsblk` and GNU `dd`, if the `image restore` command is not
available: `bash restore.sh /dev/sdX`, or a `/dev/disk/by-id/...` name. It
refuses a partition or a disk with mounted file systems, clears the partition
table, writes each volume at its partition's offset, no longer than its image,
and writes the partition table last. It does not shrink volumes, so the target
must be at least as large as the imaged disk.

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
