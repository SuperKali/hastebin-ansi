#!/bin/sh
# Entrypoint for Pterodactyl (Wings). Wings only sets the user and the
# environment, including STARTUP, so this script starts the server.
cd /home/container || exit 1

# Default startup when run outside of Wings
STARTUP="${STARTUP:-hastebin --config config.yaml}"

# Turn {{VAR}} into ${VAR}, like the official Pterodactyl yolks
MODIFIED_STARTUP=$(printf '%s' "${STARTUP}" | sed -e 's/{{/${/g' -e 's/}}/}/g')
printf ':/home/container$ %s\n' "${MODIFIED_STARTUP}"

# exec so SIGINT and SIGTERM reach hastebin directly
eval "exec ${MODIFIED_STARTUP}"
