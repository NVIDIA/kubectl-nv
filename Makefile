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
.PHONY: build fmt verify release

GO_CMD ?= go
GO_FMT ?= gofmt
GO_SRC := $(shell find . -type f -name '*.go' -not -path "./vendor/*")
CONTAINER_RUN_CMD ?= docker run

BINARY_NAME ?= kubectl-nv

VERSION := 0.1.0

build:
	@rm -rf bin
	$(GO_CMD) build -o bin/$(BINARY_NAME) cmd/main.go

fmt:
	@$(GO_FMT) -w -l $$(find . -name '*.go')

verify:
	@out=`$(GO_FMT) -w -l -d $$(find . -name '*.go')`; \
	if [ -n "$$out" ]; then \
	    echo "$$out"; \
	    exit 1; \
	fi

ci-lint:
	golangci-lint run --timeout 10m

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

release:
	@rm -rf bin
	@mkdir -p bin
	@for os in linux darwin; do \
		for arch in amd64 arm64; do \
			echo "Building $$os-$$arch"; \
			GOOS=$$os GOARCH=$$arch $(GO_CMD) build -o bin/$(BINARY_NAME)-$$os-$$arch cmd/main.go; \
		done; \
	done