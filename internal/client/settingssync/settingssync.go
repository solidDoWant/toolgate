// Package settingssync merges the settings fragment the gateway serves into
// Claude Code's settings file, changing only the keys toolgate owns
// (SPEC.md §13, docs/design/phase-1.md §5, "Settings sync").
package settingssync

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/solidDoWant/toolgate/internal/client/atomicfile"
	"github.com/solidDoWant/toolgate/internal/settings"
)

// OwnedFileName is the file, in the toolgate configuration directory, that
// records the value toolgate last wrote for each key it owns.
const OwnedFileName = "owned-settings.json"

// MaxFileBytes bounds the settings and owned-settings files Sync reads.
const MaxFileBytes = 4 << 20

// Key is a settings key: a top-level key, or a top-level key from
// settings.NestedKeys followed by one key inside it.
type Key []string

// String joins the key with dots, for reports.
func (k Key) String() string {
	return strings.Join(k, ".")
}

// Result says what Sync did.
type Result struct {
	// Written keys were set to the profile's value.
	Written []string
	// Removed keys were removed from the file because the profile no longer
	// sets them.
	Removed []string
	// UserChanged keys hold a value the user set, so Sync left them alone.
	UserChanged []string
}

// ownedEntry is one key in the owned-settings file.
type ownedEntry struct {
	Key   Key             `json:"key"`
	Value json.RawMessage `json:"value"`
}

type ownedFile struct {
	Keys []ownedEntry `json:"keys"`
}

// Sync merges profile (the gateway's merged settings) into the settings file
// at settingsPath. ownedPath records what toolgate wrote last time. For each
// key, Sync writes the profile's value only when the file still holds the
// value toolgate last wrote, or when toolgate never wrote the key and the
// file doesn't have it. A key toolgate owns that the profile no longer sets
// is removed when the file still holds toolgate's value, and is no longer
// owned either way. Every key Sync can't change is reported as user-changed.
// Both files are written atomically, and only when something changed.
func Sync(settingsPath, ownedPath string, profile map[string]json.RawMessage) (Result, error) {
	var result Result

	current, mode, err := readObject(settingsPath)
	if err != nil {
		return result, err
	}

	owned, err := readOwned(ownedPath)
	if err != nil {
		return result, err
	}

	wanted := flatten(profile)
	nextOwned := []ownedEntry{}
	changed := false

	for _, entry := range wanted {
		fileValue, present, settable := lookup(current, entry.Key)
		lastValue, isOwned := findOwned(owned, entry.Key)

		writable := settable && (isOwned && present && equal(fileValue, lastValue) || !isOwned && !present)
		if !writable {
			result.UserChanged = append(result.UserChanged, entry.Key.String())

			if isOwned {
				nextOwned = append(nextOwned, ownedEntry{Key: entry.Key, Value: lastValue})
			}

			continue
		}

		if !present || !equal(fileValue, entry.Value) {
			if err := set(current, entry.Key, entry.Value); err != nil {
				return Result{}, err
			}

			changed = true

			result.Written = append(result.Written, entry.Key.String())
		}

		nextOwned = append(nextOwned, ownedEntry{Key: entry.Key, Value: entry.Value})
	}

	for _, entry := range owned {
		if slices.ContainsFunc(wanted, func(other ownedEntry) bool { return slices.Equal(other.Key, entry.Key) }) {
			continue
		}

		fileValue, present, _ := lookup(current, entry.Key)

		switch {
		case !present:
		case equal(fileValue, entry.Value):
			remove(current, entry.Key)

			changed = true

			result.Removed = append(result.Removed, entry.Key.String())
		default:
			result.UserChanged = append(result.UserChanged, entry.Key.String())
		}
	}

	if changed {
		encoded, err := json.MarshalIndent(current, "", "  ")
		if err != nil {
			return Result{}, err
		}

		if err := atomicfile.Write(settingsPath, append(encoded, '\n'), mode); err != nil {
			return Result{}, err
		}
	}

	if !sameOwned(owned, nextOwned) {
		encoded, err := json.MarshalIndent(ownedFile{Keys: nextOwned}, "", "  ")
		if err != nil {
			return Result{}, err
		}

		if err := atomicfile.Write(ownedPath, append(encoded, '\n'), 0o600); err != nil {
			return Result{}, err
		}
	}

	return result, nil
}

// flatten splits profile into its keys, sorted.
func flatten(profile map[string]json.RawMessage) []ownedEntry {
	entries := []ownedEntry{}

	for key, value := range profile {
		var inner map[string]json.RawMessage
		if slices.Contains(settings.NestedKeys, key) && json.Unmarshal(value, &inner) == nil && inner != nil {
			for innerKey, innerValue := range inner {
				entries = append(entries, ownedEntry{Key: Key{key, innerKey}, Value: innerValue})
			}

			continue
		}

		entries = append(entries, ownedEntry{Key: Key{key}, Value: value})
	}

	slices.SortFunc(entries, func(a, b ownedEntry) int { return slices.Compare(a.Key, b.Key) })

	return entries
}

// lookup returns the file's value for key, whether it is present, and
// whether Sync could set it (a nested key can't be set when its parent holds
// something other than an object).
func lookup(object map[string]json.RawMessage, key Key) (json.RawMessage, bool, bool) {
	value, ok := object[key[0]]
	if len(key) == 1 {
		return value, ok, true
	}

	if !ok {
		return nil, false, true
	}

	var inner map[string]json.RawMessage
	if json.Unmarshal(value, &inner) != nil || inner == nil {
		return nil, false, false
	}

	innerValue, ok := inner[key[1]]

	return innerValue, ok, true
}

func set(object map[string]json.RawMessage, key Key, value json.RawMessage) error {
	if len(key) == 1 {
		object[key[0]] = value

		return nil
	}

	inner := map[string]json.RawMessage{}
	if existing, ok := object[key[0]]; ok {
		if err := json.Unmarshal(existing, &inner); err != nil || inner == nil {
			return fmt.Errorf("settings key %s is not an object", key[0])
		}
	}

	inner[key[1]] = value

	encoded, err := json.Marshal(inner)
	if err != nil {
		return err
	}

	object[key[0]] = encoded

	return nil
}

func remove(object map[string]json.RawMessage, key Key) {
	if len(key) == 1 {
		delete(object, key[0])

		return
	}

	var inner map[string]json.RawMessage
	if json.Unmarshal(object[key[0]], &inner) != nil {
		return
	}

	delete(inner, key[1])

	encoded, err := json.Marshal(inner)
	if err == nil {
		object[key[0]] = encoded
	}
}

// equal compares two JSON values as parsed JSON.
func equal(a, b json.RawMessage) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}

	return reflect.DeepEqual(left, right)
}

func findOwned(owned []ownedEntry, key Key) (json.RawMessage, bool) {
	for _, entry := range owned {
		if slices.Equal(entry.Key, key) {
			return entry.Value, true
		}
	}

	return nil, false
}

func sameOwned(a, b []ownedEntry) bool {
	return slices.EqualFunc(a, b, func(left, right ownedEntry) bool {
		return slices.Equal(left.Key, right.Key) && equal(left.Value, right.Value)
	})
}

// readObject reads the JSON object in path, and its file mode. A missing
// file is an empty object.
func readObject(path string) (map[string]json.RawMessage, fs.FileMode, error) {
	content, mode, err := readBounded(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]json.RawMessage{}, 0o600, nil
	}

	if err != nil {
		return nil, 0, err
	}

	object := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(content))) == 0 {
		return object, mode, nil
	}

	if err := json.Unmarshal(content, &object); err != nil || object == nil {
		return nil, 0, fmt.Errorf("%s does not hold a JSON object", path)
	}

	return object, mode, nil
}

func readOwned(path string) ([]ownedEntry, error) {
	content, _, err := readBounded(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var owned ownedFile
	if err := json.Unmarshal(content, &owned); err != nil {
		return nil, fmt.Errorf("%s is not valid: %w", path, err)
	}

	valid := owned.Keys[:0]

	for _, entry := range owned.Keys {
		if len(entry.Key) == 1 || len(entry.Key) == 2 && slices.Contains(settings.NestedKeys, entry.Key[0]) {
			valid = append(valid, entry)
		}
	}

	return valid, nil
}

func readBounded(path string) ([]byte, fs.FileMode, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}

	content, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", path, err)
	}

	if len(content) > MaxFileBytes {
		return nil, 0, fmt.Errorf("%s is larger than %d bytes", path, MaxFileBytes)
	}

	return content, info.Mode().Perm(), nil
}
