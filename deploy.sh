#!/bin/bash
set -e

MINI="benjaminconn@192.168.1.83"
REMOTE_DIR="/Users/benjaminconn/runbook"
PLIST_LABEL="com.bennyconn.runbook"
PLIST_PATH="$HOME/Library/LaunchAgents/$PLIST_LABEL.plist"

echo "==> Building runbook for arm64 macOS..."
GOOS=darwin GOARCH=arm64 go build -o runbook ./cmd/live/main.go

echo "==> Syncing binary and scripts..."
ssh "$MINI" "mkdir -p $REMOTE_DIR/scripts $REMOTE_DIR/data $REMOTE_DIR/logs"
rsync -av runbook "$MINI:$REMOTE_DIR/"

echo "==> Installing plist and restarting service..."
rsync -av Service.plist "$MINI:$HOME/Library/LaunchAgents/$PLIST_LABEL.plist"
ssh "$MINI" "launchctl unload ~/Library/LaunchAgents/$PLIST_LABEL.plist 2>/dev/null; launchctl load ~/Library/LaunchAgents/$PLIST_LABEL.plist"

echo "==> Done. Tailing logs (ctrl+c to exit)..."
ssh "$MINI" "tail -f $REMOTE_DIR/logs/stdout.log"
