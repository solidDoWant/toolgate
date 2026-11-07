package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/solidDoWant/toolgate/internal/principal"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testtime"
)

// Temporary block bounds.
const (
	// MaxBlockListItems is the most items in one list of a block's match.
	MaxBlockListItems = 64
	// MaxBlockItemLength is the most characters in one item of a block's match.
	MaxBlockItemLength = 256
	// MaxBlockReasonLength is the most characters in a block's reason.
	MaxBlockReasonLength = 1024
	// MinBlockDuration and MaxBlockDuration bound how long a block lasts.
	MinBlockDuration = time.Minute
	MaxBlockDuration = 30 * 24 * time.Hour
)

// RuleSource gives the Blocks page the ToolgateRule objects from the
// informer cache.
type RuleSource interface {
	Rules(ctx context.Context) ([]v1alpha1.ToolgateRule, error)
}

// blockForm is the Blocks page's form, as the browser sent it.
type blockForm struct {
	Principals, Groups, Workspaces, Sessions, Servers, Tools string
	Attestation                                              string
	Reason                                                   string
	Minutes                                                  string
}

func readBlockForm(request *http.Request) blockForm {
	return blockForm{
		Principals:  request.PostFormValue("principals"),
		Groups:      request.PostFormValue("groups"),
		Workspaces:  request.PostFormValue("workspaces"),
		Sessions:    request.PostFormValue("sessions"),
		Servers:     request.PostFormValue("servers"),
		Tools:       request.PostFormValue("tools"),
		Attestation: request.PostFormValue("attestation"),
		Reason:      request.PostFormValue("reason"),
		Minutes:     request.PostFormValue("minutes"),
	}
}

// parseList reads one item per line, ignoring blank lines.
func parseList(name, text string) ([]string, error) {
	var items []string

	for line := range strings.SplitSeq(text, "\n") {
		item := strings.TrimSpace(line)
		if item == "" {
			continue
		}

		if len([]rune(item)) > MaxBlockItemLength {
			return nil, fmt.Errorf("%s: an item is longer than %d characters", name, MaxBlockItemLength)
		}

		items = append(items, item)
	}

	if len(items) > MaxBlockListItems {
		return nil, fmt.Errorf("%s: at most %d items are allowed", name, MaxBlockListItems)
	}

	return items, nil
}

// parse validates the form and returns the match, the reason and the
// lifetime of the block it describes.
func (f blockForm) parse() (v1alpha1.RuleMatch, string, time.Duration, error) {
	var match v1alpha1.RuleMatch

	lists := []struct {
		name string
		text string
		into *[]string
	}{
		{"principals", f.Principals, &match.Principals},
		{"groups", f.Groups, &match.Groups},
		{"workspaces", f.Workspaces, &match.Workspaces},
		{"sessions", f.Sessions, &match.Sessions},
		{"servers", f.Servers, &match.Servers},
		{"tools", f.Tools, &match.Tools},
	}

	for _, list := range lists {
		items, err := parseList(list.name, list.text)
		if err != nil {
			return match, "", 0, err
		}

		*list.into = items
	}

	match.Attestation = f.Attestation

	switch match.Attestation {
	case "", auth.AttestationUser, auth.AttestationAttested:
	default:
		return match, "", 0, errors.New("attestation must be attested or user")
	}

	if len(match.Principals)+len(match.Groups)+len(match.Workspaces)+len(match.Sessions)+len(match.Servers)+len(match.Tools) == 0 && match.Attestation == "" {
		return match, "", 0, errors.New("set at least one match field: a block with none would block every tool for every user")
	}

	// The same compile as the rules' and the decision's, so a glob that
	// passes here matches the way it will match later.
	if _, err := policy.NewTemporaryBlock("0", match, "x", time.Time{}); err != nil {
		return match, "", 0, fmt.Errorf("match: %w", err)
	}

	reason := strings.TrimSpace(f.Reason)
	if reason == "" {
		return match, "", 0, errors.New("a reason is required")
	}

	if len([]rune(reason)) > MaxBlockReasonLength {
		return match, "", 0, fmt.Errorf("the reason is longer than %d characters", MaxBlockReasonLength)
	}

	minutes, err := strconv.Atoi(strings.TrimSpace(f.Minutes))
	if err != nil {
		return match, "", 0, errors.New("the duration must be a whole number of minutes")
	}

	// Compare in minutes so a huge value can't overflow a Duration.
	if minutes < int(MinBlockDuration/time.Minute) || minutes > int(MaxBlockDuration/time.Minute) {
		return match, "", 0, errors.New("the duration must be from 1 minute to 30 days (43200 minutes)")
	}

	return match, reason, time.Duration(minutes) * time.Minute, nil
}

// blockRow is one live temporary block as the page shows it.
type blockRow struct {
	ID        string
	Match     string
	Reason    string
	CreatedBy string
	// Who is CreatedBy as people read it.
	Who       principal.Display
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ruleRow is one ToolgateRule as the page shows it.
type ruleRow struct {
	Namespace string
	Name      string
	Match     string
	Reason    string
	Expires   *time.Time
	Valid     bool
	Message   string
}

type blocksPage struct {
	Blocks []blockRow
	Rules  []ruleRow
	Form   blockForm
	Error  string
}

// describeMatch writes a match as `field: a, b; field: c`.
func describeMatch(match v1alpha1.RuleMatch) string {
	var parts []string

	for _, field := range []struct {
		name  string
		items []string
	}{
		{"principals", match.Principals}, {"groups", match.Groups}, {"workspaces", match.Workspaces},
		{"sessions", match.Sessions}, {"servers", match.Servers}, {"tools", match.Tools},
	} {
		if len(field.items) > 0 {
			parts = append(parts, field.name+": "+strings.Join(field.items, ", "))
		}
	}

	if match.Attestation != "" {
		parts = append(parts, "attestation: "+match.Attestation)
	}

	return strings.Join(parts, "; ")
}

func (u *UI) blocksPage(ctx context.Context) (blocksPage, error) {
	var page blocksPage

	rows, err := u.queries.ListLiveTemporaryBlocks(ctx)
	if err != nil {
		return page, fmt.Errorf("list temporary blocks: %w", err)
	}

	for _, row := range rows {
		var match v1alpha1.RuleMatch
		if err := json.Unmarshal(row.Match, &match); err != nil {
			return page, fmt.Errorf("read the match of block %s: %w", row.ID, err)
		}

		page.Blocks = append(page.Blocks, blockRow{
			ID: row.ID, Match: describeMatch(match), Reason: row.Reason, CreatedBy: row.CreatedBy,
			CreatedAt: row.CreatedAt.Time, ExpiresAt: row.ExpiresAt.Time,
		})
	}

	creators := make([]string, 0, len(page.Blocks))
	for _, block := range page.Blocks {
		creators = append(creators, block.CreatedBy)
	}

	self, _ := SessionFrom(ctx)
	names := u.namesFor(ctx, self.Principal, creators...)

	for index := range page.Blocks {
		page.Blocks[index].Who = names.show(page.Blocks[index].CreatedBy)
	}

	if u.options.Rules == nil {
		return page, nil
	}

	rules, err := u.options.Rules.Rules(ctx)
	if err != nil {
		return page, fmt.Errorf("list rules: %w", err)
	}

	for index := range rules {
		rule := &rules[index]
		row := ruleRow{
			Namespace: rule.Namespace, Name: rule.Name, Match: describeMatch(rule.Spec.Match), Reason: rule.Spec.Reason,
			Message: "Not checked yet.",
		}

		if rule.Spec.Expires != nil {
			expires := rule.Spec.Expires.Time
			row.Expires = &expires
		}

		if condition := meta.FindStatusCondition(rule.Status.Conditions, policy.ConditionValid); condition != nil {
			row.Valid = condition.Status == "True"
			row.Message = condition.Message
		}

		page.Rules = append(page.Rules, row)
	}

	slices.SortFunc(page.Rules, func(a, b ruleRow) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})

	return page, nil
}

func (u *UI) blocks(writer http.ResponseWriter, request *http.Request) {
	u.showBlocks(writer, request, http.StatusOK, blockForm{Minutes: "60"}, "")
}

func (u *UI) showBlocks(writer http.ResponseWriter, request *http.Request, status int, form blockForm, problem string) {
	page, err := u.blocksPage(request.Context())
	if err != nil {
		u.logger.ErrorContext(request.Context(), "cannot read the blocks for the web UI", "error", err)
		http.Error(writer, "The gateway can't read the blocks. Try again later.", http.StatusInternalServerError)

		return
	}

	page.Form = form
	page.Error = problem

	u.renderStatus(writer, request, status, "blocks", "Blocks", page)
}

// createBlock makes a temporary block: the row, the block:<id> notification
// and the block.create audit event commit together.
func (u *UI) createBlock(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	form := readBlockForm(request)

	match, reason, lifetime, err := form.parse()
	if err != nil {
		u.showBlocks(writer, request, http.StatusBadRequest, form, err.Error())

		return
	}

	encoded, err := json.Marshal(match)
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot encode a temporary block's match", "error", err)
		http.Error(writer, "The gateway can't save the block. Try again later.", http.StatusInternalServerError)

		return
	}

	identifier := make([]byte, 16)
	_, _ = rand.Read(identifier) // crypto/rand.Read never fails.
	id := hex.EncodeToString(identifier)

	err = pgx.BeginFunc(ctx, u.options.Pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		if err := queries.LockTemporaryBlockInsert(ctx); err != nil {
			return fmt.Errorf("lock temporary block insert: %w", err)
		}

		row, err := queries.InsertTemporaryBlockWithinLimit(ctx, db.InsertTemporaryBlockWithinLimitParams{
			ID: id, Match: encoded, Reason: reason, CreatedBy: session.Principal,
			Lifetime:  pgtype.Interval{Microseconds: testtime.Scale(lifetime).Microseconds(), Valid: true},
			LiveLimit: store.MaxLiveTemporaryBlocks,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrLimitReached
		}

		if err != nil {
			return fmt.Errorf("insert temporary block: %w", err)
		}

		if err := store.Notify(ctx, tx, store.KindBlock, id); err != nil {
			return err
		}

		return audit.NewRecorder(u.auditOutput(), queries).Record(ctx, audit.Event{
			Type:        audit.TypeBlockCreate,
			Principal:   session.Principal,
			Attestation: auth.AttestationUser,
			Details: map[string]any{
				"block":      id,
				"match":      match,
				"reason":     reason,
				"expires_at": row.ExpiresAt.Time.UTC().Format(time.RFC3339),
			},
		})
	})
	if errors.Is(err, store.ErrLimitReached) {
		u.showBlocks(writer, request, http.StatusConflict, form, fmt.Sprintf("%d temporary blocks are live already. Wait for one to end.", store.MaxLiveTemporaryBlocks))

		return
	}

	if err != nil {
		u.logger.ErrorContext(ctx, "cannot create a temporary block", "error", err)
		http.Error(writer, "The gateway can't save the block. Try again later.", http.StatusInternalServerError)

		return
	}

	http.Redirect(writer, request, "/ui/blocks", http.StatusSeeOther)
}
