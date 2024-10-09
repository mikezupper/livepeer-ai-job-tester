#!/bin/sh

# Set default values if environment variables are not set
CONFIG_FILE="${CONFIG_FILE:-/app/configs/config.json}"
CRONTAB_SCHEDULE="${CRONTAB_SCHEDULE:-0 */2 * * *}"
echo "Current directory: $PWD"
echo "Path: $PATH"
echo "CRONTAB_SCHEDULE: $CRONTAB_SCHEDULE"

echo "$CRONTAB_SCHEDULE cd /app && /app/jobtester -f $CONFIG_FILE >> /proc/1/fd/1 2>&1" | crontab -

# Start cron in the background
echo "Starting crontab...."
crond -f -l 8