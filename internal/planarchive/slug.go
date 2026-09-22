package planarchive

import (
	"path"
	"strings"
)

// maxSlug is the longest a slug may be, in bytes. With a run id in front
// and ".md" behind, an archived plan's name stays under 90 bytes: short
// enough to read in a directory listing, and nowhere near the 255 bytes a
// file name may take.
const maxSlug = 48

// emptySlug names the plan of a goal that has no ASCII letter or digit in
// it, so every archived plan's name still says what the file is.
const emptySlug = "plan"

// Slug returns the readable half of an archived plan's name: goal in
// lower-case ASCII letters and digits, with every run of anything else in
// between turned into one hyphen, and no hyphen at either end.
//
// Anything else means everything else. A slash would make a directory, a
// colon or a quote is refused by some file system or some shell, and a
// newline is where a goal typed as a paragraph continues; none of them can
// be part of a name, so none of them is. A letter outside ASCII is a
// separator too, rather than being transliterated: that would need a
// Unicode table belay does not carry, and git and macOS disagree about how
// to normalise non-ASCII names (NFC against NFD), so the same plan could
// otherwise end up with two names in two checkouts.
//
// A slug longer than maxSlug is cut at the last whole word that fits, so a
// name never ends part way through one; only a single word longer than
// maxSlug is cut where the limit falls. A goal with nothing left is
// emptySlug.
func Slug(goal string) string {
	var b strings.Builder
	gap := false
	for _, r := range goal {
		switch {
		case 'a' <= r && r <= 'z', '0' <= r && r <= '9':
		case 'A' <= r && r <= 'Z':
			r += 'a' - 'A'
		default:
			// A separator is written only when a letter follows it, which
			// is what keeps hyphens off both ends and never doubles one.
			gap = b.Len() > 0
			continue
		}
		if gap {
			b.WriteByte('-')
			gap = false
		}
		b.WriteRune(r)
	}

	slug := b.String()
	if len(slug) > maxSlug {
		// Every byte is ASCII, so cutting by byte cuts by character.
		cut := maxSlug
		if slug[maxSlug] != '-' {
			if i := strings.LastIndexByte(slug[:maxSlug], '-'); i > 0 {
				cut = i
			}
		}
		slug = slug[:cut]
	}
	if slug == "" {
		return emptySlug
	}
	return slug
}

// Filename returns the name a run's plan is archived under:
// <run-id>-<slug>.md.
//
// The run id comes first so archived plans sort by when their run started,
// and so the name begins with exactly what `belay timeline` and `belay plan`
// take. The slug is only for the person reading the directory; two runs
// with the same goal still get two names, because their ids differ.
func Filename(runID, goal string) string {
	return runID + "-" + Slug(goal) + ".md"
}

// Rel returns where a run's plan is archived, relative to the tree the run
// works in: dir, as the run's manifest pinned it, joined with Filename. It
// is slash-separated, like dir, because it is recorded in the manifest and
// must mean the same file on every machine the repository is checked out on.
func Rel(dir, runID, goal string) string {
	return path.Join(dir, Filename(runID, goal))
}
