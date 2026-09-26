package gateway

// The client's field selections are generated from the committed
// schema.json and the allow-lists under fields/ (docs/adr/adr-010-generated-gateway-fields.md).
// To refresh schema.json from the gateway (the schema is public, no token):
//
//	go run ./internal/genfields -refresh && go generate .
//
//go:generate go run ./internal/genfields
