#!/bin/bash
# Example pre_install script: mounts a dedicated data disk at /var/lib/norca
# before the deployer creates the Norca directories. Safe to run again.
set -euo pipefail

if mountpoint -q /var/lib/norca; then
  exit 0
fi
if [ ! -b "$DATA_DEVICE" ]; then
  echo "no data disk $DATA_DEVICE, using the root file system"
  exit 0
fi
if ! blkid "$DATA_DEVICE" >/dev/null 2>&1; then
  mkfs.ext4 -q -L norca-data "$DATA_DEVICE"
fi
mkdir -p /var/lib/norca
grep -q 'LABEL=norca-data' /etc/fstab || echo 'LABEL=norca-data /var/lib/norca ext4 defaults,noatime 0 2' >> /etc/fstab
mount /var/lib/norca
