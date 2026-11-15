Kopia (personal fork with a Wails-based KopiaUI)
================================================

![Kopia](icons/kopia.svg)
[![Release](https://github.com/pletch/kopia-wailsUI/actions/workflows/release.yml/badge.svg)](https://github.com/pletch/kopia-wailsUI/actions/workflows/release.yml)

> [!IMPORTANT]
> This is a **personal fork** of [kopia/kopia](https://github.com/kopia/kopia). It is not an official Kopia release and is not supported by the Kopia maintainers. Please report problems with stock Kopia upstream, not here, and keep your own backups of anything that matters. Builds are unsigned, so Windows SmartScreen will warn the first time you run the installer.

What this fork adds
---

### A lighter KopiaUI built with Wails

The [`app-wails/`](app-wails) directory is a replacement for the Electron-based KopiaUI shell, built with [Wails v3](https://v3.wails.io/). It uses the WebView2 runtime that ships with Windows, or WebKitGTK on Linux, and runs the same `kopia server` and the same web UI, with:

* a tray icon that starts the `kopia` server and opens repository windows
* native desktop notifications (Windows toasts, or the freedesktop notification service on Linux), including more detailed notices for failed snapshots
* remembered window size and position
* an error page when the server fails to start, instead of a blank window
* a small per-user NSIS installer on Windows (no administrator rights needed), and a `.deb` on Linux; both share the repository configuration (`%APPDATA%\kopia` or `~/.config/kopia`) with upstream KopiaUI and the CLI

**It uses less memory in most scenarios.** Closing the window shuts WebView2 down completely, whereas Electron keeps its main, GPU and helper processes running. Measured on Windows 11 (private memory, UI shell only; the `kopia.exe` server adds about 140 MB to both):

| | Window open | Window closed (tray only) |
|---|---|---|
| Electron KopiaUI (upstream) | 219 MB | 178 MB |
| Wails KopiaUI (this fork) | 237 MB | **59 MB** |

With a window open the two are about the same, since both render the page with Chromium. Because KopiaUI spends most of its time in the tray, this fork typically saves about 120 MB of RAM, plus about 350 MB of disk.

### Upstream pull requests not yet merged

This fork carries the following open pull requests from [kopia/kopia](https://github.com/kopia/kopia/pulls). Each will be dropped from the fork once it is merged (or superseded) upstream.

| PR | Change |
|---|---|
| [#5185](https://github.com/kopia/kopia/pull/5185) | Enhance gRPC session management with keepalive and cancellation support |
| [#5364](https://github.com/kopia/kopia/pull/5364) | Avoid serializing concurrent session authentications |
| [#5455](https://github.com/kopia/kopia/pull/5455) | Compute weekly retention bucket in UTC |
| [#5460](https://github.com/kopia/kopia/pull/5460) | Avoid nil-pointer crash when a cache directory is set but caching is disabled |
| [#5461](https://github.com/kopia/kopia/pull/5461) | Detect symlink target changes when reusing cached entries |
| [#5492](https://github.com/kopia/kopia/pull/5492) | Stop cache hits from corrupting LRU size accounting |
| [#5527](https://github.com/kopia/kopia/pull/5527) | Do not deadlock pack GC when deletes keep failing |
| [#5544](https://github.com/kopia/kopia/pull/5544) | Retry failed scheduled snapshots |
| [#5583](https://github.com/kopia/kopia/pull/5583) | Parse time of day using base 10 |
| [#5611](https://github.com/kopia/kopia/pull/5611) | Don't panic when flushing a delete marker after a transient storage failure |
| [#5616](https://github.com/kopia/kopia/pull/5616) | Use actual written size when a file changes during upload |
| [#5639](https://github.com/kopia/kopia/pull/5639) | Avoid holding the server lock during repository refresh I/O |
| [#5674](https://github.com/kopia/kopia/pull/5674) | KopiaUI: wait for server address before loading the repository window |
| [#5719](https://github.com/kopia/kopia/pull/5719) | Reject directory entry names that escape the restore target |
| [#5735](https://github.com/kopia/kopia/pull/5735) | KopiaUI: poll server status only while it can change |
| [#5746](https://github.com/kopia/kopia/pull/5746) | Overwrite read-only files on Windows when restoring |
| [#5769](https://github.com/kopia/kopia/pull/5769) | Separate weekly and monthly retention buckets |
| [#5785](https://github.com/kopia/kopia/pull/5785) | Do not reschedule a snapshot that is waiting for an upload slot |
| [#5797](https://github.com/kopia/kopia/pull/5797) | Skip unrelated filesystem blob metadata reads |

### Repository server features

These are for running `kopia server start` as a shared repository server, for example on a home server that other machines back up to.

#### Snapshots served over WebDAV (`--webdav-mounts`)

In upstream Kopia, "Mount as Local Filesystem" in the web UI mounts the snapshot on the server's own machine, which is no use when you browse the UI from another computer. With `--webdav-mounts`, the server instead shares snapshots as read-only WebDAV on its own HTTPS port, protected by the web UI login:

* `https://<server>:51515/webdav/all/` is every snapshot of every source, arranged as `user@host/<path>/<snapshot time>/`. Map it once as a drive, or put it in fstab.
* `https://<server>:51515/webdav/<snapshot root ID>/` is a single snapshot. The web UI's mount button now shows this URL, ready to copy, and nothing is mounted on the server.
* Opening any `/webdav/` folder in a browser lists its contents and offers commands and downloadable scripts that mount that folder: a `.cmd` that maps it to a free drive letter on Windows, and a `.sh` that mounts it with GVfs on Linux.

Mounting by hand:

```
:: Windows (cmd), prompts for the web UI password
net use Z: "https://kopia.example:51515/webdav/all/" /user:kopia /persistent:no

# Linux with GVfs (GNOME Files, Nemo, Caja, Thunar)
gio mount davs://kopia.example:51515/webdav/all/

# Linux with davfs2, in /etc/fstab (credentials go in /etc/davfs2/secrets)
https://kopia.example:51515/webdav/all/ /mnt/kopia davfs ro,user,noauto,_netdev 0 0
```

Windows only connects over HTTPS to a server whose certificate it trusts, and by default the WebDAV client refuses to open files larger than 50 MB. To raise that limit, set `HKLM\SYSTEM\CurrentControlSet\Services\WebClient\Parameters\FileSizeLimitInBytes` to `0xFFFFFFFF` and restart the WebClient service. Only the web UI user can use the WebDAV shares; repository users (`user@host`) cannot.

#### Built-in ACME certificates (`--tls-acme-domain`)

The server can obtain and renew its own TLS certificate from an ACME certificate authority, either Let's Encrypt (the default) or a private CA such as step-ca. It uses the TLS-ALPN-01 challenge, so the CA must be able to reach the server on port 443. Grant the binary `CAP_NET_BIND_SERVICE`, or forward port 443 and set `--tls-acme-alpn-port`.

```
kopia server start --address 0.0.0.0:51515 \
  --tls-acme-domain kopia.example \
  --tls-acme-directory https://ca.example/acme/acme/directory \
  --tls-acme-ca-root /usr/local/share/ca-certificates/my-root-ca.crt
```

`--tls-acme-domain` can be repeated. Accounts, keys and certificates are stored in `acme` next to the config file unless `--tls-acme-storage-dir` is set, and `--tls-acme-email` sets the account contact.

#### Certificate file reload

When the server uses `--tls-cert-file` and `--tls-key-file`, it picks up renewed files within 10 seconds, or straight away on `SIGHUP`, without restarting. An external tool such as certbot or lego can therefore renew the certificate without interrupting backups.

### Other personal changes

* **Reconnect after sleep:** fixes a gRPC `readLoop` deadlock and reconnects to a repository server cleanly after Windows resumes from sleep.
* **Better failed-snapshot notifications** in the Electron KopiaUI as well (see [app/NOTIFICATIONS.md](app/NOTIFICATIONS.md)).
* **Named snapshot drives on Windows:** a snapshot mounted as a drive letter, from KopiaUI or `kopia mount`, appears in Explorer under its source and snapshot time (or "Kopia - All Snapshots") instead of "DavWWWRoot".

Installing
---

Releases are numbered `<upstream version>-wails.<n>`: `v0.23.1-wails.1` is the first fork release based on upstream Kopia v0.23.1 (including upstream changes made after v0.23.1; each release's notes name the exact upstream commit).

Download `KopiaUI-Wails-Setup-<version>.exe` (or the portable zip) from this fork's [Releases](https://github.com/pletch/kopia-wailsUI/releases) page. It installs per-user to `%LOCALAPPDATA%\Programs\KopiaUI-Wails` and bundles a `kopia.exe` built from this fork. Existing repositories configured with upstream KopiaUI are picked up automatically.

The same releases include the `kopia` command-line tool for Linux (`kopia-<version>-linux-x64.tar.gz` and `kopia-<version>-linux-arm64.tar.gz`), built from this fork with all of the changes above except the KopiaUI ones. They are static binaries with no dependencies: extract and run `kopia`.

For Debian and Ubuntu on x64 there is also `kopia-wails_<version>_amd64.deb`, installed with `sudo apt install ./kopia-wails_<version>_amd64.deb`. It puts `kopia` in `/usr/bin` and replaces the upstream `kopia` package if that is installed. Because it has a different package name, an upstream Kopia apt repository will not silently switch you back; remove `kopia-wails` and install `kopia` to return to upstream.

KopiaUI-Wails for Linux is `kopiaui-wails_<version>_amd64.deb`:

```sh
sudo apt install ./kopiaui-wails_<version>_amd64.deb
```

Like upstream KopiaUI, it includes its own copy of `kopia` (in `/usr/lib/kopiaui-wails/server/`), which it runs as its server, so it does not need `kopia-wails`. Install `kopia-wails` as well if you also want `kopia` in a terminal; the two packages do not conflict.

It is built on Ubuntu 22.04 against GTK 3 and WebKitGTK 4.1, so it runs on Ubuntu 22.04 / Linux Mint 21 and newer and on Debian 12 and newer. Start it from the applications menu as "KopiaUI-Wails". Mounting a snapshot uses FUSE (the `fuse3` package). The tray icon needs a desktop that shows status notifier icons: KDE, Cinnamon and XFCE do, while GNOME needs the AppIndicator extension. Without a tray, starting KopiaUI-Wails again brings its window back.

There are no macOS builds, rpm packages or Docker images.

Building
---

On Windows with Go and NSIS 3 installed, from Git Bash:

```sh
go build -o dist/kopia_windows_amd64/kopia.exe github.com/kopia/kopia
app-wails/build-installer.sh
```

On Linux with Go, a C compiler and the GTK 3 / WebKitGTK 4.1 headers (`sudo apt install build-essential libgtk-3-dev libwebkit2gtk-4.1-dev`):

```sh
app-wails/build-linux-deb.sh   # dist/kopiaui-wails_<version>_amd64.deb
```

To run it from the source tree instead, build with `go build -tags gtk3` in `app-wails/`. Without the tag, Wails builds against GTK 4 and WebKitGTK 6.0.

Pushing a `v<upstream version>-wails.<n>` tag runs the [Release](.github/workflows/release.yml) workflow, which builds the Windows installer and portable zip, the Linux CLI archives, and the `kopia-wails` and `kopiaui-wails` amd64 `.deb` packages, and publishes them as a GitHub release. See [BUILD.md](BUILD.md) for building the rest of Kopia.

About Kopia
---

Kopia is a fast and secure open-source backup/restore tool that allows you to create [encrypted](https://kopia.io/docs/features/#user-controlled-end-to-end-encryption) snapshots of your data and save the snapshots to [remote or cloud storage](https://kopia.io/docs/features/#save-snapshots-to-cloud-network-or-local-storage) of your choice, [to network-attached storage or server](https://kopia.io/docs/features/#save-snapshots-to-cloud-network-or-local-storage), or [locally on your machine](https://kopia.io/docs/features/#save-snapshots-to-cloud-network-or-local-storage). Kopia does not 'image' your whole machine. Rather, Kopia allows you to backup/restore any and all files/directories that you deem are important or critical.

Kopia has both [CLI (command-line interface)](https://kopia.io/docs/features/#both-command-line-and-graphical-user-interfaces) and [GUI (graphical user interface)](https://kopia.io/docs/features/#both-command-line-and-graphical-user-interfaces) versions, making it the perfect tool for both advanced and regular users. You can read more about Kopia's unique [features](https://kopia.io/docs/features/) -- which include [compression](https://kopia.io/docs/features/#compression), [deduplication](https://kopia.io/docs/features/#backup-files-and-directories-using-snapshots), [user-controlled end-to-end encryption](https://kopia.io/docs/features/#user-controlled-end-to-end-encryption), and [error correction](https://kopia.io/docs/features/#error-correction) -- to get a better understanding of how Kopia works.

When ready, head to the [installation](https://kopia.io/docs/installation/) page to download and install Kopia, and make sure to read the [Getting Started Guide](https://kopia.io/docs/getting-started/) for a step-by-step walkthrough of how to use Kopia.

Pick the Cloud Storage Provider You Want
---

Kopia supports saving your [encrypted](https://kopia.io/docs/features/#user-controlled-end-to-end-encryption) and [compressed](https://kopia.io/docs/features/#compression) snapshots to all of the following [storage locations](https://kopia.io/docs/features/#save-snapshots-to-cloud-network-or-local-storage):

* **Amazon S3** and any **cloud storage that is compatible with S3**
* **Azure Blob Storage**
* **Backblaze B2**
* **Google Cloud Storage**
* Any remote server or cloud storage that supports **WebDAV**
* Any remote server or cloud storage that supports **SFTP**
* Some of the cloud storage options supported by **Rclone**
  * Requires you to download and setup Rclone in addition to Kopia, but after that Kopia manages/runs Rclone for you
  * Rclone support is experimental: not all the cloud storage products supported by Rclone have been tested to work with Kopia, and some may not work with Kopia; Kopia has been tested to work with **Dropbox**, **OneDrive**, and **Google Drive** through Rclone
* Your local machine and any network-attached storage or server
* Your own server by setting up a [Kopia Repository Server](https://kopia.io/docs/repository-server/)

And Kopia uses [data deduplication](https://kopia.io/docs/features/#backup-files-and-directories-using-snapshots) to save you money! Read the [repositories help page](https://kopia.io/docs/repositories/) for more information on supported storage locations.

With Kopia you are in full control of where to store your snapshots, that is, you pick the storage provider you want to use. You must provision and pay for the storage provider for whatever storage locations you want to use, and then tell Kopia what those storage locations are. You can even use multiple storage locations for different backup repositories if you want. Kopia also supports backing up multiple machines to the same storage location.

Kopia in Action
---

Using Kopia via command-line interface:

[![asciicast](https://asciinema.org/a/ykx6uzEhKY3451fWEnX9nm9uo.svg)](https://asciinema.org/a/ykx6uzEhKY3451fWEnX9nm9uo)

Using Kopia via graphical user interface (note: the video is of an older version of Kopia and the interface is different in the current version of Kopia, but the main principles of the interface are the same):

[![Kopia UI Tutorial](https://img.youtube.com/vi/sHJjSpasWIo/0.jpg)](https://www.youtube.com/watch?v=sHJjSpasWIo)

Getting Started
---
See [Kopia Documentation](https://kopia.io/docs/) for more information. Also check out the [users forum](https://kopia.discourse.group).

Licensing
---
Kopia is licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for the full license text.

Building Kopia
---
See [Build Infrastructure](BUILD.md) for more information on building Kopia and working with the source code.

Contribution Guidelines
---
Kopia is open source. For more information see the [Contribution Guidelines](https://kopia.io/docs/contribution-guidelines/).

Reporting Security Issues
---
If you find a security issue you'd like to disclose privately, please contact `security@kopia.io`.

[![Netlify Status](https://api.netlify.com/api/v1/badges/6b5c1fe4-a0da-4e7e-939b-ff1105251985/deploy-status)](https://app.netlify.com/sites/kopia/deploys)
