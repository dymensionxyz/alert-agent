#!/usr/bin/make -f
BRANCH := $(shell git rev-parse --abbrev-ref HEAD)
COMMIT := $(shell git log -1 --format='%H')
TIME ?= $(shell date +%Y-%m-%dT%H:%M:%S%z)

# don't override user values
ifeq (,$(VERSION))
  VERSION := $(shell git describe --tags)
  # if VERSION is empty, then populate it with branch's name and raw commit hash
  ifeq (,$(VERSION))
    VERSION := $(BRANCH)-$(COMMIT)
  endif
endif

ldflags = -X main.BuildVersion=$(VERSION) \
		  -X main.BuildCommit=$(COMMIT) \
		  -X main.BuildTime=$(TIME)

BUILD_FLAGS := -ldflags '$(ldflags)' -tags=cgo

# ---------------------------------------------------------------------------- #
#                                 Make targets                                   #
# ---------------------------------------------------------------------------- #
.PHONY: install
install: go.sum ## Installs the alert-agent binary
	go install -mod=readonly $(BUILD_FLAGS) .

.PHONY: build
build: ## Compiles the alert-agent binary
	go build -o build/alert-agent $(BUILD_FLAGS) .

###############################################################################
###                                Releasing                                  ###
###############################################################################

PACKAGE_NAME:=github.com/dymensionxyz/alert-agent
GOLANG_CROSS_VERSION  = v1.23
GOPATH ?= '$(HOME)/go'

release-dry-run:
	podman run \
		--rm \
		--privileged \
		-e CGO_ENABLED=1 \
		-v /var/run/docker.sock:/var/run/docker.sock \
		-v `pwd`:/go/src/$(PACKAGE_NAME) \
		-v ${GOPATH}/pkg:/go/pkg \
		-w /go/src/$(PACKAGE_NAME) \
		ghcr.io/goreleaser/goreleaser-cross:${GOLANG_CROSS_VERSION} \
		--clean --skip=validate --skip=publish --snapshot

release:
	@if [ ! -f ".release-env" ]; then \
		echo "\033[91m.release-env is required for release\033[0m";\
		exit 1;\
	fi
	docker run \
		--rm \
		--privileged \
		-e CGO_ENABLED=1 \
		--env-file .release-env \
		-v /var/run/docker.sock:/var/run/docker.sock \
		-v `pwd`:/go/src/$(PACKAGE_NAME) \
		-w /go/src/$(PACKAGE_NAME) \
		ghcr.io/goreleaser/goreleaser-cross:${GOLANG_CROSS_VERSION} \
		release --clean --skip=validate

.PHONY: release-dry-run release
