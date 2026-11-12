package rest

import (
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/mcpauthority/policyfile"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
)

// An explicit invalid path/configuration fails startup. Absence keeps the
// default five reads and zero write authority; no HTTP option enables writes.
func configureMCPWriteStartup(core *download.Downloader, cfg *model.StartConfig) error {
	path := cfg.MCPWritePolicyFile
	if launchPath := os.Getenv("KOWNLOAD_MCP_WRITE_POLICY_FILE"); launchPath != "" {
		if path != "" && path != launchPath {
			return download.ErrMCPWriteConfiguration
		}
		path = launchPath
	}
	if path == "" {
		if cfg.MCPWriteHTTPClient != nil {
			return download.ErrMCPWriteConfiguration
		}
		if os.Getenv("KOWNLOAD_MCP_WRITE_ALLOWED_CIDRS") != "" || os.Getenv("KOWNLOAD_MCP_WRITE_ALLOWED_PORTS") != "" {
			return download.ErrMCPWriteConfiguration
		}
		return nil
	}
	if cfg.Network != "unix" {
		return download.ErrMCPWriteConfiguration
	}
	policy, err := policyfile.LoadPolicyFile(path)
	if err != nil {
		return download.ErrMCPWriteConfiguration
	}
	client := cfg.MCPWriteHTTPClient
	created := client == nil
	networkConfig, err := mcpWriteLaunchNetwork(os.Getenv("KOWNLOAD_MCP_WRITE_ALLOWED_CIDRS"), os.Getenv("KOWNLOAD_MCP_WRITE_ALLOWED_PORTS"))
	if err != nil {
		return err
	}
	if !created && (len(networkConfig.AllowedCIDRs) != 0 || len(networkConfig.AllowedPorts) != 0) {
		return download.ErrMCPWriteConfiguration
	}
	if created {
		network, err := transfer.NewMCPNetworkPolicy(networkConfig, nil, nil)
		if err != nil {
			return download.ErrMCPWriteConfiguration
		}
		client, err = transfer.NewMCPHTTPClient(network, transfer.MCPHTTPOptions{})
		if err != nil {
			return download.ErrMCPWriteConfiguration
		}
	}
	if err = core.ConfigureMCPWrites(policy, client); err != nil {
		if created {
			client.CloseIdleConnections()
		}
		return download.ErrMCPWriteConfiguration
	}
	return nil // Ownership passes to this Core only after Configure succeeds.
}

// These comma-separated values come only from this process's trusted launch
// environment. They never enter the HTTP/FFI config or persisted GUI settings.
func mcpWriteLaunchNetwork(cidrs, ports string) (transfer.MCPNetworkPolicyConfig, error) {
	var config transfer.MCPNetworkPolicyConfig
	if cidrs != "" {
		for _, item := range strings.Split(cidrs, ",") {
			item = strings.TrimSpace(item)
			prefix, err := netip.ParsePrefix(item)
			if err != nil || prefix != prefix.Masked() || prefix.String() != item {
				return config, download.ErrMCPWriteConfiguration
			}
			config.AllowedCIDRs = append(config.AllowedCIDRs, prefix)
		}
	}
	if ports != "" {
		for _, item := range strings.Split(ports, ",") {
			item = strings.TrimSpace(item)
			port, err := strconv.ParseUint(item, 10, 16)
			if err != nil || port == 0 || strconv.FormatUint(port, 10) != item {
				return config, download.ErrMCPWriteConfiguration
			}
			config.AllowedPorts = append(config.AllowedPorts, uint16(port))
		}
	}
	return config, nil
}
