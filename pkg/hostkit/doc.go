// Package hostkit is the bridgev2 glue shared by both hosts of the call core (the standalone bridge in
// pkg/connector and the embedded mautrix-jabber host): the host config and its example YAML, the ghost
// intent adapter, room helpers (encryption check, owner read receipt, call power levels), the call
// stack construction, the call bot commands, command spans, field prompts and the MatrixRTC login
// resolver.
//
// It is a host-class package (like pkg/connector): it may import bridgev2, so core packages must never
// import it (internal/importcheck enforces this).
package hostkit
