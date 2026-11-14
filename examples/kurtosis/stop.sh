#!/usr/bin/env bash
# Stops and removes everything examples/kurtosis/run.sh started.
set -u

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
work=$PWD/.tmp/e2e
prefix=upgradoor-e2e

for f in controller authstub; do
    if [ -f "$work/$f.pid" ]; then
        kill "$(cat "$work/$f.pid")" 2>/dev/null
        rm -f "$work/$f.pid"
    fi
done

for i in 1 2 3 4; do
    docker rm -f "$prefix-node-$i" >/dev/null 2>&1
    docker volume rm "$prefix-node-$i-state" >/dev/null 2>&1
done

docker rm -f "$prefix-registry" >/dev/null 2>&1
kurtosis enclave rm -f upgradoor-e2e >/dev/null 2>&1
docker image rm "$prefix-host" localhost:5055/lighthouse:example >/dev/null 2>&1

exit 0
