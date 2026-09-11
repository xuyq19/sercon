GO      ?= go
# WINDRES may be set explicitly for a cross toolchain. The default first uses a
# native installation, then MinGW's x86_64 compiler used by the release job.
WINDRES ?= $(shell command -v windres 2>/dev/null || command -v x86_64-w64-mingw32-windres 2>/dev/null)
BINDIR  := dist
MODULE  := $(shell $(GO) list -m 2>/dev/null || echo sercond)

# Stamped into every binary at link time, never edited by hand.
#
# `git describe --always` yields the tag when one exists and the short hash
# otherwise, so a build from an untagged commit still identifies itself. The
# commit and date are carried separately because "which commit was this built
# from" is the question that actually gets asked when something misbehaves.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

TARGETS := linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/arm64

.PHONY: all build gui fmt vet test check clean list version

all: check build

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./... -timeout 120s

# check is what CI runs before building anything: formatting, tests, and a vet
# pass for the platform the daemon will actually run on rather than just the
# host. Windows-only files never get type-checked by a linux vet, and that is
# exactly where the awkward code lives.
check: fmt test
	$(GO) vet ./...
	@echo "--- vet for the jump-host target ---"
	@GOOS=linux GOARCH=amd64 $(GO) vet ./...

version:
	@echo "version: $(VERSION)"
	@echo "commit : $(COMMIT)"
	@echo "date   : $(DATE)"

build:
	@mkdir -p $(BINDIR)
	@for t in $(TARGETS); do \
	  os=$${t%/*}; arch=$${t#*/}; \
	  ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -ldflags="$(LDFLAGS)" \
	    -o $(BINDIR)/sercond-$$os-$$arch$$ext ./cmd/sercond || exit 1; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -ldflags="$(LDFLAGS)" \
	    -o $(BINDIR)/sercon-$$os-$$arch$$ext ./cmd/sercon || exit 1; \
	  echo "built sercond + sercon for $$os/$$arch"; \
	done
	@$(MAKE) --no-print-directory gui

# The GUI is Windows-only, so it gets a plain name rather than a platform
# suffix: it is the file someone double-clicks, and Windows looks for its
# manifest as "<exe>.manifest".
#
# -H=windowsgui is not cosmetic. Without it the linker produces a console
# subsystem binary, and Windows allocates a console window for it — so
# double-clicking the GUI pops up a black window alongside it.
gui:
	@mkdir -p $(BINDIR)
	@rm -f cmd/sercon-gui/resource_windows.syso
	@if [ -f cmd/sercon-gui/assets/sercon-gui.ico ]; then \
	  test -n "$(WINDRES)" && command -v "$(WINDRES)" >/dev/null || { \
	    echo "GUI icon exists but no Windows resource compiler was found."; \
	    echo "Install binutils-mingw-w64 (x86_64-w64-mingw32-windres), install windres, or set WINDRES=/path/to/windres."; \
	    exit 1; \
	  }; \
	  "$(WINDRES)" --target=pe-x86-64 -i cmd/sercon-gui/resource_windows.rc -O coff -o cmd/sercon-gui/resource_windows.syso; \
	fi
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build \
	  -ldflags="$(LDFLAGS) -H=windowsgui" \
	  -o $(BINDIR)/sercon-gui.exe ./cmd/sercon-gui
	@rm -f cmd/sercon-gui/resource_windows.syso
	@cp cmd/sercon-gui/sercon-gui.exe.manifest $(BINDIR)/ 2>/dev/null || true
	@echo "built sercon-gui.exe"

list:
	@echo "sercond          -> jump host daemon (linux, windows)"
	@echo "sercon-gui.exe  -> jump host daemon with a window (windows)"
	@echo "sercon             -> your machine (linux, windows, darwin)"

clean:
	rm -rf $(BINDIR)
