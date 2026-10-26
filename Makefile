APP             := kaodoku
BIN             := bin
DIST            := dist
MAIN            := ./main.go

GOCMD           := go
GOBUILD         := $(GOCMD) build
GORUN           := $(GOCMD) run
GOCLEAN         := $(GOCMD) clean
GOFMT           := $(GOCMD) fmt
GOVET           := $(GOCMD) vet
GOTEST          := $(GOCMD) test
STATICCHECK     := staticcheck
GOCACHE         ?= $(CURDIR)/.cache/go-build

VERSION         := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LD_FLAGS        := -X 'github.com/brogergvhs/kaodoku/cmd.Version=$(VERSION)'

.PHONY: all build install run fmt vet test test-race lint check clean package bench-seed bench-serve bench-serve-docker bench

all: build

build:
	@echo "Building $(APP) (version: $(VERSION))..."
	@mkdir -p $(BIN)
	$(GOBUILD) -ldflags "$(LD_FLAGS)" -o $(BIN)/$(APP) $(MAIN)
	@echo "Binary: $(BIN)/$(APP)"

install: build
	@echo "Installing $(APP) into /usr/local/bin..."
	@sudo install -m 0755 $(BIN)/$(APP) /usr/local/bin/$(APP)
	@echo "Installed as /usr/local/bin/$(APP)"

run:
	$(GORUN) $(MAIN) $(ARGS)

fmt:
	$(GOFMT) ./...

vet:
	$(GOVET) ./...

test:
	@mkdir -p $(GOCACHE)
	GOCACHE=$(GOCACHE) $(GOTEST) ./...

test-race:
	@mkdir -p $(GOCACHE)
	GOCACHE=$(GOCACHE) $(GOTEST) -race ./...

lint:
	$(STATICCHECK) ./...

check: fmt vet test lint

BENCH_DATA      := $(CURDIR)/bench/data
BENCH_CBZ       ?= $(BENCH_DATA)/cbz
BENCH_ENV       := XDG_CONFIG_HOME=$(BENCH_DATA)/config KAODOKU_DOWNLOAD_DIR=$(BENCH_DATA) KAODOKU_ADMIN_USER=bench KAODOKU_ADMIN_PASSWORD=bench
CPUS            ?= 1
MEM             ?= 2g

bench-seed:
	$(GORUN) ./bench/seed -dir $(BENCH_DATA) -cbz "$(BENCH_CBZ)" $(ARGS)

bench-serve: build
	$(BENCH_ENV) $(BIN)/$(APP) serve --db $(BENCH_DATA)/kaodoku.db --addr 127.0.0.1:8090

bench-serve-docker:
	docker build -t kaodoku-bench .
	docker run --rm --cpus=$(CPUS) --memory=$(MEM) --memory-swap=$(MEM) -p 127.0.0.1:8090:8090 \
	-v $(BENCH_DATA):$(BENCH_DATA) -v "$(BENCH_CBZ):$(BENCH_CBZ)" $(addprefix -e ,$(BENCH_ENV)) \
	kaodoku-bench serve --db $(BENCH_DATA)/kaodoku.db --addr 0.0.0.0:8090

bench:
	k6 run -e USERNAME=bench -e PASSWORD=bench $(ARGS) bench/load.js

clean:
	@echo "Cleaning..."
	@rm -rf $(BIN) $(DIST)
	@$(GOCLEAN)
	@echo "Cleaned"

package:
	@echo "Packaging cross-platform binaries (version: $(VERSION))..."
	@mkdir -p $(DIST)

	# Linux
	GOOS=linux   GOARCH=amd64  $(GOBUILD) -ldflags "$(LD_FLAGS)" -o $(DIST)/$(APP)-linux-amd64   $(MAIN)
	GOOS=linux   GOARCH=arm64  $(GOBUILD) -ldflags "$(LD_FLAGS)" -o $(DIST)/$(APP)-linux-arm64   $(MAIN)

	# macOS
	GOOS=darwin  GOARCH=amd64  $(GOBUILD) -ldflags "$(LD_FLAGS)" -o $(DIST)/$(APP)-darwin-amd64  $(MAIN)
	GOOS=darwin  GOARCH=arm64  $(GOBUILD) -ldflags "$(LD_FLAGS)" -o $(DIST)/$(APP)-darwin-arm64  $(MAIN)

	# Windows
	GOOS=windows GOARCH=amd64  $(GOBUILD) -ldflags "$(LD_FLAGS)" -o $(DIST)/$(APP)-windows-amd64.exe $(MAIN)

	@echo "Build complete."
	@echo "Files in $(DIST):"
	@ls -1 $(DIST)
