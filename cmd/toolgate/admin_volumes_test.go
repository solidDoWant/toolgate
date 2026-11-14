package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdminVolumesDeleteReportsUnknownIDsAndFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "Bearer token", request.Header.Get("Authorization"))

		switch request.URL.Path {
		case "/api/v1/admin/volumes/aaa":
			writer.WriteHeader(http.StatusAccepted)
			_, _ = writer.Write([]byte(`{"id":"aaa","inUseBy":null,"deleteReason":"admin"}`))
		case "/api/v1/admin/volumes/bbb":
			writer.WriteHeader(http.StatusAccepted)
			_, _ = writer.Write([]byte(`{"id":"bbb","inUseBy":"inst1","deleteReason":"admin"}`))
		default:
			http.Error(writer, "No such volume.", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	access := gatewayAccess{gateway: server.URL, accessToken: "token"}

	var out bytes.Buffer

	missing, err := deleteVolumesWith(t.Context(), access, []string{"aaa", "unknown", "bbb"}, &out)
	require.NoError(t, err)
	assert.True(t, missing, "an unknown ID makes the command exit 1")
	assert.Contains(t, out.String(), "aaa: marked for deletion\n")
	assert.Contains(t, out.String(), "unknown: no such volume\n")
	assert.Contains(t, out.String(), "bbb: marked for deletion; instance inst1 uses it")

	out.Reset()

	missing, err = deleteVolumesWith(t.Context(), access, []string{"aaa"}, &out)
	require.NoError(t, err)
	assert.False(t, missing)
}

func TestAdminVolumesListFollowsPagesAndPrintsTable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "alice", request.URL.Query().Get("owner"))

		if request.URL.Query().Get("after") == "" {
			_, _ = writer.Write([]byte(`{"volumes":[{"id":"a1","server":"eparts","volume":"cache","scope":"per-user","owner":"alice","sizeBytes":1073741824,"inUseBy":"i9","lastUsedAt":"2026-11-13T12:00:00Z","deleteReason":""}],"next":"a1"}`))

			return
		}

		assert.Equal(t, "a1", request.URL.Query().Get("after"))

		_, _ = writer.Write([]byte(`{"volumes":[{"id":"a2","server":"eparts","volume":"data","scope":"per-template","owner":"","sizeBytes":1536,"inUseBy":null,"lastUsedAt":"2026-11-13T12:00:00Z","deleteReason":"admin"}]}`))
	}))
	t.Cleanup(server.Close)

	var out, errOut bytes.Buffer

	require.NoError(t, listVolumesWith(t.Context(), gatewayAccess{gateway: server.URL, accessToken: "t"}, "alice", false, &out, &errOut))

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 3, out.String())
	assert.Regexp(t, `^ID\s+SERVER\s+VOLUME\s+SCOPE\s+OWNER\s+SIZE\s+IN USE BY\s+LAST USED\s+DELETION$`, lines[0])
	assert.Regexp(t, `^a1\s+eparts\s+cache\s+per-user\s+alice\s+1Gi\s+i9\s+2026-11-13T12:00:00Z\s+-$`, lines[1])
	assert.Regexp(t, `^a2\s+eparts\s+data\s+per-template\s+-\s+1.5Ki\s+-\s+2026-11-13T12:00:00Z\s+admin$`, lines[2])
	assert.Empty(t, errOut.String())
}
