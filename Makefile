TEST_PKGS ?= ./...
TEST_FLAGS ?= -count=1 -timeout=180s

.PHONY: build test test-race check check-go check-proto tools proto
build: tmux
	go build -o bin/dune ./cmd/dune

test:
	go test $(TEST_PKGS) $(TEST_FLAGS)

test-race:
	go test -race $(TEST_PKGS) $(TEST_FLAGS)

check: check-go check-proto

check-go:
	go vet ./...

check-proto:
	.tools/buf lint

tools:
	GOBIN=$(CURDIR)/.tools go install github.com/bufbuild/buf/cmd/buf@v1.54.0
	GOBIN=$(CURDIR)/.tools go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6

proto:
	.tools/buf generate
	.tools/buf lint

.PHONY: tmux web web-deps web-check web-build release
tmux:
	python3 scripts/fetch-tmux.py
web-deps:
	npm --prefix web ci

web-check:
	npm --prefix web run typecheck

web-build:
	npm --prefix web run build

web: web-deps
	$(MAKE) web-check
	$(MAKE) web-build

release: build web
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/dune-linux-amd64 ./cmd/dune
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/dune-linux-arm64 ./cmd/dune
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o bin/dune-darwin-amd64 ./cmd/dune
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o bin/dune-darwin-arm64 ./cmd/dune
	python3 scripts/fetch-tmux.py --all
	python3 scripts/package-release.py
