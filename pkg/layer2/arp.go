package layer2

import (
	"fmt"
	"github.com/go-logr/logr"
	"github.com/j-keck/arping"
	"github.com/mdlayher/arp"
	"github.com/mdlayher/ethernet"
	"github.com/mdlayher/raw"
	"github.com/vishvananda/netlink"
	"io"
	"net"
)

const protocolARP = 0x0806

type arpResponder struct {
	logger logr.Logger

	intf   *net.Interface
	conn   *arp.Client
	p      *raw.Conn
	closed chan struct{}

	ip2mac map[string]*net.HardwareAddr
}

func newARPResponder(log logr.Logger, ifi *net.Interface) (*arpResponder, error) {
	p, err := raw.ListenPacket(ifi, protocolARP, nil)
	if err != nil {
		return nil, err
	}
	client, err := arp.New(ifi, p)
	if err != nil {
		return nil, fmt.Errorf("creating ARP responder for %q: %s", ifi.Name, err)
	}

	ret := &arpResponder{
		logger: log.WithName("arpResponder"),
		intf:   ifi,
		conn:   client,
		p:      p,
		closed: make(chan struct{}),
		ip2mac: make(map[string]*net.HardwareAddr),
	}
	go ret.run()
	return ret, nil
}

func (a *arpResponder) Close() error {
	close(a.closed)
	return a.conn.Close()
}

//The source mac address must be on the network card, otherwise arp spoof could drop you packets.
func generateArp(intfHW net.HardwareAddr, op arp.Operation, srcHW net.HardwareAddr, srcIP net.IP, dstHW net.HardwareAddr, dstIP net.IP) ([]byte, error) {
	pkt, err := arp.NewPacket(op, srcHW, srcIP, dstHW, dstIP)
	if err != nil {
		return nil, err
	}

	pb, err := pkt.MarshalBinary()
	if err != nil {
		return nil, err
	}

	f := &ethernet.Frame{
		Destination: dstHW,
		Source:      intfHW,
		EtherType:   ethernet.EtherTypeARP,
		Payload:     pb,
	}

	fb, err := f.MarshalBinary()
	if err != nil {
		return nil, err
	}

	return fb, err
}

func (a *arpResponder) DeleteIP(ip string) {
	delete(a.ip2mac, ip)
}

func resolveIP(nodeIP net.IP, iface *net.Interface) (hwAddr net.HardwareAddr, err error) {
	//Resolve mac
	for i := 0; i < 3; i++ {
		hwAddr, _, err = arping.PingOverIface(nodeIP, *iface)
		if err != nil {
			continue
		} else {
			break
		}
	}

	return
}

func (a *arpResponder) Gratuitous(ip, nodeIP net.IP) error {
	var (
		hwAddr net.HardwareAddr
		err    error
	)

	if ip.To4() == nil {
		return nil
	}

	routers, err := netlink.RouteGet(nodeIP)
	if err != nil {
		return err
	}

	iface, err := net.InterfaceByIndex(routers[0].LinkIndex)
	if err != nil {
		return err
	}

	if iface.Name != "lo" && routers[0].LinkIndex != a.intf.Index {
		return nil
	}

	if iface.Name == "lo" {
		hwAddr = a.intf.HardwareAddr
	} else {
		hwAddr, err = resolveIP(nodeIP, a.intf)
		if err != nil {
			return err
		}
	}

	a.ip2mac[ip.String()] = &hwAddr

	for _, op := range []arp.Operation{arp.OperationRequest, arp.OperationReply} {
		a.logger.Info("send gratuitous arp packet", "eip", ip, "nodeIP", nodeIP, "hwAddr", hwAddr)

		fb, err := generateArp(a.intf.HardwareAddr, op, hwAddr, ip, ethernet.Broadcast, ip)
		if err != nil {
			return err
		}

		if _, err = a.p.WriteTo(fb, &raw.Addr{HardwareAddr: ethernet.Broadcast}); err != nil {
			a.logger.Error(err, "send gratuitous arp packet")
			return err
		}
	}

	return nil
}

func (a *arpResponder) run() {
	for a.processRequest() != dropReasonClosed {
	}
}

func (a *arpResponder) processRequest() dropReason {
	pkt, _, err := a.conn.Read()
	if err != nil {
		// ARP listener doesn't cleanly return EOF when closed, so we
		// need to hook into the call to arpResponder.close()
		// independently.
		select {
		case <-a.closed:
			return dropReasonClosed
		default:
		}
		if err == io.EOF {
			return dropReasonClosed
		}
		return dropReasonError
	}

	// Ignore ARP replies.
	if pkt.Operation != arp.OperationRequest {
		return dropReasonARPReply
	}

	hwAddr := a.ip2mac[pkt.TargetIP.String()]
	if hwAddr == nil {
		return dropReasonAnnounceIP
	}
	a.logger.Info("got ARP request, sending response", "interface", a.intf.Name, "ip", pkt.TargetIP, "senderIP", pkt.SenderIP, "senderMAC", pkt.SenderHardwareAddr, "responseMAC", hwAddr)
	fb, err := generateArp(a.intf.HardwareAddr, arp.OperationReply, *hwAddr, pkt.TargetIP, pkt.SenderHardwareAddr, pkt.SenderIP)
	if err != nil {
		return dropReasonError
	}
	if _, err := a.p.WriteTo(fb, &raw.Addr{HardwareAddr: pkt.SenderHardwareAddr}); err != nil {
		a.logger.Error(err, "op", "arpReply", "interface", a.intf.Name, "ip", pkt.TargetIP, "senderIP", pkt.SenderIP, "senderMAC", pkt.SenderHardwareAddr, "responseMAC", hwAddr)
	}
	return dropReasonNone
}
