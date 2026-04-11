# TSUNAMI Build Makefile
#
# Usage:
#   make                          # build all platforms, version=dev
#   make VERSION=1.2.0            # build all platforms, version=v1.2.0
#   make VERSION=1.2.0 linux      # build linux targets only
#   make clean VERSION=1.2.0      # clean versioned output
#   make checksums VERSION=1.2.0  # regenerate checksums

VERSION    ?= dev
VERSION_TAG = v$(VERSION)
MODULE      = github.com/tsunami-protocol/tsunami
BUILD_DIR   = $(CURDIR)/$(VERSION_TAG)
PROJECT_DIR = $(CURDIR)/..

GIT_COMMIT  := $(shell git -C $(PROJECT_DIR) rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME  := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS     := -s -w -X main.version=$(VERSION_TAG) -X main.commit=$(GIT_COMMIT) -X main.buildTime=$(BUILD_TIME)

COMMANDS = tsunami-server tsunami-client

# ── Platform targets ──────────────────────────────────────────────────────
LINUX_TARGETS   = linux-amd64 linux-arm64
WINDOWS_TARGETS = windows-amd64
DARWIN_TARGETS  = darwin-amd64 darwin-arm64
ALL_TARGETS     = $(LINUX_TARGETS) $(WINDOWS_TARGETS) $(DARWIN_TARGETS)

.PHONY: all linux windows darwin clean checksums

all: $(ALL_TARGETS) checksums

linux: $(LINUX_TARGETS)

windows: $(WINDOWS_TARGETS)

darwin: $(DARWIN_TARGETS)

# ── Generic build rule ────────────────────────────────────────────────────
define BUILD_CMD
$(1)-$(2):
	@echo "[build] $(2) for $(subst -, /,$(1)) ..."
	@mkdir -p $(BUILD_DIR)/$(1)
	@CGO_ENABLED=0 GOOS=$(word 1,$(subst -, ,$(1))) GOARCH=$(word 2,$(subst -, ,$(1))) \
		go build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(BUILD_DIR)/$(1)/$(2)$(if $(findstring windows,$(1)),.exe) \
		$(MODULE)/cmd/$(2)
endef

# Generate build targets for each platform × command
$(foreach target,$(ALL_TARGETS), \
	$(foreach cmd,$(COMMANDS), \
		$(eval $(call BUILD_CMD,$(target),$(cmd))) \
	) \
)

# Platform phony depends on all commands for that platform
$(foreach target,$(ALL_TARGETS), \
	$(eval .PHONY: $(target)) \
	$(eval $(target): $(foreach cmd,$(COMMANDS),$(target)-$(cmd))) \
)

# ── Checksums ─────────────────────────────────────────────────────────────
checksums:
	@echo "[checksum] Generating SHA-256 checksums ..."
	@cd $(BUILD_DIR) && find . -type f ! -name "checksums.sha256" | sort | \
		xargs sha256sum | sed 's| \./| |' > checksums.sha256
	@echo "  Saved: $(BUILD_DIR)/checksums.sha256"

# ── Clean ─────────────────────────────────────────────────────────────────
clean:
	@echo "[clean] Removing $(BUILD_DIR) ..."
	@rm -rf $(BUILD_DIR)
