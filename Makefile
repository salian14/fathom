# Fathom -- top-level release Makefile.
#
# The components build and test independently (agent/Makefile, mcp/Makefile,
# front/Makefile) and nothing here replaces that. This file exists for
# the one operation that spans them all: cutting a version.
#
#     make release BUMP=minor
#
# runs every component check, bumps every component to the same next semver,
# commits and tags that bump, and builds and pushes every image at that tag.
#
# It does not deploy, and nothing in this repository does. The lab is deployed
# by Argo CD from the `bifrost` branch (deploy/applications.yaml, docs/deploy.md):
# the release commit reaching `bifrost` through its PR is the rollout.
#
# The versions move in lockstep on purpose -- one Fathom version, not three.
# The base is the highest version any chart currently declares, so the charts
# converge on the first release even when they have drifted (mcp appVersion
# 0.3.0 vs agent 0.2.0 at the time this was written). A new component joins at
# the current version rather than at 0.1.0 for the same reason: the base is read
# from the charts, and a low outlier is invisible while a high one moves it.
#
# The image tag is not passed around out of band: every chart defaults
# `image.tag` to `.Chart.AppVersion`, so the bump commit is what re-points the
# Deployment once Argo CD sees it. That is also why the order below matters:
# the images are pushed before the commit can be merged, so the tag a chart
# names always exists by the time anything tries to pull it.
#
# The bump stages, commits and tags itself (NO_COMMIT=1 / NO_TAG=1 opt out), so
# `push` builds from a committed tree -- which is what preflight's dirty-tree
# check already claims to guarantee. Nothing here ever contacts a remote: the
# commit and the tag stay local and go out through the normal PR flow. The
# one thing that does leave the machine is the images, to $(REGISTRY).

SHELL := /bin/bash
.DEFAULT_GOAL := help
.NOTPARALLEL:

# Build/push/check order, not alphabetical: it follows the dependency chain --
# mcp exposes the tools the agent dials on startup, and fathom-front proxies to
# the agent -- so a failure surfaces in the component that caused it.
COMPONENTS := mcp agent front eval
CHARTS     := mcp/helm/Chart.yaml agent/helm/Chart.yaml front/helm/Chart.yaml eval/helm/Chart.yaml

# Everything the bump rewrites and then commits. CURRENT is still derived from
# the charts alone: the agent package files are followers, not a source of
# truth, and their stale version must not drag the release base down.
#
# fathom-front adds nothing here. Like mcp, its version is injected at build
# time via -ldflags, so there is no version string checked into its source to
# fall behind.
VERSIONED := $(CHARTS) agent/pyproject.toml agent/src/fathom_agent/__init__.py \
             eval/pyproject.toml eval/src/fathom_eval/__init__.py

BUMP        ?=
ALLOW_DIRTY ?=
SKIP_CHECK  ?=
NO_COMMIT   ?=
NO_TAG      ?=

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
	@echo "  make release BUMP=major|minor|patch   check, bump, build, push -- all components"
	@echo "  make release-major | release-minor | release-patch"
	@echo "  make release VERSION=1.4.2            pin an exact version instead of bumping"
	@echo
	@echo "  make version                          show the current and next version"
	@echo "  make check                            run every component check, then contracts-check"
	@echo "  make contracts                        regenerate contracts/ from the components themselves"
	@echo "  make contracts-check                  fail if contracts/ has drifted from the code"
	@echo "  make hooks                            install the checked-in git hooks (.githooks)"
	@echo "  make bump BUMP=patch                  rewrite the version files, then commit and tag them"
	@echo "  make push  BUMP=patch                 build and push every image only"
	@echo "  make render                           render every chart at the next version"
	@echo "  make clean                            clean every component"
	@echo
	@echo "Knobs:  ALLOW_DIRTY=1  SKIP_CHECK=1  NO_COMMIT=1  NO_TAG=1  COMPONENT=<name>"
	@echo
	@echo "Deploying is not a make target: Argo CD syncs the lab from bifrost."
	@echo "See docs/deploy.md."
	@echo
	@echo "Current version: $(CURRENT)"

.PHONY: version
version:
	@echo "current: $(CURRENT)"
	@echo "next:    $(if $(NEXT),$(NEXT),unset -- pass BUMP=major|minor|patch or VERSION=x.y.z)"

# Everything that can fail cheaply fails here, before an image is built or a
# chart is rewritten -- a release that dies halfway leaves the charts bumped and
# no image behind the tag they name.
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
check: contracts-check
	@if [ -n "$(SKIP_CHECK)" ]; then echo "==> skipping component checks (SKIP_CHECK set)"; else \
	  for c in $(COMPONENTS); do \
	    echo "==> check: $$c"; $(MAKE) --no-print-directory -C $$c check || exit 1; \
	  done; \
	fi

# --- contracts ---------------------------------------------------------------
#
# contracts/ is generated, never edited -- and nothing in any component exists to
# generate it. The writer builds each component's image, starts it in a throwaway
# container configured the way its own chart configures it, and scrapes it over the
# wire: FastAPI's own /openapi.json for the agent, a real tools/list for mcp, the
# applied SQLite schema for eval. It merges in the facts only the chart knows --
# namespace, Service type, nodePort, RBAC, probes.
#
# The container run is also the port check: it dials the chart's own containerPort,
# so a pod that would start, pass its probes and be unreachable through its Service
# fails here instead.
#
# This needs Docker, because it runs the services -- with one carve-out. FastAPI
# publishes /openapi.json itself, so `make contracts` can serve agent and eval from
# their own venvs when no daemon is there and still produce the identical document;
# `mcp`'s tool surface and the two port checks need a container and are left alone.
# `contracts-check` deliberately does not take that path: it is the drift gate inside
# `make check`, and a gate that silently checks less than it claims is worse than one
# that refuses.
#
# The writer needs PyYAML, httpx and the MCP SDK. agent's venv carries all three
# already, so it is used and built only if it does not exist.
CONTRACT_PY := $(wildcard agent/.venv/bin/python)

.PHONY: contract-venv
contract-venv:
	@if [ -z "$(CONTRACT_PY)" ]; then \
	  echo "==> no agent venv yet; building it for PyYAML, httpx and the MCP SDK"; \
	  $(MAKE) --no-print-directory -C agent venv; \
	fi

.PHONY: contracts
contracts: contract-venv
	@py="$(CONTRACT_PY)"; [ -n "$$py" ] || py=agent/.venv/bin/python; \
	$$py scripts/contracts.py --allow-offline $(COMPONENT)

# Regenerates into a temp tree and diffs, so a drifted contract is reported
# without being silently repaired -- `make contracts` is how you repair it.
# The two hand-written things are excluded: integration-notes.md, which the writer
# only ever creates and never rewrites, and contracts/README.md. Neither exists in
# a freshly generated tree, and neither absence is drift.
.PHONY: contracts-check
contracts-check: contract-venv
	@py="$(CONTRACT_PY)"; [ -n "$$py" ] || py=agent/.venv/bin/python; \
	tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	$$py scripts/contracts.py --out "$$tmp" $(COMPONENT) || exit 1; \
	scope="$(COMPONENT)"; rc=0; : >"$$tmp.diff"; \
	if [ -n "$$scope" ]; then \
	  for c in $$scope; do \
	    diff -r -x integration-notes.md -x README.md "contracts/$$c" "$$tmp/$$c" >>"$$tmp.diff" 2>&1 || rc=1; \
	  done; \
	else \
	  diff -r -x integration-notes.md -x README.md contracts "$$tmp" >>"$$tmp.diff" 2>&1 || rc=1; \
	fi; \
	if [ $$rc -eq 0 ]; then \
	  echo "==> contracts up to date"; \
	else \
	  echo "==> contracts have drifted from the code:"; cat "$$tmp.diff"; \
	  echo; echo "Run 'make contracts' and commit the result."; \
	  rm -f "$$tmp.diff"; exit 1; \
	fi; \
	rm -f "$$tmp.diff"

# Opt in to the checked-in hooks. Separate from anything that runs on its own:
# pointing core.hooksPath at a directory in the repo means a `git pull` can change
# what runs on your machine at commit time, and that is a decision to take
# knowingly rather than a side effect of running a build.
.PHONY: hooks
hooks:
	@git config core.hooksPath .githooks
	@echo "core.hooksPath = .githooks"
	@echo "pre-commit will regenerate contracts/ for the components a commit touches."
	@echo "Bypass once with 'git commit --no-verify', or always with FATHOM_SKIP_CONTRACTS=1."
	@echo "Undo with 'git config --unset core.hooksPath'."

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
	@sed -i -E 's/^version = .*/version = "$(NEXT)"/' eval/pyproject.toml
	@sed -i -E 's/^__version__ = .*/__version__ = "$(NEXT)"/' eval/src/fathom_eval/__init__.py
	@touch agent/requirements.lock eval/requirements.lock
	@echo "==> agent and eval packages -> $(NEXT)"
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

# Preview only: this renders the *current* charts with the next image tag, so the
# chart labels still read the old version until `make bump` has run.
.PHONY: render
render: guard-next
	@for c in $(COMPONENTS); do \
	  helm template fathom-$$c ./$$c/helm --set image.tag=$(NEXT); \
	done

.PHONY: release
release: preflight check bump push
	@echo
	@echo "==> released $(NEXT)"
	@echo "    images:  fathom-mcp:$(NEXT), fathom-agent:$(NEXT), fathom-front:$(NEXT), fathom-eval:$(NEXT)"
	@echo "    commit:  $(COMMIT_MSG)"
	@echo "    tag:     $(TAG)"
	@echo "    neither is pushed -- take the branch through the usual PR flow."
	@echo "    Argo CD rolls the lab to $(NEXT) within a minute of the merge to bifrost."
	@echo "    if push failed above, this version is committed but has no images:"
	@echo "    git tag -d $(TAG) && git reset --hard HEAD~1 undoes it."

.PHONY: release-major release-minor release-patch
release-major:
	@$(MAKE) --no-print-directory release BUMP=major
release-minor:
	@$(MAKE) --no-print-directory release BUMP=minor
release-patch:
	@$(MAKE) --no-print-directory release BUMP=patch

# The individual steps are useful on their own (re-push a failed image, render
# a chart at the next tag), but none of them can run without a version.
.PHONY: guard-next
guard-next:
	@if [ -z "$(NEXT)" ]; then \
	  echo "error: set BUMP=major|minor|patch (or VERSION=x.y.z)"; exit 1; fi

.PHONY: clean
clean:
	@for c in $(COMPONENTS); do $(MAKE) --no-print-directory -C $$c clean; done
