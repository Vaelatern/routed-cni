package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

type NomadTLS struct {
	CAFile   string `json:"caFile"`
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
	Insecure bool   `json:"insecure"` // skip TLS verify
}

type NetConf struct {
	types.NetConf
	ContainerIP    string    `json:"containerIP"`
	GWIP           string    `json:"gwIP"`
	Gocast         string    `json:"gocast"`         // http://... or "service:nomad"
	NomadAddr      string    `json:"nomadAddr"`      // default http://127.0.0.1:4646
	NomadTokenFile string    `json:"nomadTokenFile"` // empty = no token
	NomadTLS       *NomadTLS `json:"nomadTls"`       // optional TLS for Nomad API
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

	// announce to gocast (any extra args like community are forwarded)
	extra := parseExtraArgs(args.Args)
	notifyGocast(conf, "announce", conf.ContainerIP+"/32", extra)

	return types.PrintResult(res, conf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	conf := &NetConf{}
	json.Unmarshal(args.StdinData, conf)
	// support containerIP coming from cni.args on del too
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
		}
		netlink.LinkDel(hostLink)
	}

	// withdraw from gocast
	extra := parseExtraArgs(args.Args)
	notifyGocast(conf, "withdraw", conf.ContainerIP+"/32", extra)

	return nil
}

func cmdCheck(args *skel.CmdArgs) error {
	return nil
}

func init() {
	// CNI result is on stdout; diagnostics go to stderr (Nomad agent log).
	log.SetOutput(os.Stderr)
	log.SetPrefix("routed-cni: ")
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
}

// notifyGocast tells gocast (on localhost) to announce/withdraw the /32.
// Any extra key=value from cni.args (e.g. community) are forwarded.
// Magic: "service:nomad" or "service:nomad:service:NAME" → resolve via Nomad API.
func notifyGocast(conf *NetConf, action, prefix string, extra map[string]string) {
	if conf == nil || conf.Gocast == "" {
		log.Printf("gocast: skip (no conf/gocast) action=%s prefix=%s", action, prefix)
		return
	}

	url := conf.Gocast
	if name, ok := parseNomadService(conf.Gocast); ok {
		log.Printf("nomad: resolve service=%s addr=%q tokenFile=%q tls=%v", name, conf.NomadAddr, conf.NomadTokenFile, conf.NomadTLS != nil)
		var err error
		url, err = resolveLocalService(name, conf)
		if err != nil {
			log.Printf("nomad: resolve FAILED: %v", err)
			return
		}
		log.Printf("nomad: resolve ok → %s", url)
	} else {
		log.Printf("gocast: static url=%s", url)
	}

	payload := map[string]any{
		"action": action,
		"prefix": prefix,
	}
	for k, v := range extra {
		payload[k] = v
	}
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("gocast: POST %s FAILED: %v", url, err)
		return
	}
	defer resp.Body.Close()
	log.Printf("gocast: POST %s action=%s prefix=%s → %s", url, action, prefix, resp.Status)
}

// parseNomadService accepts "service:nomad" (default name gocast)
// or "service:nomad:service:NAME".
func parseNomadService(s string) (name string, ok bool) {
	if s == "service:nomad" {
		return "gocast", true
	}
	if name, ok := strings.CutPrefix(s, "service:nomad:service:"); ok && name != "" {
		return name, true
	}
	return "", false
}

func parseExtraArgs(s string) map[string]string {
	m := map[string]string{}
	for _, kv := range strings.Split(s, ";") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			k = strings.ToLower(strings.TrimSpace(k))
			if k != "containerip" && k != "ip" && k != "gwip" {
				m[k] = strings.TrimSpace(v)
			}
		}
	}
	return m
}

// resolveLocalService queries the local Nomad agent for serviceName
// running on the same IP as this host.
// nomadAddr default: http://127.0.0.1:4646. Token: nomadTokenFile only (empty = none).
func resolveLocalService(serviceName string, conf *NetConf) (string, error) {
	hostIP := getHostIP()
	if hostIP == "" {
		return "", fmt.Errorf("could not determine host IP")
	}

	nomadAddr := conf.NomadAddr
	if nomadAddr == "" {
		nomadAddr = "http://127.0.0.1:4646"
	}

	url := fmt.Sprintf("%s/v1/service/%s", strings.TrimRight(nomadAddr, "/"), serviceName)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	token := loadNomadToken(conf.NomadTokenFile)
	if token != "" {
		req.Header.Set("X-Nomad-Token", token)
	}
	log.Printf("nomad: GET %s token=%v hostIP=%s", url, token != "", hostIP)

	client, err := nomadHTTPClient(conf)
	if err != nil {
		return "", fmt.Errorf("tls client: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s (auth/tls/acl?)", url, resp.Status)
	}

	var services []struct {
		Address string `json:"Address"`
		Port    int    `json:"Port"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
		return "", err
	}

	log.Printf("nomad: %d registration(s) for %s", len(services), serviceName)
	for _, svc := range services {
		log.Printf("nomad: candidate Address=%s Port=%d matchHost=%v", svc.Address, svc.Port, svc.Address == hostIP)
		if svc.Address == hostIP && svc.Port != 0 {
			return fmt.Sprintf("http://%s:%d", hostIP, svc.Port), nil
		}
	}
	return "", fmt.Errorf("no local %s service found on %s (%d candidates)", serviceName, hostIP, len(services))
}

func loadNomadToken(tokenFile string) string {
	if tokenFile == "" {
		return ""
	}
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// nomadHTTPClient builds a client with optional TLS from conf.nomadTls.
func nomadHTTPClient(conf *NetConf) (*http.Client, error) {
	c := &http.Client{Timeout: 2 * time.Second}
	if conf == nil || conf.NomadTLS == nil {
		return c, nil
	}
	log.Printf("nomad: tls ca=%q cert=%q key=%q insecure=%v",
		conf.NomadTLS.CAFile, conf.NomadTLS.CertFile, conf.NomadTLS.KeyFile, conf.NomadTLS.Insecure)
	tlsCfg := &tls.Config{InsecureSkipVerify: conf.NomadTLS.Insecure}
	if conf.NomadTLS.CAFile != "" {
		b, err := os.ReadFile(conf.NomadTLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("nomadTls.caFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("nomadTls.caFile: no certs")
		}
		tlsCfg.RootCAs = pool
	}
	if conf.NomadTLS.CertFile != "" || conf.NomadTLS.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(conf.NomadTLS.CertFile, conf.NomadTLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("nomadTls client cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	c.Transport = &http.Transport{TLSClientConfig: tlsCfg}
	return c, nil
}

// getHostIP returns the primary non-loopback IPv4 address of the host.
func getHostIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}
