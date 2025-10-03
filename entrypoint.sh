#!/bin/sh

# Set default values if environment variables are not set
CONFIG_FILE="${CONFIG_FILE:-/app/configs/config.json}"
# Wait for the specific URL endpoint to be up and ready
URL_TO_CHECK=$(grep -o '"broadcasterCliEndpoint": *"[^"]*"' "$CONFIG_FILE" | sed -E 's/.*: *"([^"]*)"/\1/' || echo "http://localhost:8080/health")
echo "Waiting for Broadcaster ($URL_TO_CHECK) to be ready..."
until wget --server-response --spider "$URL_TO_CHECK" 2>&1 | grep -q "HTTP/1.1 404 Not Found"; do
    echo "Broadcaster is not ready (did not return 'HTTP/1.1 404 Not Found'). Retrying in 5 seconds..."
    sleep 5
done
echo "Broadcaster is ready."

if grep -q '"testMode": *true' "$CONFIG_FILE"; then
    echo "Running in test mode. Starting test run once immediately..."
    cd /app && /app/jobtester -f $CONFIG_FILE >> /proc/1/fd/1 2>&1
    exit 0
fi

CRONTAB_SCHEDULE="${CRONTAB_SCHEDULE:-0 */2 * * *}"
echo "Current directory: $PWD"
echo "Path: $PATH"
echo "CRONTAB_SCHEDULE: $CRONTAB_SCHEDULE"

echo "$CRONTAB_SCHEDULE cd /app && /app/jobtester -f $CONFIG_FILE >> /proc/1/fd/1 2>&1" | crontab -

# Start cron in the background
echo "Starting crontab...."
crond -f -l 8