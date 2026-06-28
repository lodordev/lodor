package main

// Onboarding CLI modes — the engine half of the native first-run wizard
// (lodor-onboarding-design.md). Each shells out to the romm pairing primitives and
// the config writer, then prints ONLY its RESULT line (BLUEPRINT §8 style) so the
// native launcher can parse it byte-for-byte.
//
// Exit codes (per §8): 0 ok · 2 config/flag · 3 unreachable · 4 ran-but-errored.
//
// SECURITY (HARD): no token, host, or device_id ever reaches stdout/stderr/a file.
// The RESULT lines carry only 0|1 flags; diagnostics go to stderr through safeErr
// and name only the failing step or the (advisory) missing scopes — never a secret.

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"lodor/config"
	"lodor/romm"
)

// writeIdentity routes a per-user identity write (token/device_id/device_name/
// username) to the ACTIVE profile when one is selected (multi-user), else to
// hosts[0]. The server-address fields are NEVER per-user, so those still go through
// WriteHostUpdate/SetServer directly. This is the single seam that makes the existing
// pair/register/rename modes profile-aware without changing their RESULT contract.
func writeIdentity(cfg *config.Config, u config.HostUpdate) error {
	if name := cfg.ActiveProfileName(); name != "" {
		return config.WriteProfileUpdate(name, u)
	}
	return config.WriteHostUpdate(u)
}

// runSetServer persists the wizard's server URL (scheme+host), optional port, and
// the HTTPS skip-verify toggle to config.json BEFORE pairing — the engine's --pair
// reads root_uri from the config, so the address must land first. It creates
// config.json when absent (the fresh, config-less device case) by leaning on the
// writer's missing-file repair. Contract:
//
//	RESULT server_set=<0|1>
//
// Exit: 2 bad/empty URL (a flag error the wizard re-prompts on) · 4 write failed ·
// 0 written. NOTHING about the URL/host is ever echoed (security gate): a parse
// failure reports only "invalid url", and a write failure is scrubbed by safeErr.
//
// This mode does NOT touch the network — it only writes config. The wizard runs
// --validate next to test reachability, so set-server stays fast and offline-safe.
func runSetServer(rawURL string, port int, insecure bool) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		fmt.Fprintln(os.Stderr, "SETSERVERFAIL empty url")
		fmt.Println("RESULT server_set=0")
		os.Exit(2)
	}

	// Require an explicit, supported scheme and a non-empty host. The wizard always
	// prepends http:// or https:// (the protocol toggle), so a missing/odd scheme here
	// means a malformed call — reject it rather than silently persisting garbage that
	// --pair would then fail on cryptically. Host-free diagnostics only.
	u, perr := url.Parse(rawURL)
	if perr != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		fmt.Fprintln(os.Stderr, "SETSERVERFAIL invalid url")
		fmt.Println("RESULT server_set=0")
		os.Exit(2)
	}
	if port < 0 || port > 65535 {
		fmt.Fprintln(os.Stderr, "SETSERVERFAIL invalid port "+strconv.Itoa(port))
		fmt.Println("RESULT server_set=0")
		os.Exit(2)
	}

	// Normalize: store scheme://host[/path] without a trailing slash (URL() also trims,
	// but keeping the stored value clean avoids "//api" surprises). Drop any port that
	// rode in on the URL itself in favor of the explicit --port flag when one is given;
	// otherwise leave the URL as-is (host may legitimately carry no port).
	rootURI := strings.TrimSuffix(rawURL, "/")

	upd := config.HostUpdate{}
	upd.SetServer(rootURI, port, insecure)
	if werr := config.WriteHostUpdate(upd); werr != nil {
		fmt.Fprintf(os.Stderr, "SETSERVERFAIL write: %s\n", safeErr(werr))
		fmt.Println("RESULT server_set=0")
		os.Exit(4)
	}

	fmt.Println("RESULT server_set=1")
	os.Exit(0)
}

// runPair exchanges a pairing code for a client-token, validates it, fills in the
// username, and persists the token (clearing any password). Contract:
//
//	RESULT paired=<0|1> scopes_ok=<0|1>
//
// Pairing still succeeds (paired=1) when scopes are incomplete — scopes_ok=0 and the
// missing scopes are logged host-free to STDERR — because a partial-scope token is
// better than none and the warning tells the user exactly what to re-grant. Exit:
// 2 config (no host) · 3 exchange unreachable · 4 ran-but-errored (validate/write) ·
// 0 paired.
func runPair(cfg *config.Config, code string) {
	code = strings.TrimSpace(code)
	if code == "" {
		fmt.Fprintln(os.Stderr, "PAIRFAIL empty pairing code")
		fmt.Println("RESULT paired=0 scopes_ok=0")
		os.Exit(2)
	}
	host := cfg.ActiveHost()

	// Exchange runs PRE-token using only the base URL + TLS-skip flag.
	exch, err := romm.ExchangeToken(host.URL(), code, host.InsecureSkipVerify)
	if err != nil {
		// Distinguish unreachable (network) from a server-rejected code (ran but
		// errored): a network error scrubs to "network error" via safeErr.
		msg := safeErr(err)
		fmt.Fprintf(os.Stderr, "PAIRFAIL exchange: %s\n", msg)
		fmt.Println("RESULT paired=0 scopes_ok=0")
		if msg == "network error" {
			os.Exit(3)
		}
		os.Exit(4)
	}
	if exch.RawToken == "" {
		fmt.Fprintln(os.Stderr, "PAIRFAIL exchange: empty token in response")
		fmt.Println("RESULT paired=0 scopes_ok=0")
		os.Exit(4)
	}

	// Scope check is advisory — warn, never block (security req #2).
	missing := romm.MissingSyncScopes(exch.Scopes)
	scopesOK := len(missing) == 0
	if !scopesOK {
		fmt.Fprintf(os.Stderr, "PAIRWARN token missing sync scopes: %s\n", strings.Join(missing, " "))
	}

	// Validate the freshly-minted token against the server with a real authed GET.
	tokenHost := host
	tokenHost.Token = exch.RawToken
	tokenHost.Password = "" // never authenticate with a stale password once we hold a token
	apiTimeout := time.Duration(cfg.ApiTimeout.Int()) * time.Second
	client := romm.NewClient(tokenHost, apiTimeout)
	if verr := client.ValidateToken(); verr != nil {
		fmt.Fprintf(os.Stderr, "PAIRFAIL validate: %s\n", safeErr(verr))
		fmt.Println("RESULT paired=0 scopes_ok=0")
		os.Exit(4)
	}

	// Auto-fill the username if we don't already have one (best-effort; a failure
	// here doesn't sink the pairing — the token is what matters).
	username := host.Username
	if username == "" {
		if u, uerr := client.GetCurrentUser(); uerr == nil && u.Username != "" {
			username = u.Username
		}
	}

	upd := config.HostUpdate{Username: username}
	upd.SetToken(exch.RawToken, exch.Name, exch.ExpiresAt, exch.Scopes)
	if werr := writeIdentity(cfg, upd); werr != nil {
		fmt.Fprintf(os.Stderr, "PAIRFAIL write: %s\n", safeErr(werr))
		fmt.Println("RESULT paired=0 scopes_ok=0")
		os.Exit(4)
	}

	fmt.Printf("RESULT paired=1 scopes_ok=%d\n", b2i(scopesOK))
	os.Exit(0)
}

// runRegisterDevice registers this device by name and stores the server-assigned
// device_id + device_name in config.json. Contract: RESULT registered=<0|1>.
// device_name is REQUIRED (an empty name blanks the cross-device "which device"
// column). Exit: 2 config/empty-name · 3 unreachable · 4 errored · 0 registered.
func runRegisterDevice(cfg *config.Config, name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		fmt.Fprintln(os.Stderr, "REGFAIL empty device name (device_name is required)")
		fmt.Println("RESULT registered=0")
		os.Exit(2)
	}
	host := cfg.ActiveHost()
	apiTimeout := time.Duration(cfg.ApiTimeout.Int()) * time.Second
	client := romm.NewClient(host, apiTimeout)

	dev, err := client.RegisterDevice(name)
	if err != nil {
		msg := safeErr(err)
		fmt.Fprintf(os.Stderr, "REGFAIL register: %s\n", msg)
		fmt.Println("RESULT registered=0")
		if msg == "network error" {
			os.Exit(3)
		}
		os.Exit(4)
	}
	if dev.ID == "" {
		fmt.Fprintln(os.Stderr, "REGFAIL register: server returned no device id")
		fmt.Println("RESULT registered=0")
		os.Exit(4)
	}

	if werr := writeIdentity(cfg, config.HostUpdate{DeviceID: dev.ID, DeviceName: name}); werr != nil {
		fmt.Fprintf(os.Stderr, "REGFAIL write: %s\n", safeErr(werr))
		fmt.Println("RESULT registered=0")
		os.Exit(4)
	}

	fmt.Println("RESULT registered=1")
	os.Exit(0)
}

// runRenameDevice renames the already-registered device (PUT /api/devices/{id}) and
// updates device_name in config.json. Contract: RESULT renamed=<0|1>. Requires an
// existing device_id. Exit: 2 config/no-device/empty-name · 3 unreachable · 4
// errored · 0 renamed.
func runRenameDevice(cfg *config.Config, name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		fmt.Fprintln(os.Stderr, "RENAMEFAIL empty device name")
		fmt.Println("RESULT renamed=0")
		os.Exit(2)
	}
	host := cfg.ActiveHost()
	if host.DeviceID == "" {
		fmt.Fprintln(os.Stderr, "RENAMEFAIL no device_id — register the device first")
		fmt.Println("RESULT renamed=0")
		os.Exit(2)
	}
	apiTimeout := time.Duration(cfg.ApiTimeout.Int()) * time.Second
	client := romm.NewClient(host, apiTimeout)

	if _, err := client.UpdateDevice(host.DeviceID, name); err != nil {
		msg := safeErr(err)
		fmt.Fprintf(os.Stderr, "RENAMEFAIL update: %s\n", msg)
		fmt.Println("RESULT renamed=0")
		if msg == "network error" {
			os.Exit(3)
		}
		os.Exit(4)
	}

	if werr := writeIdentity(cfg, config.HostUpdate{DeviceName: name}); werr != nil {
		fmt.Fprintf(os.Stderr, "RENAMEFAIL write: %s\n", safeErr(werr))
		fmt.Println("RESULT renamed=0")
		os.Exit(4)
	}

	fmt.Println("RESULT renamed=1")
	os.Exit(0)
}

// runValidate checks the host's reachability (heartbeat) and auth (token validity)
// — the validate-on-save loop the wizard runs before committing the form. Contract:
//
//	RESULT reachable=<0|1> auth=<0|1>
//
// Exit reflects the worst failure: 3 unreachable (heartbeat failed — auth can't be
// judged, reported 0), 4 reachable-but-auth-failed, 0 both ok. auth is only trusted
// when reachable=1, so an unreachable host always prints auth=0.
func runValidate(cfg *config.Config) {
	host := cfg.ActiveHost()
	apiTimeout := time.Duration(cfg.ApiTimeout.Int()) * time.Second
	client := romm.NewClient(host, apiTimeout)

	// Reachability via the heartbeat, auth via a real authed GET. Live-server note:
	// this RomM's /api/heartbeat is AUTH-GATED (a bad token 500s it), so heartbeat
	// alone cannot cleanly separate "host down" from "token bad". We therefore also
	// run ValidateToken: a successful auth check PROVES the host is reachable even if
	// the heartbeat errored, so reachable is true whenever EITHER probe lands.
	hbErr := client.Heartbeat()
	authErr := client.ValidateToken()

	auth := authErr == nil
	reachable := hbErr == nil || auth

	fmt.Printf("RESULT reachable=%d auth=%d\n", b2i(reachable), b2i(auth))
	switch {
	case !reachable:
		os.Exit(3) // nothing answered — host is down/unresolvable
	case !auth:
		os.Exit(4) // reachable but the token is bad/expired/forbidden
	default:
		os.Exit(0)
	}
}
