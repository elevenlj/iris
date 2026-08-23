#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="/Users/eleven/project/iris"
SESSION_NAME="iris_8080"
PORT="8080"
CONFIG_DIR="conf"
LOG_DIR="$PROJECT_DIR/log"
LOG_FILE="$LOG_DIR/restart_iris_8080.log"

mkdir -p "$LOG_DIR"

{
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] restart check started"

  if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] port $PORT already running"
    exit 0
  fi

  if tmux has-session -t "$SESSION_NAME" 2>/dev/null; then
    tmux kill-session -t "$SESSION_NAME" || true
  fi

  cd "$PROJECT_DIR"
  tmux new-session -d -s "$SESSION_NAME" "./iris -p $PORT --config-dir $CONFIG_DIR >> log/iris_8080.log 2>&1"

  sleep 3

  if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] port $PORT restarted"
    exit 0
  fi

  echo "[$(date '+%Y-%m-%d %H:%M:%S')] restart failed"
  exit 1
} >> "$LOG_FILE" 2>&1
