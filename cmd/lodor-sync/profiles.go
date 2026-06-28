package main

// MULTI-USER profile management — the engine half of the launcher's Switch Profile /
// Add Profile UI (lodor-multiuser). All four modes are OFFLINE config.json edits: no
// host, no network, no device_id, so main dispatches them BEFORE the hosts gate.
//
// Identity model (config.go): hosts[0] is the SHARED family server; profiles[] each
// carry one user's token + server-registered device_id; active_profile (or the
// LODOR_PROFILE env) selects which one ActiveHost() overlays. Adding a profile here only
// creates the entry + marks it active; the launcher then runs the normal --pair and
// --register-device with LODOR_PROFILE set, so writeIdentity routes those onto the new
// profile. Switching just rewrites active_profile.
//
// SECURITY (HARD): no token/device_id ever reaches stdout. --list-profiles prints only
// the LABEL and boolean has-token/has-device flags (never the secrets themselves).

import (
	"fmt"
	"os"
	"strings"

	"lodor/config"
)

// runListProfiles prints one line per configured profile:
//
//	<active?0|1>\t<label>\t<has_token 0|1>\t<has_device 0|1>
//
// Secrets are NEVER printed — only presence flags. Always exits 0 (a config with no
// profiles prints nothing, which the launcher reads as the single-user case).
func runListProfiles(cfg *config.Config) {
	active := cfg.ActiveProfileName()
	for i := range cfg.Profiles {
		p := cfg.Profiles[i]
		isActive := 0
		if active != "" && strings.EqualFold(strings.TrimSpace(p.Label), active) {
			isActive = 1
		}
		fmt.Printf("%d\t%s\t%d\t%d\n", isActive, p.Label, b2i(p.Token != ""), b2i(p.DeviceID != ""))
	}
	os.Exit(0)
}

// runSwitchProfile sets active_profile to label, which MUST already exist in profiles[]
// (you switch to a profile you added; you don't create one by switching). Contract:
// RESULT switched=<0|1>. Exit: 2 unknown/empty label · 4 write failed · 0 switched.
func runSwitchProfile(cfg *config.Config, label string) {
	label = strings.TrimSpace(label)
	if label == "" {
		fmt.Fprintln(os.Stderr, "SWITCHFAIL empty profile label")
		fmt.Println("RESULT switched=0")
		os.Exit(2)
	}
	if !profileExists(cfg, label) {
		fmt.Fprintln(os.Stderr, "SWITCHFAIL unknown profile (add it first)")
		fmt.Println("RESULT switched=0")
		os.Exit(2)
	}
	if err := config.SetActiveProfile(label); err != nil {
		fmt.Fprintf(os.Stderr, "SWITCHFAIL write: %s\n", safeErr(err))
		fmt.Println("RESULT switched=0")
		os.Exit(4)
	}
	fmt.Println("RESULT switched=1")
	os.Exit(0)
}

// runAddProfile creates the profile entry for label (idempotent — an existing label is
// reused, not duplicated) and marks it active, so the launcher's next --pair and
// --register-device (run with LODOR_PROFILE=<label>) land their token + device_id on
// THIS profile. Contract: RESULT added=<0|1>. Exit: 2 empty label · 4 write failed ·
// 0 added. No token is minted here — that is the pairing step the launcher runs next.
func runAddProfile(cfg *config.Config, label string) {
	label = strings.TrimSpace(label)
	if label == "" {
		fmt.Fprintln(os.Stderr, "ADDFAIL empty profile label")
		fmt.Println("RESULT added=0")
		os.Exit(2)
	}
	// Create/ensure the profile entry (WriteProfileUpdate with an empty update just
	// guarantees the labeled entry exists) and mark it active.
	if err := config.WriteProfileUpdate(label, config.HostUpdate{}); err != nil {
		fmt.Fprintf(os.Stderr, "ADDFAIL write: %s\n", safeErr(err))
		fmt.Println("RESULT added=0")
		os.Exit(4)
	}
	if err := config.SetActiveProfile(label); err != nil {
		fmt.Fprintf(os.Stderr, "ADDFAIL activate: %s\n", safeErr(err))
		fmt.Println("RESULT added=0")
		os.Exit(4)
	}
	fmt.Println("RESULT added=1")
	os.Exit(0)
}

// runRemoveProfile deletes the profile and clears active_profile when it pointed at it.
// Contract: RESULT removed=<0|1>. Exit: 2 empty label · 4 write failed · 0 removed
// (idempotent — removing an absent label still succeeds, so the UI never wedges).
func runRemoveProfile(cfg *config.Config, label string) {
	label = strings.TrimSpace(label)
	if label == "" {
		fmt.Fprintln(os.Stderr, "REMOVEFAIL empty profile label")
		fmt.Println("RESULT removed=0")
		os.Exit(2)
	}
	if err := config.RemoveProfile(label); err != nil {
		fmt.Fprintf(os.Stderr, "REMOVEFAIL write: %s\n", safeErr(err))
		fmt.Println("RESULT removed=0")
		os.Exit(4)
	}
	fmt.Println("RESULT removed=1")
	os.Exit(0)
}

// profileExists reports whether a profile with the given label (case-insensitive) is
// configured.
func profileExists(cfg *config.Config, label string) bool {
	want := strings.ToLower(strings.TrimSpace(label))
	for i := range cfg.Profiles {
		if strings.ToLower(strings.TrimSpace(cfg.Profiles[i].Label)) == want {
			return true
		}
	}
	return false
}
