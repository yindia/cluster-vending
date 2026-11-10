package model

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// JSON is a JSON document column: jsonb on Postgres, text on SQLite. It
// holds the protojson form of a cloud.v1 spec or status, so the contract can
// evolve without schema migrations. Postgres stores it semantically (jsonb
// normalizes whitespace and key order), so compare decoded values, not bytes.
type JSON json.RawMessage

// Value implements driver.Valuer.
func (j JSON) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	if !json.Valid(j) {
		return nil, fmt.Errorf("model.JSON: invalid JSON document")
	}
	return string(j), nil
}

// Scan implements sql.Scanner.
func (j *JSON) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*j = nil
	case []byte:
		*j = append(JSON(nil), v...)
	case string:
		*j = JSON(v)
	default:
		return fmt.Errorf("model.JSON: cannot scan %T", src)
	}
	return nil
}

// GormDBDataType picks the column type per database.
func (JSON) GormDBDataType(db *gorm.DB, _ *schema.Field) string {
	return jsonColumnType(db)
}

// Labels is a string map stored as a JSON object (API-level labels).
type Labels map[string]string

// Value implements driver.Valuer.
func (l Labels) Value() (driver.Value, error) {
	if l == nil {
		return "{}", nil
	}
	b, err := json.Marshal(map[string]string(l))
	if err != nil {
		return nil, fmt.Errorf("model.Labels: %w", err)
	}
	return string(b), nil
}

// Scan implements sql.Scanner.
func (l *Labels) Scan(src any) error {
	var raw []byte
	switch v := src.(type) {
	case nil:
		*l = nil
		return nil
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("model.Labels: cannot scan %T", src)
	}
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("model.Labels: %w", err)
	}
	*l = m
	return nil
}

// GormDBDataType picks the column type per database.
func (Labels) GormDBDataType(db *gorm.DB, _ *schema.Field) string {
	return jsonColumnType(db)
}

func jsonColumnType(db *gorm.DB) string {
	if db.Name() == "postgres" {
		return "jsonb"
	}
	return "text"
}
