#!/bin/sh
[ -f /etc/profile.d/hw-accel.sh ] && . /etc/profile.d/hw-accel.sh

# 1. DYNAMIC CONFIG: Read mgmt_url from kernel boot parameters!
if grep -q "mgmt_url=" /proc/cmdline; then
    export MGMT_SERVER_URL=$(cat /proc/cmdline | grep -o 'mgmt_url=[^ ]*' | cut -d= -f2)
    echo "Using Management Server from boot params: $MGMT_SERVER_URL" > /tmp/boot-config.log
fi

xset s off
xset -dpms
xset s noblank
xsetroot -solid "#0b0f19"

/usr/bin/pipewire &
/usr/bin/pipewire-pulse &
/usr/bin/wireplumber &
openbox &
sleep 1

sudo mkdir -p /mnt/dev
if sudo mount -t 9p -o trans=virtio,version=9p2000.L hostdev /mnt/dev 2>/dev/null; then
    cd /mnt/dev
    UI_DIR=/mnt/dev/ui /mnt/dev/thin-kiosk-agent >/tmp/agent.log 2>&1 &
    WINDOW_BIN="/mnt/dev/thin-kiosk-window"
    [ ! -x "$WINDOW_BIN" ] && WINDOW_BIN="/opt/thinclient/bin/thin-kiosk-window"
else
    /opt/thinclient/bin/thin-kiosk-agent >/tmp/agent.log 2>&1 &
    WINDOW_BIN="/opt/thinclient/bin/thin-kiosk-window"
fi

for i in 1 2 3 4 5; do curl -s http://127.0.0.1:8080 >/dev/null && break; sleep 1; done
while true; do $WINDOW_BIN "http://127.0.0.1:8080" >/tmp/ui.log 2>&1; sleep 1; done
