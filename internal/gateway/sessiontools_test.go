package gateway

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

func TestStoredRequestContextHasNoSubjectToken(t *testing.T) {
	t.Parallel()

	request := func(id int64, token auth.SubjectToken) requestContext {
		return requestContext{
			identity:     auth.Identity{Principal: "alice", Kind: auth.KindUser},
			session:      db.Session{ID: id},
			subjectToken: token,
		}
	}

	t.Run("new session tools", func(t *testing.T) {
		t.Parallel()

		tools := newSessionTools(nil, nil, request(1, "first-token"))

		stored := tools.currentRequest()
		assert.Empty(t, stored.subjectToken.Reveal())
		assert.Equal(t, int64(1), stored.session.ID)
	})

	t.Run("set current", func(t *testing.T) {
		t.Parallel()

		tools := newSessionTools(nil, nil, requestContext{})

		current := request(2, "second-token")
		tools.setCurrent(current)

		stored := tools.currentRequest()
		assert.Empty(t, stored.subjectToken.Reveal())
		assert.Equal(t, int64(2), stored.session.ID)
		assert.Equal(t, "second-token", current.subjectToken.Reveal(), "the caller's copy keeps its token")
	})
}
