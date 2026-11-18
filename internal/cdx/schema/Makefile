PROTOC_VERSION := 29.3
PROTOC_GEN_GO_VERSION := v1.36.12

PROTOC_DIR := .protoc
PROTOC := $(PROTOC_DIR)/bin/protoc
PROTOC_GEN_GO := $(shell go env GOPATH)/bin/protoc-gen-go

PROTO_DIR := ../proto

.PHONY: all
.PHONY: clean
.PHONY: distclean
.PHONY: build

all: build

clean:
	@find . -name '*.pb.go' -delete

distclean: clean
	@rm -rf "${PROTOC_DIR}" && \
	rm $(PROTOC_GEN_GO)

$(PROTOC):
	@echo "Installing protoc ${PROTOC_VERSION}..." && \
	mkdir -p "${PROTOC_DIR}" && \
	curl -s -L -o protoc.zip https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-linux-x86_64.zip && \
	unzip protoc.zip -d "${PROTOC_DIR}" > /dev/null && \
	rm protoc.zip

$(PROTOC_GEN_GO):
	@echo "Installing protoc-gen-go ${PROTOC_GEN_GO_VERSION}..." && \
	go install google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION}

build: $(PROTOC) $(PROTOC_GEN_GO)
	@"${PROTOC}" \
	-I"${PROTOC_DIR}/include" \
	--proto_path="${PROTO_DIR}" \
	--go_out=paths=source_relative:. \
	$(PROTO_DIR)/*.proto
