package version

// Version is the build-time version string. Default "dev" for local
// builds; CI overrides via -ldflags "-X projectmesh.io/mesh-cli/internal/version.Version=vX.Y.Z".
var Version = "dev"
