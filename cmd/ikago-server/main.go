package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/xtaci/kcp-go"
	"ikago/internal/addr"
	"ikago/internal/config"
	"ikago/internal/crypto"
	"ikago/internal/exec"
	"ikago/internal/log"
	"ikago/internal/pcap"
	"ikago/internal/stat"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

type quintuple struct {
	src      string
	dst      string
	protocol gopacket.LayerType
}

type natIndicator struct {
	src    net.Addr
	embSrc net.Addr
	conn   net.Conn
}

func (indicator *natIndicator) embSrcIP() net.IP {
	switch t := indicator.embSrc.(type) {
	case *net.IPAddr:
		return indicator.embSrc.(*net.IPAddr).IP
	case *net.TCPAddr:
		return indicator.embSrc.(*net.TCPAddr).IP
	case *net.UDPAddr:
		return indicator.embSrc.(*net.UDPAddr).IP
	case *addr.ICMPQueryAddr:
		return indicator.embSrc.(*addr.ICMPQueryAddr).IP
	default:
		panic(fmt.Errorf("type %T not support", t))
	}
}

const keepAlive float64 = 30 // seconds

var (
	version = ""
	build   = ""
)

var (
	argListDevs       = flag.Bool("list-devices", false, "List all valid devices in current computer.")
	argConfig         = flag.String("c", "", "Configuration file.")
	argListenDevs     = flag.String("listen-devices", "", "Devices for listening.")
	argUpDev          = flag.String("upstream-device", "", "Device for routing upstream to.")
	argGateway        = flag.String("gateway", "", "Gateway address.")
	argMethod         = flag.String("method", "plain", "Method of encryption.")
	argPassword       = flag.String("password", "", "Password of encryption.")
	argKCP            = flag.Bool("kcp", false, "Enable KCP.")
	argKCPMTU         = flag.Int("kcp-mtu", kcp.IKCP_MTU_DEF, "KCP tuning option mtu.")
	argKCPSendWindow  = flag.Int("kcp-sndwnd", kcp.IKCP_WND_SND, "KCP tuning option sndwnd.")
	argKCPRecvWindow  = flag.Int("kcp-rcvwnd", kcp.IKCP_WND_RCV, "KCP tuning option rcvwnd.")
	argKCPDataShard   = flag.Int("kcp-datashard", 10, "KCP tuning option datashard.")
	argKCPParityShard = flag.Int("kcp-parityshard", 3, "KCP tuning option parityshard.")
	argKCPACKNoDelay  = flag.Bool("kcp-acknodelay", false, "KCP tuning option acknodelay.")
	argKCPNoDelay     = flag.Bool("kcp-nodelay", false, "KCP tuning option nodelay.")
	argKCPInterval    = flag.Int("kcp-interval", kcp.IKCP_INTERVAL, "KCP tuning option interval.")
	argKCPResend      = flag.Int("kcp-resend", 0, "KCP tuning option resend.")
	argKCPNC          = flag.Int("kcp-nc", 0, "KCP tuning option nc.")
	argRule           = flag.Bool("rule", false, "Add firewall rule.")
	argVerbose        = flag.Bool("v", false, "Print verbose messages.")
	argLog            = flag.String("log", "", "Log.")
	argPort           = flag.Int("p", 0, "Port for listening.")
)

var (
	port       uint16
	listenDevs []*pcap.Device
	upDev      *pcap.Device
	gatewayDev *pcap.Device
	crypt      crypto.Crypt
	isKCP      bool
	kcpConfig  *config.KCPConfig
)

var (
	isClosed     bool
	listeners    []net.Listener
	upConn       *pcap.RawConn
	c            chan pcap.ConnBytes
	listenFrags  map[uint]*pcap.FragIndicator
	upFrags      map[uint]*pcap.FragIndicator
	nextTCPPort  uint16
	tcpPortPool  []time.Time
	nextUDPPort  uint16
	udpPortPool  []time.Time
	nextICMPv4Id uint16
	icmpv4IdPool []time.Time
	patMap       map[quintuple]uint16
	natLock      sync.RWMutex
	nat          map[pcap.NATGuide]*natIndicator
	listenStats  *stat.TrafficManager
	upStats      *stat.TrafficManager
)

func init() {
	if version != "" && build != "" {
		log.Infof("IkaGo-server %s-%s\n\n", version, build)
	} else {
		log.Infof("IkaGo-server %s%s\n\n", version, build)
	}

	// Parse arguments
	flag.Parse()

	// Load config.json by default
	if len(os.Args) <= 1 {
		_, err := os.Stat("config.json")
		if err == nil {
			*argConfig = "config.json"
		}
	}

	listenDevs = make([]*pcap.Device, 0)

	listeners = make([]net.Listener, 0)
	c = make(chan pcap.ConnBytes, 1000)
	listenFrags = make(map[uint]*pcap.FragIndicator)
	upFrags = make(map[uint]*pcap.FragIndicator)
	tcpPortPool = make([]time.Time, 16384)
	udpPortPool = make([]time.Time, 16384)
	icmpv4IdPool = make([]time.Time, 65536)
	patMap = make(map[quintuple]uint16)
	nat = make(map[pcap.NATGuide]*natIndicator)
	listenStats = stat.NewTrafficManager()
	upStats = stat.NewTrafficManager()
}

func main() {
	var (
		err     error
		cfg     *config.Config
		gateway net.IP
	)

	// Configuration file
	if *argConfig != "" {
		cfg, err = config.ParseFile(*argConfig)
		if err != nil {
			log.Fatalln(fmt.Errorf("parse config file %s: %w", *argConfig, err))
		}
		log.Infof("Load configuration from %s\n", *argConfig)
	} else {
		cfg = &config.Config{
			ListenDevs: splitArg(*argListenDevs),
			UpDev:      *argUpDev,
			Gateway:    *argGateway,
			Method:     *argMethod,
			Password:   *argPassword,
			KCP:        *argKCP,
			KCPConfig: config.KCPConfig{
				MTU:         *argKCPMTU,
				SendWindow:  *argKCPSendWindow,
				RecvWindow:  *argKCPRecvWindow,
				DataShard:   *argKCPDataShard,
				ParityShard: *argKCPParityShard,
				ACKNoDelay:  *argKCPACKNoDelay,
				NoDelay:     *argKCPNoDelay,
				Interval:    *argKCPInterval,
				Resend:      *argKCPResend,
				NC:          *argKCPNC,
			},
			Rule:    *argRule,
			Verbose: *argVerbose,
			Log:     *argLog,
			Port:    *argPort,
		}
	}

	// Log
	log.SetVerbose(cfg.Verbose)
	err = log.SetLog(cfg.Log)
	if err != nil {
		log.Fatalln(fmt.Errorf("log %s: %w", cfg.Log, err))
	}
	if cfg.Log != "" {
		log.Infof("Save log to file %s\n", cfg.Log)
	}

	// Check permission
	if runtime.GOOS != "windows" {
		if os.Geteuid() != 0 {
			ex, err := os.Executable()
			if err != nil {
				ex = "path_to_ikago"
			}

			log.Infoln("You are running IkaGo as non-root, if IkaGo does not work, run")
			log.Infof("  sudo setcap cap_net_raw+ep %s\n", ex)
			log.Infoln("  before opening IkaGo, or just run as root with sudo.")
		}
	}

	// Exclusive commands
	if *argListDevs {
		log.Infoln("Available devices are listed below, use -listen-devices [devices] or -upstream-device [device] to designate device:")
		devs, err := pcap.FindAllDevs()
		if err != nil {
			log.Fatalln(fmt.Errorf("list devices: %w", err))
		}
		for _, dev := range devs {
			log.Infof("  %s\n", dev)
		}
		os.Exit(0)
	}

	// Verify parameters
	if cfg.Port == 0 {
		log.Fatalln("Please provide listen port by -p port.")
	}
	if cfg.Gateway != "" {
		gateway = net.ParseIP(cfg.Gateway)
		if gateway == nil {
			log.Fatalln(fmt.Errorf("invalid gateway %s", cfg.Gateway))
		}
	}
	if cfg.KCPConfig.MTU > 1500 {
		log.Fatalln(fmt.Errorf("kcp mtu %d out of range", cfg.KCPConfig.MTU))
	}
	if cfg.KCPConfig.SendWindow <= 0 || cfg.KCPConfig.SendWindow > 4294967295 {
		log.Fatalln(fmt.Errorf("kcp send window %d out of range", cfg.KCPConfig.SendWindow))
	}
	if cfg.KCPConfig.RecvWindow <= 0 || cfg.KCPConfig.RecvWindow > 4294967295 {
		log.Fatalln(fmt.Errorf("kcp receive window %d out of range", cfg.KCPConfig.RecvWindow))
	}
	if cfg.KCPConfig.DataShard < 0 {
		log.Fatalln(fmt.Errorf("kcp data shard %d out of range", cfg.KCPConfig.DataShard))
	}
	if cfg.KCPConfig.ParityShard < 0 {
		log.Fatalln(fmt.Errorf("kcp parity shard %d out of range", cfg.KCPConfig.ParityShard))
	}
	if cfg.KCPConfig.Interval < 0 {
		log.Fatalln(fmt.Errorf("kcp interval %d out of range", cfg.KCPConfig.Interval))
	}
	if cfg.KCPConfig.Resend < 0 {
		log.Fatalln(fmt.Errorf("kcp resend %d out of range", cfg.KCPConfig.Resend))
	}
	if cfg.KCPConfig.NC < 0 {
		log.Fatalln(fmt.Errorf("kcp nc %d out of range", cfg.KCPConfig.NC))
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		log.Fatalln(fmt.Errorf("listen port %d out of range", cfg.Port))
	}

	// Port
	port = uint16(cfg.Port)

	// Add firewall rule
	if cfg.Rule {
		err := exec.AddGlobalFirewallRule()
		if err != nil {
			log.Fatalln(fmt.Errorf("add firewall rule: %w", err))
		}

		log.Infoln("Add firewall rule")
	}

	// Crypt
	crypt, err = crypto.ParseCrypt(cfg.Method, cfg.Password)
	if err != nil {
		log.Fatalln(fmt.Errorf("parse crypt: %w", err))
	}
	method := crypt.Method()
	if method != crypto.MethodPlain {
		log.Infof("Encrypt with %s...Cost %d Bytes\n", method, crypt.Cost())
	}

	// KCP
	isKCP = cfg.KCP
	kcpConfig = &cfg.KCPConfig
	if isKCP {
		log.Infoln("Enable KCP...Cost 24 Bytes")
	}

	log.Infof("Proxy from :%d\n", cfg.Port)

	// Find devices
	listenDevs, err = pcap.FindListenDevs(cfg.ListenDevs)
	if err != nil {
		log.Fatalln(fmt.Errorf("find listen devices: %w", err))
	}
	if len(cfg.ListenDevs) <= 0 {
		// Remove loopback devices by default
		result := make([]*pcap.Device, 0)

		for _, dev := range listenDevs {
			if dev.IsLoop() {
				continue
			}
			result = append(result, dev)
		}

		listenDevs = result
	}
	if len(listenDevs) <= 0 {
		log.Fatalln(errors.New("cannot determine listen device"))
	}

	upDev, gatewayDev, err = pcap.FindUpstreamDevAndGatewayDev(cfg.UpDev, gateway)
	if err != nil {
		log.Fatalln(fmt.Errorf("parse: %w", err))
	}
	if upDev == nil && gatewayDev == nil {
		log.Fatalln(errors.New("cannot determine upstream device and gateway device"))
	}
	if upDev == nil {
		log.Fatalln(errors.New("cannot determine upstream device"))
	}
	if gatewayDev == nil {
		log.Fatalln(errors.New("cannot determine gateway device"))
	}

	// Wait signals
	sig := make(chan os.Signal)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		closeAll()
		log.Close()
		os.Exit(0)
	}()

	// Open pcap
	err = open()
	if err != nil {
		log.Fatalln(fmt.Errorf("open pcap: %w", err))
	}
}

func open() error {
	var err error

	// Verify
	if port <= 0 || port > 65535 {
		return fmt.Errorf("port %d out of range", port)
	}
	if len(listenDevs) <= 0 {
		return errors.New("missing listen device")
	}
	if upDev == nil {
		return errors.New("missing upstream device")
	}
	if gatewayDev == nil {
		return errors.New("missing gateway")
	}

	if len(listenDevs) == 1 {
		log.Infof("Listen on %s\n", listenDevs[0].String())
	} else {
		log.Infoln("Listen on:")
		for _, dev := range listenDevs {
			log.Infof("  %s\n", dev.String())
		}
	}
	if !gatewayDev.IsLoop() {
		log.Infof("Route upstream from %s to %s\n", upDev, gatewayDev)
	} else {
		log.Infof("Route upstream in %s\n", upDev)
	}

	for _, dev := range listenDevs {
		var err error
		var listener net.Listener

		if dev.IsLoop() {
			if isKCP {
				listener, err = pcap.ListenWithKCP(dev, dev, port, crypt, kcpConfig.DataShard, kcpConfig.ParityShard)
			} else {
				listener, err = pcap.Listen(dev, dev, port, crypt)
			}
		} else {
			if isKCP {
				listener, err = pcap.ListenWithKCP(dev, gatewayDev, port, crypt, kcpConfig.DataShard, kcpConfig.ParityShard)
			} else {
				listener, err = pcap.Listen(dev, gatewayDev, port, crypt)
			}
		}
		if err != nil {
			return fmt.Errorf("open listen device %s: %w", dev.Alias(), err)
		}

		listeners = append(listeners, listener)
	}

	// Handles for routing upstream
	upConn, err = pcap.CreateRawConn(upDev, gatewayDev, fmt.Sprintf("ip && (((tcp || udp) && not dst port %d) || icmp)", port))
	if err != nil {
		return fmt.Errorf("open upstream device %s: %w", upDev.Alias(), err)
	}

	// Start handling
	for _, listener := range listeners {
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					if isClosed {
						return
					}
					log.Errorln(fmt.Errorf("accept: %w", err))
					continue
				}

				// Tune
				switch conn.(type) {
				case *kcp.UDPSession:
					err := pcap.TuneKCP(conn.(*kcp.UDPSession), kcpConfig)
					if err != nil {
						conn.Close()
						log.Errorln(fmt.Errorf("tune: %w", err))
						continue
					}
				default:
					break
				}

				log.Infof("Connect from client %s\n", conn.RemoteAddr().String())

				go func() {
					for {
						b := make([]byte, pcap.MaxMTU)

						n, err := conn.Read(b)
						if err != nil {
							if isClosed {
								return
							}
							log.Errorln(fmt.Errorf("read listen: %w", err))
							continue
						}

						c <- pcap.ConnBytes{
							Bytes: b[:n],
							Conn:  conn,
						}
					}
				}()
			}
		}()
	}

	go func() {
		for cab := range c {
			err := handleListen(cab.Bytes, cab.Conn)
			if err != nil {
				log.Errorln(fmt.Errorf("handle listen in address %s: %w", cab.Conn.LocalAddr().String(), err))
				log.Verbosef("Source: %s\nSize: %d Bytes\n\n", cab.Conn.RemoteAddr().String(), len(cab.Bytes))
				continue
			}
		}
	}()

	for {
		packet, err := upConn.ReadPacket()
		if err != nil {
			if isClosed {
				return nil
			}
			log.Errorln(fmt.Errorf("read upstream in device %s: %w", upConn.LocalDev().Alias(), err))
			continue
		}

		err = handleUpstream(packet)
		if err != nil {
			log.Errorln(fmt.Errorf("handle upstream in device %s: %w", upConn.LocalDev().Alias(), err))
			log.Verboseln(packet)
			continue
		}
	}
}

func closeAll() {
	isClosed = true
	for _, handle := range listeners {
		if handle != nil {
			handle.Close()
		}
	}
	if upConn != nil {
		upConn.Close()
	}

	// Statistics
	log.Infof("\nOutbound statistics:\n")
	for _, node := range listenStats.Nodes() {
		indicator, err := listenStats.Indicator(node)
		if err != nil {
			log.Errorln(fmt.Errorf("statistics %s: %w", node, err))
		}

		log.Infof("%s: %s\n", node, indicator.String())
	}
	log.Infof("\nInbound statistics:\n")
	for _, node := range upStats.Nodes() {
		indicator, err := upStats.Indicator(node)
		if err != nil {
			log.Errorln(fmt.Errorf("statistics %s: %w", node, err))
		}

		log.Infof("%s: %s\n", node, indicator.String())
	}
}

func handleListen(contents []byte, conn net.Conn) error {
	var (
		err               error
		embIndicator      *pcap.PacketIndicator
		upValue           uint16
		newTransportLayer gopacket.Layer
		newNetworkLayer   gopacket.NetworkLayer
		upIP              net.IP
		newLinkLayerType  gopacket.LayerType
		newLinkLayer      gopacket.Layer
		data              []byte
		guide             pcap.NATGuide
		ni                *natIndicator
	)

	// Empty payload
	if len(contents) <= 0 {
		// return errors.New("empty payload")
		return nil
	}

	// Parse embedded packet
	embIndicator, err = pcap.ParseEmbPacket(contents)
	if err != nil {
		return fmt.Errorf("parse embedded packet: %w", err)
	}

	// Fragment
	if embIndicator.IsFrag() {
		fragIndicator, ok := listenFrags[embIndicator.NetworkId()]
		if !ok || fragIndicator == nil {
			fragIndicator = pcap.NewFragIndicator()
			listenFrags[embIndicator.NetworkId()] = fragIndicator
		}

		fragIndicator.Append(embIndicator)

		if !fragIndicator.IsCompleted() {
			return nil
		}

		embIndicator, err = fragIndicator.Concatenate()
		if err != nil {
			return fmt.Errorf("concatenate: %w", err)
		}

		listenFrags[embIndicator.NetworkId()] = nil
	}

	// Distribute port/Id by source and client address and protocol
	q := quintuple{
		src:      embIndicator.NATSrc().String(),
		dst:      conn.RemoteAddr().String(),
		protocol: embIndicator.NATProtocol(),
	}
	upValue, ok := patMap[q]
	if !ok {
		var err error

		// if ICMPv4 error is not in NAT, drop it
		if t := embIndicator.TransportLayer().LayerType(); t == layers.LayerTypeICMPv4 && !embIndicator.ICMPv4Indicator().IsQuery() {
			return errors.New("missing nat")
		}

		upValue, err = dist(embIndicator.TransportLayer().LayerType())
		if err != nil {
			return fmt.Errorf("distribute: %w", err)
		}

		patMap[q] = upValue
	}

	// Create new transport layer
	if embIndicator.TransportLayer() != nil {
		switch t := embIndicator.TransportLayer().LayerType(); t {
		case layers.LayerTypeTCP:
			tcpLayer := embIndicator.TCPLayer()
			temp := *tcpLayer
			newTransportLayer = &temp

			newTCPLayer := newTransportLayer.(*layers.TCP)

			newTCPLayer.SrcPort = layers.TCPPort(upValue)
		case layers.LayerTypeUDP:
			udpLayer := embIndicator.UDPLayer()
			temp := *udpLayer
			newTransportLayer = &temp

			newUDPLayer := newTransportLayer.(*layers.UDP)

			newUDPLayer.SrcPort = layers.UDPPort(upValue)
		case layers.LayerTypeICMPv4:
			if embIndicator.ICMPv4Indicator().IsQuery() {
				temp := *embIndicator.ICMPv4Indicator().ICMPv4Layer()
				newTransportLayer = &temp

				newICMPv4Layer := newTransportLayer.(*layers.ICMPv4)

				newICMPv4Layer.Id = upValue
			} else {
				newTransportLayer = embIndicator.ICMPv4Indicator().NewPureICMPv4Layer()

				newICMPv4Layer := newTransportLayer.(*layers.ICMPv4)

				temp := *embIndicator.ICMPv4Indicator().EmbIPv4Layer()
				newEmbIPv4Layer := &temp

				newEmbIPv4Layer.DstIP = upConn.LocalDev().IPAddr().IP

				var (
					err                  error
					newEmbTransportLayer gopacket.Layer
				)

				embTransportLayerType := embIndicator.ICMPv4Indicator().EmbTransportLayer().LayerType()
				switch embTransportLayerType {
				case layers.LayerTypeTCP:
					temp := *embIndicator.ICMPv4Indicator().EmbTCPLayer()
					newEmbTransportLayer = &temp

					newEmbTCPLayer := newEmbTransportLayer.(*layers.TCP)

					newEmbTCPLayer.DstPort = layers.TCPPort(upValue)

					err = newEmbTCPLayer.SetNetworkLayerForChecksum(newEmbIPv4Layer)
				case layers.LayerTypeUDP:
					temp := *embIndicator.ICMPv4Indicator().EmbUDPLayer()
					newEmbTransportLayer = &temp

					newEmbUDPLayer := newEmbTransportLayer.(*layers.UDP)

					newEmbUDPLayer.DstPort = layers.UDPPort(upValue)

					err = newEmbUDPLayer.SetNetworkLayerForChecksum(newEmbIPv4Layer)
				case layers.LayerTypeICMPv4:
					temp := *embIndicator.ICMPv4Indicator().EmbICMPv4Layer()
					newEmbTransportLayer = &temp

					if embIndicator.ICMPv4Indicator().IsEmbQuery() {
						newEmbICMPv4Layer := newEmbTransportLayer.(*layers.ICMPv4)

						newEmbICMPv4Layer.Id = upValue
					}
				default:
					return fmt.Errorf("create transport layer: %w", fmt.Errorf("transport layer type %s not support", embTransportLayerType))
				}
				if err != nil {
					return fmt.Errorf("create transport layer: %w", fmt.Errorf("set network layer for checksum: %w", err))
				}

				payload, err := pcap.Serialize(newEmbIPv4Layer, newEmbTransportLayer.(gopacket.SerializableLayer))
				if err != nil {
					return fmt.Errorf("create transport layer: %w", fmt.Errorf("serialize: %w", err))
				}

				newICMPv4Layer.Payload = payload
			}
		default:
			return fmt.Errorf("transport layer type %s not support", t)
		}
	}

	// Create new network layer
	switch t := embIndicator.NetworkLayer().LayerType(); t {
	case layers.LayerTypeIPv4:
		ipv4Layer := embIndicator.NetworkLayer().(*layers.IPv4)
		temp := *ipv4Layer
		newNetworkLayer = &temp

		newIPv4Layer := newNetworkLayer.(*layers.IPv4)

		newIPv4Layer.SrcIP = upConn.LocalDev().IPAddr().IP
		upIP = newIPv4Layer.SrcIP
	default:
		return fmt.Errorf("network layer type %s not support", t)
	}

	// Set network layer for transport layer
	if newTransportLayer != nil {
		switch t := newTransportLayer.LayerType(); t {
		case layers.LayerTypeTCP:
			tcpLayer := newTransportLayer.(*layers.TCP)

			err = tcpLayer.SetNetworkLayerForChecksum(newNetworkLayer)
		case layers.LayerTypeUDP:
			udpLayer := newTransportLayer.(*layers.UDP)

			err = udpLayer.SetNetworkLayerForChecksum(newNetworkLayer)
		case layers.LayerTypeICMPv4, gopacket.LayerTypeFragment:
			break
		default:
			return fmt.Errorf("transport layer type %s not support", t)
		}
		if err != nil {
			return fmt.Errorf("set network layer for checksum: %w", err)
		}
	}

	// Decide Loopback or Ethernet
	if upConn.IsLoop() {
		newLinkLayerType = layers.LayerTypeLoopback
	} else {
		newLinkLayerType = layers.LayerTypeEthernet
	}

	// Create new link layer
	switch newLinkLayerType {
	case layers.LayerTypeLoopback:
		newLinkLayer = pcap.CreateLoopbackLayer()
	case layers.LayerTypeEthernet:
		newLinkLayer, err = pcap.CreateEthernetLayer(upConn.LocalDev().HardwareAddr(), upConn.RemoteDev().HardwareAddr(), newNetworkLayer)
	default:
		return fmt.Errorf("link layer type %s not support", newLinkLayerType)
	}
	if err != nil {
		return fmt.Errorf("create link layer: %w", err)
	}

	// Serialize layers
	if newTransportLayer == nil {
		data, err = pcap.Serialize(newLinkLayer.(gopacket.SerializableLayer),
			newNetworkLayer.(gopacket.SerializableLayer))
	}

	// Serialize layers
	if newTransportLayer == nil {
		data, err = pcap.Serialize(newLinkLayer.(gopacket.SerializableLayer),
			newNetworkLayer.(gopacket.SerializableLayer),
			gopacket.Payload(embIndicator.Payload()))
	} else {
		data, err = pcap.Serialize(newLinkLayer.(gopacket.SerializableLayer),
			newNetworkLayer.(gopacket.SerializableLayer),
			newTransportLayer.(gopacket.SerializableLayer),
			gopacket.Payload(embIndicator.Payload()))
	}
	if err != nil {
		return fmt.Errorf("serialize: %w", err)
	}

	// Write packet data
	_, err = upConn.Write(data)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	if embIndicator.TransportLayer() != nil {
		// Record the source and the source device of the packet
		var addNAT bool
		switch t := embIndicator.TransportLayer().LayerType(); t {
		case layers.LayerTypeTCP:
			a := net.TCPAddr{
				IP:   upIP,
				Port: int(upValue),
			}
			guide = pcap.NATGuide{
				Src:      a.String(),
				Protocol: t,
			}
			addNAT = true
		case layers.LayerTypeUDP:
			a := net.UDPAddr{
				IP:   upIP,
				Port: int(upValue),
			}
			guide = pcap.NATGuide{
				Src:      a.String(),
				Protocol: t,
			}
			addNAT = true
		case layers.LayerTypeICMPv4:
			if embIndicator.ICMPv4Indicator().IsQuery() {
				guide = pcap.NATGuide{
					Src: addr.ICMPQueryAddr{
						IP: upIP,
						Id: upValue,
					}.String(),
					Protocol: t,
				}
				addNAT = true
			}
		default:
			return fmt.Errorf("transport layer type %s not support", t)
		}
		if addNAT {
			ni = &natIndicator{
				src:    conn.RemoteAddr(),
				embSrc: embIndicator.NATSrc(),
				conn:   conn,
			}
			natLock.Lock()
			nat[guide] = ni
			natLock.Unlock()
		}

		// Keep alive
		protocol := embIndicator.NATProtocol()
		switch protocol {
		case layers.LayerTypeTCP:
			tcpPortPool[convertFromPort(upValue)] = time.Now()
		case layers.LayerTypeUDP:
			udpPortPool[convertFromPort(upValue)] = time.Now()
		case layers.LayerTypeICMPv4:
			icmpv4IdPool[upValue] = time.Now()
		default:
			return fmt.Errorf("transport layer type %s not support", protocol)
		}
	}

	// Statistics
	listenStats.Add(conn.RemoteAddr().String(), uint(embIndicator.Size()))

	log.Verbosef("Redirect an inbound %s packet: %s -> %s -> %s (%d Bytes)\n",
		embIndicator.TransportProtocol(), embIndicator.Src().String(), conn.RemoteAddr().String(), embIndicator.Dst().String(), embIndicator.Size())

	return nil
}

func handleUpstream(packet gopacket.Packet) error {
	var (
		err               error
		indicator         *pcap.PacketIndicator
		ni                *natIndicator
		embTransportLayer gopacket.Layer
		embNetworkLayer   gopacket.NetworkLayer
		contents          []byte
	)

	// Parse packet
	indicator, err = pcap.ParsePacket(packet)
	if err != nil {
		return fmt.Errorf("parse packet: %w", err)
	}

	// Fragment
	if indicator.IsFrag() {
		fragIndicator, ok := upFrags[indicator.NetworkId()]
		if !ok || fragIndicator == nil {
			fragIndicator = pcap.NewFragIndicator()
			upFrags[indicator.NetworkId()] = fragIndicator
		}

		fragIndicator.Append(indicator)

		if !fragIndicator.IsCompleted() {
			return nil
		}

		indicator, err = fragIndicator.Concatenate()
		if err != nil {
			return fmt.Errorf("concatenate: %w", err)
		}

		upFrags[indicator.NetworkId()] = nil
	}

	// NAT
	guide := pcap.NATGuide{
		Src:      indicator.NATDst().String(),
		Protocol: indicator.TransportLayer().LayerType(),
	}
	natLock.RLock()
	ni, ok := nat[guide]
	natLock.RUnlock()
	if !ok {
		return nil
	}

	// Keep alive
	protocol := indicator.NATProtocol()
	switch protocol {
	case layers.LayerTypeTCP:
		tcpPortPool[convertFromPort(indicator.DstPort())] = time.Now()
	case layers.LayerTypeUDP:
		udpPortPool[convertFromPort(indicator.DstPort())] = time.Now()
	case layers.LayerTypeICMPv4:
		icmpv4IdPool[indicator.ICMPv4Indicator().Id()] = time.Now()
	default:
		return fmt.Errorf("transport layer type %s not support", protocol)
	}

	// Create embedded transport layer
	if indicator.TransportLayer() != nil {
		switch t := indicator.TransportLayer().LayerType(); t {
		case layers.LayerTypeTCP:
			embTCPLayer := indicator.TCPLayer()
			temp := *embTCPLayer
			embTransportLayer = &temp

			newEmbTCPLayer := embTransportLayer.(*layers.TCP)

			newEmbTCPLayer.DstPort = layers.TCPPort(ni.embSrc.(*net.TCPAddr).Port)
		case layers.LayerTypeUDP:
			embUDPLayer := indicator.UDPLayer()
			temp := *embUDPLayer
			embTransportLayer = &temp

			newEmbUDPLayer := embTransportLayer.(*layers.UDP)

			newEmbUDPLayer.DstPort = layers.UDPPort(ni.embSrc.(*net.UDPAddr).Port)
		case layers.LayerTypeICMPv4:
			if indicator.ICMPv4Indicator().IsQuery() {
				embICMPv4Layer := indicator.ICMPv4Indicator().ICMPv4Layer()
				temp := *embICMPv4Layer
				embTransportLayer = &temp

				newEmbICMPv4Layer := embTransportLayer.(*layers.ICMPv4)

				newEmbICMPv4Layer.Id = ni.embSrc.(*addr.ICMPQueryAddr).Id
			} else {
				embTransportLayer = indicator.ICMPv4Indicator().NewPureICMPv4Layer()

				newEmbICMPv4Layer := embTransportLayer.(*layers.ICMPv4)

				temp := *indicator.ICMPv4Indicator().EmbIPv4Layer()
				newEmbEmbIPv4Layer := &temp

				newEmbEmbIPv4Layer.SrcIP = ni.embSrcIP()

				var (
					err                     error
					newEmbEmbTransportLayer gopacket.Layer
				)

				switch t := indicator.ICMPv4Indicator().EmbTransportLayer().LayerType(); t {
				case layers.LayerTypeTCP:
					temp := *indicator.ICMPv4Indicator().EmbTCPLayer()
					newEmbEmbTransportLayer = &temp

					newEmbEmbTCPLayer := newEmbEmbTransportLayer.(*layers.TCP)

					newEmbEmbTCPLayer.SrcPort = layers.TCPPort(ni.embSrc.(*net.TCPAddr).Port)

					err = newEmbEmbTCPLayer.SetNetworkLayerForChecksum(newEmbEmbIPv4Layer)
				case layers.LayerTypeUDP:
					temp := *indicator.ICMPv4Indicator().EmbUDPLayer()
					newEmbEmbTransportLayer = &temp

					newEmbEmbUDPLayer := newEmbEmbTransportLayer.(*layers.UDP)

					newEmbEmbUDPLayer.SrcPort = layers.UDPPort(ni.embSrc.(*net.UDPAddr).Port)

					err = newEmbEmbUDPLayer.SetNetworkLayerForChecksum(newEmbEmbIPv4Layer)
				case layers.LayerTypeICMPv4:
					temp := *indicator.ICMPv4Indicator().EmbICMPv4Layer()
					newEmbEmbTransportLayer = &temp

					if indicator.ICMPv4Indicator().IsEmbQuery() {
						newEmbEmbICMPv4Layer := newEmbEmbTransportLayer.(*layers.ICMPv4)

						newEmbEmbICMPv4Layer.Id = ni.embSrc.(*addr.ICMPQueryAddr).Id
					}
				default:
					return fmt.Errorf("create embedded transport layer: %w", fmt.Errorf("transport layer type %s not support", t))
				}
				if err != nil {
					return fmt.Errorf("create embedded transport layer: %w", fmt.Errorf("set network layer for checksum: %w", err))
				}

				payload, err := pcap.Serialize(newEmbEmbIPv4Layer, newEmbEmbTransportLayer.(gopacket.SerializableLayer))
				if err != nil {
					return fmt.Errorf("create embedded transport layer: %w", fmt.Errorf("serialize: %w", err))
				}

				newEmbICMPv4Layer.Payload = payload
			}
		default:
			return fmt.Errorf("embedded transport layer type %s not support", t)
		}
	}

	// Create embedded network layer
	switch t := indicator.NetworkLayer().LayerType(); t {
	case layers.LayerTypeIPv4:
		embIPv4Layer := indicator.IPv4Layer()
		temp := *embIPv4Layer
		embNetworkLayer = &temp

		newEmbIPv4Layer := embNetworkLayer.(*layers.IPv4)

		newEmbIPv4Layer.DstIP = ni.embSrcIP()
	default:
		return fmt.Errorf("embedded network layer type %s not support", t)
	}

	// Set network layer for transport layer
	if embTransportLayer != nil {
		switch t := embTransportLayer.LayerType(); t {
		case layers.LayerTypeTCP:
			embTCPLayer := embTransportLayer.(*layers.TCP)

			err = embTCPLayer.SetNetworkLayerForChecksum(embNetworkLayer)
		case layers.LayerTypeUDP:
			embUDPLayer := embTransportLayer.(*layers.UDP)

			err = embUDPLayer.SetNetworkLayerForChecksum(embNetworkLayer)
		case layers.LayerTypeICMPv4:
			break
		default:
			return fmt.Errorf("embedded transport layer type %s not support", t)
		}
		if err != nil {
			return fmt.Errorf("set embedded network layer for checksum: %w", err)
		}
	}

	// Construct contents of new application layer
	if embTransportLayer == nil {
		contents, err = pcap.Serialize(embNetworkLayer.(gopacket.SerializableLayer),
			gopacket.Payload(indicator.Payload()))
	} else {
		contents, err = pcap.Serialize(embNetworkLayer.(gopacket.SerializableLayer),
			embTransportLayer.(gopacket.SerializableLayer),
			gopacket.Payload(indicator.Payload()))
	}
	if err != nil {
		return fmt.Errorf("serialize embedded: %w", err)
	}

	// Write packet data
	n, err := ni.conn.Write(contents)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	// Statistics
	upStats.Add(ni.conn.RemoteAddr().String(), uint(n))

	log.Verbosef("Redirect an outbound %s packet: %s <- %s <- %s (%d Bytes)\n",
		indicator.TransportProtocol(), ni.embSrc.String(), ni.src.String(), indicator.Src(), n)

	return nil
}

func dist(t gopacket.LayerType) (uint16, error) {
	now := time.Now()

	switch t {
	case layers.LayerTypeTCP:
		for i := 0; i < 16384; i++ {
			s := nextTCPPort % 16384

			// Point to next port
			nextTCPPort++

			// Check if the port is alive
			last := tcpPortPool[s]
			if now.Sub(last).Seconds() > keepAlive {
				return 49152 + s, nil
			}
		}
	case layers.LayerTypeUDP:
		for i := 0; i < 16384; i++ {
			s := nextUDPPort % 16384

			// Point to next port
			nextUDPPort++

			// Check if the port is alive
			last := udpPortPool[s]
			if now.Sub(last).Seconds() > keepAlive {
				return 49152 + s, nil
			}
		}
	case layers.LayerTypeICMPv4:
		for i := 0; i < 65536; i++ {
			s := nextICMPv4Id

			// Point to next Id
			nextICMPv4Id++

			// Check if the Id is alive
			last := icmpv4IdPool[s]
			if now.Sub(last).Seconds() > keepAlive {
				return s, nil
			}
		}
	default:
		return 0, fmt.Errorf("transport layer type %s not support", t)
	}

	return 0, fmt.Errorf("%s pool empty", t)
}

func convertFromPort(port uint16) uint16 {
	return port - 49152
}

func splitArg(s string) []string {
	if s == "" {
		return nil
	}

	result := make([]string, 0)

	strs := strings.Split(s, ",")

	for _, str := range strs {
		result = append(result, strings.Trim(str, " "))
	}

	return result
}
