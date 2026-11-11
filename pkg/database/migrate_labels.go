package database

import (
	"context"
	"encoding/json"
	"fmt"

	"gorm.io/gorm"

	"github.com/yindia/cluster-vending/pkg/database/model"
)

// tagRow is the part of a network or cluster row the tags migration
// touches.
type tagRow struct {
	ID     string
	Labels model.Labels
	Spec   model.JSON
	Input  model.JSON
}

// migrateTagsToLabels moves spec.tags (removed from the contract: labels
// are the cloud tags now) into labels, for every network and cluster row
// that still has them; labels win on a key clash. spec.tags is dropped
// from the stored spec and input. Idempotent: rows without spec.tags are
// left alone.
func migrateTagsToLabels(ctx context.Context, db *gorm.DB) error {
	for _, table := range []string{"networks", "clusters"} {
		var rows []tagRow
		err := db.WithContext(ctx).Table(table).Select("id", "labels", "spec", "input").
			Where("CAST(spec AS TEXT) LIKE ? OR CAST(input AS TEXT) LIKE ?", `%"tags"%`, `%"tags"%`).Find(&rows).Error
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		for _, r := range rows {
			spec, specTags, err := withoutTags(r.Spec)
			if err != nil {
				return fmt.Errorf("%s %s spec: %w", table, r.ID, err)
			}
			input, inputTags, err := withoutTags(r.Input)
			if err != nil {
				return fmt.Errorf("%s %s input: %w", table, r.ID, err)
			}
			labels := model.Labels{}
			for _, tags := range []map[string]string{inputTags, specTags, r.Labels} { // later wins
				for k, v := range tags {
					labels[k] = v
				}
			}
			err = db.WithContext(ctx).Table(table).Where("id = ?", r.ID).
				Updates(map[string]any{"labels": labels, "spec": spec, "input": input}).Error
			if err != nil {
				return fmt.Errorf("%s %s: %w", table, r.ID, err)
			}
		}
	}
	return nil
}

// withoutTags returns raw (a stored Network or Cluster, or its spec)
// without spec.tags / tags, and the tags it had.
func withoutTags(raw model.JSON) (model.JSON, map[string]string, error) {
	if len(raw) == 0 {
		return raw, nil, nil
	}
	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, err
	}
	tags := map[string]string{}
	take := func(m map[string]json.RawMessage) error {
		t, ok := m["tags"]
		if !ok {
			return nil
		}
		delete(m, "tags")
		return json.Unmarshal(t, &tags)
	}
	// A stored input is a whole resource (tags under spec); a stored spec
	// is the spec itself.
	if s, ok := doc["spec"]; ok {
		spec := map[string]json.RawMessage{}
		if err := json.Unmarshal(s, &spec); err != nil {
			return nil, nil, err
		}
		if err := take(spec); err != nil {
			return nil, nil, err
		}
		b, err := json.Marshal(spec)
		if err != nil {
			return nil, nil, err
		}
		doc["spec"] = b
	} else if err := take(doc); err != nil {
		return nil, nil, err
	}
	out, err := json.Marshal(doc)
	return model.JSON(out), tags, err
}
