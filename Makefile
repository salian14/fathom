# Fathom -- top-level release Makefile.
#
# The components build and test independently (agent/Makefile, mcp/Makefile) and
# nothing here replaces that. This file exists for the one operation that spans
# both: cutting a version.
#
#     make release BUMP=minor
#
# runs both component checks, bumps agent and mcp to the same next semver,
# builds and pushes both images at that tag, and rolls both charts out with it.
#
# The two versions move in lockstep on purpose -- one Fathom version, not two.
# The base is the highest version either chart currently declares, so the charts
# converge on the first release even though they have drifted (mcp appVersion
# 0.3.0 vs agent 0.2.0 at the time this was written).
#
# The image tag is not passed around out of band: both charts default
# `image.tag` to `.Chart.AppVersion`, so bumping the chart is what re-points the
# Deployment. `--set image.tag` is still passed at deploy time so a stale chart
# checkout cannot quietly ship the previous image.

SHELL := /bin/bash
.DEFAULT_GOAL := help
.NOTPARALLEL:

# Deploy order, not alphabetical: the mcp chart owns the fathom Namespace and
# exposes the tools the agent dials on startup.
COMPONENTS := mcp agent
CHARTS     := mcp/helm/Chart.yaml agent/helm/Chart.yaml

BUMP        ?=
ALLOW_DIRTY ?=
SKIP_CHECK  ?=

# The charts pin every resource to `namespace.name` (fathom) themselves, so the
# release namespace only decides where Helm keeps its own release metadata.
# Empty by default to match the READMEs, which install from the current context.
HELM_NAMESPACE ?=
HELM_NS_FLAG   := $(if $(HELM_NAMESPACE),--namespace $(HELM_NAMESPACE),)
HELM_FLAGS     ?= --wait --timeout 5m

CURRENT := $(shell grep -hE '^(version|appVersion):' $(CHARTS) \
             | sed -E 's/.*:[[:space:]]*"?([0-9]+\.[0-9]+\.[0-9]+)"?.*/\1/' \
             | sort -V | tail -1)

# Simply-expanded (:=) on purpose: the recipes rewrite Chart.yaml mid-run, and a
# recursively-expanded NEXT would re-derive itself from the already-bumped chart
# and hand the later steps a different version than the earlier ones used.
# VERSION=x.y.z pins an exact version and skips the bump arithmetic entirely.
NEXT := $(if $(VERSION),$(VERSION),$(shell echo "$(CURRENT)" | awk -F. -v b="$(BUMP)" \
          '{ if (b == "major")      printf "%d.0.0",    $$1+1; \
             else if (b == "minor") printf "%d.%d.0",   $$1, $$2+1; \
             else if (b == "patch") printf "%d.%d.%d",  $$1, $$2, $$3+1 }'))

.PHONY: help
help:
	@echo "Fathom release targets"
	@echo
	@echo "  make release BUMP=major|minor|patch   check, bump, build, push, deploy -- both components"
	@echo "  make release-major | release-minor | release-patch"
	@echo "  make release VERSION=1.4.2            pin an exact version instead of bumping"
	@echo
	@echo "  make version                          show the current and next version"
	@echo "  make check                            run both component checks"
	@echo "  make bump BUMP=patch                  rewrite both Chart.yamls only"
	@echo "  make push  BUMP=patch                 build and push both images only"
	@echo "  make deploy                           helm upgrade --install both charts"
	@echo "  make render                           render both charts at the next version"
	@echo "  make clean                            clean both components"
	@echo
	@echo "Knobs:  ALLOW_DIRTY=1  SKIP_CHECK=1  HELM_NAMESPACE=<ns>  HELM_FLAGS='...'"
	@echo
	@echo "Current version: $(CURRENT)"

.PHONY: version
version:
	@echo "current: $(CURRENT)"
	@echo "next:    $(if $(NEXT),$(NEXT),unset -- pass BUMP=major|minor|patch or VERSION=x.y.z)"

# Everything that can fail cheaply fails here, before an image is built or a
# chart is rewritten -- a release that dies halfway leaves the charts bumped and
# the cluster on the old tag.
.PHONY: preflight
preflight:
	@if [ -z "$(NEXT)" ]; then \
	  echo "error: set BUMP=major|minor|patch (or VERSION=x.y.z)"; exit 1; fi
	@if ! [[ "$(NEXT)" =~ ^[0-9]+\.[0-9]+\.[0-9]+$$ ]]; then \
	  echo "error: '$(NEXT)' is not a semver x.y.z"; exit 1; fi
	@if [ "$(NEXT)" = "$(CURRENT)" ] || \
	    [ "$$(printf '%s\n%s\n' '$(CURRENT)' '$(NEXT)' | sort -V | tail -1)" != "$(NEXT)" ]; then \
	  echo "error: $(NEXT) does not advance on the current version $(CURRENT)"; exit 1; fi
	@for t in git docker helm; do \
	  command -v $$t >/dev/null 2>&1 || { echo "error: $$t not on PATH"; exit 1; }; \
	done
	@if [ -z "$(ALLOW_DIRTY)" ] && [ -n "$$(git status --porcelain)" ]; then \
	  echo "error: working tree is dirty -- the pushed image would match no commit."; \
	  echo "       commit first, or re-run with ALLOW_DIRTY=1."; \
	  git status --short; exit 1; \
	fi
	@echo "==> releasing $(CURRENT) -> $(NEXT) ($(COMPONENTS))"

.PHONY: check
check:
	@if [ -n "$(SKIP_CHECK)" ]; then echo "==> skipping component checks (SKIP_CHECK set)"; else \
	  for c in $(COMPONENTS); do \
	    echo "==> check: $$c"; $(MAKE) --no-print-directory -C $$c check || exit 1; \
	  done; \
	fi

# Chart version and appVersion move together: appVersion is what the Deployment
# reads for its image tag, and a chart whose own version did not change will not
# be seen as an upgrade by anything that tracks chart versions.
.PHONY: bump
bump: guard-next
	@for f in $(CHARTS); do \
	  sed -i -E 's/^version: .*/version: $(NEXT)/; s/^appVersion: .*/appVersion: "$(NEXT)"/' $$f; \
	  echo "==> $$f -> $(NEXT)"; \
	done

.PHONY: images
images: guard-next
	@for c in $(COMPONENTS); do \
	  echo "==> image: $$c:$(NEXT)"; \
	  $(MAKE) --no-print-directory -C $$c image VERSION=$(NEXT) || exit 1; \
	done

.PHONY: push
push: guard-next
	@for c in $(COMPONENTS); do \
	  echo "==> push: $$c:$(NEXT)"; \
	  $(MAKE) --no-print-directory -C $$c push VERSION=$(NEXT) || exit 1; \
	done

.PHONY: deploy
deploy: guard-next
	@for c in $(COMPONENTS); do \
	  echo "==> deploy: fathom-$$c at $(NEXT)"; \
	  helm upgrade --install fathom-$$c ./$$c/helm $(HELM_NS_FLAG) $(HELM_FLAGS) \
	    --set image.tag=$(NEXT) || exit 1; \
	done

# Preview only: this renders the *current* charts with the next image tag, so the
# chart labels still read the old version until `make bump` has run.
.PHONY: render
render: guard-next
	@for c in $(COMPONENTS); do \
	  helm template fathom-$$c ./$$c/helm --set image.tag=$(NEXT); \
	done

.PHONY: release
release: preflight check bump push deploy
	@echo
	@echo "==> released $(NEXT)"
	@echo "    images:  fathom-mcp:$(NEXT), fathom-agent:$(NEXT)"
	@echo "    charts:  $(CHARTS) bumped in the working tree -- commit them,"
	@echo "             or the next release derives its base from an uncommitted file."
	@kubectl -n fathom get deploy 2>/dev/null || true

.PHONY: release-major release-minor release-patch
release-major:
	@$(MAKE) --no-print-directory release BUMP=major
release-minor:
	@$(MAKE) --no-print-directory release BUMP=minor
release-patch:
	@$(MAKE) --no-print-directory release BUMP=patch

# The individual steps are useful on their own (re-push a failed image, deploy a
# chart that is already bumped), but none of them can run without a version.
.PHONY: guard-next
guard-next:
	@if [ -z "$(NEXT)" ]; then \
	  echo "error: set BUMP=major|minor|patch (or VERSION=x.y.z)"; exit 1; fi

.PHONY: clean
clean:
	@for c in $(COMPONENTS); do $(MAKE) --no-print-directory -C $$c clean; done
