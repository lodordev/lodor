package main

// Playtime CLI modes (task #146): --session-start / --session-end (offline,
// sub-second, config OPTIONAL — they must work on an unpaired card) and
// --sync-playtime (network: pull peers' .lodortime meta-saves and merge).
// The per-session .lodortime PUSH rides runPushSave (modes.go) so it lands
// right after the save it describes.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lodor/catalog"
	"lodor/config"
	"lodor/playtime"
	"lodor/romm"
	"lodor/sync"
)

// playtimeKey resolves the session key for a local ROM path per the frozen
// contract: the ROM's content hash where the catalog index knows it
// ("md5:<hash>"), else "tag:<TAG>/<canonical basename>". cfg may be nil (an
// unpaired card still tracks locally under the fallback key).
func playtimeKey(cfg *config.Config, romPath string) (key, romBase string) {
	romBase = catalog.CanonicalRomBasename(romPath)
	if h, ok := catalog.RomHashForPath(cfg, romPath); ok {
		return "md5:" + h, romBase
	}
	return "tag:" + romFolderTag(romPath) + "/" + romBase, romBase
}

// romFolderTag extracts the MinUI emulator TAG from the ROM's parent folder
// ("Nintendo 64 (N64)" -> "N64"); a folder without a parenthetical TAG names
// itself (matches the wrappers' _rom_systag).
func romFolderTag(romPath string) string {
	d := filepath.Base(filepath.Dir(romPath))
	if i := strings.LastIndex(d, "("); i >= 0 {
		if j := strings.Index(d[i:], ")"); j > 1 {
			return d[i+1 : i+j]
		}
	}
	return d
}

// playtimeDeviceName is this device's identity in playtime records — the
// registered RomM device name; "" (unregistered) still tracks locally.
func playtimeDeviceName(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.ActiveHost().DeviceName
}

// runSessionStart stages the in-flight session: wall clock for display,
// /proc/uptime for the duration anchor. Silent on stdout; never blocks a
// launch (the hooks discard rc anyway, but 0 = staged, 4 = could not).
func runSessionStart(rom string) {
	up, ok := playtime.ReadUptime()
	if !ok {
		fmt.Fprintln(os.Stderr, "session-start: /proc/uptime unreadable — session not staged")
		os.Exit(4)
	}
	if err := playtime.WriteStage(rom, time.Now().Unix(), up); err != nil {
		fmt.Fprintf(os.Stderr, "session-start: %v\n", err)
		os.Exit(4)
	}
	os.Exit(0)
}

// runSessionEnd consumes the staged session, computes the duration from the
// UPTIME DELTA (clock-jump immune), back-computes start_utc = wall_end −
// duration, appends to sessions.jsonl and rebuilds totals.json/totals.tsv.
// Offline, no network. Contract: RESULT recorded=<0|1> [secs=<N>] [reason=..].
func runSessionEnd(cfg *config.Config, rom string) {
	st, ok := playtime.TakeStage(rom)
	if !ok {
		fmt.Println("RESULT recorded=0 reason=no-session")
		os.Exit(0)
	}
	up, uok := playtime.ReadUptime()
	if !uok {
		fmt.Println("RESULT recorded=0 reason=no-uptime")
		os.Exit(0)
	}
	wallEnd := time.Now().Unix()
	startUTC, secs, cok := playtime.ComputeSession(st, up, wallEnd)
	if !cok {
		fmt.Println("RESULT recorded=0 reason=empty-session")
		os.Exit(0)
	}
	key, romBase := playtimeKey(cfg, rom)
	s := playtime.Session{Key: key, Rom: romBase, Device: playtimeDeviceName(cfg), StartUTC: startUTC, Secs: secs}
	if err := playtime.AppendSession(s); err != nil {
		fmt.Fprintf(os.Stderr, "session-end: append: %v\n", err)
		fmt.Println("RESULT recorded=0 reason=write-failed")
		os.Exit(4)
	}
	if err := playtime.WriteTotals(); err != nil {
		fmt.Fprintf(os.Stderr, "session-end: totals: %v\n", err) // session itself IS recorded
	}
	sessionEndPersistClock()
	fmt.Printf("RESULT recorded=1 secs=%d\n", secs)
	os.Exit(0)
}

// sessionEndPersistClock is the #147 persist-cadence hook: a finished session
// proves the device was just alive at "now", so refresh datetime.txt (the
// forward-only boot-restore source) — but ONLY when the clock reads a sane
// year: an epoch-boot session that never saw NTP must not overwrite a good
// persisted time with garbage. Honors DATETIME_PATH like the shell lib;
// temp+rename (FAT32). Best-effort by design: no error escapes.
func sessionEndPersistClock() {
	now := time.Now()
	if now.Year() < 2024 {
		return
	}
	p := os.Getenv("DATETIME_PATH")
	if p == "" {
		sd := os.Getenv("SDCARD_PATH")
		if sd == "" {
			sd = "/mnt/SDCARD"
		}
		p = filepath.Join(sd, ".userdata", "shared", "datetime.txt")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, []byte(now.Format("2006-01-02 15:04:05")+"\n"), 0o644) == nil {
		_ = os.Rename(tmp, p)
	}
}

// pushSessionMetas pushes this device's cross-device sidecars for one ROM
// right after the save push they belong to (called from runPushSave): the
// compact .lodortime playtime record (#146) and the newest local
// <rom>.auto.png preview as .lodorshot.png (#149). BEST-EFFORT by contract:
// every failure is a quiet stderr note, never an outcome change, never stdout.
func pushSessionMetas(client *romm.Client, cfg *config.Config, romPath string) {
	romID, ok := catalog.ResolveRomID(cfg, romPath)
	if !ok || romID == 0 {
		return
	}
	rom, err := client.GetRom(romID)
	if err != nil || rom.ID == 0 {
		return
	}

	// playtime record (#146)
	key, _ := playtimeKey(cfg, romPath)
	if rec := playtime.BuildRecord(key, playtimeDeviceName(cfg)); rec.Plays > 0 {
		if payload, merr := json.Marshal(rec); merr == nil {
			name := sync.MetaFileName(rom, romPath, ".lodortime")
			if perr := sync.PushMetaSave(client, cfg, rom, name, payload); perr != nil {
				fmt.Fprintf(os.Stderr, "playtime-meta: push skipped: %s\n", safeErr(perr))
			}
		}
	}

	// preview screenshot (#149): the wrapper/minarch capture landed at the local
	// .minui convention; ship those exact bytes (PushMetaSave dedups by hash, so
	// an unchanged preview costs one list call).
	if pv := sync.PreviewLocalPath(romPath); pv != "" {
		if data, rerr := os.ReadFile(pv); rerr == nil && len(data) > 0 {
			name := sync.MetaFileName(rom, romPath, ".lodorshot.png")
			if perr := sync.PushMetaSave(client, cfg, rom, name, data); perr != nil {
				fmt.Fprintf(os.Stderr, "preview-meta: push skipped: %s\n", safeErr(perr))
			}
		}
	}
}

// runSyncPlaytime pulls peers' .lodortime meta-saves across mapped platforms,
// dedup-merges them into the local peers store (newest record per device+key;
// our own device's records are skipped) and rebuilds the totals. A per-save-id
// updated_at memo skips re-downloading unchanged records. Contract:
//
//	RESULT playtime_fetched=<N> merged=<M>
//
// fetched = records downloaded this run; merged = records that changed the
// store. Total platform-list failure exits 3 like the other net modes.
func runSyncPlaytime(client *romm.Client, cfg *config.Config) {
	platforms, err := mappedPlatforms(client, cfg)
	if err != nil {
		noteAuthErr(err)
		fmt.Fprintf(os.Stderr, "FATAL sync-playtime: %s\n", safeErr(err))
		exitMode(3)
	}
	self := playtimeDeviceName(cfg)
	memo := loadPlaytimeMemo()
	fetched, merged := 0, 0
	for _, p := range platforms {
		saves, gerr := client.GetSaves(romm.SaveQuery{PlatformID: p.ID})
		if gerr != nil {
			noteAuthErr(gerr)
			continue
		}
		for _, s := range saves {
			if sync.IsGhostSave(s) || !sync.IsMetaSave(s) ||
				!strings.HasSuffix(strings.ToLower(s.FileName), ".lodortime") {
				continue
			}
			if ts, seen := memo[s.ID]; seen && ts == s.UpdatedAt.Unix() {
				continue // unchanged since last pull
			}
			data, derr := client.DownloadSaveContent(s.ID, "", false) // side-effect-free read
			if derr != nil || len(data) == 0 {
				continue
			}
			fetched++
			memo[s.ID] = s.UpdatedAt.Unix()
			var rec playtime.Record
			if json.Unmarshal(data, &rec) != nil {
				continue
			}
			if changed, merr := playtime.MergePeer(rec, self); merr == nil && changed {
				merged++
			}
		}
	}
	savePlaytimeMemo(memo)
	if merged > 0 {
		if werr := playtime.WriteTotals(); werr != nil {
			fmt.Fprintf(os.Stderr, "sync-playtime: totals: %v\n", werr)
		}
	}
	fmt.Printf("RESULT playtime_fetched=%d merged=%d\n", fetched, merged)
	exitMode(0)
}

// playtimeMemoPath stores save_id -> updated_at for fetched .lodortime records.
func playtimeMemoPath() string { return filepath.Join(playtime.Dir(), "fetched.json") }

func loadPlaytimeMemo() map[int]int64 {
	m := map[int]int64{}
	if data, err := os.ReadFile(playtimeMemoPath()); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	return m
}

func savePlaytimeMemo(m map[int]int64) {
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	if err := os.MkdirAll(playtime.Dir(), 0o755); err != nil {
		return
	}
	tmp := playtimeMemoPath() + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, playtimeMemoPath())
	}
}
