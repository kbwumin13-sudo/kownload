package download

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/mcpauthority"
	"github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"
)

var ErrMCPWriteConfiguration = errors.New("mcp_write_configuration_unavailable")

// The holder is installed only by trusted startup. Its identity and client are
// immutable; neither caller-provided P nor a stored receipt can manufacture it.
type mcpWriteHolder struct {
	authority *mcpauthority.VerifiedAuthority
	client    *transfer.MCPHTTPClient
	dataset   string
	network   string
	policy    mcpwritepolicy.PolicySnapshotV1
	closing   atomic.Bool
	tasks     sync.Map // TaskID -> *mcpTaskRuntime; never reconstructed by Query
}

// ConfigureMCPWrites must run after explicit dataset initialization and before
// Setup. It owns the verified authority; the client must be the trusted startup
// instance, never a model-provided transport or a client selected by PolicyID.
func (d *Downloader) ConfigureMCPWrites(policy *mcpwritepolicy.Policy, client *transfer.MCPHTTPClient) error {
	if policy == nil || client == nil || client.PolicyID() == "" || d.closed.Load() {
		return ErrMCPWriteConfiguration
	}
	d.mcpConfigMu.Lock()
	if d.mcpSetupStarted || d.mcpConfiguring || d.mcpConfigured != nil {
		d.mcpConfigMu.Unlock()
		return ErrMCPWriteConfiguration
	}
	d.mcpConfiguring = true
	d.mcpConfigMu.Unlock()
	// Dataset view, ACL/membership calls and pathname probes run without Core
	// or holder locks. A failed configuration does not create or repair D.
	identity, err := d.DatasetIdentity()
	var authority *mcpauthority.VerifiedAuthority
	var snapshot mcpwritepolicy.PolicySnapshotV1
	if err == nil {
		snapshot, err = policy.SnapshotV1()
	}
	if err == nil {
		authority, err = mcpauthority.OpenVerifiedAuthority(policy, identity.DatasetID, client.PolicyID())
	}
	d.mcpConfigMu.Lock()
	d.mcpConfiguring = false
	if err == nil && !d.closed.Load() && !d.mcpSetupStarted {
		d.mcpConfigured = &mcpWriteHolder{authority: authority, client: client, dataset: identity.DatasetID, network: client.PolicyID(), policy: snapshot}
		d.mcpConfigMu.Unlock()
		return nil
	}
	d.mcpConfigMu.Unlock()
	if authority != nil {
		_ = authority.Close()
	}
	return ErrMCPWriteConfiguration
}

func (d *Downloader) beginMCPSetup() error {
	d.mcpConfigMu.Lock()
	defer d.mcpConfigMu.Unlock()
	if d.mcpConfiguring {
		return ErrMCPWriteConfiguration
	}
	d.mcpSetupStarted = true
	return nil
}

func (d *Downloader) checkMCPSetupDataset() error {
	d.mcpConfigMu.Lock()
	holder := d.mcpConfigured
	d.mcpConfigMu.Unlock()
	if holder == nil {
		return nil
	}
	identity, err := d.DatasetIdentity()
	if err != nil || identity.DatasetID != holder.dataset || holder.closing.Load() {
		return ErrMCPWriteConfiguration
	}
	return nil
}

func (d *Downloader) mcpWriteHolder() *mcpWriteHolder {
	d.mcpConfigMu.Lock()
	holder := d.mcpConfigured
	d.mcpConfigMu.Unlock()
	if holder == nil || holder.closing.Load() || d.closed.Load() {
		return nil
	}
	return holder
}

func (d *Downloader) stopMCPWrites() {
	d.mcpConfigMu.Lock()
	holder := d.mcpConfigured
	if holder != nil {
		holder.closing.Store(true)
	}
	d.mcpConfigMu.Unlock()
	if holder != nil {
		// Closing disables new root borrowing immediately. Busy is expected
		// while Task-owned leases drain; closeMCPWrites reports the final result.
		_ = holder.authority.Close()
	}
}

func (d *Downloader) closeMCPWrites() error {
	d.mcpConfigMu.Lock()
	holder := d.mcpConfigured
	d.mcpConfigMu.Unlock()
	if holder == nil {
		return nil
	}
	holder.tasks.Range(func(key, value any) bool {
		entry := value.(*mcpTaskRuntime)
		entry.mu.Lock()
		lease := entry.lease
		entry.lease = nil
		entry.mu.Unlock()
		holder.tasks.Delete(key)
		if lease != nil {
			_ = lease.Close()
		}
		return true
	})
	holder.client.CloseIdleConnections()
	return holder.authority.Close()
}
