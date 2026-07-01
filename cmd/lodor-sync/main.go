// Command lodor-sync is the headless CLI entrypoint for the Lodor save-sync
// engine: a from-scratch, CGO-free RomM client whose stdout is parsed byte-for-byte
// by the native MinUI launcher (minui.c). Every mode prints ONLY its documented
// RESULT/line contract (BLUEPRINT §8) — no HOST/DEVICE/PLAN noise reaches stdout —
// and exits with the code the shell switches on.
//
// Exit codes:
//
//	0  ok
//	2  config / flag error
//	3  unreachable / could not resolve
//	4  ran but one or more items errored
//
// The orchestration shell (romm-run / romm-sync-lib.sh) runs this binary with
// cwd = Grout.pak (so config.json loads CWD-relative), CFW=MinUI, and the
// SDCARD_PATH / PLATFORM env the path helpers read. It owes nothing to grout's
// code; grout is consulted only as a behavioral/wire oracle.
//
// CGO-free, stdlib only.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"lodor/clocksync"
	"lodor/config"
	"lodor/romm"
)

// tierProbeTimeout bounds the tier-1 (Tailscale) reachability check that
// ResolveHost uses to decide between the preferred internal endpoint and the
// public Cloudflare Access fallback. It must be SHORT: a device off the tailnet
// should fail fast and fall back, not stall the whole sync. Only consulted when a
// config actually carries more than one endpoint (a single-host config never
// probes), so it adds zero latency to existing single-endpoint cards.
const tierProbeTimeout = 2 * time.Second

func main() {
	var (
		mirrorCatalog     bool
		mirrorFull        bool
		mirrorCollections bool
		pushPending       bool
		downloadBios      bool
		downloadQueue     bool
		syncFeed          bool
		recent            bool
		syncSave          string
		pushSave          string
		listSaves         string
		restoreSave       string
		downloadRom       string
		reconcile         string
		pair              string
		pairProfile       string
		registerDevice    string
		renameDevice      string
		validate          bool
		setServer         string
		setServerPort     int
		setServerInsecure bool
		raLogin           string
		raStatus          bool
		listProfiles      bool
		listUsers         bool
		loginProfile      string
		loginUser         string
		loginDevice       string
	)
	flag.BoolVar(&mirrorCatalog, "mirror-catalog", false, "stub every not-downloaded RomM game into Roms/ and write catalog-index.json; prints MIRROR created=.. existing=.. skipped=.. multifile=.. covers=..  (UPDATE: only new games + missing covers)")
	flag.BoolVar(&mirrorFull, "full", false, "with --mirror-catalog: FULL refresh — re-fetch every cover even if already present (default is the fast incremental update)")
	flag.BoolVar(&mirrorCollections, "mirror-collections", false, "write Collections/<name>.txt per RomM collection; prints COLLECTIONS written=.. empty=.. total=..")
	flag.BoolVar(&listProfiles, "list-profiles", false, "MULTI-USER: print the profile list the Switch-Profile menu parses: one line per host '<active>\\t<label>\\t<hastoken>\\t<hasdevice>'")
	flag.BoolVar(&listUsers, "list-users", false, "MULTI-USER: list the RomM server's users for the Switch-User picker: '<active>\\t<username>\\t<role>\\t<signedin>' per line (admin token; falls back to stored profiles offline)")
	flag.StringVar(&loginProfile, "login-profile", "", "MULTI-USER: sign in as an existing RomM user under this profile LABEL (OAuth password grant; password on STDIN, never argv); needs --login-user; prints RESULT logged_in=<0|1>")
	flag.StringVar(&loginUser, "login-user", "", "with --login-profile: the existing RomM username to sign in as")
	flag.StringVar(&loginDevice, "login-device", "", "with --login-profile: optional device id to attach to the profile")
	flag.BoolVar(&pushPending, "push-pending", false, "upload every save in pending-saves.txt; prints RESULT pushed=<N> total=<M> stuck=<K>")
	flag.BoolVar(&downloadBios, "download-bios", false, "download BIOS/firmware for every mapped platform; prints RESULT bios=<count>")
	flag.BoolVar(&downloadQueue, "download-queue", false, "download every ROM queued in download-queue.txt (resolve, fetch, hash-verify each, reusing the --download path), dropping landed entries and keeping failures for retry; prints RESULT downloaded=<N> failed=<M> remaining=<K>")
	flag.BoolVar(&syncFeed, "sync-feed", false, "list recent server saves across mapped platforms, newest first, tab-separated")
	flag.BoolVar(&recent, "recent", false, "print the single most-recently-played game across devices as <localRomPath>\\t<game>\\t<when>\\t<device> (drives the Continue tile); empty if unreachable/none")
	flag.StringVar(&syncSave, "sync-save", "", "pull-then-push the save for one ROM; prints RESULT pulled=<0|1> pushed=<0|1>")
	flag.StringVar(&pushSave, "push-save", "", "HYBRID post-game push for one ROM: push the changed save directly; on a LANDED push write last-synced.txt (the launcher's synced-✓ signal); if it does NOT land, stage the save into pending-saves.txt for later; prints RESULT pushed=<0|1> staged=<N>")
	flag.StringVar(&listSaves, "list-saves", "", "list every server save for one ROM, newest first, tab-separated")
	flag.StringVar(&restoreSave, "restore-save", "", "restore a specific server save by id for one ROM (save id is the positional arg); prints RESULT restored=<0|1>")
	flag.StringVar(&downloadRom, "download", "", "download one ROM's real file (resolve, fetch, hash-verify); prints RESULT downloaded=<0|1>")
	flag.StringVar(&reconcile, "reconcile", "", "post-launch: flip ONE downloaded ROM's on-disk state marker (✘→✓) to match the bytes now present, carrying its save+cover with the rename; offline, no device; prints RESULT reconciled=<0|1>")
	flag.StringVar(&pair, "pair", "", "exchange a RomM pairing code for a client-token, validate it, write config.json (clearing any password); prints RESULT paired=<0|1> scopes_ok=<0|1>")
	flag.StringVar(&pairProfile, "pair-profile", "", "MULTI-USER: sign a profile in with a RomM PAIRING CODE (client-token exchange, not a password); stores the token owner as a profile; prints RESULT paired=<0|1> username=<name>")
	flag.StringVar(&registerDevice, "register-device", "", "register this device by name, store device_id+device_name in config.json; prints RESULT registered=<0|1>")
	flag.StringVar(&renameDevice, "rename-device", "", "rename the registered device, update config.json; prints RESULT renamed=<0|1>")
	flag.BoolVar(&validate, "validate", false, "check host reachability (heartbeat) + auth (token); prints RESULT reachable=<0|1> auth=<0|1>")
	flag.StringVar(&setServer, "set-server", "", "persist the server URL (scheme+host) to config.json BEFORE pairing, creating config.json if absent; with --port/--insecure; prints RESULT server_set=<0|1>")
	flag.IntVar(&setServerPort, "port", 0, "optional numeric port for --set-server (0 = none)")
	flag.BoolVar(&setServerInsecure, "insecure", false, "for --set-server: skip TLS verification (HTTPS only)")
	flag.StringVar(&raLogin, "ra-login", "", "log in to RetroAchievements as <user>; reads the password from STDIN (never argv), exchanges it for the long-lived RA token, stores {ra_username, ra_token} in config.json (never the password); prints RESULT ra_login=<0|1>")
	flag.BoolVar(&raStatus, "ra-status", false, "report RetroAchievements login state; prints RESULT ra_logged_in=<0|1> ra_user=<username>")
	flag.Parse()

	// --set-server runs BEFORE config.Load(): the fresh-device case has no config.json
	// at all (Load would fail) and no hosts (the gate below would exit). This mode is
	// exactly the one that creates/repairs that file, so it owns its own write path and
	// must not be gated on the config already existing.
	if setServer != "" {
		runSetServer(setServer, setServerPort, setServerInsecure)
		return // runSetServer always exits; defensive
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL config: %v\n", err)
		os.Exit(2)
	}
	// --reconcile is filesystem-only and OFFLINE: it flips a downloaded ROM's state
	// marker (✘→✓) using only the local directory_mappings + index, with no host,
	// network, or device_id. Run it before the hosts gate so a marker flip in the
	// post-launch hook never depends on the server being configured/reachable.
	if reconcile != "" {
		runReconcile(cfg, reconcile)
		return // runReconcile always exits; defensive
	}
	// RetroAchievements credential-spine modes (task #46): account-global, they need
	// config.json (to read/write the RA creds) but NOT a RomM host, so they run before
	// the hosts gate. --ra-status is read-only; --ra-login does its own RA-host network
	// call (not the RomM host) and reads the password from stdin.
	if raStatus {
		runRAStatus(cfg)
		return // runRAStatus always exits; defensive
	}
	if raLogin != "" {
		runRALogin(cfg, raLogin)
		return // runRALogin always exits; defensive
	}

	if len(cfg.Hosts) == 0 {
		fmt.Fprintln(os.Stderr, "FATAL config: no hosts configured in config.json")
		os.Exit(2)
	}

	// --mirror-catalog is filesystem-only: it walks the live library to stub ROMs
	// and write the index. It still needs the network (GetRoms), so build the client
	// for it too — but it requires no device_id.
	// MULTI-USER + TWO-TIER: ResolveHost() picks the preferred reachable endpoint
	// (tier-1 Tailscale when up — probed THROUGH its SOCKS5 proxy with a short
	// timeout — else the tier-2 public Cloudflare Access fallback, which carries the
	// CF-Access service-token headers), then keeps the multi-user identity: when an
	// active profile is selected it stays byte-identical to ActiveHost (no probe). A
	// legacy single-endpoint, single-profile config resolves to hosts[0] byte-identical
	// (and does NO probe), so existing cards are unaffected.
	host := cfg.ResolveHost(func(h config.Host) bool {
		return romm.ProbeReachableHost(h, tierProbeTimeout)
	})
	// RTC-less handhelds boot to a garbage date; set the clock from the server before any HTTPS
	// (a wrong clock fails TLS cert validation -> every fetch dies). No-op when already sane.
	if err := clocksync.Ensure(host.URL(), host.SkipTLSVerify()); err != nil {
		fmt.Fprintf(os.Stderr, "clocksync: %v\n", err)
	}
	apiTimeout := time.Duration(cfg.ApiTimeout.Int()) * time.Second
	client := romm.NewClient(host, apiTimeout)

	switch {
	case validate:
		runValidate(cfg)
	case pair != "":
		runPair(cfg, pair)
	case pairProfile != "":
		runPairProfile(cfg, pairProfile)
	case registerDevice != "":
		runRegisterDevice(cfg, registerDevice)
	case renameDevice != "":
		runRenameDevice(cfg, renameDevice)
	case listProfiles:
		runListProfiles(cfg)
	case listUsers:
		runListUsers(cfg)
	case loginProfile != "":
		runLoginProfile(cfg, loginProfile, loginUser, loginDevice)
	case mirrorCatalog:
		runMirrorCatalog(client, cfg, mirrorFull)
	case mirrorCollections:
		runMirrorCollections(client, cfg)
	case downloadRom != "":
		// --download uses the LONG download timeout, not the api timeout: a large
		// ROM over a 30s api timeout was a real past failure. Rebuild the client.
		dlClient := romm.NewClient(host, time.Duration(cfg.DownloadTimeout.Int())*time.Second)
		runDownloadRom(dlClient, cfg, downloadRom)
	case downloadBios:
		// BIOS fetches are file transfers too — give them the download timeout.
		dlClient := romm.NewClient(host, time.Duration(cfg.DownloadTimeout.Int())*time.Second)
		runDownloadBios(dlClient, cfg)
	case downloadQueue:
		// Each queued ROM is a (possibly large) file transfer — use the long download
		// timeout, same as the single --download path it reuses.
		dlClient := romm.NewClient(host, time.Duration(cfg.DownloadTimeout.Int())*time.Second)
		runDownloadQueue(dlClient, cfg)
	case syncFeed:
		runSyncFeed(client, cfg)
	case recent:
		runRecent(client, cfg)
	case syncSave != "":
		requireDevice(host)
		runSyncSave(client, cfg, syncSave)
	case pushSave != "":
		requireDevice(host)
		// HYBRID post-game push: uploads can be large, so use the download timeout
		// for the transfer path (same as --push-pending), not the short api timeout.
		dlClient := romm.NewClient(host, time.Duration(cfg.DownloadTimeout.Int())*time.Second)
		runPushSave(dlClient, cfg, pushSave)
	case listSaves != "":
		runListSaves(client, cfg, listSaves)
	case restoreSave != "":
		requireDevice(host)
		runRestoreSave(client, cfg, restoreSave, flag.Arg(0))
	case pushPending:
		requireDevice(host)
		// Save uploads can be large; use the download timeout for the transfer path.
		dlClient := romm.NewClient(host, time.Duration(cfg.DownloadTimeout.Int())*time.Second)
		runPushPending(dlClient, cfg)
	default:
		fmt.Fprintln(os.Stderr, "FATAL flag: no mode selected (need one of --pair --register-device --rename-device --validate --mirror-catalog --mirror-collections --download --download-queue --download-bios --push-pending --sync-save --push-save --list-saves --restore-save --sync-feed --ra-login --ra-status)")
		os.Exit(2)
	}
}

// requireDevice exits 2 if the host has no device_id — the net write/sync modes
// cannot run without one (a save is keyed to the device).
func requireDevice(host config.Host) {
	if host.DeviceID == "" {
		fmt.Fprintln(os.Stderr, "FATAL config: no device_id on host — register the device first")
		os.Exit(2)
	}
}
