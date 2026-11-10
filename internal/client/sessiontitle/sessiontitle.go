// Package sessiontitle reads a Claude Code session's title from its
// transcript, and remembers which title was last sent for each session.
//
// The transcript format isn't documented by Claude Code, so anything the
// reader doesn't expect means "no title", never an error.
package sessiontitle

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/solidDoWant/toolgate/internal/client/atomicfile"
)

const (
	// TailBytes is how much of the end of the transcript a normal read
	// looks at. Claude Code writes the title records again and again, so
	// the newest one is near the end.
	TailBytes = 1 << 20
	// WholeBytes bounds a read of the whole transcript.
	WholeBytes = 128 << 20
	// MaxLength is the longest title, in characters, after cleaning.
	MaxLength = 120
	// ResendAfter is how long a sent title stays sent. After it, the same
	// title is sent again, so it stays inside the dashboard's range for a
	// session that runs for days.
	ResendAfter = time.Hour
	// keepFor is how long a sent-title file is kept.
	keepFor = 7 * 24 * time.Hour
	// lineBuffer is the read buffer. Title records are short; a longer line
	// is skipped without being held in memory.
	lineBuffer = 64 << 10
)

// titleMarker is in every line that holds a title, so other lines are
// skipped without decoding them.
var titleMarker = []byte(`-title"`)

// record is the part of a transcript line the reader uses.
type record struct {
	Type        string `json:"type"`
	CustomTitle string `json:"customTitle"`
	AITitle     string `json:"aiTitle"`
	SessionID   string `json:"sessionId"`
}

// Read returns the session's cleaned title from the transcript at path: the
// newest `custom-title` (set by /rename), else the newest `ai-title`. It
// reads the last TailBytes, or with whole set the last WholeBytes. It
// returns "" when there is no title or the file can't be read.
func Read(path, sessionID string, whole bool) string {
	file, err := os.Open(path) //nolint:gosec // The path is the transcript Claude Code names in the hook input.
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}

	window := int64(TailBytes)
	if whole {
		window = WholeBytes
	}

	offset := max(info.Size()-window, 0)
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return ""
	}

	custom, generated := scan(bufio.NewReaderSize(io.LimitReader(file, window), lineBuffer), sessionID, offset > 0)
	if custom != "" {
		return custom
	}

	return generated
}

// scan returns the newest custom and generated titles of the session. With
// partial set, the first line is skipped, because the read started inside
// it.
func scan(reader *bufio.Reader, sessionID string, partial bool) (string, string) {
	var custom, generated string

	skipping := partial

	for {
		line, err := reader.ReadSlice('\n')

		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			// The rest of this line is skipped too.
			skipping = true

			continue
		case skipping:
			skipping = false
		case bytes.Contains(line, titleMarker):
			var entry record
			if json.Unmarshal(line, &entry) == nil && entry.SessionID == sessionID {
				switch entry.Type {
				case "custom-title":
					if title := Clean(entry.CustomTitle); title != "" {
						custom = title
					}
				case "ai-title":
					if title := Clean(entry.AITitle); title != "" {
						generated = title
					}
				}
			}
		}

		if err != nil {
			return custom, generated
		}
	}
}

// Clean turns control characters and runs of white space into one space,
// trims the ends, and cuts a title longer than MaxLength characters to
// MaxLength-1 followed by "…".
func Clean(title string) string {
	fields := strings.FieldsFunc(strings.ToValidUTF8(title, ""), func(character rune) bool {
		return unicode.IsSpace(character) || unicode.IsControl(character)
	})
	cleaned := strings.Join(fields, " ")

	if utf8.RuneCountInString(cleaned) <= MaxLength {
		return cleaned
	}

	return strings.TrimSpace(string([]rune(cleaned)[:MaxLength-1])) + "…"
}

// sessionIDPattern is Claude Code's session ID, a UUID. Only such an ID
// names a sent-title file, so an ID can't name another path.
var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidSessionID reports whether id is a UUID.
func ValidSessionID(id string) bool {
	return sessionIDPattern.MatchString(id)
}

// Sent is the title last sent for a session, and when.
type Sent struct {
	Title string    `json:"title"`
	At    time.Time `json:"at"`
}

// Store keeps the last sent title of each session, one file per session.
type Store struct {
	Dir string
}

// Last returns the title last sent for the session, and whether there is
// one.
func (s Store) Last(sessionID string) (Sent, bool) {
	if !ValidSessionID(sessionID) {
		return Sent{}, false
	}

	content, err := os.ReadFile(filepath.Join(s.Dir, sessionID))
	if err != nil {
		return Sent{}, false
	}

	var sent Sent
	if json.Unmarshal(content, &sent) != nil {
		return Sent{}, false
	}

	return sent, true
}

// Due reports whether title should be sent at now: it differs from the last
// one sent, or that was sent more than ResendAfter ago.
func Due(last Sent, found bool, title string, now time.Time) bool {
	return !found || last.Title != title || now.Sub(last.At) > ResendAfter
}

// Record stores title as sent at now, and removes files older than a week.
func (s Store) Record(sessionID, title string, now time.Time) error {
	if !ValidSessionID(sessionID) {
		return errors.New("the session ID is not a UUID")
	}

	content, err := json.Marshal(Sent{Title: title, At: now})
	if err != nil {
		return err
	}

	if err := atomicfile.Write(filepath.Join(s.Dir, sessionID), content, 0o600); err != nil {
		return err
	}

	s.prune(now)

	return nil
}

// prune removes the sent-title files not written for keepFor.
func (s Store) prune(now time.Time) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !ValidSessionID(entry.Name()) {
			continue
		}

		info, err := entry.Info()
		if err == nil && now.Sub(info.ModTime()) > keepFor {
			_ = os.Remove(filepath.Join(s.Dir, entry.Name()))
		}
	}
}
