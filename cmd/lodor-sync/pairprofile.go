package main

// MULTI-USER pair-code sign-in. Unlike --login-profile (OAuth password grant),
// --pair-profile signs a profile in with a RomM CLIENT-TOKEN pairing code — the SAME
// exchange the device onboarding uses — so there is NO password and NO typed name. The
// exchanged token's OWNER (resolved from /api/users/me) becomes the profile's
// username+label; the token is stored as a profile host (Hosts[] append, never
// clobbering Hosts[0]), reusing Hosts[0]'s server + device_id. Prints
// "RESULT paired=<0|1> username=<name>". The launcher reads username and activates it.
// CGO-free, stdlib only.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"lodor/clocksync"
	"lodor/config"
	"lodor/romm"
)

func runPairProfile(cfg *config.Config, code string) {
	code = strings.TrimSpace(code)
	if code == "" {
		fmt.Fprintln(os.Stderr, "PAIRFAIL empty pairing code")
		fmt.Println("RESULT paired=0 username=")
		os.Exit(2)
	}
	if len(cfg.Hosts) == 0 {
		fmt.Fprintln(os.Stderr, "PAIRFAIL no host configured")
		fmt.Println("RESULT paired=0 username=")
		os.Exit(2)
	}
	base := cfg.Hosts[0]
	// RTC-less handhelds: set the clock from the server before HTTPS (a wrong clock
	// fails TLS cert validation). No-op when already sane.
	if cerr := clocksync.Ensure(base.URL(), base.SkipTLSVerify()); cerr != nil {
		fmt.Fprintf(os.Stderr, "clocksync: %v\n", cerr)
	}
	// Exchange the pairing code for a client-token (PRE-token: base URL + TLS flag only).
	exch, err := romm.ExchangeToken(base, code)
	if err != nil {
		// lodor#35 parity with --pair: a certificate-verification failure is
		// deterministic and would otherwise scrub to the misleading generic copy.
		// emitCertFail tags the RESULT line (ADDITIVE reason=tls token) so the wizard
		// can offer the skip-verify trust path for profile pairing too; exit 3 — the
		// code was NOT consumed, so the SAME code survives the trust retry.
		if emitCertFail(err, "RESULT paired=0 username= reason=tls") {
			os.Exit(3)
		}
		msg := safeErr(err)
		fmt.Fprintf(os.Stderr, "PAIRFAIL exchange: %s\n", msg)
		fmt.Println("RESULT paired=0 username=")
		if msg == "network error" {
			os.Exit(3)
		}
		os.Exit(4)
	}
	if exch.RawToken == "" {
		fmt.Fprintln(os.Stderr, "PAIRFAIL exchange: empty token")
		fmt.Println("RESULT paired=0 username=")
		os.Exit(4)
	}
	// Validate the fresh token and resolve its OWNER (authoritative — whoever minted
	// the code in their RomM session). That username is the profile.
	th := base
	th.Token = exch.RawToken
	th.Password = ""
	client := romm.NewClient(th, time.Duration(cfg.ApiTimeout.Int())*time.Second)
	if verr := client.ValidateToken(); verr != nil {
		fmt.Fprintf(os.Stderr, "PAIRFAIL validate: %s\n", safeErr(verr))
		fmt.Println("RESULT paired=0 username=")
		os.Exit(4)
	}
	username := ""
	if u, uerr := client.GetCurrentUser(); uerr == nil {
		username = strings.TrimSpace(u.Username)
	}
	if username == "" {
		fmt.Fprintln(os.Stderr, "PAIRFAIL no username from /api/users/me")
		fmt.Println("RESULT paired=0 username=")
		os.Exit(4)
	}
	// Store as a profile keyed by username, reusing Hosts[0]'s device_id (same device).
	// WriteProfileHost appends/updates a host by profile_label; it never writes a password.
	// Register a device under THIS user. RomM devices are per-user; reusing the admin's
	// device_id makes save uploads 404 ("device with ID ... not found"). Best-effort: on
	// failure store empty and let a later sync re-register.
	devName := base.DeviceName
	if devName == "" {
		devName = "Lodor"
	}
	deviceID := ""
	// REUSE an existing same-named lodor device for this user if present. RomM's
	// device register is NOT idempotent, so re-pairs/re-flashes would otherwise
	// multiply devices and fragment saves. Register a new one only when none matches.
	if devs, derr := client.GetDevices(); derr == nil {
		for _, d := range devs {
			if d.Name == devName && (d.Client == "lodor" || d.Client == "") {
				deviceID = d.ID
				break
			}
		}
	}
	if deviceID == "" {
		if dev, derr := client.RegisterDevice(devName); derr == nil {
			deviceID = dev.ID
		} else {
			fmt.Fprintln(os.Stderr, "PAIRWARN device register: "+safeErr(derr))
		}
	}
	if werr := config.WriteProfileHost(username, username, exch.RawToken, deviceID, exch.Scopes); werr != nil {
		fmt.Fprintf(os.Stderr, "PAIRFAIL write: %s\n", safeErr(werr))
		fmt.Println("RESULT paired=0 username=")
		os.Exit(5)
	}
	fmt.Printf("RESULT paired=1 username=%s\n", username)
	os.Exit(0)
}
