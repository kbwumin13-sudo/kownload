package main

func writeDatasetSchema() schema {
	return schema{"type": "string", "pattern": "^[0-9a-f]{32}$", "maxLength": 32}
}
func writePolicySchema() schema {
	return schema{"type": "string", "pattern": "^mcp-authority-v1:[0-9a-f]{64}$", "maxLength": 81}
}
func writeOperationSchema() schema {
	return schema{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$", "maxLength": 128}
}
func nullableWrite(s schema) schema { return schema{"anyOf": []any{s, schema{"type": "null"}}} }
func writeTupleSchema() schema {
	positive := schema{"type": "integer", "minimum": 1, "maximum": maxJSONInteger}
	return object(schema{"episode": identifier(), "generation": positive, "sourceRevision": positive}, "episode", "generation", "sourceRevision")
}
func writeCreateSchema() schema {
	return object(schema{"url": schema{"type": "string", "minLength": 1, "maxLength": 2048}, "outputRootId": identifier(), "fileName": schema{"type": "string", "minLength": 1, "maxLength": 255}}, "url", "outputRootId", "fileName")
}
func writeActionSchema() schema {
	return object(schema{"taskId": identifier(), "expected": writeTupleSchema()}, "taskId", "expected")
}
func writeInputSchema(kind string) schema {
	inspect := schema{"mode": schema{"const": "inspect"}, "datasetId": writeDatasetSchema()}
	field := "taskId"
	if kind == "create" {
		field = "outputRootId"
	}
	inspect[field] = identifier()
	submit := schema{"mode": schema{"const": "submit"}, "datasetId": writeDatasetSchema(), "policyId": writePolicySchema(), "operationId": writeOperationSchema()}
	payload := "action"
	value := writeActionSchema()
	if kind == "create" {
		payload = "create"
		value = writeCreateSchema()
	}
	submit[payload] = value
	// Top-level object is required by MCP; branches each close their properties.
	return schema{"type": "object", "oneOf": []any{
		object(inspect, "mode", "datasetId", field),
		object(submit, "mode", "datasetId", "policyId", "operationId", payload),
		object(schema{"mode": schema{"const": "query"}, "datasetId": writeDatasetSchema(), "policyId": writePolicySchema(), "operationId": writeOperationSchema()}, "mode", "datasetId", "policyId", "operationId"),
	}}
}

// Keep this list in lockstep with the dedicated receiver. Backend error text is
// never accepted in place of a fixed code.
func writeErrorSchema() schema {
	return enum("invalid_request", "permission_denied", "capability_unavailable", "dataset_mismatch", "policy_mismatch", "operation_conflict", "task_unavailable", "stale_target", "network_denied", "output_conflict", "storage_unavailable", "execution_failed", "cancelled")
}
func writeCapabilitySchema() schema {
	return object(schema{"version": schema{"const": 1}, "datasetId": writeDatasetSchema(), "policyId": nullableWrite(writePolicySchema()), "networkPolicyId": nullableWrite(schema{"type": "string", "pattern": "^mcp-network/v1:[0-9a-f]{64}$", "maxLength": 79}), "operations": object(schema{"create": schema{"type": "boolean"}, "pause": schema{"type": "boolean"}, "resume": schema{"type": "boolean"}}, "create", "pause", "resume"), "outputRootIds": schema{"type": "array", "items": identifier(), "maxItems": 16, "uniqueItems": true}, "task": nullableWrite(object(schema{"taskId": identifier(), "expected": writeTupleSchema()}, "taskId", "expected")), "reason": nullableWrite(writeErrorSchema())}, "version", "datasetId", "policyId", "networkPolicyId", "operations", "outputRootIds", "task", "reason")
}
func writeReceiptSchema() schema {
	result := object(schema{"taskId": nullableWrite(identifier()), "outcome": enum("created", "applied", "rejected"), "errorCode": nullableWrite(writeErrorSchema()), "expected": nullableWrite(writeTupleSchema()), "accepted": nullableWrite(writeTupleSchema())}, "taskId", "outcome", "errorCode", "expected", "accepted")
	return object(schema{"version": schema{"const": 1}, "datasetId": writeDatasetSchema(), "policyId": writePolicySchema(), "operationId": writeOperationSchema(), "kind": nullableWrite(enum("create", "pause", "resume")), "state": enum("unknown", "not_recorded", "pending", "settled"), "result": nullableWrite(result), "queryRequired": schema{"type": "boolean"}}, "version", "datasetId", "policyId", "operationId", "kind", "state", "result", "queryRequired")
}
func writeOutputSchema() schema {
	return object(schema{"mode": enum("inspect", "submit", "query"), "capability": nullableWrite(writeCapabilitySchema()), "receipt": nullableWrite(writeReceiptSchema())}, "mode", "capability", "receipt")
}
