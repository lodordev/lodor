package main

// RetroAchievements credential-spine modes (task #46, Phase A). These are the
// firmware-agnostic half of "log in once, RA just works": --ra-login exchanges an
// RA username + password for the long-lived RA token and stores {ra_username,
// ra_token} in config.json; --ra-status reports the stored state. The minarch fork
// (LodorOS) and the World-A cfg writers (OnionOS/muOS) consume those creds later.
//
// SECURITY (HARD): the RA PASSWORD is read from STDIN only (never argv — a password
// in a process list is a leak) and is never stored or logged. The TOKEN never
// reaches stdout/stderr/a RESULT line. The RA username is the public account handle,
// not a secret, so --ra-status echoes it for the menu's "Logged in as <user>".
//
// Exit codes match the rest of the engine: 0 ok · 2 flag/config · 3 unreachable ·
// 4 ran-but-errored (bad credentials / write failure).

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"lodor/clocksync"
	"lodor/config"
	"lodor/ra"
)

// runRALogin reads the RA password from stdin, exchanges username+password for the
// long-lived token via the RA login API, and persists {ra_username, ra_token} to
// config.json (never the password). Contract:
//
//	RESULT ra_login=<0|1>
//
// Exit: 2 empty username/password · 3 unreachable · 4 bad credentials / write fail ·
// 0 logged in.
func runRALogin(cfg *config.Config, username string) {
	username = strings.TrimSpace(username)
	if username == "" {
		fmt.Fprintln(os.Stderr, "RALOGINFAIL empty username")
		fmt.Println("RESULT ra_login=0")
		os.Exit(2)
	}

	// Password from STDIN ONLY. The native menu pipes the on-screen-keyboard buffer
	// in on the binary's stdin; we read a single line and strip the trailing newline.
	// Never echoed, never stored, never placed on the command line.
	password, rerr := readSecretLine(os.Stdin)
	if rerr != nil || password == "" {
		fmt.Fprintln(os.Stderr, "RALOGINFAIL empty password (read from stdin)")
		fmt.Println("RESULT ra_login=0")
		os.Exit(2)
	}

	// RTC-less handhelds boot to a garbage date -> TLS to retroachievements.org fails
	// cert validation. Set the clock from the RA host before the HTTPS login (no-op
	// when the clock is already sane). Best-effort: a clocksync failure is logged but
	// does not abort — the login attempt itself surfaces the real network verdict.
	if cerr := clocksync.Ensure(ra.DefaultBaseURL, false); cerr != nil {
		fmt.Fprintf(os.Stderr, "clocksync: %v\n", cerr)
	}

	timeout := time.Duration(cfg.ApiTimeout.Int()) * time.Second
	client := ra.NewClient("", timeout) // "" -> ra.DefaultBaseURL

	resp, err := client.Login(username, password)
	if err != nil {
		msg := safeErr(err)
		fmt.Fprintf(os.Stderr, "RALOGINFAIL login: %s\n", msg)
		fmt.Println("RESULT ra_login=0")
		if msg == "network error" {
			os.Exit(3)
		}
		os.Exit(4)
	}
	if !resp.Success || resp.Token == "" {
		// Host-free, token-free diagnostic: print only RA's machine code (e.g.
		// "invalid_credentials"), never the token or the server's HTML.
		reason := resp.Code
		if reason == "" {
			reason = "rejected"
		}
		fmt.Fprintf(os.Stderr, "RALOGINFAIL auth: %s\n", reason)
		fmt.Println("RESULT ra_login=0")
		os.Exit(4)
	}

	// RA may canonicalize the username's casing; prefer the server's echo when present.
	user := resp.User
	if user == "" {
		user = username
	}
	if werr := config.WriteRACredentials(user, resp.Token); werr != nil {
		fmt.Fprintf(os.Stderr, "RALOGINFAIL write: %s\n", safeErr(werr))
		fmt.Println("RESULT ra_login=0")
		os.Exit(4)
	}

	fmt.Println("RESULT ra_login=1")
	os.Exit(0)
}

// runRAStatus reports whether RA credentials are stored and for which user. The
// username is the public RA handle (not a secret); the token is never printed.
// Contract:
//
//	RESULT ra_logged_in=<0|1> ra_user=<username>
//
// Always exits 0 (a status query, not an operation).
func runRAStatus(cfg *config.Config) {
	if cfg.RALoggedIn() {
		fmt.Printf("RESULT ra_logged_in=1 ra_user=%s\n", cfg.RAUsername)
	} else {
		fmt.Println("RESULT ra_logged_in=0 ra_user=")
	}
	os.Exit(0)
}

// readSecretLine reads ONE line (the password) from r, stripping a trailing CR/LF.
// Bounded so a wedged/garbage pipe can't grow unbounded. The returned value is
// handled by the caller and never logged.
func readSecretLine(r io.Reader) (string, error) {
	br := bufio.NewReader(io.LimitReader(r, 4096))
	line, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
