package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestGetTowerFormula_Found(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT content FROM formulas WHERE name = \?`).
		WithArgs("spire-agent-work").
		WillReturnRows(sqlmock.NewRows([]string{"content"}).AddRow("[formula]\nname = \"spire-agent-work\""))

	content, err := GetTowerFormula(db, "spire-agent-work")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "[formula]\nname = \"spire-agent-work\"" {
		t.Errorf("got content %q", content)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetTowerFormula_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT content FROM formulas WHERE name = \?`).
		WithArgs("nonexistent").
		WillReturnError(sql.ErrNoRows)

	_, err = GetTowerFormula(db, "nonexistent")
	if err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListTowerFormulas_Multiple(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"name", "version", "content", "description", "published_by", "created_at", "updated_at"}).
		AddRow("alpha", 1, "content-a", "desc-a", "user1", now, now).
		AddRow("beta", 3, "content-b", "desc-b", "user2", now, now)

	mock.ExpectQuery(`SELECT name, version, content, description, published_by, created_at, updated_at\s+FROM formulas ORDER BY name`).
		WillReturnRows(rows)

	formulas, err := ListTowerFormulas(db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(formulas) != 2 {
		t.Fatalf("expected 2 formulas, got %d", len(formulas))
	}
	if formulas[0].Name != "alpha" || formulas[0].Version != 1 {
		t.Errorf("first formula: got name=%q version=%d", formulas[0].Name, formulas[0].Version)
	}
	if formulas[1].Name != "beta" || formulas[1].Version != 3 {
		t.Errorf("second formula: got name=%q version=%d", formulas[1].Name, formulas[1].Version)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListTowerFormulas_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT name, version, content, description, published_by, created_at, updated_at\s+FROM formulas ORDER BY name`).
		WillReturnRows(sqlmock.NewRows([]string{"name", "version", "content", "description", "published_by", "created_at", "updated_at"}))

	formulas, err := ListTowerFormulas(db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(formulas) != 0 {
		t.Errorf("expected empty list, got %d", len(formulas))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishTowerFormula_Insert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO formulas`).
		WithArgs("my-formula", "toml-content", "a description", "author1").
		WillReturnResult(sqlmock.NewResult(1, 1))

	err = PublishTowerFormula(db, "my-formula", "toml-content", "a description", "author1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishTowerFormula_DBError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO formulas`).
		WithArgs("bad", "content", "desc", "author").
		WillReturnError(sql.ErrConnDone)

	err = PublishTowerFormula(db, "bad", "content", "desc", "author")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveTowerFormula_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`DELETE FROM formulas WHERE name = \?`).
		WithArgs("old-formula").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = RemoveTowerFormula(db, "old-formula")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveTowerFormula_DBError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`DELETE FROM formulas WHERE name = \?`).
		WithArgs("bad").
		WillReturnError(sql.ErrConnDone)

	err = RemoveTowerFormula(db, "bad")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
