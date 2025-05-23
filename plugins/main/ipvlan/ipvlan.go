// Copyright 2015 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"

	"github.com/vishvananda/netlink"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/netlinksafe"
	"github.com/containernetworking/plugins/pkg/ns"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
	"github.com/containernetworking/plugins/pkg/utils/sysctl"
)

type NetConf struct {
	types.NetConf
	Master     string `json:"master"`
	Mode       string `json:"mode"`
	MTU        int    `json:"mtu"`
	LinkContNs bool   `json:"linkInContainer,omitempty"`
}

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func loadConf(args *skel.CmdArgs, cmdCheck bool) (*NetConf, string, error) {
	n := &NetConf{}
	if err := json.Unmarshal(args.StdinData, n); err != nil {
		return nil, "", fmt.Errorf("failed to load netconf: %v", err)
	}

	if cmdCheck {
		return n, n.CNIVersion, nil
	}

	var result *current.Result
	var err error
	// Parse previous result
	if n.NetConf.RawPrevResult != nil {
		if err = version.ParsePrevResult(&n.NetConf); err != nil {
			return nil, "", fmt.Errorf("could not parse prevResult: %v", err)
		}

		result, err = current.NewResultFromResult(n.PrevResult)
		if err != nil {
			return nil, "", fmt.Errorf("could not convert result to current version: %v", err)
		}
	}
	if n.Master == "" {
		if result == nil {
			var defaultRouteInterface string
			defaultRouteInterface, err = getNamespacedDefaultRouteInterfaceName(args.Netns, n.LinkContNs)
			if err != nil {
				return nil, "", err
			}
			n.Master = defaultRouteInterface
		} else {
			if len(result.Interfaces) == 1 && result.Interfaces[0].Name != "" {
				n.Master = result.Interfaces[0].Name
			} else {
				return nil, "", fmt.Errorf("chained master failure. PrevResult lacks a single named interface")
			}
		}
	}
	return n, n.CNIVersion, nil
}

func modeFromString(s string) (netlink.IPVlanMode, error) {
	switch s {
	case "", "l2":
		return netlink.IPVLAN_MODE_L2, nil
	case "l3":
		return netlink.IPVLAN_MODE_L3, nil
	case "l3s":
		return netlink.IPVLAN_MODE_L3S, nil
	default:
		return 0, fmt.Errorf("unknown ipvlan mode: %q", s)
	}
}

func modeToString(mode netlink.IPVlanMode) (string, error) {
	switch mode {
	case netlink.IPVLAN_MODE_L2:
		return "l2", nil
	case netlink.IPVLAN_MODE_L3:
		return "l3", nil
	case netlink.IPVLAN_MODE_L3S:
		return "l3s", nil
	default:
		return "", fmt.Errorf("unknown ipvlan mode: %q", mode)
	}
}

func createIpvlan(conf *NetConf, ifName string, netns ns.NetNS) (*current.Interface, error) {
	ipvlan := &current.Interface{}

	mode, err := modeFromString(conf.Mode)
	if err != nil {
		return nil, err
	}

	var m netlink.Link
	if conf.LinkContNs {
		err = netns.Do(func(_ ns.NetNS) error {
			m, err = netlinksafe.LinkByName(conf.Master)
			return err
		})
	} else {
		m, err = netlinksafe.LinkByName(conf.Master)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to lookup master %q: %v", conf.Master, err)
	}

	// due to kernel bug we have to create with tmpname or it might
	// collide with the name on the host and error out
	tmpName, err := ip.RandomVethName()
	if err != nil {
		return nil, err
	}

	linkAttrs := netlink.NewLinkAttrs()
	linkAttrs.MTU = conf.MTU
	linkAttrs.Name = tmpName
	linkAttrs.ParentIndex = m.Attrs().Index
	linkAttrs.Namespace = netlink.NsFd(int(netns.Fd()))

	mv := &netlink.IPVlan{
		LinkAttrs: linkAttrs,
		Mode:      mode,
	}

	if conf.LinkContNs {
		err = netns.Do(func(_ ns.NetNS) error {
			return netlink.LinkAdd(mv)
		})
	} else {
		if err := netlink.LinkAdd(mv); err != nil {
			return nil, fmt.Errorf("failed to create ipvlan: %v", err)
		}
	}

	err = netns.Do(func(_ ns.NetNS) error {
		err := ip.RenameLink(tmpName, ifName)
		if err != nil {
			return fmt.Errorf("failed to rename ipvlan to %q: %v", ifName, err)
		}
		ipvlan.Name = ifName

		// Re-fetch ipvlan to get all properties/attributes
		contIpvlan, err := netlinksafe.LinkByName(ipvlan.Name)
		if err != nil {
			return fmt.Errorf("failed to refetch ipvlan %q: %v", ipvlan.Name, err)
		}
		ipvlan.Mac = contIpvlan.Attrs().HardwareAddr.String()
		ipvlan.Sandbox = netns.Path()

		return nil
	})
	if err != nil {
		return nil, err
	}

	return ipvlan, nil
}

func getDefaultRouteInterfaceName() (string, error) {
	routeToDstIP, err := netlinksafe.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		return "", err
	}

	for _, v := range routeToDstIP {
		if ip.IsIPNetZero(v.Dst) {
			l, err := netlink.LinkByIndex(v.LinkIndex)
			if err != nil {
				return "", err
			}
			return l.Attrs().Name, nil
		}
	}

	return "", fmt.Errorf("no default route interface found")
}

func getNamespacedDefaultRouteInterfaceName(namespace string, inContainer bool) (string, error) {
	if !inContainer {
		return getDefaultRouteInterfaceName()
	}
	netns, err := ns.GetNS(namespace)
	if err != nil {
		return "", fmt.Errorf("failed to open netns %q: %v", netns, err)
	}
	defer netns.Close()
	var defaultRouteInterface string
	err = netns.Do(func(_ ns.NetNS) error {
		defaultRouteInterface, err = getDefaultRouteInterfaceName()
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return defaultRouteInterface, nil
}

func cmdAdd(args *skel.CmdArgs) error {
	n, cniVersion, err := loadConf(args, false)
	if err != nil {
		return err
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	ipvlanInterface, err := createIpvlan(n, args.IfName, netns)
	if err != nil {
		return err
	}

	var result *current.Result
	// Configure iface from PrevResult if we have IPs and an IPAM
	// block has not been configured
	haveResult := false
	if n.IPAM.Type == "" && n.PrevResult != nil {
		result, err = current.NewResultFromResult(n.PrevResult)
		if err != nil {
			return err
		}
		if len(result.IPs) > 0 {
			haveResult = true
		}
	}
	if !haveResult {
		// run the IPAM plugin and get back the config to apply
		r, err := ipam.ExecAdd(n.IPAM.Type, args.StdinData)
		if err != nil {
			return err
		}

		// Invoke ipam del if err to avoid ip leak
		defer func() {
			if err != nil {
				ipam.ExecDel(n.IPAM.Type, args.StdinData)
			}
		}()

		// Convert whatever the IPAM result was into the current Result type
		result, err = current.NewResultFromResult(r)
		if err != nil {
			return err
		}

		if len(result.IPs) == 0 {
			return errors.New("IPAM plugin returned missing IP config")
		}
	}
	for _, ipc := range result.IPs {
		// All addresses belong to the ipvlan interface
		ipc.Interface = current.Int(0)
	}

	result.Interfaces = []*current.Interface{ipvlanInterface}

	err = netns.Do(func(_ ns.NetNS) error {
		_, _ = sysctl.Sysctl(fmt.Sprintf("net/ipv4/conf/%s/arp_notify", args.IfName), "1")
		_, _ = sysctl.Sysctl(fmt.Sprintf("net/ipv6/conf/%s/ndisc_notify", args.IfName), "1")

		return ipam.ConfigureIface(args.IfName, result)
	})
	if err != nil {
		return err
	}

	result.DNS = n.DNS

	return types.PrintResult(result, cniVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	n, _, err := loadConf(args, false)
	if err != nil {
		return err
	}

	// On chained invocation, IPAM block can be empty
	if n.IPAM.Type != "" {
		err = ipam.ExecDel(n.IPAM.Type, args.StdinData)
		if err != nil {
			return err
		}
	}

	if args.Netns == "" {
		return nil
	}

	// There is a netns so try to clean up. Delete can be called multiple times
	// so don't return an error if the device is already removed.
	err = ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
		if err := ip.DelLinkByName(args.IfName); err != nil {
			if err != ip.ErrLinkNotFound {
				return err
			}
		}
		return nil
	})
	if err != nil {
		//  if NetNs is passed down by the Cloud Orchestration Engine, or if it called multiple times
		// so don't return an error if the device is already removed.
		// https://github.com/kubernetes/kubernetes/issues/43014#issuecomment-287164444
		_, ok := err.(ns.NSPathNotExistErr)
		if ok {
			return nil
		}
		return err
	}

	return err
}

func main() {
	skel.PluginMainFuncs(skel.CNIFuncs{
		Add:    cmdAdd,
		Check:  cmdCheck,
		Del:    cmdDel,
		Status: cmdStatus,
		/* FIXME GC */
	}, version.All, bv.BuildString("ipvlan"))
}

func cmdCheck(args *skel.CmdArgs) error {
	n, _, err := loadConf(args, true)
	if err != nil {
		return err
	}
	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	if n.IPAM.Type != "" {
		// run the IPAM plugin and get back the config to apply
		err = ipam.ExecCheck(n.IPAM.Type, args.StdinData)
		if err != nil {
			return err
		}
	}

	// Parse previous result.
	if n.NetConf.RawPrevResult == nil {
		return fmt.Errorf("Required prevResult missing")
	}

	if err := version.ParsePrevResult(&n.NetConf); err != nil {
		return err
	}

	result, err := current.NewResultFromResult(n.PrevResult)
	if err != nil {
		return err
	}

	var contMap current.Interface
	// Find interfaces for names whe know, ipvlan inside container
	for _, intf := range result.Interfaces {
		if args.IfName == intf.Name {
			if args.Netns == intf.Sandbox {
				contMap = *intf
				continue
			}
		}
	}

	// The namespace must be the same as what was configured
	if args.Netns != contMap.Sandbox {
		return fmt.Errorf("Sandbox in prevResult %s doesn't match configured netns: %s",
			contMap.Sandbox, args.Netns)
	}

	if n.LinkContNs {
		err = netns.Do(func(_ ns.NetNS) error {
			_, err = netlinksafe.LinkByName(n.Master)
			return err
		})
	} else {
		_, err = netlinksafe.LinkByName(n.Master)
	}

	if err != nil {
		return fmt.Errorf("failed to lookup master %q: %v", n.Master, err)
	}

	// Check prevResults for ips, routes and dns against values found in the container
	if err := netns.Do(func(_ ns.NetNS) error {
		// Check interface against values found in the container
		err := validateCniContainerInterface(contMap, n.Mode)
		if err != nil {
			return err
		}

		err = ip.ValidateExpectedInterfaceIPs(args.IfName, result.IPs)
		if err != nil {
			return err
		}

		err = ip.ValidateExpectedRoute(result.Routes)
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	return nil
}

func validateCniContainerInterface(intf current.Interface, modeExpected string) error {
	var link netlink.Link
	var err error

	if intf.Name == "" {
		return fmt.Errorf("Container interface name missing in prevResult: %v", intf.Name)
	}
	link, err = netlinksafe.LinkByName(intf.Name)
	if err != nil {
		return fmt.Errorf("Container Interface name in prevResult: %s not found", intf.Name)
	}
	if intf.Sandbox == "" {
		return fmt.Errorf("Error: Container interface %s should not be in host namespace", link.Attrs().Name)
	}

	ipv, isIPVlan := link.(*netlink.IPVlan)
	if !isIPVlan {
		return fmt.Errorf("Error: Container interface %s not of type ipvlan", link.Attrs().Name)
	}

	mode, err := modeFromString(modeExpected)
	if err != nil {
		return err
	}
	if ipv.Mode != mode {
		currString, err := modeToString(ipv.Mode)
		if err != nil {
			return err
		}
		confString, err := modeToString(mode)
		if err != nil {
			return err
		}
		return fmt.Errorf("Container IPVlan mode %s does not match expected value: %s", currString, confString)
	}

	if intf.Mac != "" {
		if intf.Mac != link.Attrs().HardwareAddr.String() {
			return fmt.Errorf("Interface %s Mac %s doesn't match container Mac: %s", intf.Name, intf.Mac, link.Attrs().HardwareAddr)
		}
	}

	return nil
}

func cmdStatus(args *skel.CmdArgs) error {
	conf := NetConf{}
	if err := json.Unmarshal(args.StdinData, &conf); err != nil {
		return fmt.Errorf("failed to load netconf: %w", err)
	}
	if conf.IPAM.Type != "" {
		if err := ipam.ExecStatus(conf.IPAM.Type, args.StdinData); err != nil {
			return err
		}
	}

	// TODO: Check if master interface exists.

	return nil
}
