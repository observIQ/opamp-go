package types

import "github.com/open-telemetry/opamp-go/protobufs"

type Agent struct {
	// Agent information.
	InstanceUid InstanceUid

	// Previously saved state. These will be reported to the Server immediately
	// after the connection is established.

	// The remote config status. If nil is passed it will force
	// the Server to send a remote config back.
	RemoteConfigStatus *protobufs.RemoteConfigStatus

	// Defines the capabilities of the Agent. AgentCapabilities_ReportsStatus bit does not need to
	// be set in this field, it will be set automatically since it is required by OpAMP protocol.
	Capabilities protobufs.AgentCapabilities
	// PackagesStateProvider provides access to the local state of packages.
	// If nil then ReportsPackageStatuses and AcceptsPackages capabilities will be disabled,
	// i.e. package status reporting and syncing from the Server will be disabled.
	PackagesStateProvider PackagesStateProvider

	AgentDescription *protobufs.AgentDescription
}
