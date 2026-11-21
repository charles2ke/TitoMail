# TitoMail build tasks. "make" builds the web interface and the server binary.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS_BUILD := -trimpath -ldflags "-s -w -X main.version=$(VERSION)"
BIN := bin/titomail

.PHONY: all web build test e2e run docker clean

all: build

web/node_modules/.package-lock.json: web/package-lock.json
	cd web && npm ci

e2e/node_modules/.package-lock.json: e2e/package-lock.json
	cd e2e && npm ci

# Build the React web interface into web/dist, which the server embeds.
web: web/node_modules/.package-lock.json
	cd web && npm run build

# Build a static server binary with the web interface embedded.
build: web
	CGO_ENABLED=0 go build $(GOFLAGS_BUILD) -o $(BIN) ./cmd/titomail

# Go unit and integration tests.
test:
	go vet ./...
	go test -race ./...

# Playwright end-to-end tests against a freshly built server. Screenshots
# are written to e2e/screenshots. Set PW_CHANNEL=chrome to use an installed
# Chrome instead of Playwright's Chromium ("npx playwright install chromium").
e2e: build e2e/node_modules/.package-lock.json
	cd e2e && npx playwright test

# Run a local development server on http://localhost:8080. Mail stays on
# this machine; the log shows a link to create the first account.
run: build
	$(BIN) serve -data-dir ./data -hostname localhost -allow-insecure-auth -disable-outbound

docker:
	docker build --build-arg VERSION=$(VERSION) -t titomail:$(VERSION) .

clean:
	rm -rf bin e2e/test-results e2e/playwright-report e2e/screenshots e2e/.data
	find web/dist -mindepth 1 ! -name .gitkeep -exec rm -rf {} +
