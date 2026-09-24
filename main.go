package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	defaultBridge     = "routedbr"
	defaultBridgeCIDR = "172.27.64.0/20"
	vipTable          = 101
)

// overridable for tests
var dataDir = "/var/lib/cni/routed-cni"

type NetConf struct {
	types.NetConf
	ContainerIP string `json:"containerIP"`
	GWIP        string `json:"gwIP"`
	// If empty, defaults to /32 for VIP (works across host/container subnets)
	Prefix     int    `json:"prefix"`
	Bridge     string `json:"bridge"`
	BridgeCIDR string `json:"bridgeCIDR"`
	BridgeIP   string `json:"bridgeIP"` // optional; else allocated from BridgeCIDR
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, "routed-cni")
}

func parseConf(args *skel.CmdArgs) (*NetConf, error) {
	conf := &NetConf{}
	if err := json.Unmarshal(args.StdinData, conf); err != nil {
		return nil, err
	}
	for _, kv := range strings.Split(args.Args, ";") {
		if v, ok := strings.CutPrefix(kv, "containerIP="); ok && conf.ContainerIP == "" {
			conf.ContainerIP = v
		}
		if v, ok := strings.CutPrefix(kv, "IP="); ok && conf.ContainerIP == "" {
			conf.ContainerIP = v
		}
		if v, ok := strings.CutPrefix(kv, "bridgeIP="); ok && conf.BridgeIP == "" {
			conf.BridgeIP = v
		}
	}
	if conf.Bridge == "" {
		conf.Bridge = defaultBridge
	}
	if conf.BridgeCIDR == "" {
		conf.BridgeCIDR = defaultBridgeCIDR
	}
	if conf.Prefix == 0 {
		conf.Prefix = 32
	}
	return conf, nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func cmdAdd(args *skel.CmdArgs) error {
	conf, err := parseConf(args)
	if err != nil {
		return err
	}
	if conf.ContainerIP == "" || conf.GWIP == "" {
		return fmt.Errorf("containerIP and gwIP required")
	}

	_, brNet, err := net.ParseCIDR(conf.BridgeCIDR)
	if err != nil {
		return fmt.Errorf("bridgeCIDR: %w", err)
	}
	brGW := gatewayIP(brNet)
	brLink, err := ensureBridge(conf.Bridge, brGW, brNet)
	if err != nil {
		return err
	}

	brIP := net.ParseIP(conf.BridgeIP)
	if brIP == nil {
		brIP, err = allocateBridgeIP(brNet, brGW, args.ContainerID)
		if err != nil {
			return err
		}
	} else {
		if err := reserveBridgeIP(brIP, args.ContainerID); err != nil {
			return err
		}
	}

	id := shortID(args.ContainerID)
	vipHost, vipCont := "vethh-"+id, "vethc-"+id
	brHost, brCont := "vethb-"+id, "vethd-"+id

	if err := addVeth(vipHost, vipCont); err != nil {
		return err
	}
	if err := addVeth(brHost, brCont); err != nil {
		cleanupLink(vipHost)
		return err
	}

	vipHostLink, err := netlink.LinkByName(vipHost)
	if err != nil {
		return err
	}
	brHostLink, err := netlink.LinkByName(brHost)
	if err != nil {
		return err
	}
	_ = netlink.LinkSetUp(vipHostLink)
	_ = netlink.LinkSetUp(brHostLink)
	_ = netlink.LinkSetAlias(vipHostLink, "healthcheck:ok")
	if err := netlink.LinkSetMaster(brHostLink, brLink); err != nil {
		return fmt.Errorf("set master: %w", err)
	}

	_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0644)
	for _, n := range []string{vipHost, brHost, conf.Bridge} {
		_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/rp_filter", n), []byte("2\n"), 0644)
	}
	_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/proxy_arp", vipHost), []byte("1\n"), 0644)

	nsFile, err := os.Open(args.Netns)
	if err != nil {
		return err
	}
	defer nsFile.Close()
	for _, name := range []string{vipCont, brCont} {
		l, err := netlink.LinkByName(name)
		if err != nil {
			return err
		}
		if err := netlink.LinkSetNsFd(l, int(nsFile.Fd())); err != nil {
			return fmt.Errorf("set ns %s: %w", name, err)
		}
	}

	vipAddr, err := netlink.ParseAddr(conf.ContainerIP + fmt.Sprintf("/%d", conf.Prefix))
	if err != nil {
		return fmt.Errorf("vip addr: %w", err)
	}
	ones, _ := brNet.Mask.Size()
	brAddr, err := netlink.ParseAddr(fmt.Sprintf("%s/%d", brIP, ones))
	if err != nil {
		return fmt.Errorf("bridge addr: %w", err)
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
	setupErr := func() error {
		eth0, err := renameUp(vipCont, "eth0")
		if err != nil {
			return err
		}
		eth1, err := renameUp(brCont, "eth1")
		if err != nil {
			return err
		}
		if err := netlink.AddrAdd(eth0, vipAddr); err != nil {
			return err
		}
		if err := netlink.AddrAdd(eth1, brAddr); err != nil {
			return err
		}
		// local/default via bridge (Nomad-like)
		if err := netlink.RouteReplace(&netlink.Route{
			LinkIndex: eth1.Attrs().Index,
			Gw:        brGW,
		}); err != nil {
			return fmt.Errorf("default via bridge: %w", err)
		}
		// VIP-sourced traffic still exits via host gw on eth0
		rule := netlink.NewRule()
		rule.Src = &net.IPNet{IP: net.ParseIP(conf.ContainerIP), Mask: net.CIDRMask(32, 32)}
		rule.Table = vipTable
		_ = netlink.RuleDel(rule) // idempotent replace
		if err := netlink.RuleAdd(rule); err != nil {
			return fmt.Errorf("vip rule: %w", err)
		}
		if err := netlink.RouteReplace(&netlink.Route{
			Table:     vipTable,
			LinkIndex: eth0.Attrs().Index,
			Gw:        net.ParseIP(conf.GWIP),
			Flags:     int(netlink.FLAG_ONLINK),
		}); err != nil {
			return fmt.Errorf("vip default: %w", err)
		}
		return nil
	}()
	_ = netns.Set(origNs)
	if setupErr != nil {
		return setupErr
	}

	if err := netlink.RouteReplace(&netlink.Route{
		LinkIndex: vipHostLink.Attrs().Index,
		Dst:       &net.IPNet{IP: net.ParseIP(conf.ContainerIP), Mask: net.CIDRMask(conf.Prefix, conf.Prefix)},
	}); err != nil {
		return fmt.Errorf("host vip route: %w", err)
	}
	if err := setupForward(conf.ContainerIP); err != nil {
		return fmt.Errorf("vip forward: %w", err)
	}
	if err := setupForward(brIP.String()); err != nil {
		return fmt.Errorf("bridge forward: %w", err)
	}
	if err := ensureMasq(conf.BridgeCIDR, conf.Bridge); err != nil {
		return fmt.Errorf("masq: %w", err)
	}

	eth0idx, eth1idx := 0, 1
	res := &current.Result{
		CNIVersion: conf.CNIVersion,
		Interfaces: []*current.Interface{
			{Name: "eth0", Sandbox: args.Netns},
			{Name: "eth1", Sandbox: args.Netns},
		},
		IPs: []*current.IPConfig{
			{Address: *vipAddr.IPNet, Interface: &eth0idx},
			{Address: *brAddr.IPNet, Interface: &eth1idx},
		},
	}
	return types.PrintResult(res, conf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	conf, _ := parseConf(args)
	id := shortID(args.ContainerID)

	brIP := ""
	if conf != nil && conf.BridgeIP != "" {
		brIP = conf.BridgeIP
	}
	if brIP == "" {
		brIP = lookupBridgeIP(args.ContainerID)
	}

	if conf != nil && conf.ContainerIP != "" {
		// best-effort: clear vip policy in netns if still present
		if args.Netns != "" {
			_ = withNetns(args.Netns, func() error {
				rule := netlink.NewRule()
				rule.Src = &net.IPNet{IP: net.ParseIP(conf.ContainerIP), Mask: net.CIDRMask(32, 32)}
				rule.Table = vipTable
				_ = netlink.RuleDel(rule)
				_ = netlink.RouteDel(&netlink.Route{Table: vipTable, Dst: nil})
				return nil
			})
		}
		if hostLink, err := netlink.LinkByName("vethh-" + id); err == nil {
			pfx := 32
			if conf.Prefix != 0 {
				pfx = conf.Prefix
			}
			_ = netlink.RouteDel(&netlink.Route{
				LinkIndex: hostLink.Attrs().Index,
				Dst:       &net.IPNet{IP: net.ParseIP(conf.ContainerIP), Mask: net.CIDRMask(pfx, pfx)},
			})
			_ = netlink.LinkDel(hostLink)
		}
		teardownForward(conf.ContainerIP)
	} else if hostLink, err := netlink.LinkByName("vethh-" + id); err == nil {
		_ = netlink.LinkDel(hostLink)
	}

	if brHost, err := netlink.LinkByName("vethb-" + id); err == nil {
		_ = netlink.LinkDel(brHost)
	}
	if brIP != "" {
		teardownForward(brIP)
		releaseBridgeIP(brIP, args.ContainerID)
	}
	return nil
}

func cmdCheck(args *skel.CmdArgs) error {
	return nil
}

func addVeth(hostName, peerName string) error {
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: hostName},
		PeerName:  peerName,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("link add %s: %w", hostName, err)
	}
	return nil
}

func cleanupLink(name string) {
	if l, err := netlink.LinkByName(name); err == nil {
		_ = netlink.LinkDel(l)
	}
}

func renameUp(from, to string) (netlink.Link, error) {
	l, err := netlink.LinkByName(from)
	if err != nil {
		return nil, err
	}
	if err := netlink.LinkSetName(l, to); err != nil {
		return nil, err
	}
	l, err = netlink.LinkByName(to)
	if err != nil {
		return nil, err
	}
	if err := netlink.LinkSetUp(l); err != nil {
		return nil, err
	}
	return l, nil
}

func ensureBridge(name string, gw net.IP, subnet *net.IPNet) (netlink.Link, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}
		if err := netlink.LinkAdd(br); err != nil {
			return nil, fmt.Errorf("bridge add: %w", err)
		}
		l, err = netlink.LinkByName(name)
		if err != nil {
			return nil, err
		}
	}
	if err := netlink.LinkSetUp(l); err != nil {
		return nil, err
	}
	ones, _ := subnet.Mask.Size()
	addr, err := netlink.ParseAddr(fmt.Sprintf("%s/%d", gw, ones))
	if err != nil {
		return nil, err
	}
	addrs, _ := netlink.AddrList(l, netlink.FAMILY_V4)
	have := false
	for _, a := range addrs {
		if a.IP.Equal(gw) {
			have = true
			break
		}
	}
	if !have {
		if err := netlink.AddrAdd(l, addr); err != nil {
			return nil, fmt.Errorf("bridge addr: %w", err)
		}
	}
	return l, nil
}

func gatewayIP(subnet *net.IPNet) net.IP {
	ip := append(net.IP(nil), subnet.IP.To4()...)
	ip[3]++
	return ip
}

func nextIP(ip net.IP) net.IP {
	ip = append(net.IP(nil), ip.To4()...)
	for i := 3; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
	return ip
}

func lastIP(subnet *net.IPNet) net.IP {
	ip := append(net.IP(nil), subnet.IP.To4()...)
	for i, b := range []byte(subnet.Mask) {
		ip[i] |= ^b
	}
	return ip
}

func allocateBridgeIP(subnet *net.IPNet, gateway net.IP, id string) (net.IP, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "ips"), 0755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "by-id"), 0755); err != nil {
		return nil, err
	}
	unlock, err := flock(filepath.Join(dataDir, "lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()

	if existing := lookupBridgeIPLocked(id); existing != nil {
		return existing, nil
	}

	broadcast := lastIP(subnet)
	for ip := nextIP(subnet.IP); subnet.Contains(ip); ip = nextIP(ip) {
		if ip.Equal(gateway) || ip.Equal(broadcast) {
			continue
		}
		path := filepath.Join(dataDir, "ips", ip.String())
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			continue
		}
		_, _ = f.WriteString(id)
		f.Close()
		_ = os.WriteFile(filepath.Join(dataDir, "by-id", id), []byte(ip.String()), 0644)
		return ip, nil
	}
	return nil, fmt.Errorf("bridge subnet exhausted")
}

func reserveBridgeIP(ip net.IP, id string) error {
	if err := os.MkdirAll(filepath.Join(dataDir, "ips"), 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "by-id"), 0755); err != nil {
		return err
	}
	unlock, err := flock(filepath.Join(dataDir, "lock"))
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(dataDir, "ips", ip.String())
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("bridgeIP %s in use: %w", ip, err)
	}
	_, _ = f.WriteString(id)
	f.Close()
	return os.WriteFile(filepath.Join(dataDir, "by-id", id), []byte(ip.String()), 0644)
}

func releaseBridgeIP(ip, id string) {
	unlock, err := flock(filepath.Join(dataDir, "lock"))
	if err != nil {
		return
	}
	defer unlock()
	_ = os.Remove(filepath.Join(dataDir, "ips", ip))
	_ = os.Remove(filepath.Join(dataDir, "by-id", id))
}

func lookupBridgeIP(id string) string {
	unlock, err := flock(filepath.Join(dataDir, "lock"))
	if err != nil {
		b, _ := os.ReadFile(filepath.Join(dataDir, "by-id", id))
		return strings.TrimSpace(string(b))
	}
	defer unlock()
	ip := lookupBridgeIPLocked(id)
	if ip == nil {
		return ""
	}
	return ip.String()
}

func lookupBridgeIPLocked(id string) net.IP {
	b, err := os.ReadFile(filepath.Join(dataDir, "by-id", id))
	if err != nil {
		return nil
	}
	return net.ParseIP(strings.TrimSpace(string(b)))
}

func flock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func withNetns(path string, fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		return err
	}
	defer orig.Close()
	ns, err := netns.GetFromPath(path)
	if err != nil {
		return err
	}
	defer ns.Close()
	if err := netns.Set(ns); err != nil {
		return err
	}
	defer netns.Set(orig)
	return fn()
}

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

func ensureMasq(cidr, bridge string) error {
	ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
	if err != nil {
		return err
	}
	args := []string{"-s", cidr, "!", "-o", bridge, "-j", "MASQUERADE"}
	return ipt.AppendUnique("nat", "POSTROUTING", args...)
}
