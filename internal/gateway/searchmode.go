package gateway

import (
	"net/http"
	"strings"

	"github.com/solidDoWant/toolgate/internal/auth"
)

// searchModeOf reads the tool search mode of a request: the
// X-Toolgate-Tool-Search header when it says `on` or `off`, otherwise the
// gateway's setting (docs/design/phase-4.md §5, "Search mode"). The mode is
// read on each request. It is a claimed value: it only changes what a tool
// list holds and never a decision.
func searchModeOf(request *http.Request, setting bool) bool {
	switch strings.ToLower(strings.TrimSpace(request.Header.Get(auth.ToolSearchHeader))) {
	case "on":
		return true
	case "off":
		return false
	default:
		return setting
	}
}
