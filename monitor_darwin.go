package tun

import (
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/sagernet/sing/common/buf"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/common/x/list"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

type networkUpdateMonitor struct {
	access      sync.Mutex
	callbacks   list.List[NetworkUpdateCallback]
	routeSocket int
	logger      logger.Logger
}

func NewNetworkUpdateMonitor(logger logger.Logger) (NetworkUpdateMonitor, error) {
	return &networkUpdateMonitor{logger: logger}, nil
}

func (m *networkUpdateMonitor) Start() error {
	go m.loopUpdate()
	return nil
}

func (m *networkUpdateMonitor) loopUpdate() {
	for {
		err := m.loopUpdate0()
		if err != nil {
			m.logger.Error("listen network update: ", err)
			return
		}
	}
}

func (m *networkUpdateMonitor) loopUpdate0() error {
	routeSocket, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, 0)
	if err != nil {
		return err
	}
	m.loopUpdate1(os.NewFile(uintptr(routeSocket), "route"))
	return nil
}

func (m *networkUpdateMonitor) loopUpdate1(routeSocketFile *os.File) {
	defer routeSocketFile.Close()
	buffer := buf.NewPacket()
	defer buffer.Release()
	n, err := routeSocketFile.Read(buffer.FreeBytes())
	if err != nil {
		return
	}
	buffer.Truncate(n)
	messages, err := route.ParseRIB(route.RIBTypeRoute, buffer.Bytes())
	if err != nil {
		return
	}
	for _, message := range messages {
		if _, isRouteMessage := message.(*route.RouteMessage); isRouteMessage {
			m.emit()
			return
		}
	}
}

func (m *networkUpdateMonitor) Close() error {
	return unix.Close(m.routeSocket)
}

func (m *defaultInterfaceMonitor) checkUpdate() error {
	ribMessage, err := route.FetchRIB(unix.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return err
	}
	routeMessages, err := route.ParseRIB(route.RIBTypeRoute, ribMessage)
	if err != nil {
		return err
	}
	var defaultInterface *net.Interface
	for _, rawRouteMessage := range routeMessages {
		routeMessage := rawRouteMessage.(*route.RouteMessage)
		if len(routeMessage.Addrs) <= unix.RTAX_NETMASK {
			continue
		}
		destination, isIPv4Destination := routeMessage.Addrs[unix.RTAX_DST].(*route.Inet4Addr)
		if !isIPv4Destination {
			continue
		}
		if destination.IP != netip.IPv4Unspecified().As4() {
			continue
		}
		mask, isIPv4Mask := routeMessage.Addrs[unix.RTAX_NETMASK].(*route.Inet4Addr)
		if !isIPv4Mask {
			continue
		}
		ones, _ := net.IPMask(mask.IP[:]).Size()
		if ones != 0 {
			continue
		}
		routeInterface, err := net.InterfaceByIndex(routeMessage.Index)
		if err != nil {
			return err
		}
		if routeMessage.Flags&unix.RTF_UP == 0 {
			continue
		}
		if routeMessage.Flags&unix.RTF_GATEWAY == 0 {
			continue
		}
		if routeMessage.Flags&unix.RTF_IFSCOPE != 0 {
			// continue
		}
		defaultInterface = routeInterface
		break
	}
	if defaultInterface == nil {
		if m.options.UnderNetworkExtension {
			defaultInterface, err = getDefaultInterfaceBySocket()
			if err != nil {
				return err
			}
		}
	}
	if defaultInterface == nil {
		return ErrNoRoute
	}
	oldInterface := m.defaultInterfaceName
	oldIndex := m.defaultInterfaceIndex
	m.defaultInterfaceIndex = defaultInterface.Index
	m.defaultInterfaceName = defaultInterface.Name
	if oldInterface == m.defaultInterfaceName && oldIndex == m.defaultInterfaceIndex {
		return nil
	}
	m.emit(EventInterfaceUpdate)
	return nil
}

func getDefaultInterfaceBySocket() (*net.Interface, error) {
	socketFd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, E.Cause(err, "create file descriptor")
	}
	defer unix.Close(socketFd)
	go unix.Connect(socketFd, &unix.SockaddrInet4{
		Addr: [4]byte{10, 255, 255, 255},
		Port: 80,
	})
	result := make(chan netip.Addr, 1)
	go func() {
		for {
			sockname, sockErr := unix.Getsockname(socketFd)
			if sockErr != nil {
				break
			}
			sockaddr, isInet4Sockaddr := sockname.(*unix.SockaddrInet4)
			if !isInet4Sockaddr {
				break
			}
			addr := netip.AddrFrom4(sockaddr.Addr)
			if addr.IsUnspecified() {
				time.Sleep(time.Millisecond)
				continue
			}
			result <- addr
			break
		}
	}()
	var selectedAddr netip.Addr
	select {
	case selectedAddr = <-result:
	case <-time.After(time.Second):
		return nil, os.ErrDeadlineExceeded
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, E.Cause(err, "net.Interfaces")
	}
	for _, netInterface := range interfaces {
		interfaceAddrs, err := netInterface.Addrs()
		if err != nil {
			return nil, E.Cause(err, "net.Interfaces.Addrs")
		}
		for _, interfaceAddr := range interfaceAddrs {
			ipNet, isIPNet := interfaceAddr.(*net.IPNet)
			if !isIPNet {
				continue
			}
			if ipNet.Contains(selectedAddr.AsSlice()) {
				return &netInterface, nil
			}
		}
	}
	return nil, E.New("no interface found for address ", selectedAddr)
}
