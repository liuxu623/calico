###############################################################################
# Both native and cross architecture builds are supported.
# The target architecture is select by setting the ARCH variable.
# When ARCH is undefined it is set to the detected host architecture.
# When ARCH differs from the host architecture a crossbuild will be performed.
ARCHES=$(patsubst docker-image/Dockerfile.%,%,$(wildcard docker-image/Dockerfile.*))

# BUILDARCH is the host architecture
# ARCH is the target architecture
# we need to keep track of them separately
BUILDARCH ?= $(shell uname -m)
BUILDOS ?= $(shell uname -s | tr A-Z a-z)

# canonicalized names for host architecture
ifeq ($(BUILDARCH),aarch64)
        BUILDARCH=arm64
endif
ifeq ($(BUILDARCH),x86_64)
        BUILDARCH=amd64
endif

# unless otherwise set, I am building for my own architecture, i.e. not cross-compiling
ARCH ?= $(BUILDARCH)

# canonicalized names for target architecture
ifeq ($(ARCH),aarch64)
        override ARCH=arm64
endif
ifeq ($(ARCH),x86_64)
    override ARCH=amd64
endif

GO_BUILD_VER ?= v0.14
join_platforms = $(subst $(space),$(comma),$(call prefix_linux,$(strip $1)))

# for building, we use the go-build image for the *host* architecture, even if the target is different
# the one for the host should contain all the necessary cross-compilation tools
GO_BUILD_CONTAINER = calico/go-build:$(GO_BUILD_VER)-$(BUILDARCH)

# Get version from git.
GIT_VERSION?=$(shell git describe --tags --dirty)

CONTAINER_NAME=calico/typha

help:
	@echo "Typha Makefile"
	@echo
	@echo "Dependencies: docker 1.12+; go 1.8+"
	@echo
	@echo "For any target, set ARCH=<target> to build for a given target."
	@echo "For example, to build for arm64:"
	@echo
	@echo "  make build ARCH=arm64"
	@echo
	@echo "Initial set-up:"
	@echo
	@echo "  make update-tools  Update/install the go build dependencies."
	@echo
	@echo "Builds:"
	@echo
	@echo "  make all           Build all the binary packages."
	@echo "  make image         Build $(CONTAINER_NAME) docker image."
	@echo
	@echo "Tests:"
	@echo
	@echo "  make ut                Run UTs."
	@echo "  make go-cover-browser  Display go code coverage in browser."
	@echo
	@echo "Maintenance:"
	@echo
	@echo "  make update-vendor  Update the vendor directory with new "
	@echo "                      versions of upstream packages.  Record results"
	@echo "                      in glide.lock."
	@echo "  make go-fmt        Format our go code."
	@echo "  make clean         Remove binary files."
	@echo "-----------------------------------------"
	@echo "ARCH (target):          $(ARCH)"
	@echo "BUILDARCH (host):       $(BUILDARCH)"
	@echo "GO_BUILD_CONTAINER:     $(GO_BUILD_CONTAINER)"
	@echo "-----------------------------------------"

# Disable make's implicit rules, which are not useful for golang, and slow down the build
# considerably.
.SUFFIXES:

all: $(CONTAINER_NAME) bin/typha-client-$(ARCH)
test: ut

###############################################################################
# tag and push images of any tag
###############################################################################

# ensure we have a real imagetag
imagetag:
ifndef IMAGETAG
	$(error IMAGETAG is undefined - run using make <target> IMAGETAG=X.Y.Z)
endif

## push one arch
push: imagetag
	docker push $(CONTAINER_NAME):$(IMAGETAG)-$(ARCH)
	docker push quay.io/$(CONTAINER_NAME):$(IMAGETAG)-$(ARCH)

	# Push images to gcr.io, used by GKE.
	docker push gcr.io/projectcalico-org/$(CONTAINER_NAME):$(IMAGETAG)
	docker push eu.gcr.io/projectcalico-org/$(CONTAINER_NAME):$(IMAGETAG)
	docker push asia.gcr.io/projectcalico-org/$(CONTAINER_NAME):$(IMAGETAG)
	docker push us.gcr.io/projectcalico-org/$(CONTAINER_NAME):$(IMAGETAG)
ifeq ($(ARCH),amd64)
	docker push $(CONTAINER_NAME):$(IMAGETAG)
	docker push quay.io/$(CONTAINER_NAME):$(IMAGETAG)

	# Push images to gcr.io, used by GKE.
	docker push gcr.io/projectcalico-org/$(CONTAINER_NAME):$(IMAGETAG)
	docker push eu.gcr.io/projectcalico-org/$(CONTAINER_NAME):$(IMAGETAG)
	docker push asia.gcr.io/projectcalico-org/$(CONTAINER_NAME):$(IMAGETAG)
	docker push us.gcr.io/projectcalico-org/$(CONTAINER_NAME):$(IMAGETAG)
endif

## push all archs
push-all: imagetag $(addprefix sub-push-,$(ARCHES))
sub-push-%:
	$(MAKE) push ARCH=$* IMAGETAG=$(IMAGETAG)

## tag images of one arch
tag-images: imagetag
	docker tag $(CONTAINER_NAME):latest-$(ARCH) $(CONTAINER_NAME):$(IMAGETAG)-$(ARCH)
	docker tag $(CONTAINER_NAME):latest-$(ARCH) quay.io/$(CONTAINER_NAME):$(IMAGETAG)-$(ARCH)
	
	# Tag images for gcr.io, used by GKE.
	docker tag $(CONTAINER_NAME):latest-$(ARCH) gcr.io/projectcalico-org/$(CONTAINER_NAME):$(IMAGETAG)
	docker tag $(CONTAINER_NAME):latest-$(ARCH) eu.gcr.io/projectcalico-org/$(CONTAINER_NAME):$(IMAGETAG)
	docker tag $(CONTAINER_NAME):latest-$(ARCH) asia.gcr.io/projectcalico-org/$(CONTAINER_NAME):$(IMAGETAG)
	docker tag $(CONTAINER_NAME):latest-$(ARCH) us.gcr.io/projectcalico-org/$(CONTAINER_NAME):$(IMAGETAG)
ifeq ($(ARCH),amd64)
	docker tag $(CONTAINER_NAME):latest-$(ARCH) $(CONTAINER_NAME):$(IMAGETAG)
	docker tag $(CONTAINER_NAME):latest-$(ARCH) quay.io/$(CONTAINER_NAME):$(IMAGETAG)

	# Tag images for gcr.io, used by GKE.
	docker tag $(CONTAINER_NAME):latest-$(ARCH) gcr.io/projectcalico-org/$(CONTAINER_NAME):$(IMAGETAG)
	docker tag $(CONTAINER_NAME):latest-$(ARCH) eu.gcr.io/projectcalico-org/$(CONTAINER_NAME):$(IMAGETAG)
	docker tag $(CONTAINER_NAME):latest-$(ARCH) asia.gcr.io/projectcalico-org/$(CONTAINER_NAME):$(IMAGETAG)
	docker tag $(CONTAINER_NAME):latest-$(ARCH) us.gcr.io/projectcalico-org/$(CONTAINER_NAME):$(IMAGETAG)
endif

## tag images of all archs
tag-images-all: imagetag $(addprefix sub-tag-images-,$(ARCHES))
sub-tag-images-%:
	$(MAKE) tag-images ARCH=$* IMAGETAG=$(IMAGETAG)

###############################################################################

# Targets used when cross building.
.PHONY: register
# Enable binfmt adding support for miscellaneous binary formats.
# This is only needed when running non-native binaries.
register:
ifneq ($(BUILDARCH),$(ARCH))
	docker run --rm --privileged multiarch/qemu-user-static:register || true
endif


# Figure out version information.  To support builds from release tarballs, we default to
# <unknown> if this isn't a git checkout.
GIT_COMMIT:=$(shell git rev-parse HEAD || echo '<unknown>')
BUILD_ID:=$(shell git rev-parse HEAD || uuidgen | sed 's/-//g')
GIT_DESCRIPTION:=$(shell git describe --tags || echo '<unknown>')

# Calculate a timestamp for any build artefacts.
DATE:=$(shell date -u +'%FT%T%z')

# List of Go files that are generated by the build process.  Builds should
# depend on these, clean removes them.
GENERATED_GO_FILES:=

# All Typha go files.
TYPHA_GO_FILES:=$(shell find . $(foreach dir,$(NON_TYPHA_DIRS),-path ./$(dir) -prune -o) -type f -name '*.go' -print) $(GENERATED_GO_FILES)

# Figure out the users UID/GID.  These are needed to run docker containers
# as the current user and ensure that files built inside containers are
# owned by the current user.
MY_UID:=$(shell id -u)
MY_GID:=$(shell id -g)

# Build the calico/typha docker image, which contains only Typha.
.PHONY: image $(CONTAINER_NAME)
image: $(CONTAINER_NAME)
$(CONTAINER_NAME): bin/calico-typha-$(ARCH)
	rm -rf docker-image/bin
	mkdir -p docker-image/bin
	cp bin/calico-typha-$(ARCH) docker-image/bin/
	docker build --pull -t $(CONTAINER_NAME):latest-$(ARCH) docker-image --file docker-image/Dockerfile.$(ARCH)
ifeq ($(ARCH),amd64)
	docker tag $(CONTAINER_NAME):latest-$(ARCH) $(CONTAINER_NAME):latest
endif

# Pre-configured docker run command that runs as this user with the repo
# checked out to /code, uses the --rm flag to avoid leaving the container
# around afterwards.
DOCKER_RUN_RM:=docker run --rm --user $(MY_UID):$(MY_GID) -v $${PWD}:/code
DOCKER_RUN_RM_ROOT:=docker run --rm -v $${PWD}:/code

# Allow libcalico-go and the ssh auth sock to be mapped into the build container.
ifdef LIBCALICOGO_PATH
  EXTRA_DOCKER_ARGS += -v $(LIBCALICOGO_PATH):/go/src/github.com/projectcalico/libcalico-go:ro
endif
ifdef SSH_AUTH_SOCK
  EXTRA_DOCKER_ARGS += -v $(SSH_AUTH_SOCK):/ssh-agent --env SSH_AUTH_SOCK=/ssh-agent
endif
DOCKER_GO_BUILD := mkdir -p .go-pkg-cache && \
                   docker run --rm \
                              --net=host \
                              $(EXTRA_DOCKER_ARGS) \
                              -e LOCAL_USER_ID=$(MY_UID) \
                              -e GOARCH=$(ARCH) \
                              -v $${PWD}:/go/src/github.com/projectcalico/typha:rw \
                              -v $${PWD}/.go-pkg-cache:/go/pkg:rw \
                              -w /go/src/github.com/projectcalico/typha \
                              $(GO_BUILD_CONTAINER)

# Update the vendored dependencies with the latest upstream versions matching
# our glide.yaml.  If there area any changes, this updates glide.lock
# as a side effect.  Unless you're adding/updating a dependency, you probably
# want to use the vendor target to install the versions from glide.lock.
.PHONY: update-vendor
update-vendor:
	mkdir -p $$HOME/.glide
	$(DOCKER_GO_BUILD) glide up --strip-vendor
	touch vendor/.up-to-date

# vendor is a shortcut for force rebuilding the go vendor directory.
.PHONY: vendor
vendor vendor/.up-to-date: glide.lock
	mkdir -p $$HOME/.glide
	$(DOCKER_GO_BUILD) glide install --strip-vendor
	touch vendor/.up-to-date

# Linker flags for building Typha.
#
# We use -X to insert the version information into the placeholder variables
# in the buildinfo package.
#
# We use -B to insert a build ID note into the executable, without which, the
# RPM build tools complain.
LDFLAGS:=-ldflags "\
        -X github.com/projectcalico/typha/pkg/buildinfo.GitVersion=$(GIT_DESCRIPTION) \
        -X github.com/projectcalico/typha/pkg/buildinfo.BuildDate=$(DATE) \
        -X github.com/projectcalico/typha/pkg/buildinfo.GitRevision=$(GIT_COMMIT) \
        -B 0x$(BUILD_ID)"

.PHONY: build
build: bin/calico-typha
bin/calico-typha: bin/calico-typha-$(ARCH)
bin/calico-typha-$(ARCH): $(TYPHA_GO_FILES) vendor/.up-to-date
	$(MAKE) register
	@echo Building typha...
	mkdir -p bin
	$(DOCKER_GO_BUILD) \
	    sh -c 'go build -v -i -o $@ -v $(LDFLAGS) "github.com/projectcalico/typha/cmd/calico-typha" && \
		( ldd $@ 2>&1 | grep -q -e "Not a valid dynamic program" \
		-e "not a dynamic executable" || \
		( echo "Error: bin/calico-typha was not statically linked"; false ) )'

bin/typha-client-$(ARCH): $(TYPHA_GO_FILES) vendor/.up-to-date
	$(MAKE) register
	@echo Building typha client...
	mkdir -p bin
	$(DOCKER_GO_BUILD) \
	    sh -c 'go build -v -i -o $@ -v $(LDFLAGS) "github.com/projectcalico/typha/cmd/typha-client" && \
		( ldd $@ 2>&1 | grep -q -e "Not a valid dynamic program" \
		-e "not a dynamic executable" || \
		( echo "Error: bin/typha-client was not statically linked"; false ) )'

# Install or update the tools used by the build
.PHONY: update-tools
update-tools:
	go get -u github.com/Masterminds/glide
	go get -u github.com/onsi/ginkgo/ginkgo

# Run go fmt on all our go files.
.PHONY: go-fmt goimports
go-fmt goimports:
	$(DOCKER_GO_BUILD) sh -c 'glide nv -x | \
	                          grep -v -e "^\\.$$" | \
	                          xargs goimports -w -local github.com/projectcalico/'

check-licenses/dependency-licenses.txt: vendor/.up-to-date
	$(DOCKER_GO_BUILD) sh -c 'licenses ./cmd/calico-typha > check-licenses/dependency-licenses.tmp && \
	                          mv check-licenses/dependency-licenses.tmp check-licenses/dependency-licenses.txt'

.PHONY: ut
ut combined.coverprofile: vendor/.up-to-date $(TYPHA_GO_FILES)
	@echo Running Go UTs.
	$(DOCKER_GO_BUILD) ./utils/run-coverage

bin/check-licenses: $(TYPHA_GO_FILES)
	$(DOCKER_GO_BUILD) go build -v -i -o $@ "github.com/projectcalico/typha/check-licenses"

.PHONY: check-licenses
check-licenses: check-licenses/dependency-licenses.txt bin/check-licenses
	@echo Checking dependency licenses
	$(DOCKER_GO_BUILD) bin/check-licenses

.PHONY: go-meta-linter
go-meta-linter: vendor/.up-to-date $(GENERATED_GO_FILES)
	# Run staticcheck stand-alone since gometalinter runs concurrent copies, which
	# uses a lot of RAM.
	$(DOCKER_GO_BUILD) sh -c 'glide nv | xargs -n 3 staticcheck'
	$(DOCKER_GO_BUILD) gometalinter --enable-gc \
	                                --deadline=300s \
	                                --disable-all \
	                                --enable=goimports \
	                                --enable=errcheck \
	                                --vendor ./...

.PHONY: static-checks
static-checks:
	$(MAKE) go-meta-linter check-licenses

.PHONY: ut-no-cover
ut-no-cover: vendor/.up-to-date $(TYPHA_GO_FILES)
	@echo Running Go UTs without coverage.
	$(DOCKER_GO_BUILD) ginkgo -r $(GINKGO_OPTIONS)

.PHONY: ut-watch
ut-watch: vendor/.up-to-date $(TYPHA_GO_FILES)
	@echo Watching go UTs for changes...
	$(DOCKER_GO_BUILD) ginkgo watch -r $(GINKGO_OPTIONS)

# Launch a browser with Go coverage stats for the whole project.
.PHONY: cover-browser
cover-browser: combined.coverprofile
	go tool cover -html="combined.coverprofile"

.PHONY: cover-report
cover-report: combined.coverprofile
	# Print the coverage.  We use sed to remove the verbose prefix and trim down
	# the whitespace.
	@echo
	@echo ======== All coverage =========
	@echo
	@$(DOCKER_GO_BUILD) sh -c 'go tool cover -func combined.coverprofile | \
	                           sed 's=github.com/projectcalico/typha/==' | \
	                           column -t'
	@echo
	@echo ======== Missing coverage only =========
	@echo
	@$(DOCKER_GO_BUILD) sh -c "go tool cover -func combined.coverprofile | \
	                           sed 's=github.com/projectcalico/typha/==' | \
	                           column -t | \
	                           grep -v '100\.0%'"

.PHONY: upload-to-coveralls
upload-to-coveralls: combined.coverprofile
ifndef COVERALLS_REPO_TOKEN
	$(error COVERALLS_REPO_TOKEN is undefined - run using make upload-to-coveralls COVERALLS_REPO_TOKEN=abcd)
endif
	$(DOCKER_GO_BUILD) goveralls -repotoken=$(COVERALLS_REPO_TOKEN) -coverprofile=combined.coverprofile

bin/calico-typha.transfer-url: bin/calico-typha-$(ARCH)
	$(DOCKER_GO_BUILD) sh -c 'curl --upload-file bin/calico-typha-$(ARCH) https://transfer.sh/calico-typha > $@'

.PHONY: clean
clean:
	rm -rf bin \
	       docker-image/bin \
	       build \
	       $(GENERATED_GO_FILES) \
	       .glide \
	       vendor \
	       .go-pkg-cache \
	       check-licenses/dependency-licenses.txt \
	       release-notes-*
	find . -name "*.coverprofile" -type f -delete
	find . -name "coverage.xml" -type f -delete
	find . -name ".coverage" -type f -delete
	find . -name "*.pyc" -type f -delete


###############################################################################
# Release targets
###############################################################################
PREVIOUS_RELEASE=$(shell git describe --tags --abbrev=0)

## Tags and builds a release from start to finish.
release: release-prereqs
	$(MAKE) VERSION=$(VERSION) release-tag
	$(MAKE) VERSION=$(VERSION) release-build
	$(MAKE) VERSION=$(VERSION) release-verify

	@echo ""
	@echo "Release build complete. Next, push the produced images."
	@echo ""
	@echo "  make VERSION=$(VERSION) release-publish"
	@echo ""

## Produces a git tag for the release.
release-tag: release-prereqs release-notes
	git tag $(VERSION) -F release-notes-$(VERSION)
	@echo ""
	@echo "Now you can build the release:"
	@echo ""
	@echo "  make VERSION=$(VERSION) release-build"
	@echo ""

## Produces a clean build of release artifacts at the specified version.
release-build: release-prereqs clean
# Check that the correct code is checked out.
ifneq ($(VERSION), $(GIT_VERSION))
	$(error Attempt to build $(VERSION) from $(GIT_VERSION))
endif
	$(MAKE) image
	$(MAKE) tag-images IMAGETAG=$(VERSION)
	$(MAKE) tag-images IMAGETAG=latest

## Verifies the release artifacts produces by `make release-build` are correct.
release-verify: release-prereqs
	# Check the reported version is correct for each release artifact.
	docker run --rm $(CONTAINER_NAME):$(VERSION)-$(ARCH) calico-typha --version | grep $(VERSION) || ( echo "Reported version:" `docker run --rm $(CONTAINER_NAME):$(VERSION)-$(ARCH) calico-typha --version` "\nExpected version: $(VERSION)" && exit 1 )
	docker run --rm quay.io/$(CONTAINER_NAME):$(VERSION)-$(ARCH) calico-typha --version | grep $(VERSION) || ( echo "Reported version:" `docker run --rm quay.io/$(CONTAINER_NAME):$(VERSION)-$(ARCH) calico-typha --version | grep -x $(VERSION)` "\nExpected version: $(VERSION)" && exit 1 )

	# TODO: Some sort of quick validation of the produced binaries.

## Generates release notes based on commits in this version.
release-notes: release-prereqs
	mkdir -p dist
	echo "# Changelog" > release-notes-$(VERSION)
	echo "" >> release-notes-$(VERSION)
	sh -c "git cherry -v $(PREVIOUS_RELEASE) | cut '-d ' -f 2- | sed 's/^/- /' >> release-notes-$(VERSION)"

## Pushes a github release and release artifacts produced by `make release-build`.
release-publish: release-prereqs
	# Push the git tag.
	git push origin $(VERSION)

	# Push images.
	$(MAKE) push IMAGETAG=$(VERSION) ARCH=$(ARCH)

	@echo "Finalize the GitHub release based on the pushed tag."
	@echo "Attach the $(DIST)/calico-typha-amd64 binary."
	@echo ""
	@echo "  https://github.com/projectcalico/typha/releases/tag/$(VERSION)"
	@echo ""
	@echo "If this is the latest stable release, then run the following to push 'latest' images."
	@echo ""
	@echo "  make VERSION=$(VERSION) release-publish-latest"
	@echo ""

# WARNING: Only run this target if this release is the latest stable release. Do NOT
# run this target for alpha / beta / release candidate builds, or patches to earlier Calico versions.
## Pushes `latest` release images. WARNING: Only run this for latest stable releases.
release-publish-latest: release-prereqs
	# Check latest versions match.
	if ! docker run $(CONTAINER_NAME):latest-$(ARCH) calico-typha --version | grep '$(VERSION)'; then echo "Reported version:" `docker run $(CONTAINER_NAME):latest-$(ARCH) calico-typha --version` "\nExpected version: $(VERSION)"; false; else echo "\nVersion check passed\n"; fi
	if ! docker run quay.io/$(CONTAINER_NAME):latest-$(ARCH) calico-typha --version | grep '$(VERSION)'; then echo "Reported version:" `docker run quay.io/$(CONTAINER_NAME):latest-$(ARCH) calico-typha --version` "\nExpected version: $(VERSION)"; false; else echo "\nVersion check passed\n"; fi

	$(MAKE) push IMAGETAG=latest ARCH=$(ARCH)

# release-prereqs checks that the environment is configured properly to create a release.
release-prereqs:
ifndef VERSION
	$(error VERSION is undefined - run using make release VERSION=vX.Y.Z)
endif
ifeq ($(GIT_COMMIT),<unknown>)
	$(error git commit ID couldn't be determined, releases must be done from a git working copy)
endif
