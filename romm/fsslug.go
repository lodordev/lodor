package romm

import "encoding/json"

// KnownFsSlug reports whether the engine maps a RomM fs_slug to a device folder. The
// platform package sets it; nil leaves every fs_slug as the server sent it.
var KnownFsSlug func(string) bool

// resolveFsSlug keeps fs_slug when the engine knows it. A library folder with a custom
// name ("Gameboy Advance") has an fs_slug no table lists, while RomM still identifies
// the platform by its canonical slug ("gba"); only then is the canonical slug used.
func resolveFsSlug(fsSlug, slug string) string {
	if KnownFsSlug == nil || slug == "" || KnownFsSlug(fsSlug) || !KnownFsSlug(slug) {
		return fsSlug
	}
	return slug
}

func (p *Platform) UnmarshalJSON(b []byte) error {
	type plain Platform
	if err := json.Unmarshal(b, (*plain)(p)); err != nil {
		return err
	}
	p.FsSlug = resolveFsSlug(p.FsSlug, p.Slug)
	return nil
}

func (r *Rom) UnmarshalJSON(b []byte) error {
	type plain Rom
	if err := json.Unmarshal(b, (*plain)(r)); err != nil {
		return err
	}
	r.PlatformFsSlug = resolveFsSlug(r.PlatformFsSlug, r.PlatformSlug)
	return nil
}
