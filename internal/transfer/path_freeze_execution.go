package transfer

import (
	"path/filepath"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
)

// PathFreezeExecution is an owner-issued, process-local execution binding.
// Core issues it only after F and the actual root/work-directory identity Save
// have known ACKs. A persisted layout, query, or reconstructed object cannot
// issue it. Core retains the directory lease through process/pipe/writer join.
// It has no wire representation and owns no second ledger or budget.
type PathFreezeExecution struct {
	owner string
	layout pathpolicy.LayoutV1
	workPath string
	check func() error
	publish func(int) error
}

// IssuePathFreezeExecution is a trusted Core constructor. check is a pure RAM
// lifetime/F-ACK guard. publish runs outside protocol/Task/manager locks and
// owns the durable publishing transition, same-root noReplace and final ACK.
// The index selects exactly Layout.Members[index].RelativePath under workPath;
// a protocol cannot supply another path or enlarge the frozen final set.
func IssuePathFreezeExecution(owner string, layout pathpolicy.LayoutV1, workPath string, check func() error, publish func(int) error) (*PathFreezeExecution,error) {
	if owner == "" || check == nil || publish == nil || !filepath.IsAbs(workPath) || filepath.Dir(workPath) != layout.ParentRoot {
		return nil,ErrRecoveryHeld
	}
	owned,err := pathpolicy.BuildLayoutV1(layout.ParentRoot,layout.Members)
	if err != nil { return nil,ErrRecoveryHeld }
	for _,member := range owned.Members {
		// Media final outputs are bounded sibling files. Dynamic fragments
		// stay inside the separately acknowledged work directory.
		if filepath.Base(member.RelativePath) != member.RelativePath { return nil,ErrRecoveryHeld }
	}
	if err := check(); err != nil { return nil,err }
	return &PathFreezeExecution{owner:owner,layout:owned,workPath:workPath,check:check,publish:publish},nil
}

func (execution *PathFreezeExecution) OwnerID() string {
	if execution == nil { return "" }; return execution.owner
}
func (execution *PathFreezeExecution) WorkPath() string {
	if execution == nil { return "" }; return execution.workPath
}
func (execution *PathFreezeExecution) Layout() pathpolicy.LayoutV1 {
	if execution == nil { return pathpolicy.LayoutV1{} }
	owned := execution.layout
	owned.Members = append([]pathpolicy.Member(nil),owned.Members...)
	return owned
}
func (execution *PathFreezeExecution) Check() error {
	if execution == nil || execution.check == nil { return ErrRecoveryHeld }
	return execution.check()
}
func (execution *PathFreezeExecution) Publish(index int) error {
	if err := execution.Check(); err != nil { return err }
	if index < 0 || index >= len(execution.layout.Members) || execution.publish == nil { return ErrRecoveryHeld }
	if err := execution.publish(index); err != nil { return err }
	return execution.Check()
}
