# agent-conversations — build and install the `convo` CLI.
#
# Stdlib only, CGO off: the binary must be a single file you can copy onto a
# box that has no toolchain, because that is where agent sessions actually run.

GO      ?= go
BIN     := bin/convo
GOROOT_DIR := go
PREFIX  ?= $(HOME)/.local/bin

.PHONY: all build install uninstall test clean

all: build

build:
	@mkdir -p bin
	cd $(GOROOT_DIR) && CGO_ENABLED=0 $(GO) build -o ../$(BIN) ./cmd/convo
	@echo "built $(CURDIR)/$(BIN) ($$($(CURDIR)/$(BIN) version))"

install: build
	@mkdir -p $(PREFIX)
	@ln -sfn $(CURDIR)/$(BIN) $(PREFIX)/convo
	@echo "linked $(PREFIX)/convo -> $(CURDIR)/$(BIN)"
	@case ":$$PATH:" in \
	  *":$(PREFIX):"*) echo "$(PREFIX) is on PATH" ;; \
	  *) echo "warning: $(PREFIX) is NOT on PATH — add it to your shell profile" >&2 ;; \
	esac
	@echo "now run: convo self"

uninstall:
	@if [ -L "$(PREFIX)/convo" ]; then rm -f "$(PREFIX)/convo"; echo "removed $(PREFIX)/convo"; \
	 else echo "nothing to remove at $(PREFIX)/convo"; fi

test:
	cd $(GOROOT_DIR) && $(GO) test ./...

clean:
	@rm -rf bin
	@echo "removed $(CURDIR)/bin"
