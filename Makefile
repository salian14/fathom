# Fathom -- top-level release Makefile.
#
# The components build and test independently (agent/Makefile, mcp/Makefile,
# front/Makefile) and nothing here replaces that. This file exists for
# the one operation that spans them all: cutting a version.
#
#     make release BUMP=minor
#
# runs every component check, bumps all three components to the same next
# semver, commits and tags that bump, builds and pushes every image at that tag,
# and rolls every chart out with it.
#
# The versions move in lockstep on purpose -- one Fathom version, not three.
# The base is the highest version any chart currently declares, so the charts
# converge on the first release even when they have drifted (mcp appVersion
# 0.3.0 vs agent 0.2.0 at the time this was written). A new component joins at
# the current version rather than at 0.1.0 for the same reason: the base is read
# from the charts, and a low outlier is invisible while a high one moves it.
#
# The image tag is not passed around out of band: every chart defaults
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
# fathom-mcp's new pods are actually serving, and fathom-front does not roll
# until the agent's are.

SHELL := /bin/bash
.DEFAULT_GOAL := help
.NOTPARALLEL:

# Build/push/check order, not alphabetical: the mcp chart owns the fathom
# Namespace and exposes the tools the agent dials on startup, and fathom-front
# proxies to the agent. `deploy` does not loop over this -- see the deploy
# targets, where the three are not symmetric.
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
# The harness lives outside the fathom namespace on purpose: it is the only component
# with write credentials, so it is kept away from the services it tests.
EVAL_NS         ?= fathom-eval
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
	@echo "  make release BUMP=major|minor|patch   check, bump, build, push, deploy -- all components"
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
	@echo "  make deploy                           helm upgrade every chart -- each waits on the last"
	@echo "  make render                           render every chart at the next version"
	@echo "  make clean                            clean every component"
	@echo
	@echo "Knobs:  ALLOW_DIRTY=1  SKIP_CHECK=1  NO_COMMIT=1  NO_TAG=1  COMPONENT=<name>"
	@echo "        HELM_NAMESPACE=<ns>  HELM_FLAGS='...'  FATHOM_NS=<ns>  EVAL_NS=<ns>"
	@echo "        ROLLOUT_TIMEOUT=5m"
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
# This needs Docker, because it runs the services. `contracts-check` is therefore
# not cheap, and the pre-commit hook in .githooks/ skips it when Docker is not
# running rather than blocking a commit on a daemon that is not there.
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
	$$py scripts/contracts.py $(COMPONENT)

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

# Not a loop over $(COMPONENTS): the three are not symmetric. Each dials the one
# before it on startup -- the agent dials fathom-mcp, fathom-front's readiness
# is the agent's -- so each must not roll until the previous one's new pods are
# serving. A pod that comes up against a half-rolled dependency fails its
# readiness probe and restarts. HELM_FLAGS' --wait is not enough to lean on
# here: it is a knob, and overriding it would silently remove the gate.
.PHONY: deploy
deploy: deploy-mcp deploy-agent deploy-front deploy-eval

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

# Last, and gated on the agent above for the same reason the agent is gated on
# mcp: fathom-front's readiness probe *is* the agent's readiness, so rolling it
# against a half-rolled agent produces a pod that will not go ready.
.PHONY: deploy-front
deploy-front: guard-next
	@echo "==> deploy: fathom-front at $(NEXT)"
	@helm upgrade --install fathom-front ./front/helm $(HELM_NS_FLAG) $(HELM_FLAGS) \
	  --set image.tag=$(NEXT)
	@kubectl -n $(FATHOM_NS) rollout status deploy/fathom-front --timeout=$(ROLLOUT_TIMEOUT)
	@echo "==> fathom-front ready at $(NEXT)"

# Last. Gates in two parts, because this chart holds two kinds of workload.
#
# The CronJobs cannot be waited on: a CronJob has no rollout, and between scheduled
# runs there is no pod to become ready. Nor can we wait for the PVC to be Bound on
# their account -- the cluster's default StorageClass is local-path with
# WaitForFirstConsumer, so an unmounted claim stays Pending by design. What can be
# asserted is that the CronJob exists and its jobTemplate carries the tag we pushed.
#
# The read service is a Deployment and does have a rollout, so it is waited on --
# guarded by a `get`, because serve.enabled=false is a supported install and its
# absence is not a failure. It is also now the PVC's first consumer on a fresh
# install, which means the claim binds at deploy time rather than at the first run.
# That is a change in behaviour from when this chart was CronJobs only, and it is
# fine: binding becomes deterministic and early, and the Deployment carries the same
# scheduling constraints as the CronJobs so it cannot strand the volume on a node
# they cannot reach.
#
# Note the chart ships both CronJobs suspended. `make deploy` should not start a
# multi-hour evaluation against a cluster it finished rolling seconds ago; unsuspending
# is a deliberate act, and NOTES.txt says how.
.PHONY: deploy-eval
deploy-eval: guard-next
	@echo "==> deploy: fathom-eval at $(NEXT)"
	@helm upgrade --install fathom-eval ./eval/helm $(HELM_NS_FLAG) $(HELM_FLAGS) \
	  --set image.tag=$(NEXT)
	@img="$$(kubectl -n $(EVAL_NS) get cronjob fathom-eval \
	          -o jsonpath='{.spec.jobTemplate.spec.template.spec.containers[0].image}')"; \
	 case "$$img" in *:$(NEXT)) ;; \
	   *) echo "error: fathom-eval CronJob is set to run $$img, expected :$(NEXT)"; exit 1;; esac
	@echo "==> fathom-eval scheduled at $(NEXT) (suspended: $$(kubectl -n $(EVAL_NS) get cronjob fathom-eval -o jsonpath='{.spec.suspend}'))"
	@if kubectl -n $(EVAL_NS) get deploy fathom-eval-serve >/dev/null 2>&1; then \
	  echo "==> waiting for fathom-eval-serve"; \
	  kubectl -n $(EVAL_NS) rollout status deploy/fathom-eval-serve --timeout=$(ROLLOUT_TIMEOUT); \
	fi

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
	@echo "    images:  fathom-mcp:$(NEXT), fathom-agent:$(NEXT), fathom-front:$(NEXT), fathom-eval:$(NEXT)"
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
