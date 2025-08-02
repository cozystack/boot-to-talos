# boot-to-talos

Convert any OS to Talos Linux

## Installation

### Download a pre‑built release binary

Head over to [https://github.com/cozystack/boot-to-talos/releases](https://github.com/cozystack/boot-to-talos/releases), grab the archive for your OS/arch, unpack it somewhere on your `$PATH`.
## Example usage

```console
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

2025/08/02 22:01:43 created temporary directory /tmp/installer-1496483943
2025/08/02 22:01:43 pulling image ghcr.io/cozystack/cozystack/talos:v1.10.5
2025/08/02 22:01:43 extracting image layers
2025/08/02 22:01:47 creating raw disk /tmp/installer-1496483943/image.raw (2 GiB)
2025/08/02 22:01:47 attached /tmp/installer-1496483943/image.raw to /dev/loop0
2025/08/02 22:01:47 starting Talos installer
2025/08/02 22:01:47 running Talos installer v1.10.5
2025/08/02 22:01:47 WARNING: config validation:
2025/08/02 22:01:47   use "worker" instead of "" for machine type
2025/08/02 22:01:47 created EFI (C12A7328-F81F-11D2-BA4B-00A0C93EC93B) size 104857600 bytes
2025/08/02 22:01:47 created BIOS (21686148-6449-6E6F-744E-656564454649) size 1048576 bytes
2025/08/02 22:01:47 created BOOT (0FC63DAF-8483-4772-8E79-3D69D8477DE4) size 1048576000 bytes
2025/08/02 22:01:47 created META (0FC63DAF-8483-4772-8E79-3D69D8477DE4) size 1048576 bytes
2025/08/02 22:01:47 formatting the partition "/dev/loop0p1" as "vfat" with label "EFI"
2025/08/02 22:01:47 formatting the partition "/dev/loop0p2" as "zeroes" with label "BIOS"
2025/08/02 22:01:47 formatting the partition "/dev/loop0p3" as "xfs" with label "BOOT"
2025/08/02 22:01:47 formatting the partition "/dev/loop0p4" as "zeroes" with label "META"
2025/08/02 22:01:47 copying from io reader to /boot/A/vmlinuz
2025/08/02 22:01:47 copying from io reader to /boot/A/initramfs.xz
2025/08/02 22:01:47 writing /boot/grub/grub.cfg to disk
2025/08/02 22:01:47 executing: grub-install --boot-directory=/boot --removable --efi-directo0
2025/08/02 22:01:47 installation of v1.10.5 complete
2025/08/02 22:01:47 Talos installer finished successfully
2025/08/02 22:01:47 copy /tmp/installer-1496483943/image.raw → /dev/sda
2025/08/02 22:01:58 installation image copied to /dev/sda
```

---

Created for the Cozystack project. 🚀
