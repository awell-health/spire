package store

import (
	"database/sql"
	"time"
)

// TowerFormula represents a formula stored in the tower's dolt database.
// Formulas are shared across all machines attached to the tower via dolt sync.
type TowerFormula struct {
	Name        string
	Content     string
	Description string
	PublishedBy string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// GetTowerFormula returns the TOML content of the named formula.
// Returns sql.ErrNoRows if the formula does not exist; callers use
// errors.Is(err, sql.ErrNoRows) to distinguish miss from failure.
func GetTowerFormula(db *sql.DB, name string) (string, error) {
	var content string
	err := db.QueryRow(
		`SELECT content FROM formulas WHERE name = ?`, name,
	).Scan(&content)
	if err != nil {
		return "", err // sql.ErrNoRows propagates as-is
	}
	return content, nil
}

// ListTowerFormulas returns all tower formulas ordered by name.
func ListTowerFormulas(db *sql.DB) ([]TowerFormula, error) {
	rows, err := db.Query(
		`SELECT name, content, description, published_by, created_at, updated_at
		 FROM formulas ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TowerFormula
	for rows.Next() {
		var f TowerFormula
		if err := rows.Scan(&f.Name, &f.Content, &f.Description,
			&f.PublishedBy, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// PublishTowerFormula upserts a formula into the tower database.
// If a formula with the given name already exists, its content, description,
// published_by, and updated_at are overwritten.
func PublishTowerFormula(db *sql.DB, name, content, desc, author string) error {
	_, err := db.Exec(
		`INSERT INTO formulas (name, content, description, published_by, updated_at)
		 VALUES (?, ?, ?, ?, NOW())
		 ON DUPLICATE KEY UPDATE
		     content      = VALUES(content),
		     description  = VALUES(description),
		     published_by = VALUES(published_by),
		     updated_at   = NOW()`,
		name, content, desc, author,
	)
	return err
}

// RemoveTowerFormula deletes a formula from the tower database by name.
func RemoveTowerFormula(db *sql.DB, name string) error {
	_, err := db.Exec(`DELETE FROM formulas WHERE name = ?`, name)
	return err
}
