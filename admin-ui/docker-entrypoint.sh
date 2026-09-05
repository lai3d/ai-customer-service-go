#!/bin/sh
# One built image, any environment.
#
# The API's origin is written here at start-up rather than baked in by `vite build`,
# because a build-time value means a rebuilt image for every environment -- and then the
# image that was tested is not the image that ships.
set -eu

: "${ADMIN_API_BASE:=}"
# Written under /tmp rather than into the web root. The image runs as a non-root user
# and, in Kubernetes, with a read-only root filesystem; a start-up script that writes into
# /usr/share/nginx/html works on a laptop and fails in the cluster, which is the worst
# order to find out.
mkdir -p /tmp/admin-config
cat > /tmp/admin-config/config.js <<JS
window.__ADMIN_CONFIG__ = { apiBase: "${ADMIN_API_BASE}" };
JS

echo "admin-ui: API base is ${ADMIN_API_BASE:-(same origin)}"
exec "$@"
