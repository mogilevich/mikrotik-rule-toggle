#!/bin/sh
# Replace placeholders in .rsc script with env values
# Uses .orig copy to support container restarts and config changes
[ ! -f /mikrotik/remote-hook.rsc.orig ] && cp /mikrotik/remote-hook.rsc /mikrotik/remote-hook.rsc.orig
cp /mikrotik/remote-hook.rsc.orig /mikrotik/remote-hook.rsc

if [ -n "$HOST_IP" ]; then
    sed -i "s|your-server|${HOST_IP}|g" /mikrotik/remote-hook.rsc
fi
if [ -n "$AUTH_TOKEN" ]; then
    escaped_token=$(printf '%s' "$AUTH_TOKEN" | sed 's/[&|\\/"]/\\&/g')
    sed -i "s|:local token \"\"|:local token \"${escaped_token}\"|g" /mikrotik/remote-hook.rsc
fi
exec /hook-server
