#!/bin/bash
ulimit -n 100000
set -e
WS_PORT=${1:-3000}
export PORT=${WS_PORT}
node index.js
