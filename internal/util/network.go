package util

// ODDK's Docker bridge network. These are the single source of truth for the
// network name and gateway address; every instance's PostgreSQL binds to the
// gateway, helper containers attach to the network, and the CLI prints
// connection strings pointing at the gateway.
const (
	// OddkNetworkName is the name of the custom Docker bridge network.
	OddkNetworkName = "oddk-bridge"
	// GatewayIP is the bridge gateway address that PostgreSQL instances
	// publish their ports on. It is host-local: not reachable from outside
	// the host without a tunnel.
	GatewayIP = "10.88.0.1"
	// OddkSubnet is the bridge network's subnet; GatewayIP lives inside it.
	OddkSubnet = "10.88.0.0/16"
)
