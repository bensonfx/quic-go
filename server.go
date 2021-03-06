package quic

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/phuslu/quic-go/internal/crypto"
	"github.com/phuslu/quic-go/internal/handshake"
	"github.com/phuslu/quic-go/internal/protocol"
	"github.com/phuslu/quic-go/internal/utils"
	"github.com/phuslu/quic-go/internal/wire"
	"github.com/phuslu/quic-go/qerr"
)

// packetHandler handles packets
type packetHandler interface {
	Session
	getCryptoStream() cryptoStreamI
	handlePacket(*receivedPacket)
	GetVersion() protocol.VersionNumber
	run() error
	closeRemote(error)
}

type sessionRunner interface {
	onHandshakeComplete(packetHandler)
	removeConnectionID(protocol.ConnectionID)
}

type runner struct {
	onHandshakeCompleteImpl func(packetHandler)
	removeConnectionIDImpl  func(protocol.ConnectionID)
}

func (r *runner) onHandshakeComplete(p packetHandler)        { r.onHandshakeCompleteImpl(p) }
func (r *runner) removeConnectionID(c protocol.ConnectionID) { r.removeConnectionIDImpl(c) }

var _ sessionRunner = &runner{}

type sessionHandler interface {
	Add(protocol.ConnectionID, packetHandler)
	Get(protocol.ConnectionID) (packetHandler, bool)
	Remove(protocol.ConnectionID)
	Close()
}

// A Listener of QUIC
type server struct {
	tlsConf *tls.Config
	config  *Config

	conn net.PacketConn

	supportsTLS bool
	serverTLS   *serverTLS

	certChain crypto.CertChain
	scfg      *handshake.ServerConfig

	sessionHandler sessionHandler

	serverError error

	sessionQueue chan Session
	errorChan    chan struct{}

	sessionRunner sessionRunner
	// set as a member, so they can be set in the tests
	newSession func(connection, sessionRunner, protocol.VersionNumber, protocol.ConnectionID, *handshake.ServerConfig, *tls.Config, *Config, utils.Logger) (packetHandler, error)

	logger utils.Logger
}

var _ Listener = &server{}

// ListenAddr creates a QUIC server listening on a given address.
// The tls.Config must not be nil, the quic.Config may be nil.
func ListenAddr(addr string, tlsConf *tls.Config, config *Config) (Listener, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	return Listen(conn, tlsConf, config)
}

// Listen listens for QUIC connections on a given net.PacketConn.
// The tls.Config must not be nil, the quic.Config may be nil.
func Listen(conn net.PacketConn, tlsConf *tls.Config, config *Config) (Listener, error) {
	certChain := crypto.NewCertChain(tlsConf)
	kex, err := crypto.NewCurve25519KEX()
	if err != nil {
		return nil, err
	}
	scfg, err := handshake.NewServerConfig(kex, certChain)
	if err != nil {
		return nil, err
	}
	config = populateServerConfig(config)

	var supportsTLS bool
	for _, v := range config.Versions {
		if !protocol.IsValidVersion(v) {
			return nil, fmt.Errorf("%s is not a valid QUIC version", v)
		}
		// check if any of the supported versions supports TLS
		if v.UsesTLS() {
			supportsTLS = true
			break
		}
	}

	s := &server{
		conn:           conn,
		tlsConf:        tlsConf,
		config:         config,
		certChain:      certChain,
		scfg:           scfg,
		newSession:     newSession,
		sessionHandler: newSessionMap(),
		sessionQueue:   make(chan Session, 5),
		errorChan:      make(chan struct{}),
		supportsTLS:    supportsTLS,
		logger:         utils.DefaultLogger.WithPrefix("server"),
	}
	s.setup()
	if supportsTLS {
		if err := s.setupTLS(); err != nil {
			return nil, err
		}
	}
	go s.serve()
	s.logger.Debugf("Listening for %s connections on %s", conn.LocalAddr().Network(), conn.LocalAddr().String())
	return s, nil
}

func (s *server) setup() {
	s.sessionRunner = &runner{
		onHandshakeCompleteImpl: func(sess packetHandler) { s.sessionQueue <- sess },
		removeConnectionIDImpl:  s.sessionHandler.Remove,
	}
}

func (s *server) setupTLS() error {
	cookieHandler, err := handshake.NewCookieHandler(s.config.AcceptCookie, s.logger)
	if err != nil {
		return err
	}
	serverTLS, sessionChan, err := newServerTLS(s.conn, s.config, s.sessionRunner, cookieHandler, s.tlsConf, s.logger)
	if err != nil {
		return err
	}
	s.serverTLS = serverTLS
	// handle TLS connection establishment statelessly
	go func() {
		for {
			select {
			case <-s.errorChan:
				return
			case tlsSession := <-sessionChan:
				sess := tlsSession.sess
				// The connection ID is a randomly chosen 8 byte value.
				// It is safe to assume that it doesn't collide with other randomly chosen values.
				s.sessionHandler.Add(tlsSession.connID, sess)
				go sess.run()
			}
		}
	}()
	return nil
}

var defaultAcceptCookie = func(clientAddr net.Addr, cookie *Cookie) bool {
	if cookie == nil {
		return false
	}
	if time.Now().After(cookie.SentTime.Add(protocol.CookieExpiryTime)) {
		return false
	}
	var sourceAddr string
	if udpAddr, ok := clientAddr.(*net.UDPAddr); ok {
		sourceAddr = udpAddr.IP.String()
	} else {
		sourceAddr = clientAddr.String()
	}
	return sourceAddr == cookie.RemoteAddr
}

// populateServerConfig populates fields in the quic.Config with their default values, if none are set
// it may be called with nil
func populateServerConfig(config *Config) *Config {
	if config == nil {
		config = &Config{}
	}
	versions := config.Versions
	if len(versions) == 0 {
		versions = protocol.SupportedVersions
	}

	vsa := defaultAcceptCookie
	if config.AcceptCookie != nil {
		vsa = config.AcceptCookie
	}

	handshakeTimeout := protocol.DefaultHandshakeTimeout
	if config.HandshakeTimeout != 0 {
		handshakeTimeout = config.HandshakeTimeout
	}
	idleTimeout := protocol.DefaultIdleTimeout
	if config.IdleTimeout != 0 {
		idleTimeout = config.IdleTimeout
	}

	maxReceiveStreamFlowControlWindow := config.MaxReceiveStreamFlowControlWindow
	if maxReceiveStreamFlowControlWindow == 0 {
		maxReceiveStreamFlowControlWindow = protocol.DefaultMaxReceiveStreamFlowControlWindowServer
	}
	maxReceiveConnectionFlowControlWindow := config.MaxReceiveConnectionFlowControlWindow
	if maxReceiveConnectionFlowControlWindow == 0 {
		maxReceiveConnectionFlowControlWindow = protocol.DefaultMaxReceiveConnectionFlowControlWindowServer
	}
	maxIncomingStreams := config.MaxIncomingStreams
	if maxIncomingStreams == 0 {
		maxIncomingStreams = protocol.DefaultMaxIncomingStreams
	} else if maxIncomingStreams < 0 {
		maxIncomingStreams = 0
	}
	maxIncomingUniStreams := config.MaxIncomingUniStreams
	if maxIncomingUniStreams == 0 {
		maxIncomingUniStreams = protocol.DefaultMaxIncomingUniStreams
	} else if maxIncomingUniStreams < 0 {
		maxIncomingUniStreams = 0
	}

	return &Config{
		Versions:                              versions,
		HandshakeTimeout:                      handshakeTimeout,
		IdleTimeout:                           idleTimeout,
		AcceptCookie:                          vsa,
		KeepAlive:                             config.KeepAlive,
		MaxReceiveStreamFlowControlWindow:     maxReceiveStreamFlowControlWindow,
		MaxReceiveConnectionFlowControlWindow: maxReceiveConnectionFlowControlWindow,
		MaxIncomingStreams:                    maxIncomingStreams,
		MaxIncomingUniStreams:                 maxIncomingUniStreams,
	}
}

// serve listens on an existing PacketConn
func (s *server) serve() {
	for {
		data := *getPacketBuffer()
		data = data[:protocol.MaxReceivePacketSize]
		// The packet size should not exceed protocol.MaxReceivePacketSize bytes
		// If it does, we only read a truncated packet, which will then end up undecryptable
		n, remoteAddr, err := s.conn.ReadFrom(data)
		if err != nil {
			s.serverError = err
			close(s.errorChan)
			_ = s.Close()
			return
		}
		data = data[:n]
		if err := s.handlePacket(remoteAddr, data); err != nil {
			s.logger.Errorf("error handling packet: %s", err.Error())
		}
	}
}

// Accept returns newly openend sessions
func (s *server) Accept() (Session, error) {
	var sess Session
	select {
	case sess = <-s.sessionQueue:
		return sess, nil
	case <-s.errorChan:
		return nil, s.serverError
	}
}

// Close the server
func (s *server) Close() error {
	s.sessionHandler.Close()
	err := s.conn.Close()
	<-s.errorChan // wait for serve() to return
	return err
}

// Addr returns the server's network address
func (s *server) Addr() net.Addr {
	return s.conn.LocalAddr()
}

func (s *server) handlePacket(remoteAddr net.Addr, packet []byte) error {
	rcvTime := time.Now()

	r := bytes.NewReader(packet)
	hdr, err := wire.ParseHeaderSentByClient(r)
	if err != nil {
		return qerr.Error(qerr.InvalidPacketHeader, err.Error())
	}
	hdr.Raw = packet[:len(packet)-r.Len()]
	packetData := packet[len(packet)-r.Len():]

	if hdr.IsPublicHeader {
		return s.handleGQUICPacket(hdr, packetData, remoteAddr, rcvTime)
	}
	return s.handleIETFQUICPacket(hdr, packetData, remoteAddr, rcvTime)
}

func (s *server) handleIETFQUICPacket(hdr *wire.Header, packetData []byte, remoteAddr net.Addr, rcvTime time.Time) error {
	if hdr.IsLongHeader {
		if !s.supportsTLS {
			return errors.New("Received an IETF QUIC Long Header")
		}
		if protocol.ByteCount(len(packetData)) < hdr.PayloadLen {
			return fmt.Errorf("packet payload (%d bytes) is smaller than the expected payload length (%d bytes)", len(packetData), hdr.PayloadLen)
		}
		packetData = packetData[:int(hdr.PayloadLen)]
		// TODO(#1312): implement parsing of compound packets

		switch hdr.Type {
		case protocol.PacketTypeInitial:
			go s.serverTLS.HandleInitial(remoteAddr, hdr, packetData)
			return nil
		case protocol.PacketTypeHandshake:
			// nothing to do here. Packet will be passed to the session.
		default:
			// Note that this also drops 0-RTT packets.
			return fmt.Errorf("Received unsupported packet type: %s", hdr.Type)
		}
	}

	session, sessionKnown := s.sessionHandler.Get(hdr.DestConnectionID)
	if sessionKnown && session == nil {
		// Late packet for closed session
		return nil
	}
	if !sessionKnown {
		s.logger.Debugf("Received %s packet for unknown connection %s.", hdr.Type, hdr.DestConnectionID)
		return nil
	}

	session.handlePacket(&receivedPacket{
		remoteAddr: remoteAddr,
		header:     hdr,
		data:       packetData,
		rcvTime:    rcvTime,
	})
	return nil
}

func (s *server) handleGQUICPacket(hdr *wire.Header, packetData []byte, remoteAddr net.Addr, rcvTime time.Time) error {
	// ignore all Public Reset packets
	if hdr.ResetFlag {
		s.logger.Infof("Received unexpected Public Reset for connection %s.", hdr.DestConnectionID)
		return nil
	}

	session, sessionKnown := s.sessionHandler.Get(hdr.DestConnectionID)
	if sessionKnown && session == nil {
		// Late packet for closed session
		return nil
	}

	// If we don't have a session for this connection, and this packet cannot open a new connection, send a Public Reset
	// This should only happen after a server restart, when we still receive packets for connections that we lost the state for.
	if !sessionKnown && !hdr.VersionFlag {
		_, err := s.conn.WriteTo(wire.WritePublicReset(hdr.DestConnectionID, 0, 0), remoteAddr)
		return err
	}

	// a session is only created once the client sent a supported version
	// if we receive a packet for a connection that already has session, it's probably an old packet that was sent by the client before the version was negotiated
	// it is safe to drop it
	if sessionKnown && hdr.VersionFlag && !protocol.IsSupportedVersion(s.config.Versions, hdr.Version) {
		return nil
	}

	// send a Version Negotiation Packet if the client is speaking a different protocol version
	// since the client send a Public Header (only gQUIC has a Version Flag), we need to send a gQUIC Version Negotiation Packet
	if hdr.VersionFlag && !protocol.IsSupportedVersion(s.config.Versions, hdr.Version) {
		// drop packets that are too small to be valid first packets
		if len(packetData) < protocol.MinClientHelloSize {
			return errors.New("dropping small packet with unknown version")
		}
		s.logger.Infof("Client offered version %s, sending Version Negotiation Packet", hdr.Version)
		_, err := s.conn.WriteTo(wire.ComposeGQUICVersionNegotiation(hdr.SrcConnectionID, s.config.Versions), remoteAddr)
		return err
	}

	if !sessionKnown {
		// This is (potentially) a Client Hello.
		// Make sure it has the minimum required size before spending any more ressources on it.
		if len(packetData) < protocol.MinClientHelloSize {
			return errors.New("dropping small packet for unknown connection")
		}

		version := hdr.Version
		if !protocol.IsSupportedVersion(s.config.Versions, version) {
			return errors.New("Server BUG: negotiated version not supported")
		}

		s.logger.Infof("Serving new connection: %s, version %s from %v", hdr.DestConnectionID, version, remoteAddr)
		var err error
		session, err = s.newSession(
			&conn{pconn: s.conn, currentAddr: remoteAddr},
			s.sessionRunner,
			version,
			hdr.DestConnectionID,
			s.scfg,
			s.tlsConf,
			s.config,
			s.logger,
		)
		if err != nil {
			return err
		}
		s.sessionHandler.Add(hdr.DestConnectionID, session)

		go session.run()
	}

	session.handlePacket(&receivedPacket{
		remoteAddr: remoteAddr,
		header:     hdr,
		data:       packetData,
		rcvTime:    rcvTime,
	})
	return nil
}
