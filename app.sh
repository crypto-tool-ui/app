#!/bin/bash
set -e
WS_PORT=${1:-3000}
export PORT=${WS_PORT}
node index.js
