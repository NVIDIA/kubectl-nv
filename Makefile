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
.PHONY: build fmt verify test coverage e2e e2e-mokka lint ci-lint vendor mod-tidy mod-vendor mod-verify check-vendor licenses release-snapshot mdlint

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
