package download

import "github.com/GopeedLab/gopeed/internal/transfer"

func parentRecoveryProtocol(protocol string) bool {
	return protocol == "netdisk" || protocol == "media"
}

func recoveryProtocolCompatible(protocol string, version int) bool {
	return protocol == "http" || version == recoverySharedVersion && parentRecoveryProtocol(protocol)
}

func recoveryTaskSourceHash(task *Task, defaults string) (string, error) {
	if task == nil || task.Meta == nil || task.Meta.Req == nil {
		return "", transfer.ErrRecoveryHeld
	}
	if task.Protocol == "http" {
		return recoverySourceHash(task.Meta.Req, defaults)
	}
	return transfer.ProtocolExecutionSourceHash(task.Protocol, task.Meta.Req, defaults)
}
