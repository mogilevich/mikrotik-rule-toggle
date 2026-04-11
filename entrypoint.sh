#!/bin/sh
# Replace placeholder in .rsc script with actual host IP
# Uses .orig copy to support container restarts and IP changes
if [ -n "$HOST_IP" ]; then
    [ ! -f /mikrotik/remote-hook.rsc.orig ] && cp /mikrotik/remote-hook.rsc /mikrotik/remote-hook.rsc.orig
    sed "s|your-server|${HOST_IP}|g" /mikrotik/remote-hook.rsc.orig > /mikrotik/remote-hook.rsc
fi
exec /hook-server
