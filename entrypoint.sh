#!/bin/sh

CONFIG_FILE="${CONFIG_FILE:-/app/configs/config.json}"

echo "Current directory: $PWD"
echo "Path: $PATH"

if [ "${RUN_IMMEDIATE:-false}" = "true" ]; then
  echo "RUN_IMMEDIATE=true: running jobtester once and exiting"
  exec /app/jobtester -f "$CONFIG_FILE"
fi

CRONTAB_SCHEDULE="${CRONTAB_SCHEDULE:-0 */2 * * *}"
echo "CRONTAB_SCHEDULE: $CRONTAB_SCHEDULE"
echo "$CRONTAB_SCHEDULE cd /app && /app/jobtester -f $CONFIG_FILE >> /proc/1/fd/1 2>&1" | crontab -

echo "Starting crontab...."
crond -f -l 8