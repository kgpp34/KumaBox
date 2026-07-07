package network

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/types"
	types100 "github.com/containernetworking/cni/pkg/types/100"

	"github.com/kumabox/kumabox/internal/config"
)

type CNIAddRequest struct {
	VMID      string
	Network   string
	Index     int
	NetNSPath string
	CPU       int
	Existing  *Config
}

type CNIDeleteRequest struct {
	VMID      string
	Network   string
	IfName    string
	NetNSPath string
}

type cniNetworkConfig struct {
	list *libcni.NetworkConfigList
	net  *libcni.NetworkConfig
	name string
}

func AddCNI(ctx context.Context, rootDir string, cfg config.NetworkConfig, req CNIAddRequest) (*Allocation, error) {
	if req.VMID == "" {
		return nil, fmt.Errorf("vm id must not be empty")
	}
	networkName := CNIName(req.Network, cfg.Default)
	cniConfig, err := loadCNIConfig(cfg.CNIConfigDir, networkName)
	if err != nil {
		return nil, err
	}
	netnsPath := req.NetNSPath
	if netnsPath == "" {
		netnsPath = "/proc/self/ns/net"
	}
	ifName := TapName(cfg.TapPrefix, req.VMID, req.Index)
	mac, err := GenerateMAC()
	if err != nil {
		return nil, err
	}
	if req.Existing != nil {
		if req.Existing.TAP != "" {
			ifName = req.Existing.TAP
		}
		if req.Existing.MAC != "" {
			mac = strings.ToLower(req.Existing.MAC)
		}
		if req.Existing.NetnsPath != "" {
			netnsPath = req.Existing.NetnsPath
		}
	}

	runtimeConf := &libcni.RuntimeConf{
		ContainerID: req.VMID,
		NetNS:       netnsPath,
		IfName:      ifName,
		Args: [][2]string{
			{"KUMABOX_VM_ID", req.VMID},
			{"KUMABOX_NETWORK", networkName},
		},
	}
	cni := libcni.NewCNIConfigWithCacheDir(
		[]string{cfg.CNIBinDir},
		filepath.Join(rootDir, "network", "cni-cache"),
		nil,
	)
	result, err := addCNIConfig(ctx, cni, cniConfig, runtimeConf)
	if err != nil {
		return nil, fmt.Errorf("cni add %s for VM %s: %w", networkName, req.VMID, err)
	}
	current, err := types100.GetResult(result)
	if err != nil {
		return nil, fmt.Errorf("parse cni result: %w", err)
	}

	guest := guestInfoFromCNIResult(current)
	if resultMAC := macFromCNIResult(current, ifName); resultMAC != "" {
		mac = resultMAC
	}
	now := time.Now().UTC()
	record := Record{
		ID:        NetworkID(req.VMID, req.Index),
		VMID:      req.VMID,
		Network:   req.Network,
		Provider:  ProviderCNI,
		IfName:    ifName,
		TAP:       ifName,
		MAC:       mac,
		NumQueues: netNumQueues(req.CPU),
		QueueSize: defaultQueueSize,
		NetnsPath: netnsPath,
		Gateway:   guestGateway(guest),
		DNS:       guestDNS(guest),
		Cleanup:   Cleanup{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if guest != nil && guest.IP != "" {
		record.IPs = []string{fmt.Sprintf("%s/%d", guest.IP, guest.Prefix)}
	}
	vmConfig := Config{
		ID:        record.ID,
		TAP:       record.TAP,
		MAC:       record.MAC,
		NumQueues: record.NumQueues,
		QueueSize: record.QueueSize,
		Backend:   record.Provider,
		NetnsPath: record.NetnsPath,
		Network:   guest,
	}
	return &Allocation{Record: record, Config: vmConfig}, nil
}

func DeleteCNI(ctx context.Context, rootDir string, cfg config.NetworkConfig, req CNIDeleteRequest) error {
	if req.VMID == "" {
		return fmt.Errorf("vm id must not be empty")
	}
	networkName := CNIName(req.Network, cfg.Default)
	cniConfig, err := loadCNIConfig(cfg.CNIConfigDir, networkName)
	if err != nil {
		return err
	}
	if req.IfName == "" {
		return fmt.Errorf("cni interface name must not be empty")
	}
	netnsPath := req.NetNSPath
	if netnsPath == "" {
		netnsPath = "/proc/self/ns/net"
	}
	runtimeConf := &libcni.RuntimeConf{
		ContainerID: req.VMID,
		NetNS:       netnsPath,
		IfName:      req.IfName,
		Args: [][2]string{
			{"KUMABOX_VM_ID", req.VMID},
			{"KUMABOX_NETWORK", networkName},
		},
	}
	cni := libcni.NewCNIConfigWithCacheDir(
		[]string{cfg.CNIBinDir},
		filepath.Join(rootDir, "network", "cni-cache"),
		nil,
	)
	if err := delCNIConfig(ctx, cni, cniConfig, runtimeConf); err != nil {
		return fmt.Errorf("cni del %s for VM %s: %w", networkName, req.VMID, err)
	}
	return nil
}

func CNIName(network, fallback string) string {
	if strings.HasPrefix(network, "cni:") {
		name := strings.TrimPrefix(network, "cni:")
		if name != "" {
			return name
		}
	}
	if network == ProviderCNI && fallback != "" {
		return fallback
	}
	if network != "" && network != ProviderCNI {
		return network
	}
	if fallback != "" {
		return fallback
	}
	return "default"
}

func IsCNISelection(network string) bool {
	return network == ProviderCNI || strings.HasPrefix(network, "cni:")
}

func loadCNIConfig(configDir, name string) (*cniNetworkConfig, error) {
	if configDir == "" {
		return nil, fmt.Errorf("cni config dir must not be empty")
	}
	if name == "" {
		return nil, fmt.Errorf("cni network name must not be empty")
	}
	if list, err := libcni.LoadConfList(configDir, name); err == nil {
		return &cniNetworkConfig{list: list, name: list.Name}, nil
	}
	netConf, err := libcni.LoadConf(configDir, name)
	if err != nil {
		return nil, fmt.Errorf("load cni config %q from %s: %w", name, configDir, err)
	}
	return &cniNetworkConfig{net: netConf, name: netConf.Network.Name}, nil
}

func addCNIConfig(
	ctx context.Context,
	cni *libcni.CNIConfig,
	config *cniNetworkConfig,
	runtimeConf *libcni.RuntimeConf,
) (types.Result, error) {
	if config.list != nil {
		return cni.AddNetworkList(ctx, config.list, runtimeConf)
	}
	return cni.AddNetwork(ctx, config.net, runtimeConf)
}

func delCNIConfig(
	ctx context.Context,
	cni *libcni.CNIConfig,
	config *cniNetworkConfig,
	runtimeConf *libcni.RuntimeConf,
) error {
	if config.list != nil {
		return cni.DelNetworkList(ctx, config.list, runtimeConf)
	}
	return cni.DelNetwork(ctx, config.net, runtimeConf)
}

func guestInfoFromCNIResult(result *types100.Result) *GuestInfo {
	if result == nil || len(result.IPs) == 0 || result.IPs[0] == nil {
		return nil
	}
	ipConfig := result.IPs[0]
	ip := ipConfig.Address.IP.To4()
	if ip == nil {
		return nil
	}
	prefix, _ := ipConfig.Address.Mask.Size()
	guest := &GuestInfo{
		IP:     ip.String(),
		Prefix: prefix,
		DNS:    append([]string(nil), result.DNS.Nameservers...),
	}
	if ipConfig.Gateway != nil {
		guest.Gateway = ipConfig.Gateway.String()
	}
	return guest
}

func macFromCNIResult(result *types100.Result, ifName string) string {
	if result == nil {
		return ""
	}
	for _, intf := range result.Interfaces {
		if intf != nil && intf.Name == ifName && intf.Mac != "" {
			if _, err := net.ParseMAC(intf.Mac); err == nil {
				return strings.ToLower(intf.Mac)
			}
		}
	}
	return ""
}

func guestGateway(guest *GuestInfo) string {
	if guest == nil {
		return ""
	}
	return guest.Gateway
}

func guestDNS(guest *GuestInfo) []string {
	if guest == nil {
		return nil
	}
	return append([]string(nil), guest.DNS...)
}
