# Supported Systems and Package Managers

What the deployer installs Norca on, in one place. Details are in [Getting started](getting-started.md#supported-operating-systems), [Host packages and scripts](host-packages-and-scripts.md) and [Windows nodes](windows.md).

## Operating systems

| Family | Systems | Init system | Package manager | Norca binary |
| --- | --- | --- | --- | --- |
| Debian | Debian 11+, Ubuntu 20.04+, systems with `ID_LIKE=debian` | systemd | `apt` | glibc release |
| RHEL | RHEL 9+, Rocky Linux 9+, AlmaLinux 9+ (RHEL 10 included) | systemd | `dnf` | glibc release |
| Alpine | Alpine 3.22+ | OpenRC | `apk` | musl release (`_musl` archive) |
| Windows | Windows Server with OpenSSH and Windows PowerShell 5.1 | Windows service | none | Windows release |

- **Detection:** before it changes anything, `apply` reads `/etc/os-release`, the package manager and the init system of every node and prints them (`Host: debian 12 (debian family), amd64, systemd, apt`). When the detection is wrong, set [`host_facts`](configuration.md#host_facts) of the node.
- **Refused before anything changes:** a family other than these, no known package manager (set `host_facts.package_manager: none` for a system without one), an init system other than systemd or OpenRC, and versions older than the minimums: Debian 10 and Ubuntu 18.04 (glibc too old, no deb822 `Signed-By`), RHEL 8 and its rebuilds (glibc 2.28; Norca needs 2.34), and Alpine before 3.22 (no longer supported by Alpine). The [install script of scaled machines](node-scaling-machines.md) refuses the same systems.
- **Detected, not tested:** Fedora and CentOS Stream count to the RHEL family.
- **Architectures:** the Norca releases have Linux `amd64` and `arm64` (glibc and musl) and Windows `amd64`. `arm` and `386` are detected, but need a binary of your own (`release.source: local-path`).
- **Becoming root:** SSH as `root`, or a user with `sudo` or `doas` (`connection.become: true`). On Windows, an administrator account; `become` is refused there.
- **Shell:** the deployer's own commands run in `sh` (busybox `sh` on Alpine is enough). `bash` is needed only for scripts of the plan with `interpreter: bash`, the default on Linux.

## Package managers

`host_packages` has one block per package manager. A node takes the block of its own package manager and ignores the others.

| Block | Systems | Versions | Repositories | Local files | Pin a version | Hold |
| --- | --- | --- | --- | --- | --- | --- |
| `host_packages.apt` | Debian, Ubuntu | apt of Debian 11+ / Ubuntu 20.04+ | deb822 sources with `Signed-By` (key inline or as a file) | `.deb` | yes | `apt-mark hold` |
| `host_packages.dnf` | RHEL, Rocky Linux, AlmaLinux | dnf 4 and dnf5 | `.repo` files, `gpgcheck=1` (ASCII-armored key) | `.rpm` | yes | versionlock (the plugin on dnf 4, built into dnf5) |
| `host_packages.apk` | Alpine | apk 2.14 and apk 3 | `/etc/apk/repositories` lines; signed by a key of Alpine or by a key of the plan | `.apk` | yes | `name=version` in `/etc/apk/world` |
| none | Windows | | | | | |

- **Unsigned repositories are not supported.** The node never downloads a key itself: it comes from the plan (or, on Alpine, is one of Alpine's own).
- **A package without a version** on dnf and apk stays at the version it was first installed with (`dnf install` and `apk add` do not upgrade an installed package). Pin a `version` to move it with the plan.
- **One manager only:** `validate` warns, and `apply` refuses, when the packages of a node's roles are only in the block of another package manager (for example a plan with an `apt` block only and an Alpine node).
- **Windows** has no `host_packages`; use [scripts](windows.md#scripts) in PowerShell.

## What follows from the system

| | Debian / RHEL | Alpine | Windows |
| --- | --- | --- | --- |
| Agent service | systemd unit | OpenRC script under `supervise-daemon` | Windows service `norca` |
| Firewall (`apply_firewall_rules`) | an active ufw, a running firewalld, else nftables, else iptables | the same order | not configured: open the ports yourself |
| `consul.dns_forwarding` | systemd-resolved or dnsmasq | dnsmasq only ([setup](networking-and-tuning.md#alpine-dnsmasq)) | skipped with a warning |
| SELinux | labels restored on RHEL; enforcing is not verified | | |
