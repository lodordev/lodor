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

	"lodor/config"
	"lodor/romm"
)

func main() {
	var (
		mirrorCatalog     bool
		mirrorCollections bool
		pushPending       bool
		downloadBios      bool
		syncFeed          bool
		recent            bool
		syncSave          string
		listSaves         string
		restoreSave       string
		downloadRom       string
		pair              string
		registerDevice    string
		renameDevice      string
		validate          bool
		setServer         string
		setServerPort     int
		setServerInsecure bool
	)
	flag.BoolVar(&mirrorCatalog, "mirror-catalog", false, "stub every not-downloaded RomM game into Roms/ and write catalog-index.json; prints MIRROR created=.. existing=.. skipped=.. multifile=..")
	flag.BoolVar(&mirrorCollections, "mirror-collections", false, "write Collections/<name>.txt per RomM collection; prints COLLECTIONS written=.. empty=.. total=..")
	flag.BoolVar(&pushPending, "push-pending", false, "upload every save in pending-saves.txt; prints RESULT pushed=<N> total=<M> stuck=<K>")
	flag.BoolVar(&downloadBios, "download-bios", false, "download BIOS/firmware for every mapped platform; prints RESULT bios=<count>")
	flag.BoolVar(&syncFeed, "sync-feed", false, "list recent server saves across mapped platforms, newest first, tab-separated")
	flag.BoolVar(&recent, "recent", false, "print the single most-recently-played game across devices as <localRomPath>\\t<game>\\t<when>\\t<device> (drives the Continue tile); empty if unreachable/none")
	flag.StringVar(&syncSave, "sync-save", "", "pull-then-push the save for one ROM; prints RESULT pulled=<0|1> pushed=<0|1>")
	flag.StringVar(&listSaves, "list-saves", "", "list every server save for one ROM, newest first, tab-separated")
	flag.StringVar(&restoreSave, "restore-save", "", "restore a specific server save by id for one ROM (save id is the positional arg); prints RESULT restored=<0|1>")
	flag.StringVar(&downloadRom, "download", "", "download one ROM's real file (resolve, fetch, hash-verify); prints RESULT downloaded=<0|1>")
	flag.StringVar(&pair, "pair", "", "exchange a RomM pairing code for a client-token, validate it, write config.json (clearing any password); prints RESULT paired=<0|1> scopes_ok=<0|1>")
	flag.StringVar(&registerDevice, "register-device", "", "register this device by name, store device_id+device_name in config.json; prints RESULT registered=<0|1>")
	flag.StringVar(&renameDevice, "rename-device", "", "rename the registered device, update config.json; prints RESULT renamed=<0|1>")
	flag.BoolVar(&validate, "validate", false, "check host reachability (heartbeat) + auth (token); prints RESULT reachable=<0|1> auth=<0|1>")
	flag.StringVar(&setServer, "set-server", "", "persist the server URL (scheme+host) to config.json BEFORE pairing, creating config.json if absent; with --port/--insecure; prints RESULT server_set=<0|1>")
	flag.IntVar(&setServerPort, "port", 0, "optional numeric port for --set-server (0 = none)")
	flag.BoolVar(&setServerInsecure, "insecure", false, "for --set-server: skip TLS verification (HTTPS only)")
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
	if len(cfg.Hosts) == 0 {
		fmt.Fprintln(os.Stderr, "FATAL config: no hosts configured in config.json")
		os.Exit(2)
	}

	// --mirror-catalog is filesystem-only: it walks the live library to stub ROMs
	// and write the index. It still needs the network (GetRoms), so build the client
	// for it too — but it requires no device_id.
	host := cfg.Hosts[0]
	apiTimeout := time.Duration(cfg.ApiTimeout.Int()) * time.Second
	client := romm.NewClient(host, apiTimeout)

	switch {
	case validate:
		runValidate(cfg)
	case pair != "":
		runPair(cfg, pair)
	case registerDevice != "":
		runRegisterDevice(cfg, registerDevice)
	case renameDevice != "":
		runRenameDevice(cfg, renameDevice)
	case mirrorCatalog:
		runMirrorCatalog(client, cfg)
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
	case syncFeed:
		runSyncFeed(client, cfg)
	case recent:
		runRecent(client, cfg)
	case syncSave != "":
		requireDevice(host)
		runSyncSave(client, cfg, syncSave)
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
		fmt.Fprintln(os.Stderr, "FATAL flag: no mode selected (need one of --pair --register-device --rename-device --validate --mirror-catalog --mirror-collections --download --download-bios --push-pending --sync-save --list-saves --restore-save --sync-feed)")
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
