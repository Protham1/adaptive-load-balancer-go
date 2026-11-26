package policy

// Wire names are the strategy identifiers used in the Go<->Python JSON contract
// (schema v1). They are snake_case, stable, and separate from the display names
// returned by SelectionStrategy.Name(). Unknown names must be rejected.
var wireNames = [NumActions]string{
	ActionRoundRobin:       "round_robin",
	ActionLeastConnections: "least_connections",
	ActionStaticSWRR:       "static_swrr",
	ActionConsistentHash:   "consistent_hash",
	ActionRuleAdaptive:     "rule_based_adaptive",
}

// WireName returns the contract name of the action.
func (a ActionID) WireName() (string, bool) {
	if a < 0 || int(a) >= NumActions {
		return "", false
	}
	return wireNames[a], true
}

// ActionFromWire resolves a contract strategy name. Unknown names return false.
func ActionFromWire(name string) (ActionID, bool) {
	for i, n := range wireNames {
		if n == name {
			return ActionID(i), true
		}
	}
	return -1, false
}

// WireNames returns all contract strategy names in action-ID order.
func WireNames() []string {
	out := make([]string, NumActions)
	copy(out, wireNames[:])
	return out
}
