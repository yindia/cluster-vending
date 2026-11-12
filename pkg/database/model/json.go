package model

import "encoding/json"

// JSON is a JSON document: the protojson form of a cloud.v1 spec, status
// or input, kept as it is so the contract can evolve without changing the
// rows.
type JSON json.RawMessage

// Labels are a resource's labels (cloud.v1 labels): its cloud tags.
type Labels map[string]string
