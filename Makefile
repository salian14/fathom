# Fathom -- top-level release Makefile.
#
# The components build and test independently (agent/Makefile, mcp/Makefile) and
# nothing here replaces that. This file exists for the one operation that spans
# both: cutting a version.
#
#     make release BUMP=minor
#
# runs both component checks, bumps agent and mcp to the same next semver,
# commits and tags that bump, builds and pushes both images at that tag, and
# rolls both charts out with it.
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
#
# The bump stages, commits and tags itself (NO_COMMIT=1 / NO_TAG=1 opt out), so
# `push` builds from a committed tree -- which is what preflight's dirty-tree
# check already claims to guarantee. Nothing here ever contacts a remote: the
# commit and the tag stay local and go out through the normal PR flow.
#
# Deploy is ordered, not merely sequenced: fathom-agent does not roll until
# fathom-mcp's new pods are actually serving.

SHELL := /bin/bash
.DEFAULT_GOAL := help
.NOTPARALLEL:

# Build/push/check order, not alphabetical: the mcp chart owns the fathom
# Namespace and exposes the tools the agent dials on startup. `deploy` does not
# loop over this -- see the deploy targets, where the two are not symmetric.
COMPONENTS := mcp agent
CHARTS     := mcp/helm/Chart.yaml agent/helm/Chart.yaml

# Everything the bump rewrites and then commits. CURRENT is still derived from
# the charts alone: the agent package files are followers, not a source of
# truth, and their stale version must not drag the release base down.
VERSIONED := $(CHARTS) agent/pyproject.toml agent/src/fathom_agent/__init__.py

BUMP        ?=
ALLOW_DIRTY ?=
SKIP_CHECK  ?=
NO_COMMIT   ?=
NO_TAG      ?=

# The charts pin every resource to `namespace.name` (fathom) themselves, so the
# release namespace only decides where Helm keeps its own release metadata.
# Empty by default to match the READMEs, which install from the current context.
HELM_NAMESPACE ?=
HELM_NS_FLAG   := $(if $(HELM_NAMESPACE),--namespace $(HELM_NAMESPACE),)
HELM_FLAGS     ?= --wait --timeout 5m

# Where the workloads actually land. Deliberately not HELM_NAMESPACE: that one
# only locates Helm's release metadata, while the pods the rollout gate waits on
# are pinned to `namespace.name` by the charts themselves.
FATHOM_NS       ?= fathom
ROLLOUT_TIMEOUT ?= 5m

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

COMMIT_MSG ?= release: v$(NEXT)
TAG        ?= v$(NEXT)

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
	@echo "  make bump BUMP=patch                  rewrite the version files, then commit and tag them"
	@echo "  make push  BUMP=patch                 build and push both images only"
	@echo "  make deploy                           helm upgrade both charts -- agent waits on mcp"
	@echo "  make render                           render both charts at the next version"
	@echo "  make clean                            clean both components"
	@echo
	@echo "Knobs:  ALLOW_DIRTY=1  SKIP_CHECK=1  NO_COMMIT=1  NO_TAG=1"
	@echo "        HELM_NAMESPACE=<ns>  HELM_FLAGS='...'  FATHOM_NS=<ns>  ROLLOUT_TIMEOUT=5m"
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
	@for t in git docker helm kubectl; do \
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
#
# The agent's package version rides along because it is the one the *running*
# service reports -- __init__.py feeds SERVICE_VERSION in telemetry.py and the
# version in /healthz -- so leaving it behind means traces from a 0.4.0 chart
# claiming to be 0.1.0. mcp needs no such edit: its version is injected at build
# time via -ldflags from VERSION.
#
# The rewrite is committed and tagged here rather than left for a human to
# remember, so the images `push` builds a moment later correspond to a commit.
#
# requirements.lock is touched, not regenerated: a version field is not a
# dependency, so the lock is still correct -- but agent/Makefile's lock-check
# compares mtimes, and rewriting pyproject.toml would make the *next* `make
# check` demand a `make lock` that has nothing to do.
#
# commit-bump and tag are recursive sub-makes, and VERSION=$(NEXT) is not
# decoration: NEXT's `:=` only pins the version within one make invocation. A
# sub-make started after the sed re-derives CURRENT from the chart it just
# rewrote and lands one version too high -- committing 0.4.1 under the message
# and tag v0.4.2.
.PHONY: bump
bump: guard-next
	@for f in $(CHARTS); do \
	  sed -i -E 's/^version: .*/version: $(NEXT)/; s/^appVersion: .*/appVersion: "$(NEXT)"/' $$f; \
	  echo "==> $$f -> $(NEXT)"; \
	done
	@sed -i -E 's/^version = .*/version = "$(NEXT)"/' agent/pyproject.toml
	@sed -i -E 's/^__version__ = .*/__version__ = "$(NEXT)"/' agent/src/fathom_agent/__init__.py
	@touch agent/requirements.lock
	@echo "==> agent package -> $(NEXT)"
	@$(MAKE) --no-print-directory commit-bump VERSION=$(NEXT)
	@$(MAKE) --no-print-directory tag VERSION=$(NEXT)

# Path-scoped, never `git commit -a`: under ALLOW_DIRTY=1 the tree can hold
# unrelated work, and a release commit that swept it in would be a lie about
# what shipped. Local only -- no push, ever.
#
# One continued shell rather than a line per step: make runs each recipe line in
# its own shell, so the NO_COMMIT `exit 0` would bail out of that line alone and
# commit anyway on the next one. Same reason in `tag`.
.PHONY: commit-bump
commit-bump: guard-next
	@if [ -n "$(NO_COMMIT)" ]; then echo "==> skipping commit (NO_COMMIT set)"; exit 0; fi; \
	set -e; \
	git add -- $(VERSIONED); \
	if git diff --cached --quiet -- $(VERSIONED); then \
	  echo "==> nothing to commit -- version files already at $(NEXT)"; \
	else \
	  git commit --only -q -m '$(COMMIT_MSG)' -- $(VERSIONED); \
	  echo "==> committed: $(COMMIT_MSG)"; \
	fi

# Annotated so `git describe --tags` in the component Makefiles resolves to
# something meaningful for ad-hoc builds between releases.
.PHONY: tag
tag: guard-next
	@if [ -n "$(NO_TAG)" ]; then echo "==> skipping tag (NO_TAG set)"; exit 0; fi; \
	set -e; \
	if git rev-parse -q --verify 'refs/tags/$(TAG)' >/dev/null 2>&1; then \
	  echo "error: tag $(TAG) already exists"; exit 1; \
	fi; \
	git tag -a '$(TAG)' -m '$(COMMIT_MSG)'; \
	echo "==> tagged $(TAG)"

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

# Not a loop over $(COMPONENTS): the two halves are not symmetric. The agent
# dials fathom-mcp on startup, so it must not roll until mcp's new pods are
# serving -- an agent pod that comes up against a half-rolled mcp fails its
# readiness probe and restarts. HELM_FLAGS' --wait is not enough to lean on
# here: it is a knob, and overriding it would silently remove the gate.
.PHONY: deploy
deploy: deploy-mcp deploy-agent

.PHONY: deploy-mcp
deploy-mcp: guard-next
	@echo "==> deploy: fathom-mcp at $(NEXT)"
	@helm upgrade --install fathom-mcp ./mcp/helm $(HELM_NS_FLAG) $(HELM_FLAGS) \
	  --set image.tag=$(NEXT)
	@echo "==> waiting for fathom-mcp pods at $(NEXT)"
	@kubectl -n $(FATHOM_NS) rollout status deploy/fathom-mcp --timeout=$(ROLLOUT_TIMEOUT)
	@img="$$(kubectl -n $(FATHOM_NS) get deploy fathom-mcp \
	          -o jsonpath='{.spec.template.spec.containers[0].image}')"; \
	 case "$$img" in *:$(NEXT)) ;; \
	   *) echo "error: fathom-mcp is running $$img, expected :$(NEXT)"; exit 1;; esac
	@echo "==> fathom-mcp ready at $(NEXT)"

.PHONY: deploy-agent
deploy-agent: guard-next
	@echo "==> deploy: fathom-agent at $(NEXT)"
	@helm upgrade --install fathom-agent ./agent/helm $(HELM_NS_FLAG) $(HELM_FLAGS) \
	  --set image.tag=$(NEXT)
	@kubectl -n $(FATHOM_NS) rollout status deploy/fathom-agent --timeout=$(ROLLOUT_TIMEOUT)
	@echo "==> fathom-agent ready at $(NEXT)"

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
	@echo "    commit:  $(COMMIT_MSG)"
	@echo "    tag:     $(TAG)"
	@echo "    neither is pushed -- take the branch through the usual PR flow."
	@echo "    if push or deploy failed above, this version is committed but never"
	@echo "    shipped: git tag -d $(TAG) && git reset --hard HEAD~1 undoes it."
	@kubectl -n $(FATHOM_NS) get deploy 2>/dev/null || true

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
