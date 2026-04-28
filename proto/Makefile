# Regenerates gRPC stubs from mesh.proto.
# Run from this directory: `make gen` (both) / `make gen-go` / `make gen-ts`.
#
# Layout assumed by `make gen-ts`: mesh-cli and mesh-api are sibling checkouts
# under the same parent directory. Stubs are checked in per language; schema
# registries are intentionally not used.

PROTO_DIR   := $(abspath .)
MESH_CLI    := $(abspath ..)
WORKSPACE   := $(abspath ../..)
PROTO_FILE  := mesh.proto

GO_OUT      := $(MESH_CLI)/proto/meshv1
TS_OUT      := $(WORKSPACE)/mesh-api/src/grpc/proto
TS_PROTO    := $(WORKSPACE)/mesh-api/node_modules/.bin/protoc-gen-ts_proto

.PHONY: gen gen-go gen-ts clean help

help:
	@echo "targets:"
	@echo "  gen      regenerate Go + TS stubs"
	@echo "  gen-go   regenerate Go stubs into $(GO_OUT)"
	@echo "  gen-ts   regenerate TS stubs into $(TS_OUT) (requires mesh-api as a sibling checkout)"
	@echo "  clean    remove generated stubs in both repos"

gen: gen-go gen-ts

gen-go:
	@mkdir -p $(GO_OUT)
	protoc \
		--proto_path=$(PROTO_DIR) \
		--go_out=$(GO_OUT) \
		--go_opt=paths=source_relative \
		--go-grpc_out=$(GO_OUT) \
		--go-grpc_opt=paths=source_relative \
		$(PROTO_FILE)

gen-ts:
	@test -d $(WORKSPACE)/mesh-api || { \
		echo "mesh-api not found at $(WORKSPACE)/mesh-api — clone it as a sibling of mesh-cli first."; \
		exit 1; \
	}
	@mkdir -p $(TS_OUT)
	@test -x $(TS_PROTO) || { \
		echo "protoc-gen-ts_proto not found at $(TS_PROTO)"; \
		echo "run: (cd $(WORKSPACE)/mesh-api && pnpm add -D ts-proto@1.181.2)"; \
		exit 1; \
	}
	protoc \
		--proto_path=$(PROTO_DIR) \
		--plugin=protoc-gen-ts_proto=$(TS_PROTO) \
		--ts_proto_out=$(TS_OUT) \
		--ts_proto_opt=outputServices=grpc-js,esModuleInterop=true,useOptionals=messages,useExactTypes=false,importSuffix=.js \
		$(PROTO_FILE)

clean:
	rm -rf $(GO_OUT) $(TS_OUT)
