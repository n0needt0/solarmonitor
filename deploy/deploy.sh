#!/bin/bash
set -e

PI_HOST="${PI_HOST:-nodered}"
VERSION="${VERSION:-dev}"

echo "Building solarcontrol for ARM64..."
GOOS=linux GOARCH=arm64 go build -ldflags "-X main.version=${VERSION}" -o solarcontrol .

echo "Stopping Node-RED on ${PI_HOST}..."
ssh ${PI_HOST} "sudo systemctl stop nodered 2>/dev/null || true"
ssh ${PI_HOST} "sudo systemctl disable nodered 2>/dev/null || true"

echo "Deploying binary..."
scp solarcontrol ${PI_HOST}:/tmp/
ssh ${PI_HOST} "sudo mv /tmp/solarcontrol /usr/local/bin/ && sudo chmod +x /usr/local/bin/solarcontrol"

echo "Deploying config..."
ssh ${PI_HOST} "sudo mkdir -p /etc/solarcontrol"
scp config.yaml ${PI_HOST}:/tmp/
ssh ${PI_HOST} "sudo mv /tmp/config.yaml /etc/solarcontrol/"

echo "Deploying systemd service..."
scp deploy/solarcontrol.service ${PI_HOST}:/tmp/
ssh ${PI_HOST} "sudo mv /tmp/solarcontrol.service /etc/systemd/system/"
ssh ${PI_HOST} "sudo systemctl daemon-reload"

echo "Deploying logrotate config..."
scp deploy/solarcontrol.logrotate ${PI_HOST}:/tmp/
ssh ${PI_HOST} "sudo mv /tmp/solarcontrol.logrotate /etc/logrotate.d/solarcontrol"

echo "Starting solarcontrol..."
ssh ${PI_HOST} "sudo systemctl enable solarcontrol"
ssh ${PI_HOST} "sudo systemctl start solarcontrol"

echo "Checking status..."
sleep 2
ssh ${PI_HOST} "sudo systemctl status solarcontrol --no-pager"

echo ""
echo "Deployment complete!"
echo "  View logs: ssh ${PI_HOST} 'sudo tail -f /var/log/solarcontrol.log'"
echo "  Check status: ssh ${PI_HOST} 'sudo systemctl status solarcontrol'"
