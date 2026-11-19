package proxy

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// CodexDaybreakState reads the chat's current preference and goal status.
type CodexDaybreakState interface {
	Current(context.Context, string) (enabled, goalActive bool, err error)
}

type codexSQLiteDaybreakState struct {
	home string
}

func NewCodexDaybreakState() (CodexDaybreakState, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(userHome, ".codex")
	}
	if !filepath.IsAbs(home) {
		return nil, errors.New("Codex home must be absolute")
	}
	return &codexSQLiteDaybreakState{home: home}, nil
}

func (state *codexSQLiteDaybreakState) Current(ctx context.Context, session string) (bool, bool, error) {
	if state == nil || !validCanonicalSessionID([]byte(session)) {
		return false, false, nil
	}
	enabled, err := state.readFlag(ctx, "state_5.sqlite", "SELECT daybreak_enabled = 1 FROM threads WHERE id = ?", session)
	if err != nil || !enabled {
		return false, false, err
	}
	goalActive, err := state.readFlag(ctx, "goals_1.sqlite", "SELECT status = 'active' FROM thread_goals WHERE thread_id = ?", session)
	return enabled, goalActive, err
}

func (state *codexSQLiteDaybreakState) readFlag(ctx context.Context, file, query, session string) (bool, error) {
	uri := url.URL{Scheme: "file", Path: filepath.Join(state.home, file)}
	parameters := uri.Query()
	parameters.Set("mode", "ro")
	uri.RawQuery = parameters.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return false, err
	}
	defer db.Close()
	var enabled sql.NullBool
	err = db.QueryRowContext(ctx, query, session).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return enabled.Valid && enabled.Bool, err
}
