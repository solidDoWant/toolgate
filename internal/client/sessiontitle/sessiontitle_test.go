package sessiontitle_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/client/sessiontitle"
)

const (
	session = "3c1a0758-ff40-4ddf-8de5-64b8b29ec323"
	other   = "d1d1db7c-8709-499f-b644-350cbee6d119"
)

func aiTitle(id, title string) string {
	line, _ := json.Marshal(map[string]string{"type": "ai-title", "aiTitle": title, "sessionId": id})

	return string(line)
}

func customTitle(id, title string) string {
	line, _ := json.Marshal(map[string]string{"type": "custom-title", "customTitle": title, "sessionId": id})

	return string(line)
}

// message is a transcript line that holds no title.
func message(text string) string {
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]string{"content": text}, "sessionId": session})

	return string(line)
}

func transcript(t *testing.T, lines ...string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), session+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))

	return path
}

func TestReadTakesTheNewestGeneratedTitle(t *testing.T) {
	path := transcript(t, message("hi"), aiTitle(session, "First idea"), message("more"), aiTitle(session, "Better idea"))

	assert.Equal(t, "Better idea", sessiontitle.Read(path, session, false))
}

func TestReadPrefersARenameOverANewerGeneratedTitle(t *testing.T) {
	path := transcript(t, aiTitle(session, "Generated"), customTitle(session, "Old name"), customTitle(session, "My name"), aiTitle(session, "Generated again"))

	assert.Equal(t, "My name", sessiontitle.Read(path, session, false))
}

func TestReadSkipsTitlesOfOtherSessions(t *testing.T) {
	path := transcript(t, aiTitle(session, "Mine"), aiTitle(other, "Theirs"), customTitle(other, "Their name"))

	assert.Equal(t, "Mine", sessiontitle.Read(path, session, false))
}

func TestReadGivesNoTitleForMissingOrBrokenTranscripts(t *testing.T) {
	assert.Empty(t, sessiontitle.Read(filepath.Join(t.TempDir(), "missing.jsonl"), session, false))
	assert.Empty(t, sessiontitle.Read(t.TempDir(), session, true), "a directory")

	broken := transcript(t, `{"type":"ai-title","aiTitle":`, "not json -title\"", `{"type":"ai-title","sessionId":"`+session+`"}`)
	assert.Empty(t, sessiontitle.Read(broken, session, false))

	unknown := transcript(t, `{"type":"some-new-title","title":"x","sessionId":"`+session+`"}`)
	assert.Empty(t, sessiontitle.Read(unknown, session, false))
}

func TestReadLooksOnlyAtTheEndUnlessAskedForTheWholeFile(t *testing.T) {
	filler := message(strings.Repeat("x", 4096))
	lines := []string{aiTitle(session, "Old title")}

	for range sessiontitle.TailBytes/len(filler) + 2 {
		lines = append(lines, filler)
	}

	path := transcript(t, lines...)

	assert.Empty(t, sessiontitle.Read(path, session, false), "the title is before the last 1 MiB")
	assert.Equal(t, "Old title", sessiontitle.Read(path, session, true))
}

func TestReadSkipsThePartialFirstLineOfTheTail(t *testing.T) {
	// A title record that starts before the tail is cut in half by the read,
	// and must not count.
	title := aiTitle(session, "Cut")
	padding := sessiontitle.TailBytes - len(title)/2

	path := transcript(t, title, strings.Repeat("y", padding-1))

	assert.Empty(t, sessiontitle.Read(path, session, false))
}

func TestReadSkipsLinesLongerThanItsBuffer(t *testing.T) {
	path := transcript(t, message(strings.Repeat("z", 200<<10)+`-title"`), aiTitle(session, "After a long line"))

	assert.Equal(t, "After a long line", sessiontitle.Read(path, session, false))
}

func TestCleanJoinsWhiteSpaceAndCutsLongTitles(t *testing.T) {
	assert.Equal(t, "Fix the login redirect", sessiontitle.Clean("  Fix\tthe\n login\x00\x07 redirect  "))
	assert.Empty(t, sessiontitle.Clean(" \n\t "))

	long := sessiontitle.Clean(strings.Repeat("é", 200))
	assert.Equal(t, sessiontitle.MaxLength, len([]rune(long)))
	assert.True(t, strings.HasSuffix(long, "…"))

	exact := strings.Repeat("a", sessiontitle.MaxLength)
	assert.Equal(t, exact, sessiontitle.Clean(exact))
}

func TestReadIgnoresTitlesThatAreEmptyAfterCleaning(t *testing.T) {
	path := transcript(t, aiTitle(session, "Real title"), customTitle(session, " \t "))

	assert.Equal(t, "Real title", sessiontitle.Read(path, session, false))
}

func TestStoreRecordsSentTitlesAndDecidesWhenToSendAgain(t *testing.T) {
	store := sessiontitle.Store{Dir: filepath.Join(t.TempDir(), "titles")}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	last, found := store.Last(session)
	assert.False(t, found)
	assert.True(t, sessiontitle.Due(last, found, "A", now), "never sent")

	require.NoError(t, store.Record(session, "A", now))

	last, found = store.Last(session)
	require.True(t, found)
	assert.Equal(t, "A", last.Title)
	assert.False(t, sessiontitle.Due(last, found, "A", now.Add(30*time.Minute)), "unchanged and sent recently")
	assert.True(t, sessiontitle.Due(last, found, "B", now.Add(time.Minute)), "changed")
	assert.True(t, sessiontitle.Due(last, found, "A", now.Add(sessiontitle.ResendAfter+time.Second)), "sent over an hour ago")
}

func TestStoreRefusesSessionIDsThatAreNotUUIDs(t *testing.T) {
	dir := t.TempDir()
	store := sessiontitle.Store{Dir: dir}

	require.Error(t, store.Record("../escape", "A", time.Now()))

	_, found := store.Last("../escape")
	assert.False(t, found)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestStoreRemovesFilesOlderThanAWeek(t *testing.T) {
	store := sessiontitle.Store{Dir: t.TempDir()}
	now := time.Now()

	require.NoError(t, store.Record(other, "Old", now))

	old := now.Add(-8 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(store.Dir, other), old, old))

	require.NoError(t, store.Record(session, "New", now))

	_, found := store.Last(other)
	assert.False(t, found, "the week-old file is gone")

	_, found = store.Last(session)
	assert.True(t, found)
}
