# Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#     http://www.apache.org/licenses/LICENSE-2.0
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
.PHONY: build fmt verify test coverage e2e e2e-mokka lint ci-lint vendor mod-tidy mod-vendor mod-verify check-vendor licenses release-snapshot mdlint schema

CONTAINER_RUN_CMD ?= docker run
GO_CMD ?= go
GO_FMT ?= gofmt
GO_SRC := $(shell find . -type f -name '*.go' -not -path "./vendor/*" -not -path "./dist/*")
CONTAINER_RUN_CMD ?= docker run

BINARY_NAME ?= kubectl-nv

VERSION := 0.1.0

build:
	@rm -rf bin
	$(GO_CMD) build -ldflags "-X main.version=$(VERSION)" -o bin/$(BINARY_NAME) ./cmd/kubectl-nv

fmt:
	@$(GO_FMT) -w -l $(GO_SRC)

COVERAGE_FILE := coverage.out
test:
	go test -coverprofile=$(COVERAGE_FILE) ./cmd/... ./pkg/...
coverage: test
	go tool cover -func=$(COVERAGE_FILE)

# The end to end suite is behind the "e2e" build tag, so "make test" above
# never compiles it and never needs a cluster.
MOKKA_LANE ?= mokka
MOKKA_PROFILE ?= t4
E2E_FLAGS ?= -tags e2e -count=1 -timeout 30m
MOKKA_CLUSTER := kubectl-nv-$(MOKKA_LANE)
MOKKA_CONTEXT := kind-$(MOKKA_CLUSTER)
# The same variable up.sh and down.sh read, so that the kubeconfig this target
# points the suite at and the one those scripts write are one file by
# construction rather than by two definitions agreeing.
MOKKA_BIN_DIR ?= $(CURDIR)/hack/mokka/bin
MOKKA_KUBECONFIG := $(MOKKA_BIN_DIR)/kubeconfig-$(MOKKA_LANE)
# Where a failed run leaves what it saw. Under bin/, which is already ignored,
# and never under hack/mokka/bin, which holds the lane's credentials.
E2E_DIAG_DIR ?= $(CURDIR)/bin/e2e-diagnostics-$(MOKKA_LANE)

# Runs the suite against whatever KUBECONFIG and KUBECTL_NV_BIN already name.
# Both have to be absolute paths: go test runs the test binary with the package
# directory as its working directory, so a relative one resolves somewhere the
# caller did not mean.
#
# The suite runs a must-gather against whatever cluster it is pointed at, so the
# current context has to be a lane of ours unless the caller says otherwise with
# E2E_ANY_CLUSTER=1. An unset KUBECONFIG is left to the suite, which reports it
# and never falls back to the caller's default kubeconfig; asking kubectl about
# an empty one here would read exactly that file.
e2e:
	@set -u; \
	if [ "$${E2E_ANY_CLUSTER:-}" != "1" ] && [ -n "$${KUBECONFIG:-}" ]; then \
	    ctx="$$(kubectl --kubeconfig "$${KUBECONFIG}" config current-context 2>/dev/null || true)"; \
	    case "$${ctx}" in \
	    kind-kubectl-nv-*) ;; \
	    *) printf 'make e2e: current context is %s, which is not a kubectl-nv lane\n' "$${ctx:-<none>}" >&2; \
	       printf 'make e2e: set E2E_ANY_CLUSTER=1 to run against it anyway\n' >&2; \
	       exit 2;; \
	    esac; \
	fi; \
	$(GO_CMD) test $(E2E_FLAGS) ./test/e2e/...

# Brings a Mokka lane up, runs the suite against it, and tears it down again
# whichever way the suite ended. The lane kubeconfig is named explicitly: it is
# not the caller's default one, and nothing here may touch that.
#
# A lane that failed to come up is worth a dump too: the reason it did not come
# up is in the pods and the events, and both are gone a second after the
# teardown. So the trap is armed as soon as up.sh has been allowed to touch the
# cluster, and the dump runs for any non-zero result, up.sh's or the suite's.
#
# The one exception is up.sh exit 4, which means a cluster of that name was
# already there and nothing was touched. That lane belongs to someone else: it
# is neither dumped nor deleted.
#
# INT and TERM turn into an ordinary exit, which runs the EXIT trap and nothing
# more, so a cancelled CI job tears its lane down exactly once rather than once
# per signal.
e2e-mokka: build
	@set -u; \
	export MOKKA_BIN_DIR="$(MOKKA_BIN_DIR)"; \
	hack/mokka/up.sh $(MOKKA_LANE) $(MOKKA_PROFILE); up=$$?; \
	if [ $$up -eq 4 ]; then \
	    printf 'e2e-mokka: %s already exists and was not created here; nothing touched\n' "$(MOKKA_CLUSTER)" >&2; \
	    exit 4; \
	fi; \
	trap 'exit 130' INT; \
	trap 'exit 143' TERM; \
	trap 'hack/mokka/down.sh $(MOKKA_LANE) || printf "e2e-mokka: down.sh failed; cluster %s may still be up\n" "$(MOKKA_CLUSTER)" >&2' EXIT; \
	rc=$$up; \
	if [ $$rc -eq 0 ]; then \
	    KUBECONFIG=$(MOKKA_KUBECONFIG) \
	    KUBECTL_NV_BIN=$(CURDIR)/bin/$(BINARY_NAME) \
	        $(GO_CMD) test $(E2E_FLAGS) ./test/e2e/...; \
	    rc=$$?; \
	fi; \
	if [ $$rc -ne 0 ]; then \
	    mkdir -p $(E2E_DIAG_DIR); \
	    kubectl --kubeconfig $(MOKKA_KUBECONFIG) --context $(MOKKA_CONTEXT) \
	        get pods -A -o wide > $(E2E_DIAG_DIR)/pods.txt 2>&1 || true; \
	    kubectl --kubeconfig $(MOKKA_KUBECONFIG) --context $(MOKKA_CONTEXT) \
	        get events -A --sort-by=.lastTimestamp > $(E2E_DIAG_DIR)/events.txt 2>&1 || true; \
	    kind export logs $(E2E_DIAG_DIR)/kind --name $(MOKKA_CLUSTER) \
	        > $(E2E_DIAG_DIR)/kind-export.txt 2>&1 || true; \
	    printf 'e2e-mokka: diagnostics in %s\n' "$(E2E_DIAG_DIR)" >&2; \
	fi; \
	exit $$rc

# Regenerates the published output schemas from the registered kinds. The
# generator is the plugin itself, so the schemas cannot describe a type the
# commands do not print. A test fails when the checked-in files differ from
# what this writes.
SCHEMA_DIR ?= $(CURDIR)/docs/schema
schema:
	$(GO_CMD) run ./cmd/kubectl-nv schema --write-dir $(SCHEMA_DIR)

verify:
	@out=`$(GO_FMT) -l -d $(GO_SRC)`; \
	if [ -n "$$out" ]; then \
	    echo "$$out"; \
	    exit 1; \
	fi

ci-lint:
	golangci-lint run --timeout 10m

lint:
	golangci-lint run --timeout 10m
	bash hack/lint-words_test.sh
	bash hack/lint-words.sh

vendor:  | mod-tidy mod-vendor mod-verify

mod-tidy:
	@for mod in $$(find . -name go.mod -not -path "./testdata/*" -not -path "./third_party/*"); do \
	    echo "Tidying $$mod..."; ( \
	        cd $$(dirname $$mod) && go mod tidy \
            ) || exit 1; \
	done

mod-vendor:
	@for mod in $$(find . -name go.mod -not -path "./testdata/*" -not -path "./third_party/*" -not -path "./deployments/*"); do \
		echo "Vendoring $$mod..."; ( \
			cd $$(dirname $$mod) && go mod vendor \
			) || exit 1; \
	done

mod-verify:
	@for mod in $$(find . -name go.mod -not -path "./testdata/*" -not -path "./third_party/*"); do \
	    echo "Verifying $$mod..."; ( \
	        cd $$(dirname $$mod) && go mod verify | sed 's/^/  /g' \
	    ) || exit 1; \
	done

check-vendor: vendor
	git diff --exit-code HEAD -- go.mod go.sum vendor

licenses:
	go-licenses csv $(MODULE)/...

release-snapshot:
	goreleaser release --snapshot --clean --skip=sbom

mdlint:
	${CONTAINER_RUN_CMD} \
	--rm \
	--volume "${PWD}:/workdir:ro,z" \
	--workdir /workdir \
	ruby:slim \
	/workdir/hack/mdlint.sh

# ---------------------------------------------------------------------------
# The MIG lane
# ---------------------------------------------------------------------------
#
# A second Mokka lane whose workers are partitioned: two simulated H100 boards
# each carved into seven 1g.10gb slices, so a worker advertises 14
# nvidia.com/gpu. It is a lane of its own and not a profile of e2e-mokka
# because it is built from an upstream commit rather than from the released
# chart, which cannot partition a board; hack/mokka/mig/lib.sh says why.
.PHONY: e2e-mig

MIG_LANE ?= mig
MIG_CLUSTER := kubectl-nv-$(MIG_LANE)
MIG_CONTEXT := kind-$(MIG_CLUSTER)
# The same MOKKA_BIN_DIR the whole-GPU lane uses, so both lanes' kubeconfigs
# live in the one gitignored, mode 0700 directory the scripts write.
MIG_KUBECONFIG := $(MOKKA_BIN_DIR)/kubeconfig-$(MIG_LANE)
E2E_MIG_DIAG_DIR ?= $(CURDIR)/bin/e2e-diagnostics-$(MIG_LANE)

# test/e2e/mig_test.go is behind "e2e && mig" and the rest of the suite is not,
# so this runs with both tags and then selects. The selection is needed: the
# must-gather test asserts the GPU Operator's own device plugin is on every
# worker, and this lane deliberately replaces it.
E2E_MIG_RUN := ^TestMIGLane$$
# 15m, not the whole job budget: the suite runs one parent test whose
# subtests each exec the binary once against a lane that is already up. The
# first live run took seconds. A timeout generous enough to swallow a wedged
# exec turns a hang into a job timeout with no test output, which is the one
# failure nobody can read.
E2E_MIG_FLAGS ?= -tags e2e,mig -count=1 -timeout 15m -run '$(E2E_MIG_RUN)'

e2e-mig: build
	@set -u; \
	export MOKKA_BIN_DIR="$(MOKKA_BIN_DIR)"; \
	hack/mokka/mig/build-image.sh || exit $$?; \
	lane_is_ours=0; rc=0; \
	dump_diagnostics() { \
	    mkdir -p $(E2E_MIG_DIAG_DIR); \
	    kubectl --kubeconfig $(MIG_KUBECONFIG) --context $(MIG_CONTEXT) \
	        get pods -A -o wide > $(E2E_MIG_DIAG_DIR)/pods.txt 2>&1 || true; \
	    kubectl --kubeconfig $(MIG_KUBECONFIG) --context $(MIG_CONTEXT) \
	        get nodes -o yaml > $(E2E_MIG_DIAG_DIR)/nodes.yaml 2>&1 || true; \
	    kubectl --kubeconfig $(MIG_KUBECONFIG) --context $(MIG_CONTEXT) \
	        --namespace kube-system logs -l name=nvidia-device-plugin-mock --tail=-1 \
	        > $(E2E_MIG_DIAG_DIR)/device-plugin-mock.log 2>&1 || true; \
	    kubectl --kubeconfig $(MIG_KUBECONFIG) --context $(MIG_CONTEXT) \
	        get events -A --sort-by=.lastTimestamp > $(E2E_MIG_DIAG_DIR)/events.txt 2>&1 || true; \
	    kind export logs $(E2E_MIG_DIAG_DIR)/kind --name $(MIG_CLUSTER) \
	        > $(E2E_MIG_DIAG_DIR)/kind-export.txt 2>&1 || true; \
	    printf 'e2e-mig: diagnostics in %s\n' "$(E2E_MIG_DIAG_DIR)" >&2; \
	}; \
	finish() { \
	    trap - EXIT INT TERM; \
	    if [ "$$lane_is_ours" -eq 1 ]; then \
	        if [ "$$1" -ne 0 ]; then dump_diagnostics; fi; \
	        hack/mokka/mig/down.sh $(MIG_LANE); d=$$?; \
	        if [ $$d -ne 0 ] && [ $$d -ne 2 ]; then \
	            printf 'e2e-mig: down.sh failed; cluster %s may still be up\n' "$(MIG_CLUSTER)" >&2; \
	        fi; \
	    fi; \
	    exit "$$1"; \
	}; \
	trap 'finish 130' INT; \
	trap 'finish 143' TERM; \
	trap 'finish $$rc' EXIT; \
	lane_is_ours=1; \
	hack/mokka/mig/up.sh $(MIG_LANE); up=$$?; \
	if [ $$up -eq 4 ]; then \
	    lane_is_ours=0; rc=4; \
	    printf 'e2e-mig: %s already exists and was not created here; nothing touched\n' "$(MIG_CLUSTER)" >&2; \
	    exit 4; \
	fi; \
	rc=$$up; \
	if [ $$rc -eq 0 ]; then \
	    list_out=$(E2E_MIG_DIAG_DIR)-list-stdout.txt; \
	    list_err=$(E2E_MIG_DIAG_DIR)-list-stderr.txt; \
	    mkdir -p "$$(dirname $$list_out)"; \
	    KUBECONFIG=$(MIG_KUBECONFIG) \
	    KUBECTL_NV_BIN=$(CURDIR)/bin/$(BINARY_NAME) \
	        $(GO_CMD) test -tags e2e,mig -list '$(E2E_MIG_RUN)' ./test/e2e/... \
	        > "$$list_out" 2> "$$list_err"; \
	    listed="$$(grep -c '^Test' "$$list_out" || true)"; \
	    exact="$$(grep -c '$(E2E_MIG_RUN)' "$$list_out" || true)"; \
	    if [ "$$listed" -ne 1 ] || [ "$$exact" -ne 1 ]; then \
	        printf 'e2e-mig: -run %s listed %s test(s), %s of them %s; want exactly one of each\n' \
	            "$(E2E_MIG_RUN)" "$$listed" "$$exact" "$(E2E_MIG_RUN)" >&2; \
	        printf 'e2e-mig: a renamed, deleted or duplicated TestMIGLane would otherwise run nothing and report success\n' >&2; \
	        printf 'e2e-mig: go test listed:\n' >&2; \
	        cat "$$list_out" >&2; \
	        printf 'e2e-mig: go test said:\n' >&2; \
	        cat "$$list_err" >&2; \
	        rc=2; \
	    fi; \
	    rm -f "$$list_out" "$$list_err"; \
	fi; \
	if [ $$rc -eq 0 ]; then \
	    KUBECONFIG=$(MIG_KUBECONFIG) \
	    KUBECTL_NV_BIN=$(CURDIR)/bin/$(BINARY_NAME) \
	        $(GO_CMD) test $(E2E_MIG_FLAGS) ./test/e2e/...; \
	    rc=$$?; \
	fi; \
	exit $$rc
