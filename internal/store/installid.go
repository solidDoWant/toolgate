package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// InstallID returns the ID of this install, a random value generated once and
// kept in the database. It survives restarts, release renames and leader
// changes, and gateways of one install share it.
func InstallID(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate install ID: %w", err)
	}

	queries := db.New(pool)

	if err := queries.InsertInstallID(ctx, hex.EncodeToString(random)); err != nil {
		return "", fmt.Errorf("store install ID: %w", err)
	}

	id, err := queries.GetInstallID(ctx)
	if err != nil {
		return "", fmt.Errorf("read install ID: %w", err)
	}

	return id, nil
}
