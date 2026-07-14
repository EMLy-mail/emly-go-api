#!/bin/sh
set -e

mkdir -p /logs

# Start the app, tee output to stdout and a persistent log file.
# Run the pipeline in background so we can trap signals and forward them
# to the child process; otherwise Docker may send SIGTERM to this shell
# and the real app won't receive it (resulting in SIGKILL after the
# container stop timeout).
./emly-api 2>&1 | tee -a /logs/app.log &
child=$!

# Forward SIGTERM/SIGINT to the child and wait for it to exit.
trap 'echo "entrypoint: forwarding signal to child $child"; kill -TERM "$child" 2>/dev/null' TERM INT

wait "$child"
exit $?
