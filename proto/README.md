# mesh proto

Canonical gRPC contract between `mesh-cli` node daemons and `mesh-api`.

- `mesh.proto` — service + message definitions. Package `mesh.v1`.
- `Makefile` — regenerates language stubs.

Stubs are **checked into each language repo** rather than fetched from a
schema registry (locked decision, see `mesh-api/docs/specs/99-open-questions.md`
A.1). Any change to `mesh.proto` must be followed by `make gen` and commits in
both consumer repos.

The `.proto` source lives in this repo (`mesh-cli`) because the node daemon is
the primary gRPC client. `make gen-ts` assumes `mesh-api` is checked out as a
sibling of `mesh-cli` under a shared parent directory.

## Regenerate stubs

```sh
make gen        # Go + TS
make gen-go     # mesh-cli/cmd/mesh/proto/meshv1/
make gen-ts     # mesh-api/src/grpc/proto/ (ts-proto format)
```

Outputs:

- Go → `mesh-cli/cmd/mesh/proto/meshv1/mesh.pb.go`, `mesh_grpc.pb.go`
- TS → `mesh-api/src/grpc/proto/mesh.ts` + `google/protobuf/timestamp.ts`
  (ts-proto output with `importSuffix=.js` so imports resolve under
  `moduleResolution: NodeNext`)

## Tooling versions

Dev machine must have these on `$PATH`:

| Tool | Version | Install |
| --- | --- | --- |
| `protoc` | 25.x or newer | `brew install protobuf` / apt `protobuf-compiler` |
| `protoc-gen-go` | v1.34.x | `go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2` |
| `protoc-gen-go-grpc` | v1.4.x | `go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.4.0` |
| `protoc-gen-ts_proto` | 1.181.x | `pnpm add -D ts-proto@1.181.2` in `mesh-api`, or a global install |

`make gen` assumes the ts-proto plugin resolves via `mesh-api/node_modules/.bin`
— see the Makefile for the exact lookup path.

## Versioning

Package is `mesh.v1`. Breaking changes bump to `mesh.v2` in a new file; we do
not rename or renumber fields in place. Field numbers marked `reserved` in each
message are held for forward evolution — do not reuse them.
