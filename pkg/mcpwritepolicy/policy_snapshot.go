package mcpwritepolicy

import "sort"

// PolicySnapshotV1 is an owned, deterministic description of local policy
// semantics. It is not an authority, filesystem observation or capability.
// Version fixes this representation independently of any authority hash format.
type PolicySnapshotV1 struct {
	Version    int                `json:"version"`
	Operations PolicyOperationsV1 `json:"operations"`
	Datasets   []PolicyDatasetV1  `json:"datasets"`
	Roots      []PolicyRootV1     `json:"roots"`
	Effects    []string           `json:"effects"`
}

type PolicyOperationsV1 struct {
	Create bool `json:"create"`
	Pause  bool `json:"pause"`
	Resume bool `json:"resume"`
}

type PolicyDatasetV1 struct {
	ID      string   `json:"id"`
	TaskIDs []string `json:"taskIds"`
	RootIDs []string `json:"rootIds"`
}

type PolicyRootV1 struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// SnapshotV1 sorts sets and makes omitted false/empty values explicit. Every
// slice is newly owned by the caller. A nil or zero Policy is not parsed input.
func (p *Policy) SnapshotV1() (PolicySnapshotV1, error) {
	if p == nil || p.operations == nil || p.datasets == nil || p.roots == nil {
		return PolicySnapshotV1{}, ErrInvalidPolicy
	}
	v := PolicySnapshotV1{
		Version:    1,
		Operations: PolicyOperationsV1{p.operations["create"], p.operations["pause"], p.operations["resume"]},
		Datasets:   make([]PolicyDatasetV1, 0, len(p.datasets)),
		Roots:      make([]PolicyRootV1, 0, len(p.roots)),
		Effects:    append([]string{}, p.effects...),
	}
	for id, scope := range p.datasets {
		d := PolicyDatasetV1{ID: id, TaskIDs: make([]string, 0, len(scope.tasks)), RootIDs: make([]string, 0, len(scope.roots))}
		for task := range scope.tasks {
			d.TaskIDs = append(d.TaskIDs, task)
		}
		for root := range scope.roots {
			d.RootIDs = append(d.RootIDs, root)
		}
		sort.Strings(d.TaskIDs)
		sort.Strings(d.RootIDs)
		v.Datasets = append(v.Datasets, d)
	}
	for id, path := range p.roots {
		v.Roots = append(v.Roots, PolicyRootV1{ID: id, Path: path})
	}
	sort.Slice(v.Datasets, func(i, j int) bool { return v.Datasets[i].ID < v.Datasets[j].ID })
	sort.Slice(v.Roots, func(i, j int) bool { return v.Roots[i].ID < v.Roots[j].ID })
	sort.Strings(v.Effects)
	return v, nil
}
