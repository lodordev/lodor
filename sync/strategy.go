package sync

import "lodor/romm"

// Strategy is the save-sync execution path chosen for a server, gated on its
// Capabilities (research #2/#3).
type Strategy int

const (
	// StrategyLegacy: the client reconciles per-rom (the anchor 3-way reconciler, or
	// newest-wins on a server that exposes no per-save content_hash). The only path on
	// servers older than the negotiate cutoff; always available, needs no server support.
	StrategyLegacy Strategy = iota
	// StrategyNegotiate: the server computes the whole-library sync plan via
	// POST /api/sync/negotiate (RomM >= 4.9.0). The device executes the returned plan.
	StrategyNegotiate
)

// String renders a Strategy as a short stable token.
func (s Strategy) String() string {
	switch s {
	case StrategyNegotiate:
		return "negotiate"
	default:
		return "legacy"
	}
}

// SelectStrategy picks the sync strategy from server capabilities: negotiate when the
// server supports it, legacy otherwise. Conservative by construction — an unknown
// server version leaves SupportsSyncNegotiate false and falls back to legacy.
func SelectStrategy(caps romm.Capabilities) Strategy {
	if caps.SupportsSyncNegotiate {
		return StrategyNegotiate
	}
	return StrategyLegacy
}
