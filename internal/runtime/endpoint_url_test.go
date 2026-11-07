package runtime_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

func TestEndpointURLUsesTheConfiguredPathOrTheDefault(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "unset path is /mcp", path: "", want: "http://server.ns.svc:8080/mcp"},
		{name: "explicit root stays the root", path: "/", want: "http://server.ns.svc:8080/"},
		{name: "explicit path is unchanged", path: "/other", want: "http://server.ns.svc:8080/other"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template := testTemplate()
			template.Spec.Endpoint.Path = test.path

			assert.Equal(t, test.want, runtime.EndpointURL(db.Instance{Namespace: "ns"}, template))
		})
	}
}
