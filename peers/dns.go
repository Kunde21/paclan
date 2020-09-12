package peers

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/Kunde21/paclan/config"
	"github.com/schollz/peerdiscovery"
	"github.com/sirupsen/logrus"
)

type Announce struct {
	Port     string `json:"port"`
	Tag      string `json:"tag"`
	Register bool   `json:"reg"`
}

type DNS struct {
	mu             *sync.Mutex
	settings       peerdiscovery.Settings
	closed         bool
	netIface       string
	port           string
	multicastAddr  string
	multicastPort  string
	broadcastDelay time.Duration
	ipFilter       *net.IP
	payload        []byte
	log            *logrus.Entry
	peerMap
}

// New creates a list of local servers
func New(conf *config.Paclan) *DNS {
	return &DNS{
		mu:             &sync.Mutex{},
		netIface:       conf.IFace,
		port:           conf.HTTPPort,
		peerMap:        newPeerMap(conf.PeerTimeout),
		multicastAddr:  conf.MulticastAddr,
		multicastPort:  conf.MulticastPort,
		broadcastDelay: conf.PeerTimeout / 4,
		log:            logrus.New().WithField("service", "discovery"),
	}
}

// Close the connection
func (lhd *DNS) Close() {
	lhd.mu.Lock()
	defer lhd.mu.Unlock()
	if !lhd.closed {
		close(lhd.settings.StopChan)
		lhd.closed = true
	}
}

// Serve registers peers on the LAN
func (lhd *DNS) Serve(ctx context.Context) {
	go func() {
		log := lhd.log.WithField("peer_listen", lhd.port)
		for {
			if err := lhd.Listen(ctx); err != nil {
				log.WithError(err).Error("listen error")
			}
			if ctx.Err() != nil {
				log.Info("exiting")
				return
			}
		}
	}()
	go func() {
		log := lhd.log.WithField("peer", "self registration")
		for {
			if err := lhd.RegisterSelf(ctx); err != nil {
				log.WithError(err).Error("multicast error")
			}
			if ctx.Err() != nil {
				log.Info("exiting")
				return
			}
		}
	}()
}

// RegisterSelf with the rest of the network and listen for local peers.
func (lhd *DNS) RegisterSelf(ctx context.Context) error {
	ip, err := getAddr(lhd.log, lhd.netIface)
	if err != nil {
		return err
	}
	lhd.mu.Lock()
	lhd.ipFilter = &ip
	lhd.mu.Unlock()
	payload, err := json.Marshal(&Announce{Port: lhd.port, Register: true})
	if err != nil {
		return err
	}
	lhd.payload = payload
	payloadDisc, err := json.Marshal(&Announce{Port: lhd.port})
	if err != nil {
		return err
	}
	if len(payload) == 0 || len(payloadDisc) == 0 {
		return errors.New("INVALID PAYLOAD")
	}
	stg := &lhd.settings
	stg.MulticastAddress = lhd.multicastAddr
	stg.Port = lhd.multicastPort
	stg.Payload = payloadDisc
	stg.Limit, stg.TimeLimit = -1, -1 // discover forever
	stg.StopChan = make(chan struct{})
	stg.Notify = lhd.discovered(lhd.port)
	stg.Delay = lhd.broadcastDelay
	lhd.log.Info("discovery started")
	go func() {
		<-ctx.Done()
		lhd.Close()
	}()
	if _, err := peerdiscovery.Discover(*stg); err != nil {
		return err
	}
	return nil
}

// discovered a new peer.  Register and respond.
func (lhd DNS) discovered(multicastPort string) func(disc peerdiscovery.Discovered) {
	return func(disc peerdiscovery.Discovered) {
		frIP := net.ParseIP(disc.Address)
		if frIP.Equal(*lhd.ipFilter) {
			return
		}
		log := lhd.log.WithField("discovered", frIP.String())
		var msg Announce
		if err := json.Unmarshal(disc.Payload, &msg); err != nil {
			log.WithError(err).Error("json invalid")
			return
		}
		port, err := strconv.Atoi(msg.Port)
		switch {
		case err != nil, port < 1, port > 65535:
			log.WithField("port", msg.Port).Error("invalid port")
			return
		}
		lhd.add(log, net.JoinHostPort(frIP.String(), strconv.Itoa(port)))
		if msg.Register {
			return
		}
		log = log.WithField("addr", disc.Address)

		addr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(disc.Address, multicastPort))
		if err != nil {
			log.WithError(err).Error("resolution failed")
			return
		}
		conn, err := net.DialUDP("udp4", nil, addr)
		if err != nil {
			log.WithError(err).Error("dial failed")
			return
		}
		defer conn.Close()

		msg.Port = lhd.port
		msg.Register = true
		if err := json.NewEncoder(conn).Encode(&msg); err != nil {
			log.WithError(err).Error("write failed")
			return
		}
	}
}

// Listen for response announcements.
func (lhd DNS) Listen(ctx context.Context) error {
	log := lhd.log.WithField("peer", "registration listener")
	IP, err := getAddr(log, lhd.netIface)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(lhd.port)
	if err != nil {
		return err
	}
	IP[15] = 0
	addr := &net.UDPAddr{
		IP:   IP,
		Port: port,
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	payload, oob := []byte{}, []byte{}
	for {
		_, _, _, addr, err := conn.ReadMsgUDP(payload, oob)
		if err != nil {
			log.WithError(err).Error("read failure")
			continue
		}
		if len(payload) == 0 {
			continue
		}
		var msg Announce
		if err := json.Unmarshal(payload, &msg); err != nil {
			log.WithError(err).WithField("payload", string(payload)).Error("invalid json")
			continue
		}
		log.WithField("message", msg).Info("received")
		port, err := strconv.Atoi(msg.Port)
		switch {
		case err != nil, port < 1, port > 65535:
			log.WithFields(logrus.Fields{
				"ip":   addr.IP.String(),
				"port": msg.Port,
			}).Error("invalid port")
			continue
		}
		lhd.add(log, net.JoinHostPort(addr.IP.String(), strconv.Itoa(port)))
		payload = payload[:0]
		oob = oob[:0]
	}
}

// getAddr finds an external address to advertise.
func getAddr(log *logrus.Entry, ifaceName string) (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	if ifaceName != "" {
		iface, err := net.InterfaceByName(ifaceName)
		if err != nil {
			return nil, err
		}
		ifaces = []net.Interface{*iface}
	}
	for _, i := range ifaces {
		switch i.Name {
		case "docker0", "lo":
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			log.WithError(err).WithField("iface", i.Name).Error("fetch addresses")
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				if _, b := v.Mask.Size(); b != 32 {
					continue
				}
				ip = []byte(v.IP)
			case *net.IPAddr:
				ip = []byte(v.IP)
			}
			if ip[12] == 127 {
				continue
			}
			return ip, nil
		}
	}
	return nil, errors.New("no external interface")
}
