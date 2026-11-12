package main

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var writeDefinitions = []toolDefinition{
	{"kownload_create_task", "create", "Create one HTTP(S) task only within trusted startup policy. First inspect the capability; submit an explicit stable operationId and policyId. If queryRequired, query that same operation, never submit a new ID. No model confirmation grants permission."},
	{"kownload_pause_task", "pause", "Pause an explicitly authorized task using its inspected expected episode/generation/sourceRevision. Unknown acknowledgements require same-operation query; never refresh expected values and resubmit that operation."},
	{"kownload_resume_task", "resume", "Continue an explicitly authorized paused task with its inspected expected tuple. This is not Retry, a new episode, or a budget reset. Unknown acknowledgements require same-operation query."},
}

// Registration is opt-in through a trusted, once-loaded startup policy. The
// model cannot set the policy file, API token, output paths, effects or socket.
func registerWriteTools(s *mcp.Server, b *backend, p *writePolicy) {
	if p == nil || p.value == nil {
		return
	}
	slots := make(chan struct{}, 1)
	for _, def := range writeDefinitions {
		no := false
		yes := true
		s.AddTool(&mcp.Tool{Name: def.name, Description: def.description, InputSchema: writeInputSchema(def.kind), OutputSchema: writeOutputSchema(), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &no, IdempotentHint: true, OpenWorldHint: &yes}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args writeToolArguments
			if req.Params == nil || len(req.Params.Arguments) > maxWriteArguments || strictWriteJSON(req.Params.Arguments, writeInputSchema(def.kind), &args) != nil || (args.Create != nil && !validWriteCreate(args.Create)) {
				return nil, &jsonrpc.Error{Code: -32602, Message: "Invalid arguments"}
			}
			if !writeToolAllowed(p, def.kind, args) {
				return toolFailure("permission_denied"), nil
			}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				return toolFailure("busy"), nil
			}
			out := writeToolResult{Mode: args.Mode}
			switch args.Mode {
			case "inspect":
				cap, code := b.writeCapabilities(ctx, args.DatasetID, args.TaskID)
				if code != "" {
					return toolFailure(code), nil
				}
				// Report only the intersection of Core capability and this Helper's local
				// startup policy. Core remains responsible for execution-time authorization.
				roots := make([]string, 0, len(cap.OutputRootIDs))
				for _, root := range cap.OutputRootIDs {
					if _, ok := p.allowCreate(args.DatasetID, root); ok {
						roots = append(roots, root)
					}
				}
				cap.OutputRootIDs = roots
				_, canCreate := p.allowCreate(args.DatasetID, args.OutputRootID)
				foundRoot := false
				for _, root := range roots {
					if root == args.OutputRootID {
						foundRoot = true
					}
				}
				cap.Operations.Create = cap.Operations.Create && canCreate && foundRoot
				cap.Operations.Pause = cap.Operations.Pause && p.allowTask("pause", args.DatasetID, args.TaskID) && cap.Task != nil
				cap.Operations.Resume = cap.Operations.Resume && p.allowTask("resume", args.DatasetID, args.TaskID) && cap.Task != nil
				out.Capability = cap
			case "submit", "query":
				c := writeCommand{Version: 1, DatasetID: args.DatasetID, PolicyID: args.PolicyID, OperationID: args.OperationID, Kind: def.kind, Create: args.Create, Action: args.Action}
				var receipt *writeReceipt
				var code string
				if args.Mode == "submit" {
					receipt, code = b.submitWrite(ctx, c)
				} else {
					receipt, code = b.queryWrite(ctx, c)
				}
				if code != "" {
					return toolFailure(code), nil
				}
				// Query arguments carry no task selector. Do not expose another task's
				// action receipt merely because its op ID was guessed in an allowed dataset.
				if def.kind != "create" && receipt.Result != nil && receipt.Result.TaskID != nil && !p.allowTask(def.kind, args.DatasetID, *receipt.Result.TaskID) {
					return toolFailure("permission_denied"), nil
				}
				out.Receipt = receipt
			}
			encoded, err := json.Marshal(out)
			if err != nil || len(encoded) > maxBackend {
				return toolFailure("invalid_app_response"), nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}, StructuredContent: out}, nil
		})
	}
}
func writeToolAllowed(p *writePolicy, kind string, a writeToolArguments) bool {
	if a.Mode == "inspect" {
		if kind == "create" {
			_, ok := p.allowCreate(a.DatasetID, a.OutputRootID)
			return ok
		}
		return p.allowTask(kind, a.DatasetID, a.TaskID)
	}
	if a.Mode == "submit" {
		if kind == "create" {
			if a.Create == nil {
				return false
			}
			_, ok := p.allowCreate(a.DatasetID, a.Create.OutputRootID)
			return ok
		}
		return a.Action != nil && p.allowTask(kind, a.DatasetID, a.Action.TaskID)
	}
	// A query never mutates Core; require an explicit local scope for its kind and
	// dataset, then validate an action's returned task against that same scope.
	if p == nil || p.value == nil {
		return false
	}
	v, err := p.value.SnapshotV1()
	if err != nil {
		return false
	}
	for _, d := range v.Datasets {
		if d.ID == a.DatasetID {
			switch kind {
			case "create":
				return v.Operations.Create && len(d.RootIDs) > 0
			case "pause":
				return v.Operations.Pause && len(d.TaskIDs) > 0
			case "resume":
				return v.Operations.Resume && len(d.TaskIDs) > 0
			}
		}
	}
	return false
}
