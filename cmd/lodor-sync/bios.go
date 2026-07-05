package main

// --check-bios: the OFFLINE pre-launch BIOS gate (build #158). A launcher calls this
// BEFORE handing a ROM to the emulator; a missing mandatory BIOS becomes an honest
// on-screen message instead of the silent black screen a BIOS-less core boots into.
//
// Pure local file check — no network, sub-millisecond, offline. The system is resolved
// from LODOR_ROM_TAG (the launcher's per-launch tag) or, failing that, the ROM's parent
// folder tag (romFolderTag). Extra BIOS search dirs — e.g. the vendor RetroArch
// system_directory the H700 shim reads — come in via LODOR_BIOS_DIRS (colon-separated).
//
// Contract (stdout, parsed by the launcher):
//
//	RESULT bios_ok=1
//	RESULT bios_ok=0 missing=<file1,file2> system=<name>
//
// Always exits 0: the RESULT line carries the verdict, and the launcher fails OPEN (a
// missing/empty line => launch as before). system=<name> is printed LAST because a name
// can contain a space ("Sega CD").

import (
	"fmt"
	"os"
	"strings"

	"lodor/config"
	"lodor/platform"
)

// checkBiosResult resolves the ROM's system and returns the exact RESULT line the
// launcher parses. Split out from runCheckBios so the stdout contract is unit-testable
// without os.Exit.
func checkBiosResult(rom string) string {
	tag := strings.TrimSpace(os.Getenv("LODOR_ROM_TAG"))
	if tag == "" {
		tag = romFolderTag(rom)
	}

	var extra []string
	if v := strings.TrimSpace(os.Getenv("LODOR_BIOS_DIRS")); v != "" {
		for _, d := range strings.Split(v, ":") {
			if d = strings.TrimSpace(d); d != "" {
				extra = append(extra, d)
			}
		}
	}

	ok, missing, sys := platform.CheckBIOS(tag, extra)
	if ok {
		return "RESULT bios_ok=1"
	}
	return fmt.Sprintf("RESULT bios_ok=0 missing=%s system=%s", strings.Join(missing, ","), sys)
}

func runCheckBios(cfg *config.Config, rom string) {
	_ = cfg // signature parity with the other modes; the check is config-free.
	fmt.Println(checkBiosResult(rom))
	os.Exit(0)
}
