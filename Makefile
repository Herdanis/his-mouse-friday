# ============================================
# his-mouse-friday — build / dev targets
# ============================================
# Install is via install.sh (curl | bash). See README.

GO ?= go

.PHONY: build test vet clean

build:
	$(GO) build ./...

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

clean:
	go clean -cache
