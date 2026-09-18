package state

import (
	"encoding/json"
	"fmt"
)

// migrateFromVersion1 upgrades a state.json or manifest.json from version
// 1, the only version belay v0.1.0 to v0.1.4 wrote, to version 2. It is
// registered for both files, whose version histories are the same so far.
//
// Version 2 changes no field's shape or meaning. It exists so that those
// binaries, which read exactly version 1 and ignore every key they do not
// know, refuse a run a later belay has written instead of resuming it and
// saving it without what they could not read (ADR-0011). So the migration
// only stamps the new version and gives the document the requires list
// every version-2 document carries: an empty one where the key is missing,
// as every released belay left it, or null.
//
// It deliberately keeps a requires list that is already there, even at
// version 1, where only development builds wrote one. Emptying it would
// drop exactly the kind of requirement the list exists to enforce; the
// checkRequires that follows every migration judges it like any other.
//
// It works on a map of raw values rather than on State or Manifest, so
// every other key reaches the decode that follows byte for byte as it was
// written, whatever those types look like by then.
func migrateFromVersion1(raw []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode version 1: %w", err)
	}
	doc["schema_version"] = json.RawMessage(`2`)
	if requires, ok := doc["requires"]; !ok || string(requires) == "null" {
		doc["requires"] = json.RawMessage(`[]`)
	}
	migrated, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode version 2: %w", err)
	}
	return migrated, nil
}
