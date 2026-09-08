#!/bin/sh
set -e
echo "==> Building Go binary..."
CGO_ENABLED=0 go build -ldflags="-s -w" -o accounts .
ls -lh accounts
echo "==> Done! Run with: ./accounts"
