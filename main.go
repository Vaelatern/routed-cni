package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

type NetConf struct {
	types.NetConf
	ContainerIP string `json:"containerIP"`
	GWIP        string `json:"gwIP"`
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
		return fmt.Errorf("containerIP and gwIP required in config")
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

	// ensure sysctls for forwarding and rp_filter (loose for cross-if reachability to host IPs)
	os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0644)
	os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/rp_filter", hostName), []byte("2\n"), 0644)

	// gw on host veth
	gwAddr, _ := netlink.ParseAddr(conf.GWIP + "/24")
	netlink.AddrAdd(hostLink, gwAddr)

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
	contIPNet, _ := netlink.ParseAddr(conf.ContainerIP + "/24")
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	origNs, _ := netns.Get()
	newNs, _ := netns.GetFromPath(args.Netns)
	netns.Set(newNs)
	l, _ := netlink.LinkByName(contName)
	netlink.LinkSetName(l, "eth0")
	netlink.LinkSetUp(l)
	netlink.AddrAdd(l, contIPNet)
	rt := &netlink.Route{
		LinkIndex: l.Attrs().Index,
		Gw:        net.ParseIP(conf.GWIP),
	}
	netlink.RouteReplace(rt)
	netns.Set(origNs)

	// ensure host route for container IP (/32 via host veth)
	hostRt := &netlink.Route{
		LinkIndex: hostLink.Attrs().Index,
		Dst:       &net.IPNet{IP: net.ParseIP(conf.ContainerIP), Mask: net.CIDRMask(32, 32)},
	}
	netlink.RouteReplace(hostRt)

	// result
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
				Dst:       &net.IPNet{IP: net.ParseIP(conf.ContainerIP), Mask: net.CIDRMask(32, 32)},
			}
			netlink.RouteDel(hostRt)
		}
		netlink.LinkDel(hostLink)
	}
	return nil
}

func cmdCheck(args *skel.CmdArgs) error {
	return nil
}