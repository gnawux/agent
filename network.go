//
// Copyright (c) 2017 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"fmt"
	"net"
	"reflect"
	"sync"

	pb "github.com/hyperhq/runv/hyperstart/api/grpc"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

// Network fully describes a pod network with its interfaces, routes and dns
// related information.
type network struct {
	ifacesLock sync.Mutex
	ifaces     []*pb.Iface

	routesLock sync.Mutex
	routes     []*pb.Route

	dnsLock sync.Mutex
	dns     []string
}

////////////////
// Interfaces //
////////////////

func linkByHwAddr(netHandle *netlink.Handle, hwAddr string) (netlink.Link, error) {
	links, err := netHandle.LinkList()
	if err != nil {
		return nil, err
	}

	for _, link := range links {
		lAttrs := link.Attrs()
		if lAttrs == nil {
			continue
		}

		if lAttrs.HardwareAddr.String() == hwAddr {
			return link, nil
		}
	}

	return nil, fmt.Errorf("Could not find the link corresponding to HwAddr %q", hwAddr)
}

func getStrNetMaskFromIPv4(ip net.IP) (string, error) {
	ipMask := ip.DefaultMask()
	if ipMask == nil {
		return "", fmt.Errorf("Could not deduce IP network mask from %v", ip)
	}

	ipMaskInt, _ := ipMask.Size()

	return fmt.Sprintf("%d", ipMaskInt), nil
}

func setupInterface(netHandle *netlink.Handle, iface *pb.Iface, link netlink.Link) error {
	lAttrs := link.Attrs()
	if lAttrs != nil && (lAttrs.Flags&net.FlagUp) == net.FlagUp {
		// The link is up, makes sure we get it down before
		// doing any modification.
		if err := netHandle.LinkSetDown(link); err != nil {
			return err
		}
	}

	// Rename the link.
	if iface.NewName != "" {
		if err := netHandle.LinkSetName(link, iface.NewName); err != nil {
			return err
		}
	}

	// Set MTU.
	if iface.Mtu > 0 {
		if err := netHandle.LinkSetMTU(link, int(iface.Mtu)); err != nil {
			return err
		}
	}

	for _, ipAddress := range iface.IpAddresses {
		netMask := ipAddress.Mask

		// Determine the network mask if not provided in the expected format.
		netMaskIP := net.ParseIP(netMask)
		if netMaskIP != nil {
			ip := net.ParseIP(ipAddress.Address)
			if ip == nil {
				return fmt.Errorf("Invalid IP address %q", ipAddress.Address)
			}

			tmpNetMask, err := getStrNetMaskFromIPv4(ip)
			if err != nil {
				return err
			}

			netMask = tmpNetMask
		}

		// Add an IP address.
		addr, err := netlink.ParseAddr(fmt.Sprintf("%s/%s", ipAddress.Address, netMask))
		if err != nil {
			return fmt.Errorf("Could not parse the IP address %s: %v", ipAddress.Address, err)
		}
		if err := netHandle.AddrAdd(link, addr); err != nil {
			return err
		}
	}

	// Set the link up.
	return netHandle.LinkSetUp(link)
}

func (p *pod) addInterfaces(ifaces []*pb.Iface) (err error) {
	p.network.ifacesLock.Lock()
	defer p.network.ifacesLock.Unlock()

	netHandle, err := netlink.NewHandle()
	if err != nil {
		return err
	}
	defer netHandle.Delete()

	for _, iface := range ifaces {
		var link netlink.Link

		fieldLogger := agentLog.WithFields(logrus.Fields{
			"mac-address":    iface.HwAddr,
			"interface-name": iface.Device,
		})

		if iface.HwAddr != "" {
			fieldLogger.Info("Getting interface from MAC address")

			// Find the interface link from its hardware address.
			link, err = linkByHwAddr(netHandle, iface.HwAddr)
			if err != nil {
				return err
			}
		} else if iface.Device != "" {
			fieldLogger.Info("Getting interface from name")

			// Find the interface link from its name.
			link, err = netHandle.LinkByName(iface.Device)
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("Interface HwAddr and Name are both empty")
		}

		fieldLogger.WithField("link", fmt.Sprintf("%+v", link)).Infof("Link found")

		if err := setupInterface(netHandle, iface, link); err != nil {
			return err
		}

		// Update pod interface list.
		p.network.ifaces = append(p.network.ifaces, iface)
	}

	return nil
}

func (p *pod) removeInterfaces(ifaces []*pb.Iface) error {
	p.network.ifacesLock.Lock()
	defer p.network.ifacesLock.Unlock()

	netHandle, err := netlink.NewHandle()
	if err != nil {
		return err
	}
	defer netHandle.Delete()

	for _, iface := range ifaces {
		// Find the interface by name.
		link, err := netHandle.LinkByName(iface.Device)
		if err != nil {
			return err
		}

		// Set the link down.
		if err := netHandle.LinkSetDown(link); err != nil {
			return err
		}

		for _, ipAddress := range iface.IpAddresses {
			netMask := ipAddress.Mask

			// Determine the network mask if not provided in the expected format.
			netMaskIP := net.ParseIP(netMask)
			if netMaskIP != nil {
				ip := net.ParseIP(ipAddress.Address)
				if ip == nil {
					return fmt.Errorf("Invalid IP address %q", ipAddress.Address)
				}

				tmpNetMask, err := getStrNetMaskFromIPv4(ip)
				if err != nil {
					return err
				}

				netMask = tmpNetMask
			}

			// Remove the IP address.
			addr, err := netlink.ParseAddr(fmt.Sprintf("%s/%s", ipAddress.Address, netMask))
			if err != nil {
				return fmt.Errorf("Could not parse the IP address %s: %v", ipAddress.Address, err)
			}
			if err := netHandle.AddrDel(link, addr); err != nil {
				return err
			}
		}

		// Update pod interface list.
		for idx, podIface := range p.network.ifaces {
			if reflect.DeepEqual(podIface, iface) {
				p.network.ifaces = append(p.network.ifaces[:idx], p.network.ifaces[idx+1:]...)
				break
			}
		}
	}

	return nil
}

////////////
// Routes //
////////////

func routeDestExist(ifaceIdx int, routeList []netlink.Route, dest string) bool {
	for _, route := range routeList {
		if route.LinkIndex == ifaceIdx && route.Dst.String() == dest {
			return true
		}
	}

	return false
}

func (p *pod) addRoutes(routes []*pb.Route) (err error) {
	p.network.routesLock.Lock()
	defer p.network.routesLock.Unlock()

	netHandle, err := netlink.NewHandle()
	if err != nil {
		return err
	}
	defer netHandle.Delete()

	initRouteList, err := netHandle.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		return err
	}

	for _, route := range routes {
		if route == nil {
			continue
		}

		// Find link index from route's device name.
		link, err := netHandle.LinkByName(route.Device)
		if err != nil {
			return fmt.Errorf("Could not find link from device %s: %v", route.Device, err)
		}

		linkAttrs := link.Attrs()
		if linkAttrs == nil {
			return fmt.Errorf("Could not get link's attributes for device %s", route.Device)
		}

		var dst *net.IPNet
		if route.Dest != "default" {
			_, dst, err = net.ParseCIDR(route.Dest)
			if err != nil {
				return fmt.Errorf("Could not parse route destination %s: %v", route.Dest, err)
			}
		}

		if routeDestExist(linkAttrs.Index, initRouteList, route.Dest) {
			agentLog.WithField("route-destination", route.Dest).Info("Route destination already exists, skipping")
			continue
		}

		netRoute := &netlink.Route{
			LinkIndex: linkAttrs.Index,
			Dst:       dst,
			Gw:        net.ParseIP(route.Gateway),
		}

		if err := netHandle.RouteReplace(netRoute); err != nil {
			return fmt.Errorf("Could not add/replace route dest(%s)/gw(%s)/dev(%s): %v",
				route.Dest, route.Gateway, route.Device, err)
		}

		// Update pod route list.
		p.network.routes = append(p.network.routes, route)
	}

	return nil
}

func (p *pod) removeRoutes(routes []*pb.Route) (err error) {
	p.network.routesLock.Lock()
	defer p.network.routesLock.Unlock()

	netHandle, err := netlink.NewHandle()
	if err != nil {
		return err
	}
	defer netHandle.Delete()

	for _, route := range routes {
		if route == nil {
			continue
		}

		// Find link index from route's device name.
		link, err := netHandle.LinkByName(route.Device)
		if err != nil {
			return fmt.Errorf("Could not find link from device %s: %v", route.Device, err)
		}

		linkAttrs := link.Attrs()
		if linkAttrs == nil {
			return fmt.Errorf("Could not get link's attributes for device %s", route.Device)
		}

		var dst *net.IPNet
		if route.Dest != "default" {
			_, dst, err = net.ParseCIDR(route.Dest)
			if err != nil {
				return fmt.Errorf("Could not parse route destination %s: %v", route.Dest, err)
			}
		}

		netRoute := &netlink.Route{
			LinkIndex: linkAttrs.Index,
			Dst:       dst,
			Gw:        net.ParseIP(route.Gateway),
		}

		if err := netHandle.RouteDel(netRoute); err != nil {
			return fmt.Errorf("Could not remove route dest(%s)/gw(%s)/dev(%s): %v", route.Dest, route.Gateway, route.Device, err)
		}

		// Update pod route list.
		for idx, podRoute := range p.network.routes {
			if reflect.DeepEqual(podRoute, route) {
				p.network.routes = append(p.network.routes[:idx], p.network.routes[idx+1:]...)
				break
			}
		}
	}

	return nil
}

/////////
// DNS //
/////////

func setupDNS(dns []string) error {
	return nil
}

func removeDNS(dns []string) error {
	return nil
}

////////////
// Global //
////////////

// Remove everything related to network.
func (p *pod) removeNetwork() error {
	if err := p.removeRoutes(p.network.routes); err != nil {
		return fmt.Errorf("Could not remove network routes: %v", err)
	}

	if err := p.removeInterfaces(p.network.ifaces); err != nil {
		return fmt.Errorf("Could not remove network interfaces: %v", err)
	}

	if err := removeDNS(p.network.dns); err != nil {
		return fmt.Errorf("Could not remove network DNS: %v", err)
	}

	return nil
}

