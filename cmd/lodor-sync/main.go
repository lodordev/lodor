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
//	5  (profile modes) profile write failed
//	6  PAIRING EXPIRED — the server rejected our client-token (401, or 403 blaming
//	   the token): the pairing is expired or revoked and the fix is re-pairing this
//	   device. RESULT-printing modes also emit a final stdout line `PAIRING_EXPIRED`
//	   (after their normal RESULT line); data-list modes signal via exit code only
//	   (their stdout is a byte-parsed list). config.json is NEVER modified on this
//	   path — a transient server misconfig must not wipe a valid pairing.
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
		pullSaves         bool
		syncContinue      bool
		downloadBios      bool
		downloadQueue     bool
		syncFeed          bool
		recent            bool
		syncSave          string
		pushSave          string
		pushStates        string
		queueState        string
		pushPendingStates bool
		listStates        string
		pullStateRom      string
		pullStateID       int
		pullStateSlot     string
		listSaves         string
		restoreSave       string
		downloadRom       string
		reconcile         string
		evict             string
		uninstallMirror   bool
		removeDownloads   bool
		writeGamelists    bool
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
		raCmd             string
		raRecv            bool
		sessionStart      string
		sessionEnd        string
		syncPlaytime      bool
		listProfiles      bool
		listUsers         bool
		loginProfile      string
		loginUser         string
		loginDevice       string
		trackSave         string
		untrackSave       string
		setFavorite       string
		unsetFavorite     string
		setRating         string
		setStatus         string
		setProps          string
		checkBios         string
		showVersion       bool
		checkUpdate       bool
		fetchUpdate       bool
	)
	flag.BoolVar(&mirrorCatalog, "mirror-catalog", false, "stub every not-downloaded RomM game into Roms/ and write catalog-index.json; prints MIRROR created=.. existing=.. skipped=.. multifile=.. covers=..  (UPDATE: only new games + missing covers)")
	flag.BoolVar(&mirrorFull, "full", false, "with --mirror-catalog: FULL refresh — re-fetch every cover even if already present (default is the fast incremental update)")
	flag.BoolVar(&mirrorCollections, "mirror-collections", false, "write Collections/<name>.txt per RomM collection, plus the cross-device '0) Continue' collection from recent server saves; prints COLLECTIONS written=.. empty=.. total=.. then CONTINUE entries=..")
	flag.BoolVar(&listProfiles, "list-profiles", false, "MULTI-USER: print the profile list the Switch-Profile menu parses: one line per host '<active>\\t<label>\\t<hastoken>\\t<hasdevice>'")
	flag.BoolVar(&listUsers, "list-users", false, "MULTI-USER: list the RomM server's users for the Switch-User picker: '<active>\\t<username>\\t<role>\\t<signedin>' per line (admin token; falls back to stored profiles offline)")
	flag.StringVar(&loginProfile, "login-profile", "", "MULTI-USER: sign in as an existing RomM user under this profile LABEL (OAuth password grant; password on STDIN, never argv); needs --login-user; prints RESULT logged_in=<0|1>")
	flag.StringVar(&loginUser, "login-user", "", "with --login-profile: the existing RomM username to sign in as")
	flag.StringVar(&loginDevice, "login-device", "", "with --login-profile: optional device id to attach to the profile")
	flag.BoolVar(&pushPending, "push-pending", false, "upload every save in pending-saves.txt; prints RESULT pushed=<N> total=<M> stuck=<K>")
	flag.BoolVar(&pullSaves, "pull-saves", false, "TARGETED bulk pull (fast 'Sync now' leg): for every on-card game with a real server save, decide by CONTENT-HASH LINEAGE (local==newest: no-op; local==older revision: pull newest, .bak kept; local unknown to server: push it instead — never overwrite; ghosts filtered) — no catalog mirror; prints RESULT pulled=<N> checked=<M> ghosts=<G> pushed=<K>")
	flag.BoolVar(&syncContinue, "sync-continue", false, "LIGHT Continue refresh (fast 'Sync now' leg): rebuild the cross-device '0) Continue' collection and merge it into the host's native Recently Played from the local index + server saves — no catalog mirror; prints CONTINUE entries=<N> then RECENTS merged=<M> total=<T>")
	flag.BoolVar(&downloadBios, "download-bios", false, "download BIOS/firmware for every mapped platform; prints RESULT bios=<count>")
	flag.StringVar(&checkBios, "check-bios", "", "OFFLINE pre-launch BIOS gate: does this ROM's system REQUIRE a BIOS the user must supply, and is it present where the emulator reads it? prints RESULT bios_ok=1, or RESULT bios_ok=0 missing=<f1,f2> system=<name>. System TAG from LODOR_ROM_TAG or the ROM folder; extra search dirs via LODOR_BIOS_DIRS (colon-sep)")
	flag.BoolVar(&downloadQueue, "download-queue", false, "download every ROM queued in download-queue.txt (resolve, fetch, hash-verify each, reusing the --download path), dropping landed entries and keeping failures for retry; prints RESULT downloaded=<N> failed=<M> remaining=<K>")
	flag.BoolVar(&syncFeed, "sync-feed", false, "list recent server saves across mapped platforms, newest first, tab-separated")
	flag.BoolVar(&recent, "recent", false, "print the single most-recently-played game across devices as <localRomPath>\\t<game>\\t<when>\\t<device> (drives the Continue tile); empty if unreachable/none")
	flag.StringVar(&syncSave, "sync-save", "", "pull-then-push the save for one ROM (content-hash lineage, never clock-based); prints RESULT pulled=<0|1> pushed=<0|1> ghosts=<N> reason=<token> (pushed=1 only after a verified REAL upload; an unchanged save dedups server-side and reports reason=in-sync)")
	flag.StringVar(&pushSave, "push-save", "", "HYBRID post-game push for one ROM: push the changed save directly; on a LANDED push write last-synced.txt (the launcher's synced-✓ signal); if it does NOT land, stage the save into pending-saves.txt for later; prints RESULT pushed=<0|1> staged=<N>")
	flag.StringVar(&pushStates, "push-states", "", "Handoff v1: upload this ROM's local save STATES (normalized, deduped vs the state ledger); requires statecores.json in the pak dir, else no-ops honestly; an offline attempt auto-queues into pending-states.txt; prints RESULT pushedstates=<N> skippedstates=<N> failedstates=<N> retiredstates=<N> queuedstate=<0|1> reason=<token>")
	flag.StringVar(&queueState, "queue-state", "", "Handoff v1: queue this ROM into pending-states.txt (offline, instant, deduplicated) so --push-pending-states retries its state push when online; prints RESULT queuedstate=<0|1>")
	flag.BoolVar(&pushPendingStates, "push-pending-states", false, "Handoff v1: re-run the state push for every ROM in pending-states.txt (only still-offline roms stay queued); prints PENDINGSTATE lines + RESULT pendingstates=<N> drained=<M> stuck=<K>")
	flag.StringVar(&listStates, "list-states", "", "Handoff v1: list server save states for this ROM with compatibility annotations; prints LISTSTATE lines + RESULT")
	flag.StringVar(&pullStateRom, "pull-state", "", "Handoff v1: place ONE server state locally for this ROM (use with --state-id, optional --state-slot); prints RESULT placedstate=<0|1> reason=<token>")
	flag.IntVar(&pullStateID, "state-id", 0, "server state id for --pull-state")
	flag.StringVar(&pullStateSlot, "state-slot", "", "override target slot for --pull-state (0-8 or auto)")
	flag.StringVar(&listSaves, "list-saves", "", "list every server save for one ROM, newest first, tab-separated, then a LOCAL=<none|current|older|unpushed> trailer (single field — row parsers drop it); exit 3 when the server is unreachable (empty list + exit 0 always means zero saves)")
	flag.StringVar(&restoreSave, "restore-save", "", "restore a specific server save by id for one ROM (save id is the positional arg); prints RESULT restored=<0|1>")
	flag.StringVar(&downloadRom, "download", "", "download one ROM's real file (resolve, fetch, hash-verify); prints RESULT downloaded=<0|1>")
	flag.StringVar(&reconcile, "reconcile", "", "post-launch: flip ONE downloaded ROM's on-disk state marker (✘→✓) to match the bytes now present, carrying its save+cover with the rename; offline, no device; prints RESULT reconciled=<0|1>")
	flag.StringVar(&evict, "evict", "", "delete ONE downloaded ROM's bytes from the card and re-create its 0-byte cloud stub (✓→✘), carrying its save+cover with the rename — saves are NEVER deleted; multi-disc .m3u deletes its disc files too; offline, no device; prints RESULT evicted=<0|1> [reason=…]")
	flag.BoolVar(&uninstallMirror, "uninstall-mirror", false, "remove every MIRROR-OWNED artifact from the card (manifest walk: stubs, our covers/collections/folders; downloads KEPT unless --remove-downloads; saves NEVER touched; user files byte-identical); offline; prints RESULT uninstalled=<0|1> removed=<N> kept_downloads=<K> skipped=<S>")
	flag.BoolVar(&removeDownloads, "remove-downloads", false, "with --uninstall-mirror: also delete Lodor-downloaded games (the explicit second confirmation)")
	flag.BoolVar(&writeGamelists, "write-gamelists", false, "KNULLI BUILD ONLY (#186): merge-write every owned roms/<system>/gamelist.xml from the mirror manifest — clean marker-stripped <name>, cover <image> when present, foreign entries preserved verbatim; offline; prints RESULT gamelists=<N> entries=<M> (other builds refuse, exit 2)")
	flag.StringVar(&pair, "pair", "", "exchange a RomM pairing code for a client-token, validate it, write config.json (clearing any password); prints RESULT paired=<0|1> scopes_ok=<0|1>")
	flag.StringVar(&pairProfile, "pair-profile", "", "MULTI-USER: sign a profile in with a RomM PAIRING CODE (client-token exchange, not a password); stores the token owner as a profile; prints RESULT paired=<0|1> username=<name>")
	flag.StringVar(&registerDevice, "register-device", "", "register this device by name, store device_id+device_name in config.json; prints RESULT registered=<0|1>")
	flag.StringVar(&renameDevice, "rename-device", "", "rename the registered device, update config.json; prints RESULT renamed=<0|1>")
	flag.BoolVar(&validate, "validate", false, "check host reachability (heartbeat) + auth (token); prints RESULT reachable=<0|1> auth=<0|1> pairing_expired=<0|1> (pairing_expired=1 = token expired/revoked -> re-pair; exit 6 + PAIRING_EXPIRED line)")
	flag.StringVar(&setServer, "set-server", "", "persist the server URL (scheme+host) to config.json BEFORE pairing, creating config.json if absent; with --port/--insecure; prints RESULT server_set=<0|1>")
	flag.IntVar(&setServerPort, "port", 0, "optional numeric port: for --set-server the server port (0 = none); for --ra-cmd the RetroArch network_cmd_port (0 = default 55355)")
	flag.BoolVar(&setServerInsecure, "insecure", false, "for --set-server: skip TLS verification (HTTPS only)")
	flag.StringVar(&raLogin, "ra-login", "", "log in to RetroAchievements as <user>; reads the password from STDIN (never argv), exchanges it for the long-lived RA token, stores {ra_username, ra_token} in config.json (never the password); prints RESULT ra_login=<0|1>")
	flag.BoolVar(&raStatus, "ra-status", false, "report RetroAchievements login state; prints RESULT ra_logged_in=<0|1> ra_user=<username>")
	flag.StringVar(&raCmd, "ra-cmd", "", "send one RetroArch Network Control Interface command (QUIT, SCREENSHOT, GET_STATUS, ...) over loopback UDP; fire-and-forget by default (exit 0 once the datagram is sent); with --recv, wait for the reply (250ms x3), print it, exit 0 — a silent peer exits 3 (the wrapper's ra-net UNSUPPORTED probe signal). Local-only: no config, no RomM host.")
	flag.BoolVar(&raRecv, "recv", false, "with --ra-cmd: wait for and print the command's reply (for commands that answer, e.g. GET_STATUS); exit 3 when no reply arrives")
	flag.StringVar(&sessionStart, "session-start", "", "PLAYTIME (#146): stage a play session for this ROM path (/tmp marker: wall clock + /proc/uptime anchor). Offline, sub-second, config optional — never blocks a launch.")
	flag.StringVar(&sessionEnd, "session-end", "", "PLAYTIME (#146): finish the staged session for this ROM path — duration from the UPTIME DELTA (clock-jump immune), start_utc back-computed, appended to sessions.jsonl, totals.json/totals.tsv rebuilt. Offline. Prints RESULT recorded=<0|1> [secs=<N>]")
	flag.BoolVar(&syncPlaytime, "sync-playtime", false, "PLAYTIME (#146): pull peers' .lodortime meta-saves across mapped platforms and dedup-merge them into totals (newest record per device+key; own device skipped); prints RESULT playtime_fetched=<N> merged=<M>")
	flag.StringVar(&trackSave, "track-save", "", "DEVICE-SYNC (#176): RESUME syncing this device's save for one ROM path (POST /api/saves/{id}/track on the newest server save; RomM >= 4.9.0). Best-effort, touches no bytes; prints RESULT tracked=<0|1> reason=<token>")
	flag.StringVar(&untrackSave, "untrack-save", "", "DEVICE-SYNC (#176): STOP syncing this device's save for one ROM path (POST /api/saves/{id}/untrack on the newest server save; RomM >= 4.9.0). Best-effort, touches no bytes; prints RESULT untracked=<0|1> reason=<token>")
	flag.StringVar(&setFavorite, "set-favorite", "", "WRITE-BACK (#167): mark one ROM path as a favorite (add to the user's Favourites collection, creating it on the first-ever favorite; RomM collections.write). Best-effort; prints RESULT favorited=<0|1> reason=<token>")
	flag.StringVar(&unsetFavorite, "unset-favorite", "", "WRITE-BACK (#167): remove one ROM path from favorites (Favourites collection). Best-effort; prints RESULT unfavorited=<0|1> reason=<token>")
	flag.StringVar(&setRating, "set-rating", "", "WRITE-BACK (#167): set one ROM's rating; the positional arg is 0-10 (0 clears). PUT /api/roms/{id}/props; RomM roms.user.write. Prints RESULT rating_set=<0|1> reason=<token>")
	flag.StringVar(&setStatus, "set-status", "", "WRITE-BACK (#167): set one ROM's play status; the positional arg is incomplete|finished|completed_100|retired|never_playing (or clear/null). Prints RESULT status_set=<0|1> reason=<token>")
	flag.StringVar(&setProps, "set-props", "", "WRITE-BACK (#167): set several rom_user props at once from key=val positional args (rating,difficulty [0-10]; completion [0-100]; status; backlogged,now_playing,hidden,is_main_sibling [bool]); only the given keys are written. Prints RESULT props_set=<0|1> reason=<token>")
	flag.BoolVar(&showVersion, "version", false, "print 'lodor-sync <version>' and exit (release builds are stamped via ldflags; anything else says dev)")
	flag.BoolVar(&checkUpdate, "check-update", false, "SELF-UPDATE: fetch versions.json and compare against this build; prints RESULT update=<0|1> current=<v> latest=<v> channel=<ch> (+ NOTES\\t<line> when newer); exit 3 = manifest unreachable (shell stays silent). Reads update_channel from settings.conf; never writes it. Works unpaired.")
	flag.BoolVar(&fetchUpdate, "fetch-update", false, "SELF-UPDATE (LodorOS lane): download + sha256-verify + extract this lane's update asset (key from LODOR_UPDATE_ASSET) into ./.update/tree and write the READY marker; the SHELL applies it — this mode never swaps a binary. Prints RESULT fetched=<0|1> version=<v> bytes=<n>; exit 4 = hash mismatch (staging removed).")
	flag.Parse()

	// --version / update modes run before EVERYTHING (config.Load included):
	// version is pure, and an unpaired or misconfigured device must still be
	// able to check for and stage an update — the fix for a broken card may BE
	// the update.
	if showVersion {
		runVersion()
		return // always exits; defensive
	}
	if checkUpdate {
		runCheckUpdate()
		return // always exits; defensive
	}
	if fetchUpdate {
		runFetchUpdate()
		return // always exits; defensive
	}

	// --ra-cmd is PURELY LOCAL (a loopback UDP datagram to a running RetroArch —
	// task #145 session bracketing): no config.json, no host, no device. It runs
	// before everything else so the heavy-pak wrappers can call it from any cwd on
	// a card in any pairing state — the exit bracket must work offline.
	if raCmd != "" {
		runRACmd(raCmd, raRecv, setServerPort)
		return // runRACmd always exits; defensive
	}

	// --check-bios is the OFFLINE pre-launch BIOS gate (build #158): a pure local
	// file check with NO config.json, host, network, or device. It runs before
	// config.Load() so a launcher can call it from any cwd on a card in any pairing
	// state — and so a missing/corrupt config never turns the gate into a FATAL that
	// the launcher would (correctly) treat as fail-open and launch anyway.
	if checkBios != "" {
		runCheckBios(nil, checkBios)
		return // runCheckBios always exits; defensive
	}

	// --session-start / --session-end are OFFLINE and must work in ANY pairing
	// state (an unpaired card still tracks playtime locally under the fallback
	// key), so config is loaded best-effort — a missing/broken config.json means
	// nil cfg, never a fatal (#146).
	if sessionStart != "" || sessionEnd != "" {
		scfg, serr := config.Load()
		if serr != nil {
			scfg = nil
		}
		if sessionStart != "" {
			runSessionStart(sessionStart)
		} else {
			runSessionEnd(scfg, sessionEnd)
		}
		return // both always exit; defensive
	}

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
	// --evict is the mirror image of --reconcile and equally OFFLINE: it removes a
	// downloaded ROM's bytes and re-creates the cloud stub (✓→✘), carrying the save +
	// cover with the rename. Filesystem + local index only — run before the hosts gate
	// so "Delete from card" works with Wi-Fi off / server unreachable.
	if evict != "" {
		runEvict(cfg, evict)
		return // runEvict always exits; defensive
	}
	// --uninstall-mirror is manifest-walk-only and OFFLINE (like --evict): remove
	// everything the mirror created, keep the user's tree byte-identical. Runs
	// before the hosts gate so "Remove Lodor" works with Wi-Fi off.
	if uninstallMirror {
		runUninstallMirror(cfg, removeDownloads)
		return // always exits; defensive
	}
	// --write-gamelists is manifest-walk-only and OFFLINE (like --uninstall-mirror):
	// rebuild the owned gamelist.xml files from the mirror manifest. Knulli build
	// only — every other build refuses (no host there reads gamelists). Runs before
	// the hosts gate so a display refresh works with Wi-Fi off.
	if writeGamelists {
		runWriteGamelists(cfg)
		return // always exits; defensive
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
	case listStates != "":
		runListStates(client, cfg, listStates)
	case pullStateRom != "":
		runPullState(client, cfg, pullStateRom, pullStateID, pullStateSlot)
	case pushStates != "":
		runPushStates(client, cfg, pushStates)
	case queueState != "":
		runQueueState(queueState)
	case pushPendingStates:
		runPushPendingStates(client, cfg)
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
	case pullSaves:
		requireDevice(host)
		// Save downloads are file transfers too — the long timeout, like --push-pending.
		dlClient := romm.NewClient(host, time.Duration(cfg.DownloadTimeout.Int())*time.Second)
		runPullSaves(dlClient, cfg)
	case syncContinue:
		runSyncContinue(client, cfg)
	case syncPlaytime:
		runSyncPlaytime(client, cfg)
	case trackSave != "":
		runTrackSave(client, cfg, trackSave)
	case untrackSave != "":
		runUntrackSave(client, cfg, untrackSave)
	case setFavorite != "":
		// WRITE-BACK (#167): user-scoped rom props/favorites — no device_id required.
		runSetFavorite(client, cfg, setFavorite, true)
	case unsetFavorite != "":
		runSetFavorite(client, cfg, unsetFavorite, false)
	case setRating != "":
		runSetRating(client, cfg, setRating, flag.Arg(0))
	case setStatus != "":
		runSetStatus(client, cfg, setStatus, flag.Arg(0))
	case setProps != "":
		runSetProps(client, cfg, setProps, flag.Args())
	default:
		fmt.Fprintln(os.Stderr, "FATAL flag: no mode selected (need one of --pair --register-device --rename-device --validate --mirror-catalog --mirror-collections --download --download-queue --download-bios --check-bios --push-pending --pull-saves --sync-continue --sync-save --push-save --push-states --queue-state --push-pending-states --list-states --pull-state --list-saves --restore-save --evict --write-gamelists --sync-feed --ra-login --ra-status --ra-cmd --session-start --session-end --sync-playtime --track-save --untrack-save --set-favorite --unset-favorite --set-rating --set-status --set-props)")
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
