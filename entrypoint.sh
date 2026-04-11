#!/bin/sh
# Replace placeholder in .rsc script with actual host IP
if [ -n "$HOST_IP" ]; then
    sed -i "s|your-server|${HOST_IP}|g" /mikrotik/remote-hook.rsc
fi
exec /hook-server
