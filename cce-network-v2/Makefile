# Copyright Authors of CCE
# SPDX-License-Identifier: Apache-2.0

##@ Default
all: precheck build postcheck ## Default make target that perform precheck -> build -> postcheck
	@echo "Build finished."

##@ Build, Install and Test
debug: export NOOPT=1 ## Builds CCE by disabling inlining, compiler optimizations and without stripping debug symbols, useful for debugging.
debug: export NOSTRIP=1
debug: all

include Makefile.defs

SUBDIRS := operator cmd plugins

GOFILES_EVAL := $(subst _$(ROOT_DIR)/,,$(shell $(GO_LIST) -find -e ./...))
GOFILES ?= $(GOFILES_EVAL)
GOLANG_SRCFILES := $(shell for pkg in $(subst github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/,,$(GOFILES)); do find $$pkg -name *.go -print; done | grep -v vendor | sort | uniq)

SWAGGER_VERSION := v0.25.0
SWAGGER := $(CONTAINER_ENGINE) run -u $(shell id -u):$(shell id -g) --rm -v $(CURDIR):$(CURDIR) -w $(CURDIR) --entrypoint swagger registry.baidubce.com/cce-plugin-dev/goswagger/swagger:$(SWAGGER_VERSION)

GOTEST_BASE := -test.v -timeout 600s
GOTEST_UNIT_BASE := $(GOTEST_BASE) -check.vv
GOTEST_COVER_OPTS += -coverprofile=coverage.out
BENCH_EVAL := "."
BENCH ?= $(BENCH_EVAL)
BENCHFLAGS_EVAL := -bench=$(BENCH) -run=^$ -benchtime=10s
BENCHFLAGS ?= $(BENCHFLAGS_EVAL)
# Level of logs emitted to console during unit test runs
LOGLEVEL ?= "error"
SKIP_VET ?= "false"
SKIP_KVSTORES ?= "false"
SKIP_K8S_CODE_GEN_CHECK ?= "true"
SKIP_CUSTOMVET_CHECK ?= "false"

JOB_BASE_NAME ?= cce_test

GO_VERSION := $(shell $(GO) version > GO_VERSION && cat GO_VERSION)
GO_MAJOR_AND_MINOR_VERSION := $(shell sed 's/\([0-9]\+\).\([0-9]\+\)\(.[0-9]\+\)\?/\1.\2/' GO_VERSION)
GO_IMAGE_VERSION := $(shell awk -F. '{ z=$$3; if (z == "") z=0; print $$1 "." $$2 "." z}' GO_VERSION)
GO_INSTALLED_MAJOR_AND_MINOR_VERSION := $(shell $(GO) version | sed 's/go version go\([0-9]\+\).\([0-9]\+\)\(.[0-9]\+\)\?.*/\1.\2/')

GO_MODULE_PATH = $(GOPATH)/pkg/mod
TEST_LDFLAGS=-ldflags "-X github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/kvstore.consulDummyAddress=https://consul:8443 \
	-X github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/kvstore.etcdDummyAddress=http://etcd:4002 \
	-X github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/datapath.DatapathSHA256=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

TEST_UNITTEST_LDFLAGS=-ldflags "-X github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/datapath.DatapathSHA256=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

# 生成k8s api
GENERATE_BIN_DIR = $(PWD)/output/bin
GENERATE_BIN_VERSION = v0.25.0
define generate_k8s_api
	$(call go-get-tool,$(GENERATE_BIN_DIR)/defaulter-gen,k8s.io/code-generator/cmd/defaulter-gen@$(GENERATE_BIN_VERSION))
	$(call go-get-tool,$(GENERATE_BIN_DIR)/client-gen,k8s.io/code-generator/cmd/client-gen@$(GENERATE_BIN_VERSION))
	$(call go-get-tool,$(GENERATE_BIN_DIR)/lister-gen,k8s.io/code-generator/cmd/lister-gen@$(GENERATE_BIN_VERSION))
	$(call go-get-tool,$(GENERATE_BIN_DIR)/informer-gen,k8s.io/code-generator/cmd/informer-gen@$(GENERATE_BIN_VERSION))
	$(call go-get-tool,$(GENERATE_BIN_DIR)/deepcopy-gen,k8s.io/code-generator/cmd/deepcopy-gen@$(GENERATE_BIN_VERSION))

	GOBIN=$(GENERATE_BIN_DIR) bash $(PWD)/tools/generate-groups.sh $(1) \
	    $(2) \
	    $(3) \
	    $(4) \
	    --go-header-file "$(PWD)/hack/custom-boilerplate.go.txt" \
	    -o "$(PWD)/output/k8s"
endef

DEEPEQUAL_GEN = $(PWD)/output/bin/deepequal-gen

define generate_deepequal
	$(call go-get-tool,$(DEEPEQUAL_GEN),github.com/cilium/deepequal-gen@v0.0.0-20200406125435-ad6a9003139e)
	$(DEEPEQUAL_GEN) \
	--input-dirs $(1) \
	-O zz_generated.deepequal \
	--go-header-file "$(PWD)/hack/custom-boilerplate.go.txt" \
	-o "$(PWD)/output/deepequal"
endef

define generate_k8s_api_all
	$(call generate_k8s_api,all,github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/k8s/client,$(1),$(2))
	$(call generate_deepequal,"$(call join-with-comma,$(foreach pkg,$(2),$(1)/$(subst ",,$(subst :,/,$(pkg)))))")
endef

define generate_k8s_api_deepcopy_deepequal
	$(call generate_k8s_api,deepcopy,github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/k8s/client,$(1),$(2))
	@# Explanation for the 'subst' below:
	@#   $(subst ",,$(subst :,/,$(pkg))) - replace all ':' with '/' and replace
	@#    all '"' with '' from $pkg
	@#   $(foreach pkg,$(2),$(1)/$(subst ",,$(subst :,/,$(pkg)))) - for each
	@#    "$pkg", with the characters replaced, create a new string with the
	@#    prefix $(1)
	@#   Finally replace all spaces with commas from the generated strings.
	$(call generate_deepequal,"$(call join-with-comma,$(foreach pkg,$(2),$(1)/$(subst ",,$(subst :,/,$(pkg)))))")
endef

define generate_k8s_api_deepcopy_deepequal_client
	$(call generate_k8s_api,deepcopy$(comma)client,k8s.io/$(1),$(2),$(3))
	$(call generate_deepequal,"$(call join-with-comma,$(foreach pkg,$(3),$(2)/$(subst ",,$(subst :,/,$(pkg)))))")
endef

define generate_k8s_protobuf
	PATH="$(PWD)/tools:$(PATH)" ./tools/go-to-protobuf \
		--apimachinery-packages='-k8s.io/apimachinery/pkg/util/intstr,$\
                                -k8s.io/apimachinery/pkg/api/resource,$\
                                -k8s.io/apimachinery/pkg/runtime/schema,$\
                                -k8s.io/apimachinery/pkg/runtime,$\
                                -k8s.io/apimachinery/pkg/apis/meta/v1,$\
                                -k8s.io/apimachinery/pkg/apis/meta/v1beta1'\
		--drop-embedded-fields="k8s.io/apis/meta/v1.TypeMeta" \
		--proto-import="$(PWD)" \
		--proto-import="$(PWD)/vendor" \
		--proto-import="$(PWD)/tools/protobuf" \
		--packages=$(1) \
	    --go-header-file "$(PWD)/hack/custom-boilerplate.go.txt"
endef

MOCKGEN = $(PWD)/output/bin/mockgen
define generate_mock
	# generate_mock pkg/privatecloudbase/api Client
	$(call go-get-tool,$(MOCKGEN),github.com/golang/mock/mockgen@v1.6.0)

	$(MOCKGEN) \
	-copyright_file $(PWD)/hack/custom-boilerplate.go.txt \
    -destination "$(PWD)/$(1)/testing/mock_$(shell basename $(1)).go" \
    -package=testing \
    github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/$(1) $(2)
endef

fmt:
	$(GO) fmt ./...
build: $(SUBDIRS) ## Builds all the components for CCE by executing make in the respective sub directories.

$(SUBDIRS): force fmt ## Execute default make target(make all) for the provided subdirectory.
	@ $(MAKE) $(SUBMAKEOPTS) -C $@ all

# If the developer provides TESTPKGS to filter the set of packages to use for testing, filter that to only packages with privileged tests.
# The period at EOL ensures that if TESTPKGS becomes empty, we can still pass some paths to 'xargs dirname' to avoid errors.
PRIV_TEST_PKGS_FILTER := $(shell for pkg in $(TESTPKGS); do echo $$pkg; done | xargs grep --include='*.go' -ril 'go:build [^!]*privileged_tests') .
PRIV_TEST_PKGS_EVAL := $(shell echo $(PRIV_TEST_PKGS_FILTER) | xargs dirname | sort | uniq | grep -Ev '^\.$$')
PRIV_TEST_PKGS ?= $(PRIV_TEST_PKGS_EVAL)
tests-privileged: GO_TAGS_FLAGS+=privileged_tests ## Run integration-tests for CCE that requires elevated privileges.
tests-privileged:
	$(MAKE) init-coverage
	for pkg in $(patsubst %,github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/%,$(PRIV_TEST_PKGS)); do \
		>&2 $(ECHO_TEST) $$pkg; \
		PATH=$(PATH):$(ROOT_DIR)/bpf $(GO_TEST) $(TEST_LDFLAGS) $$pkg $(GOTEST_UNIT_BASE) $(GOTEST_COVER_OPTS) -coverpkg $$pkg \
		|| exit 1; \
		tail -n +2 coverage.out >> coverage-all-tmp.out; \
	done | $(GOTEST_FORMATTER)
	$(MAKE) generate-cov

generate-cov: ## Generate coverage report for CCE integration-tests.
	# Remove generated code from coverage
	$(QUIET) grep -Ev '(^github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/api/v1)|(generated.deepcopy.go)|(^github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/k8s/client/)' \
		coverage-all-tmp.out > coverage-all.out
	$(QUIET)$(GO) tool cover -html=coverage-all.out -o=coverage-all.html
	$(QUIET) rm coverage.out coverage-all-tmp.out
	@rmdir ./daemon/1 ./daemon/1_backup 2> /dev/null || true

init-coverage: ## Initialize converage report for CCE integration-tests.
	$(QUIET) echo "mode: count" > coverage-all-tmp.out
	$(QUIET) echo "mode: count" > coverage.out

integration-tests: GO_TAGS_FLAGS+=integration_tests
integration-tests: ## Runs all integration-tests for CCE.
ifeq ($(SKIP_VET),"false")
	$(MAKE) govet
endif
	$(MAKE) init-coverage
	# It seems that in some env if the path is large enough for the full list
	# of files, the full bash command in that target gets too big for bash and
	# hence will trigger an error of too many arguments. As a workaround, we
	# have to process these packages in different subshells.
	for pkg in $(patsubst %,github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/%,$(TESTPKGS)); do \
		>&2 $(ECHO_TEST) $$pkg; \
		$(GO_TEST) $(TEST_UNITTEST_LDFLAGS) $$pkg $(GOTEST_BASE) $(GOTEST_COVER_OPTS) -coverpkg $$pkg \
		|| exit 1; \
		tail -n +2 coverage.out >> coverage-all-tmp.out; \
	done | $(GOTEST_FORMATTER)
	$(MAKE) generate-cov
	$(MAKE) stop-kvstores

clean-tags: ## Remove all the tags files from the repository.
	@$(ECHO_CLEAN) tags
	@-rm -f cscope.out cscope.in.out cscope.po.out cscope.files tags

cscope.files: $(GOLANG_SRCFILES) $(BPF_SRCFILES) ## Generate cscope.files with the list of all files to generate ctags for.
	@echo $(GOLANG_SRCFILES) $(BPF_SRCFILES) | sed 's/ /\n/g' | sort > cscope.files

tags: $(GOLANG_SRCFILES) $(BPF_SRCFILES) cscope.files ## Generate tags for Go and BPF source files.
	@ctags $(GOLANG_SRCFILES) $(BPF_SRCFILES)
	cscope -R -b -q

clean: ## Perform overall cleanup for CCE.
	-$(QUIET) for i in $(SUBDIRS); do $(MAKE) $(SUBMAKEOPTS) -C $$i clean; done

veryclean: ## Perform complete cleanup for container engine images(including build cache).
	-$(QUIET) $(CONTAINER_ENGINE) image prune -af
	-$(QUIET) $(CONTAINER_ENGINE) builder prune -af

install:
	$(QUIET)$(INSTALL) -m 0755 -d $(DESTDIR)$(BINDIR)
	for i in $(SUBDIRS); do $(MAKE) $(SUBMAKEOPTS) -C $$i install; done



# Workaround for not having git in the build environment
# Touch the file only if needed
GIT_VERSION: force
	@if [ "$(GIT_VERSION)" != "`cat 2>/dev/null GIT_VERSION`" ] ; then echo "$(GIT_VERSION)" >GIT_VERSION; fi

# DOCKER_IMAGES := cce-network-operator cce-network-operator-vpc-eni cce-network-agent exclusive-rdma-agent
DOCKER_IMAGES := cce-network-operator-vpc-eni cce-network-agent
# target for amd64 architecture docker image
docker: GOARCH := amd64
docker: export GOARCH=amd64
docker: $(DOCKER_IMAGES)
# target for arm64 architecture docker image
docker-arm: GOARCH := arm64
docker-arm: export GOARCH=arm64
docker-arm: image-suffix = -arm64
docker-arm: $(DOCKER_IMAGES)

# use docker build base on profile variable
$(DOCKER_IMAGES): build
# build docker image for amd64 and arm64 when GOARCH is amd64 and arm64
# docker build -t registry.baidubce.com/cce-plugin-$(PROFILE)/$(@):$(VERSION) -f deploy/dockerfile/$(@).Dockerfile . $(PUSH_IMAGE_FLAGS)
# docker buildx build --platform linux/$(GOARCH)  -t registry.baidubce.com/cce-plugin-$(PROFILE)/$(@):$(VERSION)$(image-suffix) -f deploy/dockerfile/$(@).Dockerfile$(image-suffix) . # $(PUSH_IMAGE_FLAGS)
	@echo "Architecture: $(GOARCH)"
# 使用 shell 条件判断 Architecture
	@if [ "$(GOARCH)" = "amd64" ]; then \
		echo "Building for amd64..."; \
		docker build -t registry.baidubce.com/cce-plugin-$(PROFILE)/$(@):$(VERSION) -f deploy/dockerfile/$(@).Dockerfile .; \
		docker push registry.baidubce.com/cce-plugin-$(PROFILE)/$(@):$(VERSION); \
    elif [ "$(GOARCH)" = "arm64" ]; then \
		echo "Building for arm64..."; \
		docker buildx build --platform linux/$(GOARCH)  -t registry.baidubce.com/cce-plugin-$(PROFILE)/$(@):$(VERSION)$(image-suffix) -f deploy/dockerfile/$(@).Dockerfile$(image-suffix) .; \
		docker push registry.baidubce.com/cce-plugin-$(PROFILE)/$(@):$(VERSION)$(image-suffix); \
    else \
		echo "*** Unsupported GOARCH: $(GOARCH) ***"; \
	fi

CONTROLLER_GEN = $(PWD)/output/bin/controller-gen
manifests: ## Generate K8s manifests e.g. CRD, RBAC etc.
	$(eval TMPDIR := $(PWD)/output/manifests)
	$(call go-get-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen@v0.6.2)
	$(CONTROLLER_GEN) rbac:roleName=cce-eni crd:crdVersions=v1 webhook paths="$(PWD)/pkg/k8s/apis/cce.baidubce.com/v2;" \
		output:dir="$(TMPDIR)"
	$(CONTROLLER_GEN) rbac:roleName=cce-eni crd:crdVersions=v1 webhook paths="$(PWD)/pkg/k8s/apis/cce.baidubce.com/v2alpha1;" \
		output:dir="$(TMPDIR)"
	$(CONTROLLER_GEN) rbac:roleName=cce-eni crd:crdVersions=v1 webhook paths="$(PWD)/pkg/k8s/apis/cce.baidubce.com/v1;" \
		output:dir="$(TMPDIR)"	

	# cpoy to helm dir
	cp -rf $(TMPDIR)/* deploy/cce-network-v2/templates/crd/
	rm -rf $(TMPDIR)

generate-api: api/v1/openapi.yaml ## Generate cce-agent client, model and server code from openapi spec.
	echo "gen api/v1/openapi.yaml"
	$(SWAGGER) generate server -s server -a restapi \
		-t api/v1 \
		-f api/v1/openapi.yaml \
		--default-scheme=unix \
		-C api/v1/cce-server.yml \
		-r hack/spdx-copyright-header.txt
	-$(QUIET)$(SWAGGER) generate client -a restapi \
		-t api/v1 \
		-f api/v1/openapi.yaml \
		-r hack/spdx-copyright-header.txt
	@# sort goimports automatically
	-$(QUIET) find api/v1/client/ -type f -name "*.go" -print | PATH="$(PWD)/tools:$(PATH)" xargs goimports -w
	-$(QUIET) find api/v1/models/ -type f -name "*.go" -print | PATH="$(PWD)/tools:$(PATH)" xargs goimports -w
	-$(QUIET) find api/v1/server/ -type f -name "*.go" -print | PATH="$(PWD)/tools:$(PATH)" xargs goimports -w

	## custom external logic
	mv api/v1/models/e_n_i.go api/v1/models/eni.go

generate-health-api: api/v1/health/openapi.yaml ## Generate cce-health client, model and server code from openapi spec.
	@$(ECHO_GEN)api/v1/health/openapi.yaml
	-$(QUIET)$(SWAGGER) generate server -s server -a restapi \
		-t api/v1 \
		-t api/v1/health/ \
		-f api/v1/health/openapi.yaml \
		--default-scheme=unix \
		-C api/v1/cce-server.yml \
		-r hack/spdx-copyright-header.txt
	-$(QUIET)$(SWAGGER) generate client -a restapi \
		-t api/v1 \
		-t api/v1/health/ \
		-f api/v1/health/openapi.yaml \
		-r hack/spdx-copyright-header.txt
	@# sort goimports automatically
	-$(QUIET) find api/v1/health/ -type f -name "*.go" -print | PATH="$(PWD)/tools:$(PATH)" xargs goimports -w

generate-operator-api: api/v1/operator/openapi.yaml ## Generate cce-operator client, model and server code from openapi spec.
	@$(ECHO_GEN)api/v1/operator/openapi.yaml
	-$(QUIET)$(SWAGGER) generate server -s server -a restapi \
		-t api/v1 \
		-t api/v1/operator/ \
		-f api/v1/operator/openapi.yaml \
		--default-scheme=http \
		-C api/v1/cce-server.yml \
		-r hack/spdx-copyright-header.txt
	-$(QUIET)$(SWAGGER) generate client -a restapi \
		-t api/v1 \
		-t api/v1/operator/ \
		-f api/v1/operator/openapi.yaml \
		-r hack/spdx-copyright-header.txt
	@# sort goimports automatically
	-$(QUIET) find api/v1/operator/ -type f -name "*.go" -print | PATH="$(PWD)/tools:$(PATH)" xargs goimports -w

# 生成k8s api
base_package = github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2
generate-k8s-api: ## Generate CCE k8s API client, deepcopy and deepequal Go sources.
	$(call generate_k8s_api_deepcopy_deepequal,$(base_package)/pkg,"$\
	ipam:types \
	privatecloudbase:api \
	bce:api")
	$(call generate_k8s_api_deepcopy_deepequal,$(base_package)/api,"$\
	v1:models")
	
	$(call generate_k8s_api_all,$(base_package)/pkg/k8s/apis,"cce.baidubce.com:v2 cce.baidubce.com:v2alpha1 cce.baidubce.com:v1")
	$(call generate_k8s_api_deepcopy_deepequal,$(base_package),"$\
	pkg:k8s")
	cp -rf $(PWD)/output/deepequal/$(base_package)/* $(PWD)/
	cp -rf $(PWD)/output/k8s/$(base_package)/* $(PWD)/
	rm -rf /output/deepequal $(PWD)/output/k8s

generate-mock:
	# $(call generate_mock,pkg/privatecloudbase/api,Client)
	$(call generate_mock,pkg/bce/api/cloud,Interface)


release: ## Perform a Git release for CCE.
	$(eval TAG_VERSION := $(shell git tag | grep v$(VERSION) > /dev/null; echo $$?))
	$(eval BRANCH := $(shell git rev-parse --abbrev-ref HEAD))
	$(info Checking if tag $(VERSION) is created '$(TAG_VERSION)' $(BRANCH))

	@if [ "$(TAG_VERSION)" -eq "0" ];then { echo Git tag v$(VERSION) is already created; exit 1; } fi
	$(MAKE) -C ./contrib/packaging/deb release
	git commit -m "Version $(VERSION)"
	git tag v$(VERSION)
	git archive --format tar $(BRANCH) | gzip > ../cce_$(VERSION).orig.tar.gz

gofmt: ## Run gofmt on Go source files in the repository.
	$(QUIET)for pkg in $(GOFILES); do $(GO) fmt $$pkg; done

govet: ## Run govet on Go source files in the repository.
	@$(ECHO_CHECK) vetting all GOFILES...
	$(QUIET) $(GO_VET) \
    ./api/... \
    ./bugtool/... \
    ./cce/... \
    ./cce-health/... \
    ./cmd/... \
    ./operator/... \
    ./pkg/... \
    ./plugins/... \
    ./proxylib/... \
    ./test/. \
    ./test/config/... \
    ./test/ginkgo-ext/... \
    ./test/helpers/... \
    ./test/runtime/... \
    ./test/k8s/... \
    ./tools/...

golangci-lint: ## Run golangci-lint
ifneq (,$(findstring $(GOLANGCILINT_WANT_VERSION),$(GOLANGCILINT_VERSION)))
	@$(ECHO_CHECK) golangci-lint
	$(QUIET) golangci-lint run
else
	$(QUIET) $(CONTAINER_ENGINE) run --rm -v `pwd`:/app -w /app docker.io/golangci/golangci-lint:v$(GOLANGCILINT_WANT_VERSION)@$(GOLANGCILINT_IMAGE_SHA) golangci-lint run
endif


lint: golangci-lint bpf-mock-lint ## Run golangci-lint and bpf-mock linters.

logging-subsys-field: ## Validate logrus subsystem field for logs in Go source code.
	@$(ECHO_CHECK) contrib/scripts/check-logging-subsys-field.sh
	$(QUIET) contrib/scripts/check-logging-subsys-field.sh

check-microk8s: ## Validate if microk8s is ready to install cce.
	@$(ECHO_CHECK) microk8s is ready...
	$(QUIET)microk8s.status >/dev/null \
		|| (echo "Error: Microk8s is not running" && exit 1)

LOCAL_IMAGE_TAG=local
microk8s: export DOCKER_REGISTRY=localhost:32000
microk8s: export LOCAL_AGENT_IMAGE=$(DOCKER_REGISTRY)/$(DOCKER_DEV_ACCOUNT)/cce-dev:$(LOCAL_IMAGE_TAG)
microk8s: export LOCAL_OPERATOR_IMAGE=$(DOCKER_REGISTRY)/$(DOCKER_DEV_ACCOUNT)/operator:$(LOCAL_IMAGE_TAG)
microk8s: check-microk8s ## Build cce-dev docker image and import to microk8s
	$(QUIET)$(MAKE) dev-docker-image DOCKER_IMAGE_TAG=$(LOCAL_IMAGE_TAG)
	@echo "  DEPLOY image to microk8s ($(LOCAL_AGENT_IMAGE))"
	$(QUIET)./contrib/scripts/microk8s-import.sh $(LOCAL_AGENT_IMAGE)
	$(QUIET)$(MAKE) dev-docker-operator-image DOCKER_IMAGE_TAG=$(LOCAL_IMAGE_TAG)
	@echo "  DEPLOY image to microk8s ($(LOCAL_OPERATOR_IMAGE))"
	$(QUIET)./contrib/scripts/microk8s-import.sh $(LOCAL_OPERATOR_IMAGE)

kind: ## Create a kind cluster for CCE development.
	$(QUIET)./contrib/scripts/kind.sh

kind-down: ## Destroy a kind cluster for CCE development.
	$(QUIET)./contrib/scripts/kind-down.sh

kind-image: export DOCKER_REGISTRY=localhost:5000
kind-image: export LOCAL_AGENT_IMAGE=$(DOCKER_REGISTRY)/$(DOCKER_DEV_ACCOUNT)/cce-dev:$(LOCAL_IMAGE_TAG)
kind-image: export LOCAL_OPERATOR_IMAGE=$(DOCKER_REGISTRY)/$(DOCKER_DEV_ACCOUNT)/operator:$(LOCAL_IMAGE_TAG)
kind-image: ## Build cce-dev docker image and import it into kind.
	@$(ECHO_CHECK) kind is ready...
	@kind get clusters >/dev/null
	$(QUIET)$(MAKE) dev-docker-image DOCKER_IMAGE_TAG=$(LOCAL_IMAGE_TAG)
	@echo "  DEPLOY image to kind ($(LOCAL_AGENT_IMAGE))"
	$(QUIET)$(CONTAINER_ENGINE) push $(LOCAL_AGENT_IMAGE)
	$(QUIET)kind load docker-image $(LOCAL_AGENT_IMAGE)
	$(QUIET)$(MAKE) dev-docker-operator-image DOCKER_IMAGE_TAG=$(LOCAL_IMAGE_TAG)
	@echo "  DEPLOY image to kind ($(LOCAL_OPERATOR_IMAGE))"
	$(QUIET)$(CONTAINER_ENGINE) push $(LOCAL_OPERATOR_IMAGE)
	$(QUIET)kind load docker-image $(LOCAL_OPERATOR_IMAGE)

precheck: check-go-version logging-subsys-field ## Peform build precheck for the source code.
ifeq ($(SKIP_K8S_CODE_GEN_CHECK),"false")
	@$(ECHO_CHECK) contrib/scripts/check-k8s-code-gen.sh
	$(QUIET) contrib/scripts/check-k8s-code-gen.sh
endif
	@$(ECHO_CHECK) contrib/scripts/check-fmt.sh
	$(QUIET) contrib/scripts/check-fmt.sh
	@$(ECHO_CHECK) contrib/scripts/check-log-newlines.sh
	$(QUIET) contrib/scripts/check-log-newlines.sh
	@$(ECHO_CHECK) contrib/scripts/check-missing-tags-in-tests.sh
	$(QUIET) contrib/scripts/check-missing-tags-in-tests.sh
	@$(ECHO_CHECK) contrib/scripts/check-assert-deep-equals.sh
	$(QUIET) contrib/scripts/check-assert-deep-equals.sh
	@$(ECHO_CHECK) contrib/scripts/lock-check.sh
	$(QUIET) contrib/scripts/lock-check.sh
	@$(ECHO_CHECK) contrib/scripts/check-viper-get-string-map-string.sh
	$(QUIET) contrib/scripts/check-viper-get-string-map-string.sh
ifeq ($(SKIP_CUSTOMVET_CHECK),"false")
	@$(ECHO_CHECK) contrib/scripts/custom-vet-check.sh
	$(QUIET) contrib/scripts/custom-vet-check.sh
endif
	@$(ECHO_CHECK) contrib/scripts/rand-check.sh
	$(QUIET) contrib/scripts/rand-check.sh

pprof-heap: ## Get Go pprof heap profile.
	$(QUIET)$(GO) tool pprof http://localhost:6060/debug/pprof/heap

pprof-profile: ## Get Go pprof profile.
	$(QUIET)$(GO) tool pprof http://localhost:6060/debug/pprof/profile

pprof-block: ## Get Go pprof block profile.
	$(QUIET)$(GO) tool pprof http://localhost:6060/debug/pprof/block

pprof-trace-5s: ## Get Go pprof trace for a duration of 5 seconds.
	curl http://localhost:6060/debug/pprof/trace?seconds=5

pprof-mutex: ## Get Go pprof mutex profile.
	$(QUIET)$(GO) tool pprof http://localhost:6060/debug/pprof/mutex

update-authors: ## Update AUTHORS file for CCE repository.
	@echo "Updating AUTHORS file..."
	@echo "The following people, in alphabetical order, have either authored or signed" > AUTHORS
	@echo "off on commits in the CCE repository:" >> AUTHORS
	@echo "" >> AUTHORS
	@contrib/scripts/extract_authors.sh >> AUTHORS
	@cat .authors.aux >> AUTHORS

test-docs: ## Build HTML documentation.
	$(MAKE) -C Documentation html

render-docs: ## Run server with live preview to render documentation.
	$(MAKE) -C Documentation live-preview

manpages: ## Generate manpage for CCE CLI.
	-rm -r man
	mkdir -p man
	cce cmdman -d man

install-manpages: ## Install manpages the CCE CLI.
	cp man/* /usr/local/share/man/man1/
	mandb

postcheck: build ## Run CCE build postcheck (update-cmdref, build documentation etc.).
	$(QUIET)$(MAKE) $(SUBMAKEOPTS) -C Documentation check

licenses-all: ## Generate file with all the License from dependencies.
	@$(GO) run ./tools/licensegen > LICENSE.all || ( rm -f LICENSE.all ; false )

check-go-version: ## Check locally install Go version against required Go version.
ifneq ($(GO_MAJOR_AND_MINOR_VERSION),$(GO_INSTALLED_MAJOR_AND_MINOR_VERSION))
	@echo "Installed Go version $(GO_INSTALLED_MAJOR_AND_MINOR_VERSION) does not match requested Go version $(GO_MAJOR_AND_MINOR_VERSION)"
	@exit 1
else
	@$(ECHO_CHECK) "Installed Go version $(GO_INSTALLED_MAJOR_AND_MINOR_VERSION) matches required version $(GO_MAJOR_AND_MINOR_VERSION)"
endif

update-go-version: ## Update Go version for all the components (images, CI, dev-doctor etc.).
	# Update dev-doctor Go version.
	$(QUIET) sed -i 's/^const minGoVersionStr = ".*"/const minGoVersionStr = "$(GO_MAJOR_AND_MINOR_VERSION)"/' tools/dev-doctor/config.go
	@echo "Updated go version in tools/dev-doctor to $(GO_MAJOR_AND_MINOR_VERSION)"
	# Update Go version in GitHub action config.
	$(QUIET) for fl in $(shell find .github/workflows -name "*.yaml" -print) ; do sed -i 's/go-version: .*/go-version: $(GO_IMAGE_VERSION)/g' $$fl ; done
	@echo "Updated go version in GitHub Actions to $(GO_IMAGE_VERSION)"
	# Update Go version in main.go.
	$(QUIET) for fl in $(shell find .  -name main.go -not -path "./vendor/*" -print); do \
		sed -i \
			-e 's|^//go:build go.*|//go:build go$(GO_MAJOR_AND_MINOR_VERSION)|g' \
			-e 's|^// +build go.*|// +build go$(GO_MAJOR_AND_MINOR_VERSION)|g' \
			$$fl ; \
	done
	# Update Go version in Travis CI config.
	$(QUIET) sed -i 's/go: ".*/go: "$(GO_VERSION)"/g' .travis.yml
	@echo "Updated go version in .travis.yml to $(GO_VERSION)"
	# Update Go version in test scripts.
	$(QUIET) sed -i 's/GO_VERSION=.*/GO_VERSION="$(GO_VERSION)"/g' test/kubernetes-test.sh
	$(QUIET) sed -i 's/GOLANG_VERSION=.*/GOLANG_VERSION="$(GO_VERSION)"/g' test/packet/scripts/install.sh
	@echo "Updated go version in test scripts to $(GO_VERSION)"
	# Update Go version in Dockerfiles.
	$(QUIET) sed -i 's/^go_version=.*/go_version=$(GO_IMAGE_VERSION)/g' images/scripts/update-golang-image.sh
	$(QUIET) $(MAKE) -C images update-golang-image
	@echo "Updated go version in image Dockerfiles to $(GO_IMAGE_VERSION)"

dev-doctor: ## Run CCE dev-doctor to validate local development environment.
	$(QUIET)$(GO) version 2>/dev/null || ( echo "go not found, see https://golang.org/doc/install" ; false )
	$(QUIET)$(GO) run ./tools/dev-doctor

help: ## Display help for the Makefile, from https://www.thapaliya.com/en/writings/well-documented-makefiles/.
	$(call print_help_from_makefile)
	@# There is also a list of target we have to manually put the information about.
	@# These are templated targets.
	$(call print_help_line,"docker-cce-image","Build cce-agent docker image")
	$(call print_help_line,"dev-docker-image","Build cce-agent development docker image")
	$(call print_help_line,"docker-plugin-image","Build cce-docker plugin image")
	$(call print_help_line,"docker-hubble-relay-image","Build hubble-relay docker image")
	$(call print_help_line,"docker-clustermesh-apiserver-image","Build docker image for CCE clustermesh APIServer")
	$(call print_help_line,"docker-operator-image","Build cce-operator docker image")
	$(call print_help_line,"docker-operator-*-image","Build platform specific cce-operator images(alibabacloud, aws, azure, generic)")
	$(call print_help_line,"docker-*-image-unstripped","Build unstripped version of above docker images(cce, hubble-relay, operator etc.)")

.PHONY: help clean clean-container dev-doctor force generate-api generate-health-api generate-operator-api generate-hubble-api install licenses-all veryclean
force :;

define go-get-tool
@[ -f $(1) ] || { \
set -e ;\
TMP_DIR=$$(mktemp -d) ;\
cd $$TMP_DIR ;\
go mod init tmp ;\
echo "Downloading $(2)" ;\
GOBIN=$(PWD)/output/bin go install $(2) ;\
rm -rf $$TMP_DIR ;\
}
endef

# 生成 docker 构建时的相关变量
define docker-build-env
ifeq ($(GOARCH), arm64)
	imageTag := $(VERSION)-$(GOARCH)
	dockerFile := deploy/dockerfile/$(@).Dockerfile-$(GOARCH)
else  ifeq ($(GOARCH), amd64)
	imageTag := $(VERSION)	
	dockerFile := deploy/dockerfile/$(@).Dockerfile
endif
endef