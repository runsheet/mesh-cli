package gateway

// HTTPError is httpError for the external tests, so errors.As can show
// a retried answer's status still reaches the caller.
type HTTPError = httpError
