# boot-to-talos

Convert any OS to Talos Linux — completely from userspace, no external dependencies except the Talos installer image.

## How it works

1. **Unpack in RAM** – layers from the Talos‑installer container are extracted into a throw‑away `tmpfs`; no Docker needed.
2. **Build system image** – a sparse `image.raw` is created, exposed via a loop device, and the Talos *installer* is executed inside a chroot; it partitions, formats and lays down GRUB + system files.
3. **Stream to disk** – the program copies `image.raw` to the chosen block device in 4 MiB chunks and `fsync`s after every write, so data is fully committed before reboot.
4. **Reboot** – `echo b > /proc/sysrq-trigger` performs an immediate reboot into the freshly flashed Talos Linux.

## Installation

Download binary from Github [releases page](https://github.com/cozystack/boot-to-talos/releases/latest)

Or use simple script to install it:
```bash
curl -sSL https://github.com/cozystack/boot-to-talos/raw/refs/heads/main/hack/install.sh | sh -s
```

## Example usage

```console
$ boot-to-talos
Target disk [/dev/sda]:
Talos installer image [ghcr.io/cozystack/cozystack/talos:v1.10.5]:
Add networking configuration? [yes]:
Interface [eth0]:
IP address [10.0.2.15]:
Netmask [255.255.255.0]:
Gateway (or 'none') [10.0.2.2]:
Configure serial console? (or 'no') [ttyS0]:

Summary:
  Image: ghcr.io/cozystack/cozystack/talos:v1.10.5
  Disk:  /dev/sda
  Extra kernel args: ip=10.0.2.15::10.0.2.2:255.255.255.0::eth0::::: console=ttyS0

WARNING: ALL DATA ON /dev/sda WILL BE ERASED!

Continue? [yes]:

2025/08/03 00:11:03 created temporary directory /tmp/installer-3221603450
2025/08/03 00:11:03 pulling image ghcr.io/cozystack/cozystack/talos:v1.10.5
2025/08/03 00:11:03 extracting image layers
2025/08/03 00:11:07 creating raw disk /tmp/installer-3221603450/image.raw (2 GiB)
2025/08/03 00:11:07 attached /tmp/installer-3221603450/image.raw to /dev/loop0
2025/08/03 00:11:07 starting Talos installer
2025/08/03 00:11:07 running Talos installer v1.10.5
2025/08/03 00:11:07 WARNING: config validation:
2025/08/03 00:11:07   use "worker" instead of "" for machine type
2025/08/03 00:11:07 created EFI (C12A7328-F81F-11D2-BA4B-00A0C93EC93B) size 104857600 bytes
2025/08/03 00:11:07 created BIOS (21686148-6449-6E6F-744E-656564454649) size 1048576 bytes
2025/08/03 00:11:07 created BOOT (0FC63DAF-8483-4772-8E79-3D69D8477DE4) size 1048576000 bytes
2025/08/03 00:11:07 created META (0FC63DAF-8483-4772-8E79-3D69D8477DE4) size 1048576 bytes
2025/08/03 00:11:07 formatting the partition "/dev/loop0p1" as "vfat" with label "EFI"
2025/08/03 00:11:07 formatting the partition "/dev/loop0p2" as "zeroes" with label "BIOS"
2025/08/03 00:11:07 formatting the partition "/dev/loop0p3" as "xfs" with label "BOOT"
2025/08/03 00:11:07 formatting the partition "/dev/loop0p4" as "zeroes" with label "META"
2025/08/03 00:11:07 copying from io reader to /boot/A/vmlinuz
2025/08/03 00:11:07 copying from io reader to /boot/A/initramfs.xz
2025/08/03 00:11:08 writing /boot/grub/grub.cfg to disk
2025/08/03 00:11:08 executing: grub-install --boot-directory=/boot --removable --efi-directory=/boot/EFI /dev/loop0
2025/08/03 00:11:08 installation of v1.10.5 complete
2025/08/03 00:11:08 Talos installer finished successfully
2025/08/03 00:11:08 remounting all filesystems read-only
2025/08/03 00:11:08 copy /tmp/installer-3221603450/image.raw → /dev/sda
2025/08/03 00:11:19 installation image copied to /dev/sda
2025/08/03 00:11:19 rebooting system
```

## Non-interactive installation

You can run `boot-to-talos` in fully automated mode by passing the required flags.  
To skip all interactive prompts, use the `-yes` flag:

```console
boot-to-talos -yes
```

You can also specify all parameters explicitly:

```console
boot-to-talos -yes -disk /dev/sda -image ghcr.io/cozystack/cozystack/talos:v1.10.5 -image-size-gib 4 -extra-kernel-arg "console=ttyS0"
```

## Available command-line flags

| Flag                  | Description                                                        | Example                                         |
|-----------------------|--------------------------------------------------------------------|-------------------------------------------------|
| `-yes`                | Run non-interactively, do not ask for confirmation                 | `-yes`                                          |
| `-disk string`        | Target disk (will be wiped)                                        | `-disk /dev/sda`                                |
| `-image string`       | Talos installer image (default: `ghcr.io/cozystack/cozystack/talos:v1.10.5`) | `-image ghcr.io/cozystack/cozystack/talos:v1.10.5` |
| `-image-size-gib uint`| Size of image.raw in GiB (default: 2)                              | `-image-size-gib 4`                             |
| `-extra-kernel-arg value` | Extra kernel argument (can be repeated)                        | `-extra-kernel-arg "console=ttyS0"`             |

**Tip:** All flags can be combined. If a flag is not provided, the installer will prompt for input (unless `-yes` is used).

---

Created for the Cozystack project. 🚀
