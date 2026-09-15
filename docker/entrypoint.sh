#!/bin/sh
set -e

mkdir -p /logs

# The app writes its own daily log files under LOG_DIR (/logs in the compose
# file) and still logs to stdout for `docker logs`, so there is no need to tee
# into a single ever-growing file here. exec replaces this shell, so Docker's
# SIGTERM reaches the app directly and graceful shutdown runs.
exec ./emly-api
