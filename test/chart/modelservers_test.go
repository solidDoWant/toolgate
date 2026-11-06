package chart

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func kindsNamed(manifests []manifest) map[string]bool {
	out := map[string]bool{}
	for _, item := range manifests {
		out[item.Kind+"/"+item.Metadata.Name] = true
	}

	return out
}

func TestModelServersAreOffByDefault(t *testing.T) {
	names := kindsNamed(renderQuickStart(t))

	for name := range names {
		assert.NotContains(t, name, "embeddings")
	}
}

func TestModelServersRenderAndWireTheGateway(t *testing.T) {
	manifests := renderQuickStart(t, "--set", "embeddings.server.enabled=true")
	names := kindsNamed(manifests)

	for _, name := range []string{
		"Deployment/" + releaseName + "-embeddings", "Service/" + releaseName + "-embeddings",
		"Job/" + releaseName + "-embeddings-download",
		"PersistentVolumeClaim/" + releaseName + "-models",
	} {
		assert.True(t, names[name], "renders %s", name)
	}

	var all strings.Builder

	for _, item := range manifests {
		all.Write(item.raw)

		assert.NotEqual(t, "NetworkPolicy", item.Kind)
	}

	text := all.String()
	assert.Contains(t, text, "http://"+releaseName+"-embeddings."+releaseNamespace+".svc:80")
	assert.Contains(t, text, "HF_HUB_OFFLINE")
}

func TestModelServerURLSetByTheAdminWins(t *testing.T) {
	manifests := renderQuickStart(t, "--set", "embeddings.server.enabled=true", "--set", "config.toolSearchEmbeddings.url=http://external:80")

	var all strings.Builder
	for _, item := range manifests {
		all.Write(item.raw)
	}

	assert.Contains(t, all.String(), "http://external:80")
	assert.NotContains(t, all.String(), "-embeddings."+releaseNamespace)
}

func TestModelServerNeedsAPinnedRevision(t *testing.T) {
	_, err := render(t, append([]string{"embeddings.server.enabled=true", "embeddings.server.modelRevision=main"}, quickStartValues...))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "modelRevision")
}
