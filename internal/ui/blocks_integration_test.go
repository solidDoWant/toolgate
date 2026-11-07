//go:build integration

package ui_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/ui"
)

// fakeRules is the Blocks page's rule source.
type fakeRules struct{ rules []v1alpha1.ToolgateRule }

func (f *fakeRules) Rules(context.Context) ([]v1alpha1.ToolgateRule, error) { return f.rules, nil }

func (e *uiEnv) adminBrowser(t *testing.T) (*http.Client, string) {
	t.Helper()

	return e.insertSession(t, "admin-session", []string{"toolgate-admins"}), csrfTokenOf("admin-session")
}

func validBlock(token string) url.Values {
	return url.Values{
		ui.CSRFField: {token}, "servers": {"github\nwiki"}, "reason": {"Incident 7."}, "minutes": {"60"},
	}
}

func TestBlocksPage(t *testing.T) {
	t.Run("a non-admin gets 403 from the page and the form, and has no Blocks link", func(t *testing.T) {
		env := startUIEnv(t, "toolgate-admins")
		user := env.insertSession(t, "user-session", []string{"dev"})

		assert.Equal(t, http.StatusForbidden, get(t, user, env.server.URL+"/ui/blocks").StatusCode)
		assert.Equal(t, http.StatusForbidden, post(t, user, env.server.URL+"/ui/blocks", validBlock(csrfTokenOf("user-session"))).StatusCode)
		assert.Zero(t, env.countRows(t, "temporary_blocks"))

		page := readBody(t, get(t, user, env.server.URL+"/ui/servers"))
		assert.NotContains(t, page, `href="/ui/blocks"`)
	})

	t.Run("an admin creates a block with its event and notification, and sees it live", func(t *testing.T) {
		env := startUIEnv(t, "toolgate-admins")
		admin, token := env.adminBrowser(t)

		listener := store.NewListener(env.pool, slog.New(slog.DiscardHandler))

		events, unsubscribe := listener.Subscribe()
		defer unsubscribe()

		go func() { _ = listener.Run(t.Context()) }()

		select {
		case <-listener.Listening():
		case <-time.After(30 * time.Second):
			t.Fatal("listener did not start listening")
		}

		response := post(t, admin, env.server.URL+"/ui/blocks", validBlock(token))
		require.Equal(t, http.StatusSeeOther, response.StatusCode)

		var (
			id      string
			created string
			expires time.Duration
		)

		require.NoError(t, env.pool.QueryRow(t.Context(),
			`SELECT id, created_by, expires_at - created_at FROM temporary_blocks`).Scan(&id, &created, &expires))
		assert.Equal(t, "dave", created)
		assert.Equal(t, time.Hour, expires)

		var match string
		require.NoError(t, env.pool.QueryRow(t.Context(), `SELECT match::text FROM temporary_blocks`).Scan(&match))
		assert.JSONEq(t, `{"servers":["github","wiki"]}`, match)

		select {
		case event := <-events:
			assert.Equal(t, store.KindBlock, event.Kind)
			assert.Equal(t, id, event.ID)
		case <-time.After(30 * time.Second):
			t.Fatal("no block notification")
		}

		var count int
		require.NoError(t, env.pool.QueryRow(t.Context(),
			`SELECT count(*) FROM audit_events WHERE type = 'block.create' AND details->>'block' = $1 AND attestation = 'user'`, id).Scan(&count))
		assert.Equal(t, 1, count)

		page := readBody(t, get(t, admin, env.server.URL+"/ui/blocks"))
		assert.Contains(t, page, id)
		assert.Contains(t, page, "Incident 7.")
		assert.Contains(t, page, "servers: github, wiki")
	})

	t.Run("a refused form changes nothing", func(t *testing.T) {
		env := startUIEnv(t, "toolgate-admins")
		admin, token := env.adminBrowser(t)

		list := func(count int) string { return strings.Repeat("a\n", count) }

		tests := []struct {
			name   string
			change url.Values
			want   int
		}{
			{"no field", url.Values{"servers": {""}}, http.StatusBadRequest},
			{"bad glob", url.Values{"servers": {"[a"}}, http.StatusBadRequest},
			{"65 items", url.Values{"servers": {list(65)}}, http.StatusBadRequest},
			{"item of 257 characters", url.Values{"tools": {strings.Repeat("a", 257)}}, http.StatusBadRequest},
			{"reason of 1025 characters", url.Values{"reason": {strings.Repeat("a", 1025)}}, http.StatusBadRequest},
			{"no reason", url.Values{"reason": {" "}}, http.StatusBadRequest},
			{"0 minutes", url.Values{"minutes": {"0"}}, http.StatusBadRequest},
			{"30 days and a minute", url.Values{"minutes": {"43201"}}, http.StatusBadRequest},
			{"not a number", url.Values{"minutes": {"soon"}}, http.StatusBadRequest},
			{"bad attestation", url.Values{"attestation": {"both"}}, http.StatusBadRequest},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				form := validBlock(token)
				for key, value := range test.change {
					form[key] = value
				}

				assert.Equal(t, test.want, post(t, admin, env.server.URL+"/ui/blocks", form).StatusCode)
				assert.Zero(t, env.countRows(t, "temporary_blocks"))
			})
		}

		t.Run("no token", func(t *testing.T) {
			form := validBlock("")
			assert.Equal(t, http.StatusForbidden, post(t, admin, env.server.URL+"/ui/blocks", form).StatusCode)
			assert.Zero(t, env.countRows(t, "temporary_blocks"))
		})
	})

	t.Run("the limits are accepted at exactly their bound", func(t *testing.T) {
		env := startUIEnv(t, "toolgate-admins")
		admin, token := env.adminBrowser(t)

		form := validBlock(token)
		form["servers"] = []string{strings.TrimSuffix(strings.Repeat("a\n", 64), "\n")}
		form["tools"] = []string{strings.Repeat("a", 256)}
		form["reason"] = []string{strings.Repeat("a", 1024)}
		form["minutes"] = []string{"43200"}

		assert.Equal(t, http.StatusSeeOther, post(t, admin, env.server.URL+"/ui/blocks", form).StatusCode)

		form["minutes"] = []string{"1"}
		assert.Equal(t, http.StatusSeeOther, post(t, admin, env.server.URL+"/ui/blocks", form).StatusCode)
		assert.Equal(t, 2, env.countRows(t, "temporary_blocks"))
	})

	t.Run("the duration takes a number and a unit, bounded at 30 days", func(t *testing.T) {
		env := startUIEnv(t, "toolgate-admins")
		admin, token := env.adminBrowser(t)

		for _, test := range []struct {
			duration, unit string
			want           int
			lifetime       time.Duration
		}{
			{"1", "minutes", http.StatusSeeOther, time.Minute},
			{"2", "hours", http.StatusSeeOther, 2 * time.Hour},
			{"30", "days", http.StatusSeeOther, 30 * 24 * time.Hour},
			{"720", "hours", http.StatusSeeOther, 30 * 24 * time.Hour},
			{"43200", "minutes", http.StatusSeeOther, 30 * 24 * time.Hour},
			{"31", "days", http.StatusBadRequest, 0},
			{"721", "hours", http.StatusBadRequest, 0},
			{"43201", "minutes", http.StatusBadRequest, 0},
			{"0", "hours", http.StatusBadRequest, 0},
			{"1", "weeks", http.StatusBadRequest, 0},
			{"9223372036854775807", "days", http.StatusBadRequest, 0},
		} {
			t.Run(test.duration+" "+test.unit, func(t *testing.T) {
				_, err := env.pool.Exec(t.Context(), `DELETE FROM temporary_blocks`)
				require.NoError(t, err)

				form := validBlock(token)
				delete(form, "minutes")
				form.Set("duration", test.duration)
				form.Set("unit", test.unit)

				require.Equal(t, test.want, post(t, admin, env.server.URL+"/ui/blocks", form).StatusCode)

				if test.want != http.StatusSeeOther {
					assert.Zero(t, env.countRows(t, "temporary_blocks"))

					return
				}

				var lifetime time.Duration
				require.NoError(t, env.pool.QueryRow(t.Context(),
					`SELECT expires_at - created_at FROM temporary_blocks`).Scan(&lifetime))
				assert.Equal(t, test.lifetime, lifetime)
			})
		}
	})

	t.Run("the new block form has its own page that posts to the Blocks page", func(t *testing.T) {
		env := startUIEnv(t, "toolgate-admins")
		admin, token := env.adminBrowser(t)
		user := env.insertSession(t, "user-session", []string{"dev"})

		assert.Equal(t, http.StatusForbidden, get(t, user, env.server.URL+"/ui/blocks/new").StatusCode)

		response := get(t, admin, env.server.URL+"/ui/blocks/new")
		require.Equal(t, http.StatusOK, response.StatusCode)

		page := readBody(t, response)
		assert.Contains(t, page, `<form method="post" action="/ui/blocks" id="block-form"`)
		assert.Contains(t, page, `name="csrf_token" value="`+token+`"`)

		for _, field := range []string{"principals", "groups", "workspaces", "sessions", "servers", "tools", "reason"} {
			assert.Contains(t, page, `<label for="field-`+field+`"`, field)
			assert.Contains(t, page, `<textarea id="field-`+field+`" name="`+field+`"`, field)
		}

		for _, value := range []string{"", "attested", "user"} {
			assert.Contains(t, page, `<input type="radio" name="attestation" value="`+value+`"`)
		}

		assert.Contains(t, page, `name="duration"`)
		assert.Contains(t, page, `<option value="days"`)
		assert.NotContains(t, page, "<dialog", "the page is the form without the dialog")
	})

	t.Run("a refused form shows again with the problem and what was typed", func(t *testing.T) {
		env := startUIEnv(t, "toolgate-admins")
		admin, token := env.adminBrowser(t)

		form := validBlock(token)
		form.Set("reason", "")
		form.Set("groups", "oncall")

		response := post(t, admin, env.server.URL+"/ui/blocks", form)
		require.Equal(t, http.StatusBadRequest, response.StatusCode)

		page := readBody(t, response)
		assert.Contains(t, page, "a reason is required")
		assert.Contains(t, page, ">oncall</textarea>")
		assert.Contains(t, page, "github\nwiki</textarea>")
	})

	t.Run("the 1001st live block is refused and an expired one doesn't count", func(t *testing.T) {
		env := startUIEnv(t, "toolgate-admins")
		admin, token := env.adminBrowser(t)

		_, err := env.pool.Exec(t.Context(), `INSERT INTO temporary_blocks (id, match, reason, created_by, created_at, expires_at)
			SELECT lpad(to_hex(n), 32, '0'), '{"servers":["x"]}', 'r', 'a',
			  now() - interval '2 hours', CASE WHEN n = 1 THEN now() - interval '1 hour' ELSE now() + interval '1 hour' END
			FROM generate_series(1, 1000) AS n`)
		require.NoError(t, err)

		assert.Equal(t, http.StatusSeeOther, post(t, admin, env.server.URL+"/ui/blocks", validBlock(token)).StatusCode, "999 are live")
		assert.Equal(t, http.StatusConflict, post(t, admin, env.server.URL+"/ui/blocks", validBlock(token)).StatusCode, "1000 are live")
		assert.Equal(t, 1001, env.countRows(t, "temporary_blocks"))
	})

	t.Run("it lists the rules read-only with their validity", func(t *testing.T) {
		env := startUIEnv(t, "toolgate-admins")
		admin, _ := env.adminBrowser(t)

		expires := metav1.NewTime(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC))
		env.rules.rules = []v1alpha1.ToolgateRule{
			{
				ObjectMeta: metav1.ObjectMeta{Namespace: "toolgate", Name: "no-deploys"},
				Spec:       v1alpha1.ToolgateRuleSpec{Match: v1alpha1.RuleMatch{Tools: []string{"ci__deploy"}}, Reason: "Freeze.", Expires: &expires},
				Status: v1alpha1.Status{Conditions: []metav1.Condition{
					{Type: policy.ConditionValid, Status: metav1.ConditionTrue, Message: "The rule is valid and applies."},
				}},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Namespace: "toolgate", Name: "broken"},
				Spec:       v1alpha1.ToolgateRuleSpec{Match: v1alpha1.RuleMatch{Servers: []string{"[a"}}, Reason: "Broken glob."},
				Status: v1alpha1.Status{Conditions: []metav1.Condition{
					{Type: policy.ConditionValid, Status: metav1.ConditionFalse, Message: "match.servers[0] is not a valid glob"},
				}},
			},
		}

		page := readBody(t, get(t, admin, env.server.URL+"/ui/blocks"))
		assert.Contains(t, page, "toolgate/no-deploys")
		assert.Contains(t, page, "tools: ci__deploy")
		assert.Contains(t, page, "2030-01-02 03:04:05 UTC")
		assert.Contains(t, page, "toolgate/broken")
		assert.Contains(t, page, "invalid: match.servers[0] is not a valid glob")
		assert.Regexp(t, `no-deploys[\s\S]*?valid`, page)
	})
}
