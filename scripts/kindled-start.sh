#!/bin/sh
# On-device launcher for kindled.
#
# Copy this and the kindled binary to /mnt/us/kindled/ on the Kindle and run
# it from KUAL, an extension, or over SSH. It waits for the network, runs
# the daemon, and restarts it if it ever exits -- which it should not, since
# kindled handles reconnection itself, but a supervisor loop costs nothing
# and the alternative is a black panel in an airport.

DIR=$(dirname "$0")
BIN="$DIR/kindled"
LOG=${KINDLED_LOG:-/mnt/us/kindled.log}

# Keep the log from growing without bound on a device nobody reboots.
if [ -f "$LOG" ] && [ "$(wc -c < "$LOG")" -gt 1000000 ]; then
    mv "$LOG" "$LOG.1"
fi

log() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') $*" >> "$LOG"
}

log "kindled-start: waiting for a default route"
while true; do
    # A default route means the hotspot is up and we know where the Pixel
    # is; kindled reads the same table to find the gateway.
    if awk '$1 != "Iface" && $2 == "00000000" { found = 1 } END { exit !found }' \
        /proc/net/route 2>/dev/null; then
        break
    fi
    sleep 2
done
log "kindled-start: network is up"

# The Kindle framework will happily repaint over us. Stopping it is the
# usual practice for full-screen apps; uncomment if the UI fights back.
#   stop lab126_gui
# Remember to `start lab126_gui` afterwards, or reboot.

while true; do
    log "starting kindled"
    "$BIN" "$@" >> "$LOG" 2>&1
    log "kindled exited ($?); restarting in 5s"
    sleep 5
done
