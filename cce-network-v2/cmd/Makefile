# Copyright Authors of CCE
# SPDX-License-Identifier: Apache-2.0

include ../Makefile.defs

TARGETS := agent webhook exclusive-rdma-agent

.PHONY: all $(TARGETS) clean install

all: $(TARGETS)

$(TARGETS):
	@$(ECHO_GO)
	$(QUIET)$(GO_BUILD) -o $(PWD)/output/bin/cmd/$(@) ./$(@)

$(TARGET):
	@$(ECHO_GO)
	$(QUIET)$(GO_BUILD) -o $@

clean:
	@$(ECHO_CLEAN)
	$(forea target, $(QUIET)rm -f $(PWD)/output/bin/cmd/$(target))
	$(GO) clean $(GOCLEAN)

install:
	$(QUIET)$(INSTALL) -m 0755 -d $(DESTDIR)$(BINDIR)
	$(foreach target,$(TARGETS), $(QUIET)$(INSTALL) -m 0755 $(target) $(DESTDIR)$(BINDIR);)
