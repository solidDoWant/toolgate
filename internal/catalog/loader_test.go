package catalog_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/catalog"
)

func TestLoaderMarksTheCatalogLoadedOnceItHoldsEveryTemplate(t *testing.T) {
	first := newTemplate("github", catalog.ScopePerUser, serviceResource)
	second := newTemplate("jira", catalog.ScopePerUser, serviceResource)
	second.Namespace = "catalog-b"
	fixture := newReconcilerFixture(t, first, second)

	loader := &catalog.Loader{
		Reader: fixture.client, Catalog: fixture.catalog, Namespaces: fixture.reconciler.Namespaces,
		PollInterval: time.Millisecond,
	}

	result := make(chan error, 1)

	go func() { result <- loader.Start(t.Context()) }()

	fixture.reconcile(t, keyOf(first))

	// One of two templates is read: the loader must not finish. A bounded
	// Start returns only through its context.
	bounded, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	partial := &catalog.Loader{Reader: fixture.client, Catalog: fixture.catalog, Namespaces: fixture.reconciler.Namespaces}
	require.ErrorIs(t, partial.Start(bounded), context.DeadlineExceeded)
	assert.False(t, fixture.catalog.IsLoaded())

	fixture.reconcile(t, keyOf(second))

	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		require.FailNow(t, "the loader did not finish once every template was read")
	}

	assert.True(t, fixture.catalog.IsLoaded())

	select {
	case <-fixture.catalog.Loaded():
	default:
		assert.Fail(t, "Loaded is closed")
	}
}
