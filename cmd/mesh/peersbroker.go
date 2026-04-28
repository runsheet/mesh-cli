package main

// This file is a thin compatibility shim. All peers-broker logic now lives in
// internal/peers; the aliases and wrappers below let the rest of cmd/mesh/
// continue to use the old unexported names without change until Phase 7
// rewires callers to use parameter-threaded broker instances.

import "projectmesh.io/mesh-cli/internal/peers"

type peerRegistration = peers.Registration
type peersBroker = peers.Broker

func newPeersBroker() *peersBroker { return peers.New() }

// localBroker is the singleton broker shared across all agent routers in the
// daemon process. Initialised once at daemon startup.
//
// TODO(spec22-phase7): replace with parameter threading.
var localBroker = peers.New()

// globalPeersBroker aliases localBroker so existing call sites need no change
// until Phase 7.
var globalPeersBroker = localBroker
