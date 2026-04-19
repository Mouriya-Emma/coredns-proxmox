COREDNS_VERSION ?= 1.14.2
BUILD_DIR       := $(CURDIR)/.build
COREDNS_SRC     := $(BUILD_DIR)/coredns
PLUGIN_MODULE   := github.com/Mouriya-Emma/coredns-proxmox
PLUGIN_NAME     := proxmox
BINARY          := coredns

.PHONY: all test vet build smoke clean

all: build smoke

test:
	go test -race -cover ./...

vet:
	go vet ./...

$(COREDNS_SRC):
	mkdir -p $(BUILD_DIR)
	git clone --depth 1 --branch v$(COREDNS_VERSION) https://github.com/coredns/coredns.git $(COREDNS_SRC)

# Inject plugin.cfg entry before the hosts plugin so chain order puts proxmox
# first — so PVE-known names win over hosts-plugin entries for the same name.
build: $(COREDNS_SRC)
	@grep -q '^$(PLUGIN_NAME):' $(COREDNS_SRC)/plugin.cfg || \
	  sed -i '/^hosts:/i $(PLUGIN_NAME):$(PLUGIN_MODULE)' $(COREDNS_SRC)/plugin.cfg
	cd $(COREDNS_SRC) && go mod edit -replace $(PLUGIN_MODULE)=$(CURDIR)
	cd $(COREDNS_SRC) && go generate
	cd $(COREDNS_SRC) && go mod tidy
	cd $(COREDNS_SRC) && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o $(CURDIR)/$(BINARY) .
	@echo
	@echo "=== plugins ==="
	@# Verify via strings (works for cross-compiled binaries, unlike running -plugins)
	@strings $(CURDIR)/$(BINARY) | grep -qw '$(PLUGIN_MODULE)' && echo "proxmox plugin linked into binary" || (echo "ERROR: proxmox plugin NOT in binary" && exit 1)

# Smoke test: start the binary against a dummy Corefile; check it parses + loads
# the plugin (initial PVE refresh will fail — that's expected and must not crash
# startup). Needs ephemeral port free.
smoke: $(BINARY)
	@echo "=== smoke ==="
	@OUT=$$(timeout 3 ./$(BINARY) -conf testdata/Corefile.smoke -dns.port 15553 2>&1 || true); \
	echo "$$OUT" | grep -q 'CoreDNS-' || (echo "$$OUT"; echo "ERROR: coredns did not start" && exit 1); \
	echo "$$OUT" | grep -q 'plugin/proxmox' || (echo "$$OUT"; echo "ERROR: proxmox plugin did not initialise" && exit 1); \
	echo "coredns started, proxmox plugin initialised (refresh expectedly failed)"

clean:
	rm -rf $(BUILD_DIR) $(BINARY)
