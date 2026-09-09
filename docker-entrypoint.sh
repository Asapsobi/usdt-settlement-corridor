#!/bin/sh
# Runs this image's own migrate binary (a real one, or the Dockerfile's
# own no-op placeholder for proofrun) to completion, then execs the
# server binary as PID 1 so it receives signals directly (docker-compose
# stop, in particular).
set -e

echo "docker-entrypoint: running migrations..."
/usr/local/bin/migrate up

echo "docker-entrypoint: starting server..."
exec /usr/local/bin/server
