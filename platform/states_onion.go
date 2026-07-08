//go:build onion

package platform

// OnionOS: state sync lands in a later pass (design ledger step 6+) — the RA
// states path on Onion needs a source check before we hardcode it. Empty root
// = every state mode no-ops honestly on this tag.
func stateRootDefault() string { return "" }

const stateNamingRA = true
