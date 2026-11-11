package main

import (
	"encoding/json"
	"github.com/google/jsonschema-go/jsonschema"
)

type schema = map[string]any

func object(properties schema, required ...string) schema {
	return schema{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func enum(values ...string) schema { return schema{"type": "string", "enum": values} }
func number() schema {
	return schema{"type": "integer", "minimum": 0, "maximum": int64(9007199254740991)}
}
func identifier() schema {
	return schema{"type": "string", "pattern": "^[A-Za-z0-9_-]{1,64}$", "maxLength": 64}
}
func cursorSchema() schema {
	return schema{"type": "string", "pattern": "^[A-Za-z0-9_-]{0,128}$", "maxLength": 128}
}
func timestamp() schema {
	return schema{"type": "string", "format": "date-time", "minLength": 20, "maxLength": 35}
}
func progressSchema() schema {
	return object(schema{"downloadedBytes": number(), "speedBytesPerSecond": number(), "uploadedBytes": number(), "uploadSpeedBytesPerSecond": number(), "totalBytes": schema{"anyOf": []any{number(), schema{"type": "null"}}}, "phase": enum("unknown", "queued", "resolving", "downloading", "merging", "verifying", "publishing", "done")}, "downloadedBytes", "speedBytesPerSecond", "uploadedBytes", "uploadSpeedBytesPerSecond", "totalBytes", "phase")
}
func taskSchema() schema {
	return object(schema{"id": identifier(), "protocol": enum("http", "https", "bt", "ed2k", "media", "netdisk", "unknown"), "status": enum("ready", "wait", "running", "pause", "done", "error"), "createdAt": timestamp(), "updatedAt": timestamp(), "progress": progressSchema()}, "id", "protocol", "status", "createdAt", "updatedAt", "progress")
}
func outputSchema(kind string) schema {
	switch kind {
	case "list":
		return object(schema{"tasks": schema{"type": "array", "items": taskSchema(), "maxItems": 100}, "nextCursor": cursorSchema()}, "tasks", "nextCursor")
	case "task":
		return taskSchema()
	case "progress":
		return progressSchema()
	case "stats":
		return object(schema{"available": schema{"type": "boolean"}, "activeStreams": number(), "targetStreams": number(), "peers": number()}, "available")
	case "errors":
		return object(schema{"available": schema{"type": "boolean"}, "category": enum("network", "authentication", "integrity", "storage", "cancelled", "unknown"), "retryable": schema{"type": "boolean"}, "occurredAt": timestamp()}, "available")
	default:
		panic("unknown schema")
	}
}
func compileSchema(s schema) *jsonschema.Resolved {
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	var js jsonschema.Schema
	if err = json.Unmarshal(data, &js); err != nil {
		panic(err)
	}
	resolved, err := js.Resolve(nil)
	if err != nil {
		panic(err)
	}
	return resolved
}
