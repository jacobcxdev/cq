package proxy

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestCodexDaybreakStateReadsSavedToggleAndActiveGoal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	threadDB, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer threadDB.Close()
	goalDB, err := sql.Open("sqlite", filepath.Join(home, "goals_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer goalDB.Close()
	for _, step := range []struct {
		db        *sql.DB
		statement string
	}{
		{threadDB, `CREATE TABLE threads (id TEXT PRIMARY KEY, daybreak_enabled BOOLEAN)`},
		{threadDB, `INSERT INTO threads VALUES ('task', 1)`},
		{goalDB, `CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY, status TEXT)`},
		{goalDB, `INSERT INTO thread_goals VALUES ('task', 'active')`},
	} {
		if _, err := step.db.Exec(step.statement); err != nil {
			t.Fatal(err)
		}
	}
	state, err := NewCodexDaybreakState()
	if err != nil {
		t.Fatal(err)
	}
	check := func(wantEnabled, wantGoal bool) {
		t.Helper()
		enabled, goal, err := state.Current(context.Background(), "task")
		if err != nil || enabled != wantEnabled || goal != wantGoal {
			t.Fatalf("saved Daybreak/goal = %t/%t, error %v; want %t/%t", enabled, goal, err, wantEnabled, wantGoal)
		}
	}
	check(true, true)
	if _, err := goalDB.Exec(`UPDATE thread_goals SET status = 'paused'`); err != nil {
		t.Fatal(err)
	}
	check(true, false)
	if _, err := threadDB.Exec(`UPDATE threads SET daybreak_enabled = 0`); err != nil {
		t.Fatal(err)
	}
	check(false, false)
}
