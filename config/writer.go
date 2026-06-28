package config

// Safe writer for the engine's canonical config.json — the same file Load reads.
// Onboarding (--pair / --register-device / --rename-device) mutates only a handful
// of fields on hosts[0]; everything else in the file (directory_mappings, the
// launcher's own keys, fields this struct doesn't model) must survive untouched.
//
// To guarantee that round-trip, the writer edits a generic JSON tree
// (map[string]any) rather than re-marshalling the typed Config — so unknown keys
// are preserved byte-for-value. Writes are atomic (.tmp → rename) and chmod 0600
// where the filesystem honors it (a no-op on the FAT/exFAT card, free on ext).
//
// SECURITY (BLUEPRINT §"Security requirements"): this package NEVER prints the
// token, host, or device_id. It returns plain errors that name only the operation
// (open/parse/encode/rename); callers scrub transport errors with safeErr before
// any are surfaced. CGO-free, stdlib only.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// configFileName is the canonical config the engine loads CWD-relative. Kept in one
// place so Load and the writer never drift.
const configFileName = "config.json"

// HostUpdate carries the onboarding fields to persist onto hosts[0]. Only non-empty
// fields are written, EXCEPT the password clear, which is unconditional whenever a
// token is being set (security req #1: token-only at rest). Use the With* setters or
// set fields directly; a zero value writes nothing for that field.
type HostUpdate struct {
	Token          string
	TokenName      string
	TokenExpiresAt string
	Scopes         []string
	Username       string
	DeviceID       string
	DeviceName     string

	// Server-address fields, written by the onboarding wizard's first step
	// (--set-server) BEFORE pairing, since --pair reads root_uri from the config.
	// RootURI carries the scheme+host (e.g. "https://example"); Port is the optional
	// numeric port (0 = none); InsecureSkipVerify is the HTTPS "Skip Verification"
	// toggle. Port and InsecureSkipVerify have meaningful zero values (no-port /
	// verify-on), so a separate setServer flag — not a non-zero test — decides whether
	// they are written, mirroring setToken's explicit-set semantics.
	RootURI            string
	Port               int
	InsecureSkipVerify bool

	// setToken records that Token was explicitly provided (even if ""), so a caller
	// that means "set token" triggers the password clear. ExchangeToken always
	// yields a non-empty token, so in practice Token != "" is the signal; this flag
	// exists only to keep the contract explicit.
	setToken bool

	// setServer records that the server-address fields (RootURI/Port/
	// InsecureSkipVerify) were explicitly provided, so port=0 / insecure=false are
	// written authoritatively rather than skipped as zero values.
	setServer bool
}

// SetServer marks the server-address fields for writing. rootURI must be the full
// scheme+host (the wizard builds "http://"|"https://" + hostname; this writer does
// not synthesize a scheme). port=0 means "no explicit port" (the port key is
// removed); insecure toggles SSL skip-verify. Setting the server arms the explicit
// write of port/insecure even at their zero values.
func (u *HostUpdate) SetServer(rootURI string, port int, insecure bool) *HostUpdate {
	u.RootURI = rootURI
	u.Port = port
	u.InsecureSkipVerify = insecure
	u.setServer = true
	return u
}

// SetToken marks the token (and its metadata) for writing and arms the password
// clear. Token-only at rest: setting a token always blanks any stored password.
func (u *HostUpdate) SetToken(token, name, expiresAt string, scopes []string) *HostUpdate {
	u.Token = token
	u.TokenName = name
	u.TokenExpiresAt = expiresAt
	u.Scopes = scopes
	u.setToken = true
	return u
}

// WriteHostUpdate applies u to hosts[0] of config.json and persists it atomically.
// It reads the existing file as a generic tree, mutates only the targeted keys,
// preserves every other key (known or not), then writes via a 0600 temp file
// renamed into place. A missing/blank/invalid hosts array is repaired into a
// single-host array so a freshly-dropped or partial config can still be paired.
//
// NEVER logs or returns the token/host/device_id; errors name the failing step only.
func WriteHostUpdate(u HostUpdate) error {
	path := configFileName

	var root map[string]any
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if uerr := json.Unmarshal(data, &root); uerr != nil {
			return fmt.Errorf("parse config: %w", uerr)
		}
		if root == nil {
			root = map[string]any{}
		}
	case os.IsNotExist(err):
		root = map[string]any{}
	default:
		return fmt.Errorf("read config: %w", err)
	}

	host := firstHostMap(root)

	if u.setServer {
		// root_uri is the one required host field — always written (no omit). port and
		// insecure_skip_verify are written authoritatively: a 0 port removes the key
		// (URL() treats 0 as "no port"); verify-on (false) removes insecure_skip_verify
		// so the default-secure config stays clean rather than carrying "false".
		host["root_uri"] = u.RootURI
		if u.Port != 0 {
			host["port"] = u.Port
		} else {
			delete(host, "port")
		}
		if u.InsecureSkipVerify {
			host["insecure_skip_verify"] = true
		} else {
			delete(host, "insecure_skip_verify")
		}
	}

	if u.setToken || u.Token != "" {
		setOrDelete(host, "token", u.Token)
		// Token-only at rest: blanking the password whenever a token is set is the
		// core security guarantee (mirror Grout's Password = "" on pairing).
		delete(host, "password")
	}
	if u.TokenName != "" {
		host["token_name"] = u.TokenName
	}
	if u.TokenExpiresAt != "" {
		host["token_expires_at"] = u.TokenExpiresAt
	}
	if u.Scopes != nil {
		host["scopes"] = u.Scopes
	}
	if u.Username != "" {
		host["username"] = u.Username
	}
	if u.DeviceID != "" {
		host["device_id"] = u.DeviceID
	}
	if u.DeviceName != "" {
		host["device_name"] = u.DeviceName
	}

	return writeJSONAtomic(path, root)
}

// WriteRACredentials persists the RetroAchievements username + long-lived token to
// the TOP LEVEL of config.json (ra_username / ra_token), preserving every other key
// (hosts, directory_mappings, the launcher's own keys) byte-for-value via the same
// generic-tree edit WriteHostUpdate uses. The RA PASSWORD is never written — only the
// token, mirroring the token-only-at-rest rule for the RomM bearer. An empty token
// CLEARS both keys (logout). Atomic 0600 write. NEVER logs the token; errors name only
// the failing step.
func WriteRACredentials(username, token string) error {
	path := configFileName

	var root map[string]any
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if uerr := json.Unmarshal(data, &root); uerr != nil {
			return fmt.Errorf("parse config: %w", uerr)
		}
		if root == nil {
			root = map[string]any{}
		}
	case os.IsNotExist(err):
		root = map[string]any{}
	default:
		return fmt.Errorf("read config: %w", err)
	}

	if token == "" {
		delete(root, "ra_username")
		delete(root, "ra_token")
	} else {
		root["ra_username"] = username
		root["ra_token"] = token
	}

	return writeJSONAtomic(path, root)
}

// WriteProfileUpdate applies u to the profile named label in config.json (matched by
// label, case-insensitive), creating the profile entry if it does not yet exist, and
// persists atomically. Used by the multi-user onboarding modes (--add-profile pairing
// + --register-device) so a family member's token/device_id land on THEIR profile, not
// on hosts[0] (the shared server). hosts[] is left untouched. Same generic-tree edit as
// WriteHostUpdate: every other key (known or not) survives byte-for-value. NEVER logs
// the token/device_id; errors name only the failing step.
//
// When ALSO marking the profile active, callers follow with SetActiveProfile (or pass
// it through the higher-level helper) — this function only writes the profile fields.
func WriteProfileUpdate(label string, u HostUpdate) error {
	path := configFileName
	root, err := readConfigTree(path)
	if err != nil {
		return err
	}
	prof := profileMapByLabel(root, label)
	applyHostUpdateFields(prof, u)
	// A profile always carries its label (it is the selector + badge source).
	if _, ok := prof["label"]; !ok {
		prof["label"] = label
	}
	return writeJSONAtomic(path, root)
}

// SetActiveProfile writes the top-level active_profile selector. An empty name CLEARS
// it (single-user). Atomic; preserves every other key.
func SetActiveProfile(label string) error {
	path := configFileName
	root, err := readConfigTree(path)
	if err != nil {
		return err
	}
	if label == "" {
		delete(root, "active_profile")
	} else {
		root["active_profile"] = label
	}
	return writeJSONAtomic(path, root)
}

// RemoveProfile deletes the profile named label (case-insensitive) from profiles[]
// and clears active_profile when it pointed at that label. A no-op (still nil error)
// when the label is absent. Atomic; preserves every other key.
func RemoveProfile(label string) error {
	path := configFileName
	root, err := readConfigTree(path)
	if err != nil {
		return err
	}
	want := strings.ToLower(strings.TrimSpace(label))
	arr, _ := root["profiles"].([]any)
	kept := arr[:0:0]
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			if l, _ := m["label"].(string); strings.ToLower(strings.TrimSpace(l)) == want {
				continue // drop
			}
		}
		kept = append(kept, e)
	}
	if len(kept) == 0 {
		delete(root, "profiles")
	} else {
		root["profiles"] = kept
	}
	if ap, _ := root["active_profile"].(string); strings.ToLower(strings.TrimSpace(ap)) == want {
		delete(root, "active_profile")
	}
	return writeJSONAtomic(path, root)
}

// readConfigTree reads config.json into a generic mutable tree, repairing a missing
// file into an empty object so a fresh card can be written. Shared by the profile
// writers; mirrors the read prologue of WriteHostUpdate.
func readConfigTree(path string) (map[string]any, error) {
	var root map[string]any
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if uerr := json.Unmarshal(data, &root); uerr != nil {
			return nil, fmt.Errorf("parse config: %w", uerr)
		}
		if root == nil {
			root = map[string]any{}
		}
	case os.IsNotExist(err):
		root = map[string]any{}
	default:
		return nil, fmt.Errorf("read config: %w", err)
	}
	return root, nil
}

// profileMapByLabel returns the profiles[] entry whose "label" matches (case-insensitive,
// trimmed), as a mutable map, creating and appending a fresh entry when none exists.
func profileMapByLabel(root map[string]any, label string) map[string]any {
	want := strings.ToLower(strings.TrimSpace(label))
	arr, _ := root["profiles"].([]any)
	for i := range arr {
		if m, ok := arr[i].(map[string]any); ok {
			if l, _ := m["label"].(string); strings.ToLower(strings.TrimSpace(l)) == want {
				return m
			}
		}
	}
	prof := map[string]any{"label": label}
	arr = append(arr, prof)
	root["profiles"] = arr
	return prof
}

// applyHostUpdateFields writes the non-empty fields of u onto an arbitrary host/profile
// map (the shared field-write logic factored out of WriteHostUpdate so profiles and
// hosts persist identically). Server-address fields are intentionally NOT written here —
// profiles never carry their own server.
func applyHostUpdateFields(host map[string]any, u HostUpdate) {
	if u.setToken || u.Token != "" {
		setOrDelete(host, "token", u.Token)
		delete(host, "password")
	}
	if u.TokenName != "" {
		host["token_name"] = u.TokenName
	}
	if u.TokenExpiresAt != "" {
		host["token_expires_at"] = u.TokenExpiresAt
	}
	if u.Scopes != nil {
		host["scopes"] = u.Scopes
	}
	if u.Username != "" {
		host["username"] = u.Username
	}
	if u.DeviceID != "" {
		host["device_id"] = u.DeviceID
	}
	if u.DeviceName != "" {
		host["device_name"] = u.DeviceName
	}
}

// firstHostMap returns hosts[0] as a mutable map, normalizing a missing, non-array,
// empty, or non-object first element into a fresh host object wired back into root.
func firstHostMap(root map[string]any) map[string]any {
	hosts, ok := root["hosts"].([]any)
	if !ok || len(hosts) == 0 {
		host := map[string]any{}
		root["hosts"] = []any{host}
		return host
	}
	host, ok := hosts[0].(map[string]any)
	if !ok {
		host = map[string]any{}
		hosts[0] = host
		root["hosts"] = hosts
	}
	return host
}

// setOrDelete sets key=val when val is non-empty, else removes the key entirely so a
// cleared field doesn't linger as "".
func setOrDelete(m map[string]any, key, val string) {
	if val == "" {
		delete(m, key)
		return
	}
	m[key] = val
}

// writeJSONAtomic marshals v (indented, to match the hand-edited config style) and
// writes it to a sibling .tmp file at mode 0600, then renames it over path so a
// reader never sees a half-written config. The temp file is removed on any failure.
func writeJSONAtomic(path string, v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	out = append(out, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename temp config: %w", err)
	}
	// Best-effort: ensure the final file is 0600 even if it pre-existed at a looser
	// mode (rename preserves the temp's mode on most FS, but be explicit).
	_ = os.Chmod(path, 0o600)
	return nil
}

// WriteDirectoryMappings persists the directory_mappings block to config.json
// atomically, preserving every other key (hosts/token/device_id, the launchers own
