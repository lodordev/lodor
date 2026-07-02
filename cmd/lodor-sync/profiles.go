package main

// MULTI-USER profile modes (task #102 / the Switch-Profile feature). A "profile" is a
// host entry in config.json carrying that RomM user's token + label. --list-profiles
// feeds the launcher's Switch-Profile list; --login-profile signs IN as an existing RomM
// user (OAuth password grant) and stores their token under a new/updated profile. The
// active profile is named by active-profile.txt (written by the launcher); the engine's
// ActiveHost() resolves it for every other mode, so per-user auth + save namespacing
// follow the active profile automatically. CGO-free, stdlib only.

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"lodor/clocksync"
	"lodor/config"
	"lodor/romm"
)

// runListUsers prints the list the launcher's Switch-User picker parses: one line per
// RomM SERVER user, "<active>\t<username>\t<role>\t<signedin>". STDOUT only — the
// launcher reads it byte-for-byte, so NOTHING else may reach stdout on this path
// (diagnostics go to stderr). Always exits 0 with a usable list.
//
//	active   = 1 if username == the active profile's user, else 0
//	role     = the user's RomM role (admin/viewer); "" in the offline/non-admin fallback
//	signedin = 1 if a stored host (profile) for this username already holds a token
//
// Lists via cfg.Hosts[0] (the admin/onboarding token) — a VIEWER profile token cannot
// read /api/users, so ActiveHost would 403. If the list fails (offline, or 403 no
// users.read), it FALLS BACK to emitting the locally stored signed-in profiles in the
// SAME 4-field shape, so the picker still shows who is signed in. Exit 0 either way.
func runListUsers(cfg *config.Config) {
	activeLabel := config.ActiveProfileLabel()
	activeUser := cfg.ActiveHost().Username
	if activeUser == "" {
		activeUser = activeLabel
	}
	isActive := func(username string) int {
		if username != "" && (strings.EqualFold(username, activeUser) || strings.EqualFold(username, activeLabel)) {
			return 1
		}
		return 0
	}
	isSignedIn := func(username string) int {
		for _, h := range cfg.Hosts {
			if h.Token == "" {
				continue
			}
			if strings.EqualFold(h.ProfileLabel, username) || strings.EqualFold(h.Username, username) {
				return 1
			}
		}
		return 0
	}

	base := cfg.Hosts[0] // admin/onboarding token — the only one allowed to list users
	// RTC-less handhelds: set the clock before HTTPS so TLS cert validation passes.
	if cerr := clocksync.Ensure(base.URL(), base.SkipTLSVerify()); cerr != nil {
		fmt.Fprintf(os.Stderr, "clocksync: %v\n", cerr)
	}
	client := romm.NewClient(base, time.Duration(cfg.ApiTimeout.Int())*time.Second)
	users, err := client.GetUsers()
	if err != nil {
		// Offline or no users.read: fall back to the locally stored signed-in profiles
		// so the picker still lists who is signed in (active flag honored, role blank).
		// An expired/revoked token still emits the fallback list but exits 6 (see
		// authgate.go) so the picker can also surface "re-pair".
		noteAuthErr(err)
		fmt.Fprintf(os.Stderr, "list-users: %s (falling back to stored profiles)\n", safeErr(err))
		seen := map[string]bool{}
		for _, h := range cfg.Hosts {
			if h.Token == "" {
				continue
			}
			u := h.Username
			if u == "" {
				u = h.ProfileLabel
			}
			if u == "" || seen[strings.ToLower(u)] {
				continue
			}
			seen[strings.ToLower(u)] = true
			fmt.Printf("%d\t%s\t%s\t%d\n", isActive(u), u, "", 1)
		}
		exitModeQuiet(0)
	}
	for _, u := range users {
		if u.Username == "" {
			continue
		}
		fmt.Printf("%d\t%s\t%s\t%d\n", isActive(u.Username), u.Username, u.Role, isSignedIn(u.Username))
	}
	os.Exit(0)
}

// runListProfiles prints the multi-user list the launcher's Switch-Profile menu parses:
// one line per host, "<active>\t<label>\t<hastoken>\t<hasdevice>". STDOUT only — the
// launcher reads exactly this. label = profile_label, else username, else "Default".
func runListProfiles(cfg *config.Config) {
	activeLabel := config.ActiveProfileLabel()
	hasExplicitActive := activeLabel != "" && !strings.EqualFold(activeLabel, "default")
	for i, h := range cfg.Hosts {
		label := h.ProfileLabel
		if label == "" {
			label = h.Username
		}
		if label == "" {
			label = "Default"
		}
		active := 0
		if hasExplicitActive {
			if strings.EqualFold(label, activeLabel) {
				active = 1
			}
		} else if i == 0 {
			active = 1 // no active-profile.txt -> hosts[0] is live
		}
		hasTok := 0
		if h.Token != "" {
			hasTok = 1
		}
		hasDev := 0
		if h.DeviceID != "" {
			hasDev = 1
		}
		fmt.Printf("%d\t%s\t%d\t%d\n", active, label, hasTok, hasDev)
	}
	os.Exit(0)
}

// runLoginProfile signs IN as an existing RomM user via OAuth password grant and stores
// the token under a multi-user profile. label = profile name; username/deviceID from the
// --login-user/--login-device flags. The PASSWORD is read from STDIN (never argv) and is
// NEVER stored. Prints RESULT logged_in=<0|1>.
func runLoginProfile(cfg *config.Config, label, username, deviceID string) {
	label = strings.TrimSpace(label)
	username = strings.TrimSpace(username)
	if label == "" || username == "" {
		fmt.Fprintln(os.Stderr, "LOGINFAIL: empty profile label or username")
		fmt.Println("RESULT logged_in=0")
		os.Exit(2)
	}
	// Password from stdin (first line only), never argv.
	pw := ""
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() {
		pw = sc.Text()
	}
	if pw == "" {
		fmt.Fprintln(os.Stderr, "LOGINFAIL: empty password")
		fmt.Println("RESULT logged_in=0")
		os.Exit(2)
	}
	if len(cfg.Hosts) == 0 {
		fmt.Fprintln(os.Stderr, "LOGINFAIL: no host configured")
		fmt.Println("RESULT logged_in=0")
		os.Exit(2)
	}
	base := cfg.Hosts[0] // the server to authenticate against
	// RTC-less handhelds: set the clock from the server before HTTPS (a wrong clock
	// fails TLS cert validation). No-op when already sane.
	if cerr := clocksync.Ensure(base.URL(), base.SkipTLSVerify()); cerr != nil {
		fmt.Fprintf(os.Stderr, "clocksync: %v\n", cerr)
	}

	token, err := romm.PasswordGrant(base.URL(), username, pw, base.SkipTLSVerify())
	if err != nil {
		fmt.Fprintf(os.Stderr, "LOGINFAIL: %s\n", safeErr(err))
		fmt.Println("RESULT logged_in=0")
		os.Exit(4)
	}

	// Validate the new token + capture the canonical username from /api/users/me.
	loginHost := base
	loginHost.Token = token
	loginHost.Password = ""
	client := romm.NewClient(loginHost, time.Duration(cfg.ApiTimeout.Int())*time.Second)
	if verr := client.ValidateToken(); verr != nil {
		fmt.Fprintf(os.Stderr, "LOGINFAIL validate: %s\n", safeErr(verr))
		fmt.Println("RESULT logged_in=0")
		os.Exit(4)
	}
	if u, uerr := client.GetCurrentUser(); uerr == nil && u.Username != "" {
		username = u.Username
	}

	if werr := config.WriteProfileHost(label, username, token, deviceID, nil); werr != nil {
		fmt.Fprintf(os.Stderr, "LOGINFAIL write: %s\n", safeErr(werr))
		fmt.Println("RESULT logged_in=0")
		os.Exit(5)
	}
	fmt.Println("RESULT logged_in=1")
	os.Exit(0)
}
