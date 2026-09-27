# /opt/sim/Makefile — sim-router and the demo topologies.
#
# Common workflow:
#   make build         # compile sim-router and sim-web
#   make test          # go vet + go test ./...
#   make demo-hidden-node          # see Phase 3 capture-effect baseline
#   make demo-hidden-node-capture  # capture-effect, asymmetric levels
#   make demo-mesh-3 / demo-linear-6 / demo-star-6 / demo-multiport-3
#
# By default the binary searches /opt/samoyed/dist for samoyed-direwolf.
# Override with -samoyed.

GO        ?= go
GOFLAGS   ?=

.PHONY: build test fmt vet clean web \
        demo-hidden-node demo-hidden-node-capture demo-mesh-3 \
        demo-linear-6 demo-star-6 demo-multiport-3 demo-two-node \
        demo-two-node-noisy demo-pdn-fm-modes install

build: sim-router sim-web

sim-router: $(shell find . -name '*.go' -not -path './testdata/*')
	$(GO) build $(GOFLAGS) -o $@ ./cmd/sim-router

sim-web: $(shell find . -name '*.go' -not -path './testdata/*') cmd/sim-web/index.html
	$(GO) build $(GOFLAGS) -o $@ ./cmd/sim-web

# Convenience target: launch the web UI on :8080, autostart-disabled, with
# the bootstrap config under /etc/sim/network.yaml so edits survive reboots.
web: build
	./sim-web -addr :8080 -config $${SIM_NETWORK_YAML:-/etc/sim/network.yaml}

test: vet
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

clean:
	rm -f sim-router sim-web

# Installer target: re-runs install.sh against the current checkout. Mostly
# useful when iterating on install.sh itself; end users run install.sh
# directly via `curl ... | sudo bash`.
install:
	@if [ "$$(id -u)" -ne 0 ]; then echo "run as root: sudo make install"; exit 1; fi
	./install.sh

# --- demos ----------------------------------------------------------------
# Each demo target is a thin wrapper. `make demo-<name>` runs the router
# with the matching config from configs/. Send/receive KISS via the ports
# noted in the per-demo summary printed at startup.

define DEMO_RUN
	@echo "=== $@ ===" ; \
	echo "Config: configs/$(1).yaml" ; \
	echo "Stop with Ctrl-C." ; \
	./sim-router -config configs/$(1).yaml
endef

demo-two-node: build
	$(call DEMO_RUN,two-node)

demo-two-node-noisy: build
	$(call DEMO_RUN,two-node-noisy)

demo-hidden-node: build
	@echo "Hidden-node, equal levels (PLAN Phase 3 acceptance):"
	@echo "  KISS A=8001  KISS B=8002  KISS C=8003"
	@echo "  Send to A and C simultaneously; B should fail to decode either."
	$(call DEMO_RUN,hidden-node)

demo-hidden-node-capture: build
	@echo "Hidden-node, A captures over C:"
	@echo "  KISS A=8001  KISS B=8002  KISS C=8003"
	@echo "  Send to A and C simultaneously; B should decode A only."
	$(call DEMO_RUN,hidden-node-capture)

demo-mesh-3: build
	@echo "Three-node mesh, AFSK1200, equal-loss everywhere:"
	@echo "  KISS A=8001  KISS B=8002  KISS C=8003"
	$(call DEMO_RUN,mesh-3)

demo-linear-6: build
	@echo "Linear 6-station chain (each hears only neighbours):"
	@echo "  KISS A..F = 8001..8006"
	$(call DEMO_RUN,linear-6)

demo-star-6: build
	@echo "Star: 1 hub + 5 spokes:"
	@echo "  KISS hub=8001  spokes=8002..8006"
	$(call DEMO_RUN,star-6)

demo-multiport-3: build
	@echo "Multi-port: middle node has VHF (8002) AND UHF (8003);"
	@echo "  a (8001) reaches middle via VHF, b (8004) via UHF."
	@echo "  middle's two ports do not interact (different radios)."
	$(call DEMO_RUN,multiport-3)

demo-pdn-fm-modes: build
	@echo "pdn-soundmodem FM modes (needs pdn-soundmodem and samoyed):"
	@echo "  qpsk3600 a=8001 b=8002  c4fsk9600 c=8003 d=8004  afsk1200 pdn e=8005 samoyed f=8006"
	$(call DEMO_RUN,pdn-fm-modes)
