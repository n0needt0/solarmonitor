.PHONY: build build-arm test deploy clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
PI_HOST ?= nodered

build:
	go build -ldflags "-X main.version=$(VERSION)" -o solarcontrol .

build-arm:
	GOOS=linux GOARCH=arm64 go build -ldflags "-X main.version=$(VERSION)" -o solarcontrol .

test:
	go test -v ./...

deploy: build-arm
	PI_HOST=$(PI_HOST) VERSION=$(VERSION) ./deploy/deploy.sh

clean:
	rm -f solarcontrol

# Quick commands for Pi
logs:
	ssh $(PI_HOST) 'sudo tail -f /var/log/solarcontrol.log'

status:
	ssh $(PI_HOST) 'sudo systemctl status solarcontrol --no-pager'

restart:
	ssh $(PI_HOST) 'sudo systemctl restart solarcontrol'

stop:
	ssh $(PI_HOST) 'sudo systemctl stop solarcontrol'
