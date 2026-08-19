REMOTE      := numa-dell
REMOTE_DIR  := /home/deparker/go-numa
PKG         ?= runtime
RUN         ?= TestNUMA
BRANCH      := $(shell git rev-parse --abbrev-ref HEAD)

.PHONY: help push build test-numa topo shell gate-json-1p

help:
	@echo "make push | build | test-numa RUN=TestFoo | topo | shell | gate-json-1p"

push:
	git push $(REMOTE) HEAD:$(BRANCH)
	ssh $(REMOTE) "cd $(REMOTE_DIR) && git fetch && git checkout -f $(BRANCH)"

build:
	ssh $(REMOTE) "cd $(REMOTE_DIR)/src && ./make.bash"

test-numa:
	ssh $(REMOTE) "cd $(REMOTE_DIR) && GOROOT=$(REMOTE_DIR) GOEXPERIMENT=numa \
	    $(REMOTE_DIR)/bin/go test $(PKG) -run '$(RUN)' -count=1"

topo:
	ssh $(REMOTE) "lscpu | grep -E 'CPU|NUMA|Socket'; echo; numactl --hardware; sysctl kernel.numa_balancing"

shell:
	ssh -t $(REMOTE) "cd $(REMOTE_DIR) && exec \$$SHELL"

gate-json-1p:
	scp numa-design/gate-json.sh $(REMOTE):$(REMOTE_DIR)/numa-design/gate-json.sh
	ssh $(REMOTE) "chmod +x $(REMOTE_DIR)/numa-design/gate-json.sh && \
	    GOROOT=$(REMOTE_DIR) GOMAXPROCS=1 $(REMOTE_DIR)/numa-design/gate-json.sh"
