package main

// Native-menu modes for the on-device rom write-back (task #167): the launcher's
// per-game Y-menu pushing favorite / rating / status / props back to RomM. Each is a
// RESULT-printing mode (so exitMode applies the PAIRING_EXPIRED tail) and NON-BLOCKING
// in the sense that it never panics or touches ROM/save bytes — but because it is a
// user-initiated action it reports the outcome HONESTLY (no fake "saved!").
//
// Contract:
//
//	--set-favorite   <romPath>            → RESULT favorited=<0|1>   reason=<token>
//	--unset-favorite <romPath>            → RESULT unfavorited=<0|1> reason=<token>
//	--set-rating     <romPath> <0-10>     → RESULT rating_set=<0|1>  reason=<token>
//	--set-status     <romPath> <status>   → RESULT status_set=<0|1>  reason=<token>
//	--set-props      <romPath> k=v [k=v…] → RESULT props_set=<0|1>   reason=<token>
//
// reason tokens: ok | resolve (path didn't map to a rom_id) | notfound (404) |
// forbidden (403 scope) | range (422 out of range) | unreachable (offline) |
// autherr (token rejected) | error | badarg (bad CLI value — exit 2).

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"lodor/config"
	"lodor/romm"
	"lodor/sync"
)

// runSetFavorite adds (favorite) or removes (!favorite) one ROM from the user's
// Favourites collection.
func runSetFavorite(client *romm.Client, cfg *config.Config, romPath string, favorite bool) {
	res := sync.SetFavoriteForRom(client, cfg, romPath, favorite)
	if res.AuthExpired {
		pairingExpired = true
	}
	verb := "favorited"
	if !favorite {
		verb = "unfavorited"
	}
	fmt.Printf("RESULT %s=%s reason=%s\n", verb, boolFlag(res.OK), res.Reason)
	exitMode(0)
}

// runSetRating sets one ROM's 0-10 rating (0 clears). The value is the positional arg.
func runSetRating(client *romm.Client, cfg *config.Config, romPath, ratingArg string) {
	rating, err := parseBounded(ratingArg, 0, 10)
	if err != nil {
		fmt.Printf("RESULT rating_set=0 reason=badarg\n")
		os.Exit(2)
	}
	res := sync.SetRomPropsForRom(client, cfg, romPath, romm.RomUserData{Rating: romm.PtrInt(rating)})
	if res.AuthExpired {
		pairingExpired = true
	}
	fmt.Printf("RESULT rating_set=%s reason=%s\n", boolFlag(res.OK), res.Reason)
	exitMode(0)
}

// runSetStatus sets one ROM's play status. The positional arg is a wire-legal enum
// value, or "clear"/"null" to clear it back to null.
func runSetStatus(client *romm.Client, cfg *config.Config, romPath, statusArg string) {
	statusArg = strings.TrimSpace(statusArg)
	var data romm.RomUserData
	switch statusArg {
	case "":
		fmt.Printf("RESULT status_set=0 reason=badarg\n")
		os.Exit(2)
	case "clear", "null":
		data.ClearStatus = true
	default:
		st := romm.RomUserStatus(statusArg)
		if !romm.ValidRomUserStatus(st) {
			fmt.Printf("RESULT status_set=0 reason=badarg\n")
			os.Exit(2)
		}
		data.Status = romm.PtrStatus(st)
	}
	res := sync.SetRomPropsForRom(client, cfg, romPath, data)
	if res.AuthExpired {
		pairingExpired = true
	}
	fmt.Printf("RESULT status_set=%s reason=%s\n", boolFlag(res.OK), res.Reason)
	exitMode(0)
}

// runSetProps sets several rom_user props at once from key=val positional args. Only
// the keys given are written (exclude_unset); an empty or malformed set is a bad-arg.
func runSetProps(client *romm.Client, cfg *config.Config, romPath string, kvs []string) {
	data, err := parsePropArgs(kvs)
	if err != nil || data.IsEmpty() {
		fmt.Printf("RESULT props_set=0 reason=badarg\n")
		os.Exit(2)
	}
	res := sync.SetRomPropsForRom(client, cfg, romPath, data)
	if res.AuthExpired {
		pairingExpired = true
	}
	fmt.Printf("RESULT props_set=%s reason=%s\n", boolFlag(res.OK), res.Reason)
	exitMode(0)
}

// parsePropArgs turns key=val strings into a partial RomUserData. Favorite is NOT a
// prop (it's a collection membership) and is intentionally not accepted here.
func parsePropArgs(kvs []string) (romm.RomUserData, error) {
	var d romm.RomUserData
	for _, kv := range kvs {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			return d, fmt.Errorf("bad pair %q", kv)
		}
		key := strings.TrimSpace(kv[:i])
		val := strings.TrimSpace(kv[i+1:])
		switch key {
		case "rating":
			n, err := parseBounded(val, 0, 10)
			if err != nil {
				return d, err
			}
			d.Rating = romm.PtrInt(n)
		case "difficulty":
			n, err := parseBounded(val, 0, 10)
			if err != nil {
				return d, err
			}
			d.Difficulty = romm.PtrInt(n)
		case "completion":
			n, err := parseBounded(val, 0, 100)
			if err != nil {
				return d, err
			}
			d.Completion = romm.PtrInt(n)
		case "status":
			switch val {
			case "", "clear", "null":
				d.ClearStatus = true
			default:
				st := romm.RomUserStatus(val)
				if !romm.ValidRomUserStatus(st) {
					return d, fmt.Errorf("bad status %q", val)
				}
				d.Status = romm.PtrStatus(st)
			}
		case "backlogged":
			b, err := parseBool(val)
			if err != nil {
				return d, err
			}
			d.Backlogged = romm.PtrBool(b)
		case "now_playing":
			b, err := parseBool(val)
			if err != nil {
				return d, err
			}
			d.NowPlaying = romm.PtrBool(b)
		case "hidden":
			b, err := parseBool(val)
			if err != nil {
				return d, err
			}
			d.Hidden = romm.PtrBool(b)
		case "is_main_sibling":
			b, err := parseBool(val)
			if err != nil {
				return d, err
			}
			d.IsMainSibling = romm.PtrBool(b)
		default:
			return d, fmt.Errorf("unknown key %q", key)
		}
	}
	return d, nil
}

// parseBounded parses an int in [lo,hi]; out-of-range or non-numeric is an error.
func parseBounded(s string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("value %d out of range [%d,%d]", n, lo, hi)
	}
	return n, nil
}

// parseBool accepts the common truthy/falsy spellings a menu shell might pass.
func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("bad bool %q", s)
}
