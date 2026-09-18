package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

type NetConf struct {
	types.NetConf
	ContainerIP string `json:"containerIP"`
	GWIP        string `json:"gwIP"`
	// If empty, defaults to /32 for both (works across host/container subnets)
	Prefix int `json:"prefix"`
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, "routed-cni")
}

func cmdAdd(args *skel.CmdArgs) error {
	conf := &NetConf{}
	if err := json.Unmarshal(args.StdinData, conf); err != nil {
		return err
	}
	if conf.ContainerIP == "" || conf.GWIP == "" {
		// try CNI_ARGS (Nomad can pass containerIP=... here)
		for _, kv := range strings.Split(args.Args, ";") {
			if v, ok := strings.CutPrefix(kv, "containerIP="); ok && conf.ContainerIP == "" {
				conf.ContainerIP = v
			}
			if v, ok := strings.CutPrefix(kv, "IP="); ok && conf.ContainerIP == "" {
				conf.ContainerIP = v
			}
		}
	}
	if conf.ContainerIP == "" || conf.GWIP == "" {
		return fmt.Errorf("containerIP and gwIP required")
	}

	id := args.ContainerID
	if len(id) > 8 {
		id = id[:8]
	}
	hostName := "vethh-" + id
	contName := "vethc-" + id

	// create veth pair
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: hostName},
		PeerName:  contName,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("link add: %w", err)
	}

	hostLink, err := netlink.LinkByName(hostName)
	if err != nil {
		return err
	}
	netlink.LinkSetUp(hostLink)
	if err := netlink.LinkSetAlias(hostLink, "healthcheck:ok"); err != nil {
		return fmt.Errorf("set alias: %w", err)
	}

	// ensure sysctls (ip_forward once is enough; rp_filter=2 on veth for host IP reachability from container)
	_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0644)
	_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/rp_filter", hostName), []byte("2\n"), 0644)
	_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/proxy_arp", hostName), []byte("1\n"), 0644)

	pfx := conf.Prefix
	if pfx == 0 {
		pfx = 32
	}

	// move cont end to netns
	contLink, err := netlink.LinkByName(contName)
	if err != nil {
		return err
	}
	nsFile, err := os.Open(args.Netns)
	if err != nil {
		return err
	}
	defer nsFile.Close()
	if err := netlink.LinkSetNsFd(contLink, int(nsFile.Fd())); err != nil {
		return fmt.Errorf("set ns: %w", err)
	}

	// config inside container ns
	contIPNet, err := netlink.ParseAddr(conf.ContainerIP + fmt.Sprintf("/%d", pfx))
	if err != nil {
		return fmt.Errorf("cont addr: %w", err)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	origNs, err := netns.Get()
	if err != nil {
		return err
	}
	newNs, err := netns.GetFromPath(args.Netns)
	if err != nil {
		return err
	}
	if err := netns.Set(newNs); err != nil {
		return err
	}
	l, err := netlink.LinkByName(contName)
	if err != nil {
		netns.Set(origNs)
		return err
	}
	// Once in the namespace, we can do things like set the network name to eth0
	if err := netlink.LinkSetName(l, "eth0"); err != nil {
		netns.Set(origNs)
		return err
	}
	if err := netlink.LinkSetUp(l); err != nil {
		netns.Set(origNs)
		return err
	}
	if err := netlink.AddrAdd(l, contIPNet); err != nil {
		netns.Set(origNs)
		return err
	}
	rt := &netlink.Route{
		LinkIndex: l.Attrs().Index,
		Gw:        net.ParseIP(conf.GWIP),
		Flags:     int(netlink.FLAG_ONLINK),
	}
	if err := netlink.RouteReplace(rt); err != nil {
		netns.Set(origNs)
		return err
	}
	netns.Set(origNs)

	// ensure host route for container IP (via host veth) so router-delivered packets reach the ns
	hostRt := &netlink.Route{
		LinkIndex: hostLink.Attrs().Index,
		Dst:       &net.IPNet{IP: net.ParseIP(conf.ContainerIP), Mask: net.CIDRMask(pfx, pfx)},
	}
	if err := netlink.RouteReplace(hostRt); err != nil {
		return fmt.Errorf("host route: %w", err)
	}
	if err := setupForward(conf.ContainerIP); err != nil {
		return fmt.Errorf("forward rules: %w", err)
	}

	idx := 0
	res := &current.Result{
		CNIVersion: conf.CNIVersion,
		Interfaces: []*current.Interface{
			{Name: "eth0", Sandbox: args.Netns},
		},
		IPs: []*current.IPConfig{
			{Address: *contIPNet.IPNet, Interface: &idx},
		},
	}
	return types.PrintResult(res, conf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	conf := &NetConf{}
	json.Unmarshal(args.StdinData, conf)
	if conf.ContainerIP == "" {
		for _, kv := range strings.Split(args.Args, ";") {
			if v, ok := strings.CutPrefix(kv, "containerIP="); ok {
				conf.ContainerIP = v
			}
			if v, ok := strings.CutPrefix(kv, "IP="); ok {
				conf.ContainerIP = v
			}
		}
	}
	pfx := conf.Prefix
	if pfx == 0 {
		pfx = 32
	}
	id := args.ContainerID
	if len(id) > 8 {
		id = id[:8]
	}
	hostName := "vethh-" + id
	hostLink, err := netlink.LinkByName(hostName)
	if err == nil {
		if conf.ContainerIP != "" {
			hostRt := &netlink.Route{
				LinkIndex: hostLink.Attrs().Index,
				Dst:       &net.IPNet{IP: net.ParseIP(conf.ContainerIP), Mask: net.CIDRMask(pfx, pfx)},
			}
			netlink.RouteDel(hostRt)
			teardownForward(conf.ContainerIP)
		}
		netlink.LinkDel(hostLink)
	} else if conf.ContainerIP != "" {
		teardownForward(conf.ContainerIP)
	}
	return nil
}

func cmdCheck(args *skel.CmdArgs) error {
	return nil
}

// full allow: routed /32 has no bridge iface for NOMAD-ADMIN-style -o matching
func forwardRules(ip string) [][]string {
	cidr := ip + "/32"
	return [][]string{
		{"-d", cidr, "-j", "ACCEPT"},
		{"-s", cidr, "-j", "ACCEPT"},
	}
}

func setupForward(ip string) error {
	ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
	if err != nil {
		return err
	}
	if err := ensureCNIForward(ipt); err != nil {
		return err
	}
	for _, r := range forwardRules(ip) {
		if err := ipt.AppendUnique("filter", "CNI-FORWARD", r...); err != nil {
			return err
		}
	}
	return nil
}

func teardownForward(ip string) {
	ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
	if err != nil {
		return
	}
	for _, r := range forwardRules(ip) {
		_ = ipt.Delete("filter", "CNI-FORWARD", r...)
	}
}

func ensureCNIForward(ipt *iptables.IPTables) error {
	ok, err := ipt.ChainExists("filter", "CNI-FORWARD")
	if err != nil {
		return err
	}
	if !ok {
		if err := ipt.NewChain("filter", "CNI-FORWARD"); err != nil {
			return err
		}
	}
	jump := []string{"-m", "comment", "--comment", "CNI firewall plugin rules", "-j", "CNI-FORWARD"}
	ok, err = ipt.Exists("filter", "FORWARD", jump...)
	if err != nil {
		return err
	}
	if !ok {
		return ipt.Insert("filter", "FORWARD", 1, jump...)
	}
	return nil
}
