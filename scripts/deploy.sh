#!/bin/sh
# Build kindled and copy it to the Kindle.
#
#   ./scripts/deploy.sh 192.168.15.244        # over USBNetwork or Wi-Fi
#
# Assumes SSH access to the jailbroken Kindle (USBNetwork or the Wi-Fi
# address it got from the hotspot) and that /mnt/us is writable.
set -e

HOST=${1:-192.168.15.244}
USER=${KINDLE_USER:-root}
DEST=${KINDLE_DEST:-/mnt/us/kindled}

cd "$(dirname "$0")/.."

echo "building kindled for armv7..."
make -C kindle kindled

echo "installing to $USER@$HOST:$DEST ..."
# $DEST is expanded here, on purpose: it is a local variable, not a remote one.
# shellcheck disable=SC2029
ssh "$USER@$HOST" "mkdir -p $DEST"
scp kindle/kindled "$USER@$HOST:$DEST/kindled"
scp scripts/kindled-start.sh "$USER@$HOST:$DEST/kindled-start.sh"
# shellcheck disable=SC2029
ssh "$USER@$HOST" "chmod +x $DEST/kindled $DEST/kindled-start.sh"

cat <<MSG

Installed. On the Kindle:

  $DEST/kindled -version          # sanity check
  $DEST/kindled-start.sh -v       # run it, logging to /mnt/us/kindled.log

If the panel size or touch orientation is wrong, override them:

  $DEST/kindled -width 1072 -height 1448 -touch-invert-y -v

MSG
